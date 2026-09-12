import copy
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
import evacuation_execution as execution
import evacuation_restart as restart
from evacuation_guards import Filesystem


class NativeFixture:
    def __init__(self, fs, record):
        self.fs, self.record = fs, record
        self.events = []
        self.policy = dict.fromkeys(restart.FIELDS, "") | {
            "LoadState": "loaded",
            "ActiveState": "active",
            "SubState": "running",
            "MainPID": "42",
            "Restart": "always",
            "FragmentPath": "/run/generated/unit",
            "NRestarts": "1",
            "ExecMainStartTimestampMonotonic": "12345",
            "InvocationID": "a" * 32,
        }
        self.current = {"id": "b" * 64, "pid": 50, "running": True}
        self.present = True
        self.on_reload = None

    def user(self, user, args, **kwargs):
        self.events.append(args)
        if args[:3] == ["systemctl", "--user", "show"]:
            return 0, "\n".join(k + "=" + v for k, v in self.policy.items()).encode()
        if args == ["systemctl", "--user", "daemon-reload"]:
            path, _ = restart.paths(self.record, "valkey")
            exists = self.fs.path(path).exists()
            self.policy.update(
                Restart="no" if exists else "always", DropInPaths=path if exists else ""
            )
            if self.on_reload:
                self.on_reload()
            return 0, b""
        if args[:2] == ["podman", "inspect"]:
            return 0, json.dumps(self.current).encode()
        if args[:3] == ["podman", "container", "exists"]:
            return (0 if self.present else 1), b""
        raise AssertionError(args)

    def stop_naturally(self):
        self.present = False
        self.policy.update(ActiveState="inactive", SubState="dead", MainPID="0")


class RestartTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.root.chmod(0o700)
        self.fs = Filesystem(self.root, os.geteuid())
        self.record = {
            "host": "fredrir-05",
            "marker": {"executionID": "1" * 32, "candidateSHA256": "a" * 64},
        }
        self.path, self.receipt = restart.paths(self.record, "valkey")
        for name in (self.path, self.receipt):
            self.fs.path(name).parent.mkdir(parents=True, exist_ok=True)
        self.commands = NativeFixture(self.fs, self.record)
        self.fence = patch.object(restart, "check_fence").start()
        self.addCleanup(patch.stopall)
        patch.object(execution, "process_identity", return_value="identity").start()

    def install(self):
        return restart.install(
            self.commands, self.record, "valkey", _filesystem=self.fs
        )

    def restore(self):
        return restart.restore(
            self.commands, self.record, "valkey", _filesystem=self.fs
        )

    def test_policy_is_installed_before_shutdown_and_restored_without_starting_service(
        self,
    ):
        result = self.install()
        self.assertEqual(result["restart"], "no")
        self.assertEqual(self.fs.path(self.path).read_bytes(), restart.CONTENT)
        restart.verify(self.commands, self.record, "valkey", _filesystem=self.fs)
        self.commands.stop_naturally()
        restart.verify(
            self.commands,
            self.record,
            "valkey",
            stopped_required=True,
            _filesystem=self.fs,
        )
        self.restore()
        self.assertFalse(self.fs.path(self.path).exists())
        self.assertEqual(self.commands.policy["Restart"], "always")
        self.assertEqual(self.commands.policy["ActiveState"], "inactive")
        self.assertEqual(self.fs.read_json(self.receipt)["status"], "restored")
        self.assertFalse(
            any(
                "start" in args or "stop" in args or "restart" in args
                for args in self.commands.events
            )
        )

    def test_process_change_during_reload_retains_inhibitor_and_recovery_receipt(self):
        self.commands.on_reload = lambda: self.commands.current.update(id="c" * 64)
        with self.assertRaisesRegex(ValueError, "changed while inhibiting"):
            self.install()
        self.assertEqual(self.fs.read_json(self.receipt)["status"], "written")
        self.assertTrue(self.fs.path(self.path).exists())

    def test_replacement_name_or_restart_counter_blocks_stopped_export_proof(self):
        self.install()
        self.commands.stop_naturally()
        self.commands.present = True
        with self.assertRaisesRegex(ValueError, "Replacement"):
            restart.verify(
                self.commands,
                self.record,
                "valkey",
                stopped_required=True,
                _filesystem=self.fs,
            )
        self.commands.present = False
        self.commands.policy["NRestarts"] = "2"
        with self.assertRaisesRegex(ValueError, "restarted during fence"):
            restart.verify(
                self.commands,
                self.record,
                "valkey",
                stopped_required=True,
                _filesystem=self.fs,
            )

    def test_missing_or_pending_policy_fields_refuse_mutation(self):
        for name in ("Job", "RestartForceExitStatus", "RestartPreventExitStatus"):
            with self.subTest(name=name):
                original = self.commands.policy.pop(name)
                with self.assertRaisesRegex(ValueError, "projection"):
                    self.install()
                self.commands.policy[name] = original
        self.commands.policy["Job"] = "123 /org/freedesktop/job/123"
        with self.assertRaisesRegex(ValueError, "queued jobs"):
            self.install()
        self.assertFalse(self.fs.path(self.path).exists())
        self.assertFalse(self.fs.path(self.receipt).exists())

    def test_partial_write_can_only_remove_receipt_bound_inode_and_expected_prefix(
        self,
    ):
        native_write = os.write

        def partial(fd, data):
            if data == restart.CONTENT:
                native_write(fd, data[:7])
                raise OSError("fixture interrupted write")
            return native_write(fd, data)

        with (
            patch.object(restart.os, "write", side_effect=partial),
            self.assertRaises(OSError),
        ):
            self.install()
        self.assertEqual(self.fs.read_json(self.receipt)["status"], "allocated")
        self.restore()
        self.assertFalse(self.fs.path(self.path).exists())

    def test_short_write_stays_allocated_and_can_restore_owned_prefix(self):
        native_write = os.write

        def short(fd, data):
            return native_write(fd, data[:7] if data == restart.CONTENT else data)

        with (
            patch.object(restart.os, "write", side_effect=short),
            self.assertRaisesRegex(ValueError, "Incomplete"),
        ):
            self.install()
        self.assertEqual(self.fs.read_json(self.receipt)["status"], "allocated")
        self.assertFalse(
            any(
                args == ["systemctl", "--user", "daemon-reload"]
                for args in self.commands.events
            )
        )
        self.restore()
        self.assertFalse(self.fs.path(self.path).exists())

    def test_replaced_inode_with_identical_content_is_not_owned(self):
        self.install()
        path = self.fs.path(self.path)
        path.rename(path.with_suffix(".original"))
        path.write_bytes(restart.CONTENT)
        path.chmod(0o644)
        with self.assertRaisesRegex(ValueError, "ownership changed"):
            self.restore()
        self.assertTrue(path.exists())

    def test_hardlinked_restart_file_is_never_accepted(self):
        self.install()
        os.link(self.fs.path(self.path), self.root / "additional-link")
        with self.assertRaisesRegex(ValueError, "Owned regular"):
            self.restore()
        self.assertTrue(self.fs.path(self.path).exists())

    def test_failure_after_unlink_can_retry_original_policy_readback(self):
        self.install()
        save = self.fs.save

        def fail_removed(path, value):
            if value["status"] == "removed":
                raise OSError("fixture receipt failure")
            save(path, value)

        with (
            patch.object(self.fs, "save", side_effect=fail_removed),
            self.assertRaises(OSError),
        ):
            self.restore()
        self.assertFalse(self.fs.path(self.path).exists())
        self.assertEqual(self.fs.read_json(self.receipt)["status"], "restoring")
        self.restore()
        self.assertEqual(self.fs.read_json(self.receipt)["status"], "restored")

    def test_reload_failure_retains_recoverable_file(self):
        self.commands.on_reload = lambda: (_ for _ in ()).throw(
            ValueError("fixture reload")
        )
        with self.assertRaisesRegex(ValueError, "fixture reload"):
            self.install()
        self.commands.on_reload = None
        self.restore()
        self.assertEqual(self.commands.policy["Restart"], "always")

    def test_tampered_or_unowned_inode_is_never_removed(self):
        self.install()
        self.fs.path(self.path).write_bytes(b"[Service]\nRestart=always\n")
        with self.assertRaisesRegex(ValueError, "content changed"):
            self.restore()
        self.assertTrue(self.fs.path(self.path).exists())

    def test_wrong_execution_cannot_remove_existing_override(self):
        self.install()
        other = copy.deepcopy(self.record)
        other["marker"]["executionID"] = "2" * 32
        with self.assertRaisesRegex(ValueError, "Unowned"):
            restart.restore(self.commands, other, "valkey", _filesystem=self.fs)
        self.assertTrue(self.fs.path(self.path).exists())

    def test_symlink_and_unwritable_ancestry_are_not_followed(self):
        target = self.root / "outside"
        target.write_bytes(b"preserved")
        self.fs.path(self.path).symlink_to(target)
        with self.assertRaises(OSError):
            self.install()
        self.assertEqual(target.read_bytes(), b"preserved")
        self.fs.path(self.path).unlink()
        self.fs.path(self.path).parent.chmod(0o777)
        with self.assertRaisesRegex(ValueError, "Unsafe guard"):
            self.install()

    def test_inhibitor_change_after_install_blocks_fence_observation(self):
        self.install()
        self.commands.policy["Restart"] = "always"
        with self.assertRaisesRegex(ValueError, "loaded restart inhibitor"):
            restart.verify(self.commands, self.record, "valkey", _filesystem=self.fs)


if __name__ == "__main__":
    unittest.main()
