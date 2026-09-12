import copy
import hashlib
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
import evacuation_backup as backup
import evacuation_recurring_backup as recurring
from test_evacuation_backup import FakeRestic


class RecurringBackupTests(unittest.TestCase):
    def test_declared_export_limit_is_accepted_by_the_actual_streaming_primitive(self):
        for maximum in (recurring.MAX_DUMP, recurring.MAX_RDB):
            code, output = recurring.Commands(5).run(
                [sys.executable, "-c", 'print("bounded native stream")'],
                maximum=maximum,
            )
            self.assertEqual((code, output), (0, b"bounded native stream\n"))

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.root.chmod(0o700)
        self.bundle = self.root / "bundle"
        self.bundle.mkdir(mode=0o700)
        data = {"database.dump": b"PGDMP-fixture", "dump.rdb": b"REDIS-fixture"}
        self.manifest = {
            "schemaVersion": 1,
            "kind": recurring.KIND,
            "host": "fredrir-09",
            "consistency": "independent-datastore-points",
            "writersFenced": False,
            "candidateSHA256": "a" * 64,
            "imageIDs": {
                "llunde-postgres": "sha256:" + "b" * 64,
                "llunde-valkey": "sha256:" + "c" * 64,
            },
            "recoveryPoints": {
                "database.dump": {"startedAt": 1000, "completedAt": 1010},
                "dump.rdb": {"startedAt": 1011, "completedAt": 1012},
            },
            "files": {
                name: {"sha256": hashlib.sha256(raw).hexdigest(), "bytes": len(raw)}
                for name, raw in data.items()
            },
        }
        for name, raw in data.items():
            backup.write_private(self.bundle / name, raw)
        backup.write_private(
            self.bundle / "manifest.json", json.dumps(self.manifest).encode()
        )

    def test_online_native_files_round_trip_through_existing_offhost_archive_contract(
        self,
    ):
        restic = FakeRestic()
        with patch.object(backup, "require_upload_host"):
            receipt = backup.backup(self.bundle, restic, self.root)
        restored = self.root / "restored"
        proof = backup.restore(receipt, restored, restic)
        self.assertTrue(proof["verified"])
        self.assertFalse(proof["applicationRestoreVerified"])
        self.assertEqual(backup.read_json(restored / "manifest.json"), self.manifest)
        self.assertEqual(receipt["tag"], "evacuation-evacuation-online-state")
        self.assertFalse(backup.read_json(restored / "manifest.json")["writersFenced"])

    def test_online_manifest_rejects_paired_claims_unbounded_times_and_unexpected_files(
        self,
    ):
        changes = [
            lambda x: x.update(writersFenced=True),
            lambda x: x.update(consistency="paired"),
            lambda x: x["recoveryPoints"]["dump.rdb"].update(completedAt=2000),
            lambda x: x["recoveryPoints"]["dump.rdb"].update(startedAt=True),
            lambda x: x["files"].update({"../escape": x["files"]["dump.rdb"]}),
            lambda x: x["files"]["dump.rdb"].update(bytes=1),
            lambda x: x["imageIDs"].update(extra="sha256:" + "a" * 64),
        ]
        for change in changes:
            value = copy.deepcopy(self.manifest)
            change(value)
            with self.assertRaises(ValueError):
                recurring.validate_online(self.bundle, value)

    def test_native_magic_and_hashes_are_both_required(self):
        path = self.bundle / "dump.rdb"
        path.write_bytes(b"wrong-header")
        value = copy.deepcopy(self.manifest)
        value["files"]["dump.rdb"] = {
            "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
            "bytes": path.stat().st_size,
        }
        with self.assertRaisesRegex(ValueError, "format"):
            recurring.validate_online(self.bundle, value)

    def test_cutover_lock_prevents_any_export(self):
        with (
            patch.object(recurring.os.path, "lexists", return_value=True),
            patch.object(recurring, "validate_plan") as plan,
        ):
            with self.assertRaisesRegex(ValueError, "Cutover lock"):
                recurring.capture("/unused", self.bundle, None)
            plan.assert_not_called()

    def test_capture_uses_bounded_native_exports_and_records_two_distinct_points(self):
        for path in self.bundle.iterdir():
            path.unlink()
        candidate = self.root / "candidate"
        candidate.mkdir()
        (candidate / "staging.json").write_bytes(b"candidate")
        images = self.manifest["imageIDs"]
        plan = {
            "services": {
                name: {"runtimeImage": image} for name, image in images.items()
            }
        }

        class Commands:
            def __init__(self):
                self.calls = []

            def user(self, user, arguments, **kwargs):
                self.calls.append((user, arguments, kwargs))
                kwargs["output"].write(
                    b"PGDMP-fixture" if "pg_dump" in arguments else b"REDIS-fixture"
                )
                return 0, b""

        commands = Commands()
        with (
            patch.object(recurring, "markers_absent"),
            patch.object(recurring, "validate_plan", return_value=plan),
            patch.object(
                recurring,
                "containers",
                return_value={"llunde-postgres": "a" * 64, "llunde-valkey": "b" * 64},
            ),
            patch.object(recurring, "valkey_headroom"),
            patch.object(
                recurring.shutil,
                "disk_usage",
                return_value=type("Space", (), {"free": 10 * 1024**3})(),
            ),
            patch.object(recurring.time, "time", side_effect=[1000, 1010, 1011, 1012]),
        ):
            result = recurring.capture(candidate, self.bundle, commands)
        self.assertEqual(result["recoveryPoints"], self.manifest["recoveryPoints"])
        self.assertFalse(result["writersFenced"])
        self.assertEqual([call[2]["timeout"] for call in commands.calls], [130, 70])
        self.assertTrue(
            all("timeout" in call[1] and "-k" in call[1] for call in commands.calls)
        )
        self.assertTrue(all(call[0] == "llunde-backend" for call in commands.calls))
        self.assertEqual(backup.validate_bundle(self.bundle)["kind"], recurring.KIND)

    def test_export_failure_never_creates_a_success_manifest(self):
        for path in self.bundle.iterdir():
            path.unlink()
        candidate = self.root / "candidate"
        candidate.mkdir()
        (candidate / "staging.json").write_bytes(b"candidate")

        class Commands:
            def user(self, *args, **kwargs):
                kwargs["output"].write(b"PGDMP-partial")
                raise ValueError("export failed")

        plan = {
            "services": {
                name: {"runtimeImage": image}
                for name, image in self.manifest["imageIDs"].items()
            }
        }
        with (
            patch.object(recurring, "markers_absent"),
            patch.object(recurring, "validate_plan", return_value=plan),
            patch.object(
                recurring,
                "containers",
                return_value={"llunde-postgres": "a" * 64, "llunde-valkey": "b" * 64},
            ),
            patch.object(
                recurring.shutil,
                "disk_usage",
                return_value=type("Space", (), {"free": 10 * 1024**3})(),
            ),
            self.assertRaisesRegex(ValueError, "export failed"),
        ):
            recurring.capture(candidate, self.bundle, Commands())
        self.assertFalse((self.bundle / "manifest.json").exists())

    def test_valkey_fork_headroom_and_existing_persistence_are_required(self):
        class Commands:
            def __init__(self, memory, persistence):
                self.values = iter([memory, persistence])

            def user(self, *args, **kwargs):
                return 0, next(self.values)

        stable = b"aof_enabled:1\naof_last_write_status:ok\nrdb_bgsave_in_progress:0\naof_rewrite_in_progress:0\n"
        recurring.valkey_headroom(Commands(b"used_memory:1048576\n", stable))
        for memory, persistence in [
            (b"used_memory:100000000\n", stable),
            (
                b"used_memory:1048576\n",
                stable.replace(b"last_write_status:ok", b"last_write_status:err"),
            ),
            (
                b"used_memory:1048576\n",
                stable.replace(b"bgsave_in_progress:0", b"bgsave_in_progress:1"),
            ),
        ]:
            with self.assertRaises(ValueError):
                recurring.valkey_headroom(Commands(memory, persistence))

    def test_status_uses_oldest_capture_and_requires_an_acknowledgement(self):
        key, hosts = self.root / "key", self.root / "known_hosts"
        backup.write_private(key, b"private fixture")
        backup.write_private(hosts, b"public fixture")

        class Commands:
            def run(self, args, **kwargs):
                self.args, self.payload = args, json.load(kwargs["source"])
                return 0, b"backup-status: accepted\n"

        commands = Commands()
        receipt = {"snapshotId": "d" * 64, "archiveSHA256": "e" * 64, "createdAt": 1013}
        recurring.deliver_status(
            commands, receipt, self.manifest, key, hosts, "172.232.145.251"
        )
        self.assertEqual(commands.payload["recoveryPointAt"], 1000)
        self.assertIn("StrictHostKeyChecking=yes", commands.args)
        self.assertIn("IdentityAgent=none", commands.args)
        self.assertEqual(commands.args[-1], "infra-backup-status@172.232.145.251")
        with patch.object(commands, "run", return_value=(0, b"wrong acknowledgement")):
            with self.assertRaises(ValueError):
                recurring.deliver_status(
                    commands, receipt, self.manifest, key, hosts, "172.232.145.251"
                )

    def test_failed_upload_sends_no_status_and_delivery_failure_retains_snapshot_receipt(
        self,
    ):
        commands = Mock(deadline=9999999999)
        restic = Mock()
        restic.identity.return_value = recurring.REPOSITORY_ID
        receipt = {"snapshotId": "f" * 64, "archiveSHA256": "e" * 64, "createdAt": 1020}
        for upload_failure in (True, False):
            with (
                patch.object(recurring, "host_identity"),
                patch.object(recurring, "capture", return_value=self.manifest),
                patch.object(recurring, "markers_absent"),
                patch.object(backup, "decrypt_credentials", return_value={}),
                patch.object(backup, "Restic", return_value=restic),
                patch.object(
                    backup,
                    "backup",
                    side_effect=ValueError("upload failed") if upload_failure else None,
                    return_value=receipt,
                ),
                patch.object(
                    recurring,
                    "deliver_status",
                    side_effect=ValueError("delivery failed"),
                ) as delivery,
            ):
                with self.assertRaisesRegex(
                    ValueError, "upload failed" if upload_failure else "delivery failed"
                ):
                    recurring.run(
                        "/candidate",
                        "/credentials",
                        "/identity",
                        self.root,
                        "/key",
                        "/known_hosts",
                        "172.232.145.251",
                        commands,
                    )
                if upload_failure:
                    delivery.assert_not_called()
                    self.assertFalse(
                        (self.root / ("snapshot-" + "f" * 64 + ".json")).exists()
                    )
                else:
                    self.assertEqual(
                        backup.read_json(
                            self.root / ("snapshot-" + "f" * 64 + ".json")
                        ),
                        receipt,
                    )
                self.assertFalse(
                    any(path.name.startswith("online-") for path in self.root.iterdir())
                )


if __name__ == "__main__":
    unittest.main()
