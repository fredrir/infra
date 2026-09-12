import copy
import hashlib
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
import evacuation_reset as reset
from evacuation_backup import validate_bundle
from evacuation_staging import EGRESS_NETWORK, prepare_networks
from test_evacuation_cutover import archive_bytes, fence
from test_evacuation_staging import fixture_plan


class ResetPlanTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.root.chmod(0o700)
        self.old, self.new, self.pair = (
            self.root / name for name in ("old", "new", "pair")
        )
        self.old.mkdir(mode=0o700)
        self.pair.mkdir(mode=0o700)
        plan = fixture_plan(self.old)
        egress = "units/llunde-backend/" + EGRESS_NETWORK
        (self.old / egress).unlink()
        del plan["unitSHA256"][egress]
        backend = "units/llunde-backend/llunde-backend.container"
        (self.old / backend).write_text(
            (self.old / backend).read_text().replace(EGRESS_NETWORK, "podman")
        )
        plan["unitSHA256"][backend] = hashlib.sha256(
            (self.old / backend).read_bytes()
        ).hexdigest()
        self.write(self.old / "staging.json", json.dumps(plan).encode())
        for path in (self.old / "units").rglob("*"):
            if path.is_file():
                path.chmod(0o600)
        self.old_sha = hashlib.sha256(
            (self.old / "staging.json").read_bytes()
        ).hexdigest()
        prepare_networks(self.old, self.new, self.old_sha)
        self.marker = {
            "candidateSHA256": self.old_sha,
            "executionID": "e" * 32,
            "run": "reverse-fixture",
            "direction": "reverse",
        }
        self.boot = "12345678-1234-1234-1234-123456789abc"
        identity = {
            "candidateSHA256": self.old_sha,
            "direction": "reverse",
            "source": "fredrir-09",
            "destination": "fredrir-05",
            "imageIDs": {
                name: "sha256:" + "b" * 64 for name in reset.cutover.DATA_IMAGES
            },
        }
        for name, data in (
            ("database.dump", b"PGDMP-fixture"),
            ("valkey.tar", archive_bytes()),
        ):
            self.write(self.pair / name, data)
        before, after = fence("reverse"), fence("reverse", 150)
        for value in (before, after):
            value["executionMarkerSHA256"] = hashlib.sha256(
                reset.canonical(self.marker)
            ).hexdigest()
        after["exportedFiles"] = {
            name: {
                "bytes": (self.pair / name).stat().st_size,
                "sha256": hashlib.sha256((self.pair / name).read_bytes()).hexdigest(),
            }
            for name in ("database.dump", "valkey.tar")
        }
        self.write(self.root / "before", json.dumps(before).encode())
        self.write(self.root / "after", json.dumps(after).encode())
        with patch.object(reset.cutover, "pair_identity", return_value=identity):
            manifest = reset.cutover.seal_pair(
                self.pair,
                self.old,
                "reverse",
                self.root / "before",
                self.root / "after",
            )
        self.pair_sha = hashlib.sha256(reset.canonical(manifest)).hexdigest()
        common = {
            "schemaVersion": 1,
            "kind": "evacuation-execution-fence",
            "phase": "pair-sealed",
            "candidateSHA256": self.old_sha,
        }
        reverse = {
            **common,
            "host": "fredrir-09",
            "source": "fredrir-09",
            "destination": "fredrir-05",
            "direction": "reverse",
            "marker": self.marker,
            "pairManifestSHA256": self.pair_sha,
            "bootId": self.boot,
            "originalData": {
                name: {"device": 1, "inode": index + 100}
                for index, name in enumerate(("postgres", "valkey"))
            },
        }
        forward = {
            **common,
            "host": "fredrir-05",
            "source": "fredrir-05",
            "destination": "fredrir-09",
            "direction": "forward",
            "marker": {
                **self.marker,
                "executionID": "f" * 32,
                "run": "forward-fixture",
                "direction": "forward",
            },
            "pairManifestSHA256": "f" * 64,
        }
        bundle = validate_bundle(self.pair)
        backup = {
            "schemaVersion": 1,
            "kind": "evacuation-offhost-backup",
            "bundle": bundle,
            "snapshotId": "b" * 64,
            "archiveSHA256": "c" * 64,
        }
        independent = {
            **backup,
            "kind": "evacuation-independent-restore",
            "verified": True,
            "independentHost": True,
        }
        retained = reset.backup_gate(self.pair, backup, independent)
        self.evidence = {
            "execution.json": forward,
            "reverse-execution.json": reverse,
            "writer-failed.json": {
                "kind": "evacuation-target-writer-attempt",
                "candidateSHA256": self.old_sha,
                "host": "fredrir-09",
                "writerStartAttempted": True,
                "connectorStartAttempted": False,
                "pairManifestSHA256": "f" * 64,
            },
            "reverse-native-restore-promoted.json": {
                "kind": "evacuation-native-data-restore",
                "candidateSHA256": self.old_sha,
                "host": "fredrir-05",
                "direction": "reverse",
                "nativeRestoreVerified": True,
                "promoted": True,
                "containersRemoved": True,
                "pairManifestSHA256": self.pair_sha,
            },
            "reverse-backup.json": backup,
            "reverse-admin-restore.json": independent,
            "rollback-writer.json": {
                "kind": "evacuation-source-writer-resume",
                "candidateSHA256": self.old_sha,
                "host": "fredrir-05",
                "writerResumeAttempted": True,
                "privateAcceptancePassed": True,
                "recovery": {"kind": "fresh-reverse-copy", **retained},
            },
            "rollback-connector.json": {
                "kind": "evacuation-source-connector-resume",
                "candidateSHA256": self.old_sha,
                "host": "fredrir-05",
                "connectorActive": True,
            },
            "reconciliation-release.json": {
                "kind": "evacuation-source-reconciliation-release",
                "candidateSHA256": self.old_sha,
                "host": "fredrir-05",
                "completed": True,
                "enabledStatesChanged": False,
                "privateAcceptance": {"connectorActive": True},
                "provider": {
                    "host": "fredrir-05",
                    "originIP": "46.62.214.182",
                    "samples": 3,
                },
            },
        }
        self.units = {
            "schemaVersion": 1,
            "kind": "evacuation-target-unit-promotion",
            "candidateSHA256": self.old_sha,
            "completed": True,
            "files": {
                path: {
                    "sha256": digest,
                    "metadata": {
                        "exists": True,
                        "uid": 0,
                        "gid": 0,
                        "mode": "0644",
                        "inode": index + 1,
                        "device": 1,
                        "sha256": digest,
                    },
                }
                for index, (path, digest) in enumerate(
                    reset.installed_paths(plan).items()
                )
            },
        }

    def write(self, path, data):
        path.write_bytes(data)
        path.chmod(0o600)

    def plan(self):
        return reset.reset_plan(
            self.old, self.new, self.pair, self.evidence, self.units
        )

    def test_verified_recovery_produces_nonexecuting_preservation_plan(self):
        before = {
            path: path.read_bytes() for path in self.root.rglob("*") if path.is_file()
        }
        with patch("subprocess.run", side_effect=AssertionError("No host commands")):
            plan = self.plan()
        self.assertFalse(plan["executionEnabled"])
        self.assertEqual(
            plan["status"], "requires-fresh-inventory-and-reviewed-executor"
        )
        self.assertEqual(len(plan["newInstalledUnitHashes"]), 9)
        self.assertEqual(before, {path: path.read_bytes() for path in before})
        phases = [value["id"] for value in plan["phases"]]
        self.assertLess(
            phases.index("restore-restart-policy"), phases.index("archive-old-units")
        )
        self.assertLess(
            phases.index("archive-approval-markers"),
            phases.index("archive-reverse-fences"),
        )
        self.assertLess(
            phases.index("unload-old-units"), phases.index("archive-reverse-fences")
        )
        self.assertLess(
            phases.index("archive-reverse-fences"),
            phases.index("promote-new-inert-units"),
        )
        self.assertEqual(
            sum(value["seconds"] for value in plan["phases"])
            + plan["budget"]["cleanupReserveSeconds"],
            plan["budget"]["totalSeconds"],
        )
        self.assertIn(
            reset.TARGET + "/target-writer-start-attempted",
            {item["source"] for item in plan["archiveRenames"]},
        )
        for item in plan["dataRenames"]:
            self.assertEqual(
                Path(item["source"]).parent, Path(item["destination"]).parent
            )
            self.assertIn(self.marker["executionID"], item["destination"])

    def test_unrelated_reverse_fence_or_boot_cannot_reset_existing_writer_attempt(self):
        reverse = self.evidence["reverse-execution.json"]
        for key, value in (
            ("bootId", "another-boot"),
            ("marker", {**self.marker, "executionID": "a" * 32}),
            ("pairManifestSHA256", "a" * 64),
        ):
            with self.subTest(key=key):
                original = reverse[key]
                reverse[key] = value
                with self.assertRaises(ValueError):
                    self.plan()
                reverse[key] = original

    def test_incomplete_or_non_reverse_source_recovery_is_rejected(self):
        for name, key, value in (
            ("reverse-native-restore-promoted.json", "promoted", False),
            ("reverse-admin-restore.json", "independentHost", False),
            ("rollback-writer.json", "privateAcceptancePassed", False),
            ("rollback-connector.json", "connectorActive", False),
            ("reconciliation-release.json", "completed", False),
            ("writer-failed.json", "pairManifestSHA256", "a" * 64),
        ):
            with self.subTest(name=name, key=key):
                original = self.evidence[name][key]
                self.evidence[name][key] = value
                with self.assertRaises(ValueError):
                    self.plan()
                self.evidence[name][key] = original

    def test_rehashed_unrelated_candidate_change_is_rejected(self):
        path = self.new / "units/Caddyfile"
        path.write_text(path.read_text().replace("respond 200", "respond 201"))
        metadata = self.new / "staging.json"
        plan = json.loads(metadata.read_bytes())
        plan["unitSHA256"]["units/Caddyfile"] = hashlib.sha256(
            path.read_bytes()
        ).hexdigest()
        self.write(metadata, json.dumps(plan).encode())
        with self.assertRaisesRegex(ValueError, "exact network replacement"):
            self.plan()

    def test_old_unit_receipt_cannot_authorize_changed_or_extra_files(self):
        original = copy.deepcopy(self.units)
        path = next(iter(self.units["files"]))
        for change in ("owner", "hash", "extra"):
            with self.subTest(change=change):
                self.units = copy.deepcopy(original)
                if change == "owner":
                    self.units["files"][path]["metadata"]["uid"] = 2001
                elif change == "hash":
                    self.units["files"][path]["sha256"] = "a" * 64
                else:
                    self.units["files"]["/etc/unrelated.conf"] = original["files"][path]
                with self.assertRaises(ValueError):
                    self.plan()

    def test_reverse_payload_drift_is_rejected_before_planning(self):
        (self.pair / "database.dump").write_bytes(b"PGDMP-changed")
        with self.assertRaises(ValueError):
            self.plan()


if __name__ == "__main__":
    unittest.main()
