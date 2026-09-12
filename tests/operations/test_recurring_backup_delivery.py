import hashlib
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
import recurring_backup_delivery as delivery


class DeliveryTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(dir=ROOT / ".infra")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        (self.root / "helpers").mkdir(mode=0o700)
        self.payload = self.root / "helpers/fixture.py"
        self.payload.write_bytes(b"valid fixture\n")
        self.payload.chmod(0o600)
        manifest = {
            "schemaVersion": 1,
            "kind": "recurring-backup-deployment",
            "remoteDirectory": str(self.root),
            "files": {
                "helpers/fixture.py": {
                    "sha256": hashlib.sha256(self.payload.read_bytes()).hexdigest(),
                    "bytes": self.payload.stat().st_size,
                }
            },
        }
        self.manifest = self.root / "manifest.json"
        self.manifest.write_text(json.dumps(manifest))
        self.manifest.chmod(0o600)
        self.sha = hashlib.sha256(self.manifest.read_bytes()).hexdigest()

    def verify(self):
        return delivery.verify_tree(str(self.root), self.sha, os.geteuid())

    def test_exact_private_bundle_passes_and_changed_helper_fails(self):
        self.assertEqual(self.verify()["schemaVersion"], 1)
        self.payload.write_bytes(b"changed fixture")
        with self.assertRaises(ValueError):
            self.verify()

    def test_unknown_file_or_symlink_helper_directory_is_rejected(self):
        extra = self.root / "extra.py"
        extra.touch()
        with self.assertRaises(ValueError):
            self.verify()
        extra.unlink()
        (self.root / "helpers").rename(self.root / "outside")
        (self.root / "helpers").symlink_to(self.root / "outside")
        with self.assertRaises((ValueError, OSError)):
            self.verify()

    def test_hardlink_and_public_mode_are_rejected_before_execution(self):
        self.payload.chmod(0o644)
        with self.assertRaises(ValueError):
            self.verify()
        self.payload.chmod(0o600)
        with tempfile.TemporaryDirectory(dir=ROOT / ".infra") as other:
            os.link(self.payload, Path(other) / "duplicate")
            with self.assertRaises(ValueError):
                self.verify()

    def test_extra_empty_directory_and_symlink_directory_are_rejected(self):
        extra = self.root / "extra"
        extra.mkdir(mode=0o700)
        with self.assertRaises(ValueError):
            self.verify()
        extra.rmdir()
        extra.symlink_to(self.root / "helpers")
        with self.assertRaises(ValueError):
            self.verify()


if __name__ == "__main__":
    unittest.main()
