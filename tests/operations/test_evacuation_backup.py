import base64
import hashlib
import importlib.util
import io
import json
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
SPEC = importlib.util.spec_from_file_location(
    "evacuation_backup", ROOT / "scripts/operations/evacuation_backup.py"
)
backup = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(backup)
CREDENTIALS = {
    "schemaVersion": 1,
    "repository": backup.REPOSITORY,
    "region": "eu-north-1",
    "password": "private-backup-fixture-password",
    "AWS_ACCESS_KEY_ID": "AKIA" + "A" * 16,
    "AWS_SECRET_ACCESS_KEY": "a" * 40,
}
PRINCIPAL = "arn:aws:iam::123456789012:user/restic-llunde-01"


def source_fixture():
    files = {}
    values = {
        "environment": "\n".join(
            f"{key}={CREDENTIALS[key]}"
            for key in ["AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"]
        )
        + "\n",
        "password": CREDENTIALS["password"] + "\n",
    }
    for name, (filename, owner) in backup.SOURCE_FILES.items():
        data = values[name].encode()
        info = {
            "uid": owner,
            "gid": 0,
            "mode": "0400",
            "device": 1,
            "inode": 2,
            "size": len(data),
            "mtimeNs": 3,
            "ctimeNs": 3,
            "links": 1,
        }
        files[name] = {
            "path": "/run/secrets/" + filename,
            "resolvedPath": "/run/secrets.d/9/" + filename,
            "metadata": info,
            "data": base64.b64encode(data).decode(),
        }
    return {
        "schemaVersion": 1,
        "hostname": "llunde-01",
        "generation": "/run/secrets.d/9",
        "capturedAt": "2026-09-12T00:00:00Z",
        "files": files,
    }


class FakeRestic:
    def __init__(self):
        self.repository_id = "a" * 64
        self.snapshot_id = "b" * 64
        self.archive = None
        self.calls = []
        self.snapshot = None
        self.fail = False

    def identity(self):
        return self.repository_id

    def call(self, args, *, source=None, output=None, maximum=262144):
        self.calls.append(args)
        if self.fail:
            raise backup.BackupError("fixture failure")
        if args[0] == "backup":
            self.archive = source.read()
            self.snapshot = {
                "hostname": "fredrir-09",
                "paths": ["/evacuation-state.tar"],
                "tags": [args[args.index("--tag") + 1]],
            }
            return json.dumps(
                {"message_type": "summary", "snapshot_id": self.snapshot_id}
            ).encode()
        if args[0] == "cat":
            return json.dumps(self.snapshot).encode()
        if args[0] == "dump":
            if len(self.archive) > maximum:
                raise backup.BackupError("output budget")
            output.write(self.archive)
            return b""
        raise AssertionError(args)


class BackupTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.root.chmod(0o700)
        self.bundle = self.root / "bundle"
        self.bundle.mkdir(mode=0o700)
        files = {
            "database.dump": b"PGDMP-fixture",
            "dump.rdb": b"REDIS-fixture",
            "Caddyfile": b"private-config-fixture",
        }
        manifest = {
            "schemaVersion": 1,
            "kind": "isolated-target-rehearsal",
            "target": "fredrir-09",
            "files": {
                name: {"sha256": hashlib.sha256(value).hexdigest()}
                for name, value in files.items()
            },
        }
        for name, value in files.items():
            backup.write_private(self.bundle / name, value)
        backup.write_private(
            self.bundle / "manifest.json", json.dumps(manifest).encode()
        )
        self.restic = FakeRestic()

    def upload(self, restic=None):
        with patch.object(backup, "require_upload_host"):
            return backup.backup(self.bundle, restic or self.restic, self.root)

    def test_metadata_excludes_plaintext_and_uses_only_the_exact_source_files(self):
        credentials, metadata = backup.source_credentials(source_fixture())
        self.assertEqual(credentials, CREDENTIALS)
        self.assertNotIn(CREDENTIALS["password"], json.dumps(metadata))
        self.assertNotIn(CREDENTIALS["AWS_SECRET_ACCESS_KEY"], json.dumps(metadata))
        self.assertFalse(metadata["repositorySecretProvenance"])
        self.assertNotIn("data", metadata["files"]["password"])

    def test_observed_source_region_is_accepted_only_when_it_matches_the_repository(
        self,
    ):
        for region in ["eu-north-1", "us-east-1"]:
            value = source_fixture()
            record = value["files"]["environment"]
            raw = (
                base64.b64decode(record["data"])
                + ("AWS_DEFAULT_REGION=" + region + "\n").encode()
            )
            record["data"], record["metadata"]["size"] = (
                base64.b64encode(raw).decode(),
                len(raw),
            )
            if region == "eu-north-1":
                self.assertEqual(backup.source_credentials(value)[0], CREDENTIALS)
            else:
                with self.assertRaises(backup.BackupError):
                    backup.source_credentials(value)

    def test_source_owner_path_scope_and_ambient_credentials_are_rejected(self):
        for mutate in [
            lambda x: x["files"]["password"]["metadata"].update(uid=2001),
            lambda x: x["files"]["environment"].update(path="/etc/environment"),
            lambda x: x.update(hostname="fredrir-09"),
            lambda x: x["files"].update(extra=x["files"]["password"]),
        ]:
            value = source_fixture()
            mutate(value)
            with self.assertRaises(backup.BackupError):
                backup.source_credentials(value)
        for changes in [
            {"repository": "/tmp/local"},
            {"AWS_SESSION_TOKEN": "unexpected"},
            {"region": "us-east-1"},
        ]:
            with self.assertRaises(backup.BackupError):
                backup.validate_credentials(CREDENTIALS | changes)

    def test_upload_and_independent_restore_bind_exact_snapshot_and_all_bytes(self):
        receipt = self.upload()
        self.assertFalse(receipt["independentRestoreVerified"])
        restored = self.root / "restored"
        proof = backup.restore(receipt, restored, self.restic)
        self.assertTrue(proof["verified"])
        self.assertTrue(proof["independentHost"])
        self.assertFalse(proof["applicationRestoreVerified"])
        for path in self.bundle.iterdir():
            self.assertEqual(path.read_bytes(), (restored / path.name).read_bytes())
        self.assertEqual(
            {args[0] for args in self.restic.calls}, {"backup", "cat", "dump"}
        )
        self.assertFalse(
            any(path.name.startswith("restic-upload-") for path in self.root.iterdir())
        )

    def test_recovery_refuses_existing_destination_and_wrong_repository(self):
        receipt = self.upload()
        destination = self.root / "existing"
        destination.mkdir(mode=0o700)
        (destination / "sentinel").write_text("preserve")
        with self.assertRaises(backup.BackupError):
            backup.restore(receipt, destination, self.restic)
        self.assertEqual((destination / "sentinel").read_text(), "preserve")
        self.restic.repository_id = "c" * 64
        with self.assertRaises(backup.BackupError):
            backup.restore(receipt, self.root / "fresh", self.restic)
        self.assertFalse((self.root / "fresh").exists())

    def test_partial_backup_never_returns_success_and_temporary_archive_is_removed(
        self,
    ):
        self.restic.fail = True
        with self.assertRaises(backup.BackupError):
            self.upload()
        self.assertEqual({path.name for path in self.root.iterdir()}, {"bundle"})

    def test_tampered_archive_or_snapshot_metadata_is_rejected_and_cleaned(self):
        receipt = self.upload()
        original = self.restic.archive
        self.restic.archive = original[:-1] + bytes([original[-1] ^ 1])
        with self.assertRaises(backup.BackupError):
            backup.restore(receipt, self.root / "restore", self.restic)
        self.assertFalse((self.root / "restore").exists())
        self.restic.archive = original
        self.restic.snapshot["hostname"] = "fredrir-05"
        with self.assertRaises(backup.BackupError):
            backup.restore(receipt, self.root / "restore", self.restic)

    def test_archive_link_is_rejected_even_with_an_updated_outer_checksum(self):
        receipt = self.upload()
        stream = io.BytesIO()
        with tarfile.open(fileobj=stream, mode="w") as archive:
            member = tarfile.TarInfo("database.dump")
            member.type, member.linkname = tarfile.SYMTYPE, "/etc/passwd"
            archive.addfile(member)
        self.restic.archive = stream.getvalue()
        receipt.update(
            archiveSHA256=hashlib.sha256(self.restic.archive).hexdigest(),
            archiveBytes=len(self.restic.archive),
        )
        with self.assertRaises(backup.BackupError):
            backup.restore(receipt, self.root / "restore", self.restic)
        self.assertFalse((self.root / "restore").exists())

    def test_unexpected_symlinked_or_changed_bundle_files_refuse_upload(self):
        sentinel = self.bundle / "unexpected"
        sentinel.symlink_to("/etc/passwd")
        with self.assertRaises(backup.BackupError):
            self.upload()
        sentinel.unlink()
        (self.bundle / "database.dump").write_bytes(b"changed")
        with self.assertRaises(backup.BackupError):
            self.upload()
        self.assertEqual(self.restic.calls, [])

    def test_forged_receipt_cannot_authorize_path_traversal(self):
        receipt = self.upload()
        sentinel = self.root / "outside"
        sentinel.write_text("preserve")
        receipt["bundle"]["files"]["../outside"] = receipt["bundle"]["files"].pop(
            "database.dump"
        )
        stream = io.BytesIO()
        with tarfile.open(fileobj=stream, mode="w") as archive:
            entry = tarfile.TarInfo("../outside")
            entry.size = 7
            archive.addfile(entry, io.BytesIO(b"changed"))
        self.restic.archive = stream.getvalue()
        receipt.update(
            archiveSHA256=hashlib.sha256(self.restic.archive).hexdigest(),
            archiveBytes=len(self.restic.archive),
        )
        with self.assertRaises(backup.BackupError):
            backup.restore(receipt, self.root / "restore", self.restic)
        self.assertFalse((self.root / "restore").exists())
        self.assertEqual(sentinel.read_text(), "preserve")

    def test_restic_stream_is_bounded_before_writing_excess_bytes(self):
        bindir = self.root / "bin"
        bindir.mkdir(mode=0o700)
        binary = bindir / "restic"
        binary.write_text(
            "#!" + sys.executable + "\nimport os\nos.write(1, b'x' * 65536)\n"
        )
        binary.chmod(0o700)
        client = backup.Restic(CREDENTIALS)
        client.environment["PATH"] = str(bindir)
        output = io.BytesIO()
        with self.assertRaisesRegex(backup.BackupError, "exceeds budget"):
            client.call(
                ["dump", "a" * 64, "/evacuation-state.tar"], output=output, maximum=32
            )
        self.assertLessEqual(len(output.getvalue()), 32)

    def test_restic_environment_drops_ambient_authority_and_forbids_deletion(self):
        with patch.dict(
            os.environ,
            {
                "AWS_SESSION_TOKEN": "ambient",
                "AWS_PROFILE": "admin",
                "DOPPLER_TOKEN": "ambient",
            },
        ):
            client = backup.Restic(CREDENTIALS)
        self.assertFalse(
            {"AWS_SESSION_TOKEN", "AWS_PROFILE", "DOPPLER_TOKEN"}
            & set(client.environment)
        )
        for action in ["init", "forget", "prune", "unlock", "key", "copy", "repair"]:
            with self.assertRaises(backup.BackupError):
                client.call([action])

    @unittest.skipUnless(shutil.which("restic"), "Native Restic unavailable")
    def test_native_local_restic_proves_stdin_snapshot_metadata_and_independent_dump(
        self,
    ):
        client = backup.Restic(CREDENTIALS)
        client.environment["RESTIC_REPOSITORY"] = str(self.root / "local-repository")
        result = subprocess.run(
            ["restic", "init"], env=client.environment, capture_output=True, timeout=20
        )
        self.assertEqual(result.returncode, 0)
        receipt = self.upload(client)
        proof = backup.restore(receipt, self.root / "native-restored", client)
        self.assertTrue(proof["verified"])
        self.assertEqual(
            (self.root / "native-restored/database.dump").read_bytes(),
            (self.bundle / "database.dump").read_bytes(),
        )

    @unittest.skipUnless(
        shutil.which("age") and shutil.which("age-keygen"), "Native age unavailable"
    )
    def test_dual_recipient_encryption_restores_credentials_without_private_key_transfer(
        self,
    ):
        identities, recipients = [], []
        for name in ["target", "admin"]:
            key = self.root / (name + ".key")
            result = subprocess.run(
                ["age-keygen", "-o", str(key)], capture_output=True, timeout=5
            )
            self.assertEqual(result.returncode, 0)
            key.chmod(0o600)
            identities.append(key)
            recipients.append(
                subprocess.check_output(["age-keygen", "-y", str(key)], timeout=5)
                .decode()
                .strip()
            )
        with (
            patch.object(backup, "TARGET_RECIPIENT", recipients[0]),
            patch.object(
                backup.subprocess,
                "run",
                return_value=subprocess.CompletedProcess(
                    [], 0, json.dumps({"Arn": PRINCIPAL}).encode(), b""
                ),
            ),
        ):
            backup.prepare_credentials(
                self.root / "credentials",
                recipients[1],
                "python3",
                PRINCIPAL,
                reader=source_fixture,
            )
        for identity in identities:
            self.assertEqual(
                backup.decrypt_credentials(self.root / "credentials", identity),
                CREDENTIALS,
            )
        self.assertFalse(
            any(
                CREDENTIALS["password"].encode() in path.read_bytes()
                for path in (self.root / "credentials").iterdir()
            )
        )


if __name__ == "__main__":
    unittest.main()
