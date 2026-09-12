import base64
import contextlib
import io
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import unittest
from datetime import UTC, datetime
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
import evacuation_secret_relay as relay
import evacuation_staging as staging

VALUES = {
    "doppler": b"DOPPLER_TOKEN=synthetic-fixture\n",
    "database": b"POSTGRES_PASSWORD=synthetic-fixture\nDB_PASSWORD=synthetic-fixture\n",
    "tunnel": b"TUNNEL_TOKEN=synthetic-fixture\n",
}


def source_fixture():
    metadata = {
        "uid": 0,
        "gid": 96,
        "mode": "0751",
        "device": 1,
        "inode": 2,
        "size": 80,
        "mtimeNs": 1,
        "ctimeNs": 1,
        "links": 1,
    }
    return {
        "schemaVersion": 1,
        "hostname": "llunde-01",
        "capturedAt": datetime.now(UTC).isoformat(),
        "generation": "/run/secrets.d/48",
        "directories": {"secrets.d": metadata.copy(), "generation": metadata.copy()},
        "files": {
            name: {
                "path": f"/run/secrets/{filename}",
                "resolvedPath": f"/run/secrets.d/48/{filename}",
                "metadata": metadata
                | {"uid": owner, "mode": "0400", "size": len(VALUES[name])},
                "data": base64.b64encode(VALUES[name]).decode(),
            }
            for name, (filename, owner) in relay.SOURCE_FILES.items()
        },
    }


class RuntimeReaderTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name) / "run"
        self.root.mkdir(mode=0o755)
        self.parent = self.root / "secrets.d"
        self.parent.mkdir(mode=0o751)
        self.generation = self.parent / "48"
        self.generation.mkdir(mode=0o751)
        self.link = self.root / "secrets"
        self.link.symlink_to(self.generation)
        for name, (filename, _) in relay.SOURCE_FILES.items():
            path = self.generation / filename
            path.write_bytes(VALUES[name])
            path.chmod(0o400)

    def invoke(self, prefix="", **overrides):
        parameters = {
            "runtime_root": str(self.root),
            "root_uid": os.geteuid(),
            "root_gid": os.getegid(),
            "directory_gid": os.getegid(),
            "hostname": socket.gethostname(),
            "owners": {name: os.geteuid() for name in VALUES},
        } | overrides
        return subprocess.run(
            [sys.executable, "-I", "-c", prefix + relay.source_program(**parameters)],
            capture_output=True,
            timeout=10,
        )

    def refused(self, **overrides):
        result = self.invoke(**overrides)
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout, b"")
        self.assertEqual(result.stderr, b"Runtime source validation failed\n")

    def test_reads_exact_generation_and_returns_only_expected_values(self):
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        document = json.loads(result.stdout)
        self.assertEqual(set(document["files"]), set(VALUES))
        for name, value in VALUES.items():
            self.assertEqual(base64.b64decode(document["files"][name]["data"]), value)
        self.assertNotIn("sha256", result.stdout.decode().lower())

    def test_refuses_wrong_host_owner_and_public_file(self):
        self.refused(hostname="unrelated-host")
        self.refused(owners={name: os.geteuid() + 1 for name in VALUES})
        (self.generation / "doppler-token").chmod(0o444)
        self.refused()

    def test_refuses_secret_hardlink_symlink_and_nonregular_file(self):
        target = self.generation / "doppler-token"
        extra = self.generation / "unlisted"
        os.link(target, extra)
        self.refused()
        extra.unlink()
        target.unlink()
        target.symlink_to(self.generation / "llunde-tunnel")
        self.refused()
        target.unlink()
        os.mkfifo(target, 0o400)
        self.refused()

    def test_refuses_generation_escape_symlink_and_directory_writes(self):
        self.link.unlink()
        self.link.symlink_to(self.parent / "../secrets.d/48")
        self.refused()
        self.link.unlink()
        self.link.symlink_to(self.generation)
        self.generation.rename(self.parent / "49")
        self.generation.symlink_to(self.parent / "49")
        self.refused()
        self.generation.unlink()
        (self.parent / "49").rename(self.generation)
        self.parent.chmod(0o773)
        self.refused()

    def test_refuses_replacement_during_read_without_partial_response(self):
        target = self.generation / "doppler-token"
        prefix = f"import os\noriginal_read = os.read\ndef changing_read(fd, size):\n    data = original_read(fd, size)\n    target = {str(target)!r}\n    os.unlink(target)\n    with open(target, 'wb') as handle:\n        handle.write(b'replaced')\n    return data\nos.read = changing_read\n"
        result = self.invoke(prefix)
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout, b"")


class RelayBoundaryTests(unittest.TestCase):
    def test_rejects_extra_keys_wrong_owner_size_and_stale_observation(self):
        for mutate in [
            lambda d: d["files"]["doppler"]["metadata"].update(uid=2000),
            lambda d: d["files"]["doppler"]["metadata"].update(size=1),
            lambda d: d["files"]["doppler"].update(
                data=base64.b64encode(b"OTHER=value\n").decode()
            ),
            lambda d: d.update(capturedAt="2000-01-01T00:00:00+00:00"),
            lambda d: d["directories"]["generation"].update(secret="forbidden"),
        ]:
            document = source_fixture()
            mutate(document)
            with self.assertRaises(staging.StagingError):
                relay.decode_source(document)

    def test_transport_pins_host_key_alias_and_filters_environment(self):
        seen = {}

        def runner(argv, **kwargs):
            seen.update(argv=argv, **kwargs)
            return subprocess.CompletedProcess(
                argv, 0, json.dumps(source_fixture()).encode(), b""
            )

        relay.read_source(
            runner=runner,
            environment={
                "PATH": "/usr/bin:/bin",
                "HOME": "/fixture",
                "SSH_AUTH_SOCK": "/socket",
                "DOPPLER_TOKEN": "not-forwarded",
                "PYTHONPATH": "not-forwarded",
            },
        )
        self.assertEqual(seen["argv"][-2], "fredrir-05")
        self.assertIn("StrictHostKeyChecking=yes", seen["argv"])
        self.assertIn("ForwardAgent=no", seen["argv"])
        self.assertIn("sudo -n env -i", seen["argv"][-1])
        self.assertEqual(set(seen["env"]), {"PATH", "HOME", "SSH_AUTH_SOCK"})
        self.assertEqual(seen["input"], b"")

    def test_accepts_existing_nix_python_and_rejects_arbitrary_executables(self):
        source_python = (
            "/nix/store/vm6nxpp97fxgczw00bwkxdqdm2an3n95-python3-3.13.12/bin/python3"
        )

        def runner(argv, **kwargs):
            self.assertIn(source_python + " -I -c", argv[-1])
            return subprocess.CompletedProcess(
                argv, 0, json.dumps(source_fixture()).encode(), b""
            )

        relay.read_source(source_python=source_python, runner=runner)
        for bad in [
            "python3 -c unwanted",
            "/tmp/python3",
            source_python + ";id",
            source_python.replace("/bin/python3", "/bin/../bin/python3"),
        ]:
            with self.subTest(path=bad), self.assertRaises(staging.StagingError):
                relay.read_source(
                    source_python=bad,
                    runner=lambda *args, **kwargs: self.fail(
                        "Invalid interpreter must not execute"
                    ),
                )

    def test_command_and_cli_errors_withhold_sensitive_output(self):
        def runner(argv, **kwargs):
            return subprocess.CompletedProcess(
                argv, 1, b"private-value", b"private-value"
            )

        with self.assertRaisesRegex(
            staging.StagingError, "^Private relay command failed$"
        ):
            relay.read_source(runner=runner)
        output = io.StringIO()
        with (
            patch.object(
                relay, "prepare_relay", side_effect=ValueError("private-value")
            ),
            contextlib.redirect_stdout(output),
        ):
            self.assertEqual(relay.main(["recipient", "destination"]), 1)
        self.assertNotIn("private-value", output.getvalue())


@unittest.skipUnless(
    shutil.which("age") and shutil.which("age-keygen"), "age tools required"
)
class EncryptedRelayTests(unittest.TestCase):
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
        self.destination = self.root / "bundle"

    def test_ciphertext_bundle_works_with_existing_renderer_without_source_hashes(self):
        receipt = relay.prepare_relay(
            self.recipient, self.destination, source_reader=source_fixture
        )
        self.assertFalse(receipt["repositorySecretProvenance"])
        manifest = json.loads((self.destination / "manifest.json").read_bytes())
        self.assertEqual(manifest["source"]["type"], "runtime-ssh")
        self.assertNotIn("sourceSHA256", json.dumps(manifest))
        self.assertNotIn("synthetic-fixture", json.dumps(manifest))
        for path in self.destination.iterdir():
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertNotIn(b"synthetic-fixture", path.read_bytes())
        runtime = self.root / "run/llunde"
        staging.render_secrets(
            self.key, self.destination, runtime, os.geteuid(), os.getegid()
        )
        for name, (user, filename, _, _, _) in staging.SECRETS.items():
            self.assertEqual((runtime / user / filename).read_bytes(), VALUES[name])

    def test_refuses_existing_destination_without_reading_source(self):
        self.destination.mkdir(mode=0o700)
        with self.assertRaises(staging.StagingError):
            relay.prepare_relay(
                self.recipient,
                self.destination,
                source_reader=lambda: self.fail("Source should not be read"),
            )
        self.assertEqual(list(self.destination.iterdir()), [])

    def test_failed_encryption_writes_no_bundle_and_leaks_no_plaintext_to_argv(self):
        def runner(argv, **kwargs):
            self.assertEqual(argv, ["age", "--recipient", self.recipient])
            self.assertIn(kwargs["input"], VALUES.values())
            self.assertNotIn("DOPPLER_TOKEN", kwargs["env"])
            return subprocess.CompletedProcess(argv, 1, b"", b"synthetic-fixture")

        with self.assertRaises(staging.StagingError):
            relay.prepare_relay(
                self.recipient,
                self.destination,
                source_reader=source_fixture,
                runner=runner,
                environment={
                    "PATH": os.environ["PATH"],
                    "DOPPLER_TOKEN": "not-forwarded",
                },
            )
        self.assertFalse(self.destination.exists())


if __name__ == "__main__":
    unittest.main()
