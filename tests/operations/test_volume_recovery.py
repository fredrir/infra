import hashlib
import io
import json
import sys
import tarfile
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))

import recovery
import volume_recovery as volume

NOW = 1789171200


class VolumeRecoveryTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name).resolve()
        self.source = self.root / "source"
        self.source.mkdir()
        (self.source / "uploads").mkdir()
        (self.source / "uploads/document.txt").write_bytes(
            b"private application data\n"
        )
        (self.source / "empty").write_bytes(b"")
        self.database = self.root / "postgres"
        self.database.mkdir(mode=0o700)
        self.private(self.database / "database.dump", b"fixture database dump")
        self.database_sha = hashlib.sha256(b"fixture database dump").hexdigest()
        self.private(
            self.database / "manifest.json",
            json.dumps(
                {
                    "schemaVersion": 1,
                    "kind": "postgres-logical",
                    "createdAt": NOW - 10,
                    "sha256": self.database_sha,
                }
            ).encode(),
        )
        self.assertion = self.root / "drain.json"
        self.claim = {
            "schemaVersion": 1,
            "kind": "project-volume-drain",
            "project": "llunde-pyparser",
            "volume": "files",
            "verifiedAt": NOW - 20,
            "writersStopped": True,
            "reportSha256": "a" * 64,
        }
        self.private(self.assertion, json.dumps(self.claim).encode())
        self.bundle = self.root / "bundle"

    def tearDown(self):
        self.temporary.cleanup()

    def private(self, path, contents):
        path.write_bytes(contents)
        path.chmod(0o600)

    def export(self, maximum=1024):
        return volume.export_volume(
            self.source,
            self.bundle,
            self.database,
            self.assertion,
            "llunde-pyparser",
            "files",
            maximum,
            now=NOW,
        )

    def test_round_trip_preserves_bytes_and_requires_paired_database(self):
        result = self.export()
        self.assertEqual(result["entries"], 3)
        self.assertEqual(result["databaseSha256"], self.database_sha)
        destination = self.root / "restored"
        restored = volume.restore_volume(
            self.bundle, self.database, destination, "llunde-pyparser", "files"
        )
        self.assertTrue(restored["restoreChecksPassed"])
        self.assertEqual(
            (destination / "uploads/document.txt").read_bytes(),
            (self.source / "uploads/document.txt").read_bytes(),
        )
        self.assertEqual((destination / "empty").read_bytes(), b"")
        self.assertEqual(
            (destination / "uploads/document.txt").stat().st_mode & 0o777, 0o600
        )
        self.assertEqual(destination.stat().st_mode & 0o777, 0o700)

    def test_explicit_cli_exports_only_a_local_verified_bundle(self):
        output = io.StringIO()
        arguments = [
            "volume-export",
            "--source",
            str(self.source),
            "--destination",
            str(self.bundle),
            "--database-bundle",
            str(self.database),
            "--drain-assertion",
            str(self.assertion),
            "--project",
            "llunde-pyparser",
            "--volume",
            "files",
            "--max-bytes",
            "1024",
            "--execute",
        ]
        with (
            patch.object(volume.time, "time", return_value=NOW),
            redirect_stdout(output),
        ):
            self.assertEqual(recovery.main(arguments), 0)
        result = json.loads(output.getvalue())
        self.assertEqual(result["kind"], "verified-project-volume")
        self.assertNotIn("private application data", output.getvalue())

    def test_changed_file_aborts_export_and_removes_partial_bundle(self):
        original = volume.inventory
        calls = 0

        def concurrent_writer(source, maximum):
            nonlocal calls
            calls += 1
            if calls == 2:
                (source / "uploads/document.txt").write_bytes(b"writer was not stopped")
            return original(source, maximum)

        with patch.object(volume, "inventory", side_effect=concurrent_writer):
            with self.assertRaisesRegex(
                volume.RecoveryError, "changed while exporting"
            ):
                self.export()
        self.assertFalse(self.bundle.exists())

    def test_export_rejects_links_and_excessive_size(self):
        (self.source / "linked").symlink_to(self.source / "uploads/document.txt")
        with self.assertRaisesRegex(volume.RecoveryError, "without links"):
            self.export()
        (self.source / "linked").unlink()
        with self.assertRaisesRegex(volume.RecoveryError, "budget"):
            self.export(maximum=1)
        self.assertFalse(self.bundle.exists())

    def test_operator_assertion_must_be_fresh_and_precede_database_backup(self):
        for changes in [
            {"writersStopped": False},
            {"verifiedAt": NOW - 3601},
            {"verifiedAt": NOW},
        ]:
            with self.subTest(changes=changes):
                self.private(self.assertion, json.dumps(self.claim | changes).encode())
                with self.assertRaises(volume.RecoveryError):
                    self.export()
                self.assertFalse(self.bundle.exists())

    def test_restore_rejects_different_database_or_existing_destination(self):
        self.export()
        destination = self.root / "existing"
        destination.mkdir(mode=0o700)
        with self.assertRaises(FileExistsError):
            volume.restore_volume(
                self.bundle, self.database, destination, "llunde-pyparser", "files"
            )
        self.private(self.database / "database.dump", b"different database")
        document = json.loads((self.database / "manifest.json").read_text())
        document["sha256"] = volume.digest(self.database / "database.dump")
        self.private(self.database / "manifest.json", json.dumps(document).encode())
        with self.assertRaisesRegex(volume.RecoveryError, "matching PostgreSQL"):
            volume.verify_volume(self.bundle, self.database, "llunde-pyparser", "files")

    def test_traversal_and_archive_links_are_refused_before_restore(self):
        for name, link in [("../escape", False), ("uploads/linked", True)]:
            with (
                self.subTest(name=name),
                tempfile.TemporaryDirectory(dir=self.root) as temporary,
            ):
                bundle = Path(temporary)
                archive = bundle / "files.tar"
                with tarfile.open(archive, "w") as output:
                    entry = tarfile.TarInfo(name)
                    if link:
                        entry.type, entry.linkname = tarfile.SYMTYPE, "../../escape"
                    else:
                        entry.size = 1
                    output.addfile(entry, None if link else io.BytesIO(b"x"))
                archive.chmod(0o600)
                manifest = {
                    "schemaVersion": 1,
                    "kind": "project-volume",
                    "project": "llunde-pyparser",
                    "volume": "files",
                    "maxBytes": 1024,
                    "databaseSha256": self.database_sha,
                    "archiveSha256": volume.digest(archive),
                    "entries": [
                        {
                            "path": name,
                            "kind": "file",
                            "size": 1,
                            "sha256": hashlib.sha256(b"x").hexdigest(),
                        }
                    ],
                }
                self.private(bundle / "manifest.json", json.dumps(manifest).encode())
                destination = self.root / "restored"
                with self.assertRaises(volume.RecoveryError):
                    volume.restore_volume(
                        bundle, self.database, destination, "llunde-pyparser", "files"
                    )
                self.assertFalse(destination.exists())
                self.assertFalse((self.root / "escape").exists())

    def test_negative_manifest_and_archive_sizes_are_refused(self):
        self.export()
        manifest = json.loads((self.bundle / "manifest.json").read_text())
        manifest["entries"][0]["size"] = -1
        self.private(self.bundle / "manifest.json", json.dumps(manifest).encode())
        with self.assertRaisesRegex(volume.RecoveryError, "nonnegative"):
            volume.verify_volume(self.bundle, self.database, "llunde-pyparser", "files")
        member = tarfile.TarInfo("negative")
        member.size = -1
        self.private(
            self.bundle / "files.tar",
            member.tobuf(format=tarfile.GNU_FORMAT) + b"\0" * 10240,
        )
        manifest["entries"] = [
            {
                "path": "negative",
                "kind": "file",
                "size": 0,
                "sha256": hashlib.sha256(b"").hexdigest(),
            }
        ]
        manifest["archiveSha256"] = volume.digest(self.bundle / "files.tar")
        self.private(self.bundle / "manifest.json", json.dumps(manifest).encode())
        with self.assertRaises((volume.RecoveryError, tarfile.ReadError)):
            volume.verify_volume(self.bundle, self.database, "llunde-pyparser", "files")


if __name__ == "__main__":
    unittest.main()
