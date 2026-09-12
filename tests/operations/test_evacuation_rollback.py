import hashlib
import sys
import tempfile
import time
import unittest
from contextlib import ExitStack
from pathlib import Path
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
import evacuation_rollback as rollback
from evacuation_execution import canonical
from test_evacuation_execution import inactive


class RollbackTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.marker = {
            "candidateSHA256": "a" * 64,
            "run": "fresh-forward",
            "direction": "forward",
            "executionID": "1" * 32,
        }
        self.execution = {
            "schemaVersion": 1,
            "kind": "evacuation-execution-fence",
            "host": "fredrir-05",
            "direction": "forward",
            "candidateSHA256": "a" * 64,
            "marker": self.marker,
            "exportApplicationName": "infra-evacuation-" + "c" * 12,
            "phase": "pair-sealed",
            "pairManifestSHA256": "d" * 64,
            "originalData": {},
        }
        for name in ("postgres", "valkey"):
            path = self.root / name
            path.mkdir()
            self.execution["originalData"][name] = {
                "device": path.stat().st_dev,
                "inode": path.stat().st_ino,
            }
        self.commands = Mock()
        self.commands.user.return_value = (0, b"")

    def destination(self, attempted=False):
        return {
            "schemaVersion": 1,
            "kind": "evacuation-destination-stopped",
            "host": "fredrir-09",
            "hostname": rollback.cutover.HOSTS["fredrir-09"],
            "observedAt": time.time(),
            "bootId": "12345678-1234-1234-1234-123456789abc",
            "guardFilesSHA256": "b" * 64,
            "writerStartAttempted": attempted,
            "writerMarker": {"candidateSHA256": "a" * 64, "writerStartAttempted": True}
            if attempted
            else None,
            "persistentFenceVerified": attempted,
            "allApplicationAndDataUnitsInactive": True,
            "allServiceUserContainersAbsent": True,
            "reversePairManifestSHA256": "d" * 64 if attempted else None,
            "reverseFenceMarkerSHA256": "e" * 64 if attempted else None,
        }

    def context(self):
        stack = ExitStack()
        for name, value in [
            ("host_identity", None),
            ("guard_state", {"host": "fredrir-05"}),
            ("marker_value", self.marker),
            ("unit_state", inactive()),
            ("archive_processes", []),
        ]:
            stack.enter_context(patch.object(rollback, name, return_value=value))
        stack.enter_context(patch.object(rollback, "DATA_PARENT", self.root))
        return stack

    def test_destination_observation_rejects_age_running_state_and_missing_reverse_identity(
        self,
    ):
        before = self.destination()
        self.assertFalse(rollback.validate_destination(before, "a" * 64))
        after = self.destination(True)
        self.assertTrue(rollback.validate_destination(after, "a" * 64))
        for value in [
            before | {"observedAt": time.time() - 31},
            before | {"observedAt": time.time() + 1},
            before | {"allServiceUserContainersAbsent": False},
            after | {"persistentFenceVerified": False},
            after | {"reversePairManifestSHA256": ""},
            after | {"reverseFenceMarkerSHA256": "bad"},
        ]:
            with self.assertRaises(ValueError):
                rollback.validate_destination(value, "a" * 64)

    def test_any_writer_attempt_refuses_original_source_resume_without_reverse_copy(
        self,
    ):
        with self.context(), patch.object(rollback, "remove_owned_marker") as remove:
            with self.assertRaisesRegex(ValueError, "Fresh reverse copy"):
                rollback.resume_source_writer(
                    {},
                    self.execution,
                    self.destination(True),
                    self.root / "receipt.json",
                    commands=self.commands,
                )
        remove.assert_not_called()
        self.commands.user.assert_not_called()
        self.assertFalse((self.root / "receipt.json").exists())

    def test_same_boot_older_reverse_pair_cannot_satisfy_current_destination_fence(
        self,
    ):
        destination = self.destination(True)
        manifest = {
            "direction": "reverse",
            "destination": "fredrir-05",
            "candidateSHA256": "a" * 64,
            "fenceAfter": {
                "bootId": destination["bootId"],
                "guardFilesSHA256": destination["guardFilesSHA256"],
                "executionMarkerSHA256": destination["reverseFenceMarkerSHA256"],
            },
        }
        with (
            self.context(),
            patch.object(rollback.cutover, "verify_pair", return_value=manifest),
            patch.object(rollback, "promoted_data") as promoted,
            patch.object(rollback, "remove_owned_marker") as remove,
        ):
            with self.assertRaisesRegex(ValueError, "different reverse pair"):
                rollback.resume_source_writer(
                    {},
                    self.execution,
                    destination,
                    self.root / "receipt.json",
                    reverse_pair=self.root / "reverse",
                    restore_receipt={},
                    backup_receipt={},
                    independent_restore={},
                    commands=self.commands,
                )
        promoted.assert_not_called()
        remove.assert_not_called()
        self.commands.user.assert_not_called()

    def test_original_resume_preserves_reconciliation_and_does_not_start_connector(
        self,
    ):
        events = []
        self.commands.user.side_effect = lambda user, args: (
            events.append(("command", args)) or (0, b"")
        )
        with (
            self.context(),
            patch.object(
                rollback,
                "remove_owned_marker",
                side_effect=lambda name, value: events.append(("marker", name)),
            ),
            patch.object(
                rollback,
                "private_acceptance",
                return_value={"authenticatedUserSessionVerified": False},
            ),
        ):
            result = rollback.resume_source_writer(
                {},
                self.execution,
                self.destination(),
                self.root / "receipt.json",
                commands=self.commands,
            )
        self.assertEqual(events[0], ("marker", "source-locked"))
        self.assertTrue(result["privateAcceptancePassed"])
        self.assertFalse(result["connectorResumeAttempted"])
        self.assertFalse(
            any(
                "cloudflared.service" in event[1]
                for event in events
                if event[0] == "command"
            )
        )
        self.assertFalse(
            any(event == ("marker", "reconciliation-locked") for event in events)
        )

    def test_original_resume_refuses_replaced_data_inode(self):
        original = self.root / "postgres"
        original.rename(self.root / "old-postgres")
        original.mkdir()
        with self.context(), patch.object(rollback, "remove_owned_marker") as remove:
            with self.assertRaisesRegex(ValueError, "directory changed"):
                rollback.resume_source_writer(
                    {},
                    self.execution,
                    self.destination(),
                    self.root / "receipt.json",
                    commands=self.commands,
                )
        remove.assert_not_called()

    def test_exact_reverse_pair_requires_native_promotion_and_independent_restore_before_resume(
        self,
    ):
        destination = self.destination(True)
        manifest = {
            "direction": "reverse",
            "destination": "fredrir-05",
            "candidateSHA256": "a" * 64,
            "fenceAfter": {
                "bootId": destination["bootId"],
                "guardFilesSHA256": destination["guardFilesSHA256"],
                "executionMarkerSHA256": destination["reverseFenceMarkerSHA256"],
            },
        }
        destination["reversePairManifestSHA256"] = hashlib.sha256(
            canonical(manifest)
        ).hexdigest()
        with (
            self.context(),
            patch.object(rollback.cutover, "verify_pair", return_value=manifest),
            patch.object(rollback, "promoted_data") as promoted,
            patch.object(
                rollback,
                "backup_gate",
                return_value={"independentRestoreVerified": True},
            ) as backup,
            patch.object(rollback, "remove_owned_marker"),
            patch.object(rollback, "private_acceptance", return_value={}),
        ):
            result = rollback.resume_source_writer(
                {},
                self.execution,
                destination,
                self.root / "receipt.json",
                reverse_pair=self.root / "reverse",
                restore_receipt={"native": "fixture"},
                backup_receipt={"backup": "fixture"},
                independent_restore={"restore": "fixture"},
                commands=self.commands,
            )
        promoted.assert_called_once_with(manifest, {"native": "fixture"})
        backup.assert_called_once_with(
            self.root / "reverse", {"backup": "fixture"}, {"restore": "fixture"}
        )
        self.assertEqual(result["recovery"]["kind"], "fresh-reverse-copy")

    def test_source_settle_stops_only_static_apps_then_native_postgres(self):
        with (
            self.context(),
            patch.object(rollback, "live_fence") as fence,
            patch.object(rollback, "postgres_clients", return_value=0),
            patch.object(rollback, "wait_stopped"),
        ):
            result = rollback.settle_source(
                {}, self.execution, self.root / "settle.json", self.commands
            )
        fence.assert_called_once()
        self.assertTrue(result["completed"])
        self.assertFalse(result["forcedPersistenceStop"])
        commands = [call.args[1] for call in self.commands.user.call_args_list]
        native = [
            value
            for value in commands
            if value[:4] == ["podman", "exec", "llunde-postgres", "pg_ctl"]
        ]
        self.assertEqual(len(native), 1)
        self.assertIn("--mode=fast", native[0])
        self.assertFalse(
            any(
                "rm" in value or "--force" in value or "llunde-valkey.service" in value
                for value in commands
            )
        )


if __name__ == "__main__":
    unittest.main()
