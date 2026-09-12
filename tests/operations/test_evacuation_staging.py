import hashlib
import importlib.util
import json
import os
import shutil
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location(
    "evacuation_staging", ROOT / "scripts/operations/evacuation_staging.py"
)
staging = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(staging)


def fixture_plan(root):
    users = {
        name: {
            "uid": uid,
            "gid": uid,
            "targetSubIdStart": 165536 + (uid - 2000) * 65536,
            "subIdCount": 65536,
        }
        for name, uid in staging.USERS.items()
    }
    services = {
        name: {
            "user": user,
            "image": "ghcr.io/fredrir/fixture@sha256:" + "a" * 64,
            "memoryMaxBytes": 256 * 1024**2,
        }
        for name, user in staging.SERVICE_USERS.items()
    }
    plan = {
        "source": {"id": "fredrir-05", "providerId": 132168416},
        "target": {"id": "fredrir-09", "architecture": "amd64"},
        "authorization": {"targetInstall": True, "sourceStop": False, "cutover": False},
        "users": users,
        "services": services,
        "capacity": {
            "preservedAggregateServiceMemoryMaxBytes": 6 * 256 * 1024**2,
            "userSliceMemoryMaxMiB": {
                "edge": 768,
                "llunde-backend": 2560,
                "llunde-frontend": 768,
            },
        },
        "unitSHA256": {},
    }
    for name, service in services.items():
        path = root / "units" / service["user"] / f"{name}.container"
        path.parent.mkdir(parents=True, exist_ok=True)
        contents = "[Unit]\nConditionPathExists=/var/lib/infra-evacuation/llunde/stage-approved\n"
        if name == "llunde-backend":
            contents += (
                "ConditionPathExists=/var/lib/infra-evacuation/llunde/source-fenced\n"
            )
        contents += f"[Container]\nImage={service['image']}\n"
        if name in ("llunde-postgres", "llunde-valkey", "llunde-backend"):
            contents += "Network=llunde-backend-data.network\n"
        if name == "llunde-backend":
            contents += "Network=llunde-backend-egress.network\n"
        contents += "[Service]\nMemoryMax=256M\n"
        path.write_text(contents)
    (root / "units/Caddyfile").write_text(
        "http://:8085 {\n bind 127.0.0.1\n respond 200\n}\n"
    )
    (root / "units/llunde-backend/llunde-backend-data.network").write_text(
        "[Network]\nInternal=true\n"
    )
    (root / "units/llunde-backend/llunde-backend-egress.network").write_text(
        staging.EGRESS_CONTENT
    )
    for path in (root / "units").rglob("*"):
        if path.is_file():
            plan["unitSHA256"][str(path.relative_to(root))] = hashlib.sha256(
                path.read_bytes()
            ).hexdigest()
    (root / "staging.json").write_text(json.dumps(plan))
    return plan


class StagingPlanTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.plan = fixture_plan(self.root)

    def save(self):
        (self.root / "staging.json").write_text(json.dumps(self.plan))

    def test_dormant_plan_accepts_exact_candidates_and_rejects_activation(self):
        self.assertEqual(staging.validate_plan(self.root), self.plan)
        for field in ("sourceStop", "cutover"):
            with self.subTest(field=field):
                self.plan["authorization"][field] = True
                self.save()
                with self.assertRaises(staging.StagingError):
                    staging.validate_plan(self.root)
                self.plan["authorization"][field] = False

    def test_unlisted_candidate_and_symlink_are_rejected(self):
        path = self.root / "units/edge/unreviewed.container"
        path.write_text("[Container]\nImage=unreviewed\n")
        with self.assertRaises(staging.StagingError):
            staging.validate_plan(self.root)
        path.unlink()
        path.symlink_to(self.root / "staging.json")
        with self.assertRaises(staging.StagingError):
            staging.validate_plan(self.root)

    def test_rehashed_candidate_cannot_remove_fence_or_change_memory(self):
        path = self.root / "units/llunde-backend/llunde-backend.container"
        original = path.read_text()
        for modified in (
            original.replace(
                "ConditionPathExists=/var/lib/infra-evacuation/llunde/source-fenced\n",
                "",
            ),
            original.replace("MemoryMax=256M", "MemoryMax=1024M"),
        ):
            with self.subTest(contents=modified):
                path.write_text(modified)
                self.plan["unitSHA256"][str(path.relative_to(self.root))] = (
                    hashlib.sha256(path.read_bytes()).hexdigest()
                )
                self.save()
                with self.assertRaises(staging.StagingError):
                    staging.validate_plan(self.root)

    def test_existing_subordinate_ranges_are_preserved_or_refused(self):
        existing = "administrator:100000:65536\n"
        staging.validate_accounts(
            self.plan,
            "administrator:x:1000:1000::/home/administrator:/bin/bash\n",
            "administrator:x:1000:\n",
            existing,
            existing,
        )
        complete = existing + "".join(
            f"{name}:{user['targetSubIdStart']}:65536\n"
            for name, user in self.plan["users"].items()
        )
        staging.validate_accounts(self.plan, "", "", complete, complete)
        for bad in (
            "other:200000:65536\n",
            "edge:100000:65536\n",
            complete + "edge:500000:65536\n",
        ):
            with self.subTest(mapping=bad), self.assertRaises(staging.StagingError):
                staging.validate_accounts(self.plan, "", "", bad, existing)

    def test_data_parent_creation_preserves_existing_private_data(self):
        parent = self.root / "service-home"
        parent.mkdir(mode=0o750)
        fd = os.open(parent, os.O_RDONLY | os.O_DIRECTORY)
        self.addCleanup(os.close, fd)
        with patch.object(staging.os, "fchown", wraps=os.fchown) as chown:
            result = staging.prepare_data_child(fd, os.geteuid(), os.getegid())
            self.assertTrue(result["created"])
            self.assertEqual(result["mode"], "0700")
            self.assertEqual(chown.call_count, 1)
            sentinel = parent / "data/postgres"
            sentinel.write_bytes(b"existing datastore")
            before = sentinel.stat()
            again = staging.prepare_data_child(fd, os.geteuid(), os.getegid())
            self.assertFalse(again["created"])
            self.assertEqual(chown.call_count, 1)
            self.assertEqual(sentinel.read_bytes(), b"existing datastore")
            self.assertEqual(sentinel.stat().st_ino, before.st_ino)

    def test_data_parent_rejects_existing_symlink_wrong_owner_group_and_mode(self):
        parent = self.root / "service-home"
        parent.mkdir(mode=0o750)
        fd = os.open(parent, os.O_RDONLY | os.O_DIRECTORY)
        self.addCleanup(os.close, fd)
        outside = self.root / "outside"
        outside.mkdir(mode=0o755)
        (outside / "sentinel").write_bytes(b"untouched")
        (parent / "data").symlink_to(outside)
        with self.assertRaises(OSError):
            staging.prepare_data_child(fd, os.geteuid(), os.getegid())
        self.assertEqual(stat.S_IMODE(outside.stat().st_mode), 0o755)
        (parent / "data").unlink()
        (parent / "data").mkdir(mode=0o755)
        for owner, group, mode in (
            (os.geteuid(), os.getegid(), 0o755),
            (os.geteuid() + 1, os.getegid(), 0o700),
            (os.geteuid(), os.getegid() + 1, 0o700),
        ):
            with self.subTest(owner=owner, group=group, mode=mode):
                (parent / "data").chmod(mode)
                with self.assertRaises(staging.StagingError):
                    staging.prepare_data_child(fd, owner, group)
                self.assertEqual(stat.S_IMODE((parent / "data").stat().st_mode), mode)
        self.assertEqual((outside / "sentinel").read_bytes(), b"untouched")

    def test_rehashed_network_changes_cannot_break_dns_or_datastore_isolation(self):
        backend = "units/llunde-backend/llunde-backend.container"
        egress = "units/llunde-backend/llunde-backend-egress.network"
        changes = [
            (backend, "Network=llunde-backend-egress.network", "Network=podman"),
            (
                backend,
                "Network=llunde-backend-egress.network",
                "Network=llunde-backend-egress.network\nDNS=1.1.1.1",
            ),
            (egress, "DisableDNS=false", "DisableDNS=true"),
            (egress, "Internal=false", "Internal=true"),
            (egress, "Driver=bridge", "Driver=macvlan"),
            (backend, "[Container]", "[Container]\nPodmanArgs=--network=host"),
            (backend, "[Container]", "[Container]\nGlobalArgs=--network=host"),
            (backend, "[Container]", "[Container]\nPod=outside.pod"),
            (
                "units/llunde-backend/llunde-backend-data.network",
                "Internal=true",
                "Internal=false",
            ),
            (
                "units/llunde-backend/llunde-valkey.container",
                "Network=llunde-backend-data.network",
                "Network=llunde-backend-data.network\nNetwork=llunde-backend-egress.network",
            ),
        ]
        for name, before, after in changes:
            with self.subTest(name=name, after=after):
                path = self.root / name
                original = path.read_text()
                path.write_text(original.replace(before, after))
                self.plan["unitSHA256"][name] = hashlib.sha256(
                    path.read_bytes()
                ).hexdigest()
                self.save()
                with self.assertRaises(staging.StagingError):
                    staging.validate_plan(self.root)
                path.write_text(original)
                self.plan["unitSHA256"][name] = hashlib.sha256(
                    path.read_bytes()
                ).hexdigest()
                self.save()

    def test_fresh_legacy_upgrade_preserves_inputs_and_all_other_unit_bytes(self):
        name = "units/llunde-backend/llunde-backend-egress.network"
        (self.root / name).unlink()
        self.plan["unitSHA256"].pop(name)
        backend = "units/llunde-backend/llunde-backend.container"
        path = self.root / backend
        path.write_text(
            path.read_text().replace(
                "Network=llunde-backend-egress.network", "Network=podman"
            )
        )
        self.plan["unitSHA256"][backend] = hashlib.sha256(path.read_bytes()).hexdigest()
        self.save()
        before = {
            name: (self.root / name).read_bytes() for name in self.plan["unitSHA256"]
        }
        before["staging.json"] = (self.root / "staging.json").read_bytes()
        for name in before:
            (self.root / name).chmod(0o600)
        digest = hashlib.sha256(before["staging.json"]).hexdigest()
        with self.assertRaises(staging.StagingError):
            staging.validate_plan(self.root)
        with self.assertRaises(staging.StagingError):
            staging.prepare_networks(self.root, self.root / "wrong", "0" * 64)
        self.assertFalse((self.root / "wrong").exists())
        destination = self.root / "updated"
        result = staging.prepare_networks(self.root, destination, digest)
        self.assertEqual(result["unitCount"], 9)
        updated = staging.validate_plan(destination)
        self.assertEqual(updated["services"], self.plan["services"])
        self.assertEqual(updated["sourceNetworkCandidateSHA256"], digest)
        for name, raw in before.items():
            self.assertEqual((self.root / name).read_bytes(), raw)
            if name not in (backend, "staging.json"):
                self.assertEqual((destination / name).read_bytes(), raw)
        with self.assertRaises(staging.StagingError):
            staging.prepare_networks(self.root, destination, digest)

    def test_user_and_group_collisions_are_rejected(self):
        for passwd, group in (
            ("other:x:2000:2000::/home/other:/bin/sh", ""),
            ("edge:x:2000:1000::/home/edge:/bin/sh", ""),
            ("edge:x:2000:2000::/home/unrelated:/bin/sh", ""),
            ("", "other:x:2001:"),
        ):
            with (
                self.subTest(passwd=passwd, group=group),
                self.assertRaises(staging.StagingError),
            ):
                staging.validate_accounts(self.plan, passwd, group, "", "")


@unittest.skipUnless(
    shutil.which("age") and shutil.which("age-keygen"), "age tools required"
)
class SecretBundleTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.key = self.root / "identity.key"
        subprocess.run(
            ["age-keygen", "-o", str(self.key)], capture_output=True, check=True
        )
        self.key.chmod(0o600)
        self.recipient = (
            subprocess.run(
                ["age-keygen", "-y", str(self.key)], capture_output=True, check=True
            )
            .stdout.decode()
            .strip()
        )
        (self.root / "secrets").mkdir()
        self.plaintext = {}
        for name, (_, _, keys, source, _) in staging.SECRETS.items():
            (self.root / "secrets" / source).write_text("encrypted-source-fixture")
            self.plaintext[source] = "".join(
                f"{key}=fixture-{name}\n" for key in sorted(keys)
            ).encode()
        self.bundle = self.root / "bundle"
        self.runtime = self.root / "run/llunde"
        self.real_run = staging.run_private

    def fixture_run(self, argv, data=None):
        if argv[0] == "sops":
            return self.plaintext[Path(argv[-1]).name]
        return self.real_run(argv, data)

    def prepare(self):
        with patch.object(staging, "run_private", side_effect=self.fixture_run):
            return staging.prepare_secrets(self.root, self.recipient, self.bundle)

    def render(self):
        return staging.render_secrets(
            self.key, self.bundle, self.runtime, os.geteuid(), os.getegid()
        )

    def test_encrypted_roundtrip_regenerates_runtime_and_is_idempotent(self):
        self.assertTrue(self.prepare()["changed"])
        original = {p.name: p.read_bytes() for p in self.bundle.iterdir()}
        self.assertFalse(self.prepare()["changed"])
        self.assertEqual(
            original, {p.name: p.read_bytes() for p in self.bundle.iterdir()}
        )
        for _ in range(2):
            self.assertEqual(self.render(), {"renderedFiles": 3})
            for _, (user, filename, _, source, _) in staging.SECRETS.items():
                path = self.runtime / user / filename
                self.assertEqual(path.read_bytes(), self.plaintext[source])
                self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o640)
            shutil.rmtree(self.runtime)

    def test_bad_digest_or_environment_fails_before_runtime_writes(self):
        self.prepare()
        (self.bundle / "tunnel.age").write_bytes(b"corrupt ciphertext")
        with self.assertRaises(staging.StagingError):
            self.render()
        self.assertFalse(self.runtime.exists())
        self.plaintext["doppler.yaml"] = b"UNEXPECTED=fixture\n"
        with self.assertRaises(staging.StagingError):
            self.prepare()
        self.assertFalse(self.runtime.exists())

    def test_bundle_symlink_and_hardlink_are_refused_without_truncation(self):
        self.bundle.mkdir(mode=0o700)
        victim = self.root / "victim"
        victim.write_bytes(b"unchanged")
        victim.chmod(0o600)
        target = self.bundle / "doppler.age"
        target.symlink_to(victim)
        with self.assertRaises((OSError, staging.StagingError)):
            self.prepare()
        target.unlink()
        os.link(victim, target)
        with self.assertRaises(staging.StagingError):
            self.prepare()
        self.assertEqual(victim.read_bytes(), b"unchanged")

    def test_wrong_recipient_and_private_file_mode_are_refused(self):
        self.prepare()
        manifest_path = self.bundle / "manifest.json"
        manifest = json.loads(manifest_path.read_text())
        manifest["recipient"] = "different"
        manifest_path.write_text(json.dumps(manifest))
        with self.assertRaises(staging.StagingError):
            self.render()
        self.key.chmod(0o644)
        with self.assertRaises(staging.StagingError):
            self.render()
        self.assertFalse(self.runtime.exists())

    def test_runtime_symlink_is_refused(self):
        self.prepare()
        self.runtime.parent.mkdir(mode=0o755)
        self.runtime.symlink_to(self.root)
        with self.assertRaises(staging.StagingError):
            self.render()

    def test_secret_service_umask_creates_exact_new_runtime_directory_modes(self):
        self.prepare()
        previous = os.umask(0o077)
        try:
            self.assertEqual(self.render(), {"renderedFiles": 3})
        finally:
            os.umask(previous)
        self.assertEqual(stat.S_IMODE(self.runtime.parent.stat().st_mode), 0o755)
        self.assertEqual(stat.S_IMODE(self.runtime.stat().st_mode), 0o755)
        for user in ("edge", "llunde-backend"):
            self.assertEqual(stat.S_IMODE((self.runtime / user).stat().st_mode), 0o750)

    def test_existing_restricted_runtime_directory_is_not_silently_relaxed(self):
        self.prepare()
        self.runtime.parent.mkdir(mode=0o755)
        self.runtime.mkdir(mode=0o700)
        with self.assertRaises(staging.StagingError):
            self.render()
        self.assertEqual(stat.S_IMODE(self.runtime.stat().st_mode), 0o700)
        self.assertEqual(list(self.runtime.iterdir()), [])


if __name__ == "__main__":
    unittest.main()
