import copy
import importlib.util
import json
import os
import pathlib
import tempfile
import time
import unittest
from types import SimpleNamespace
from unittest import mock

PATH = (
    pathlib.Path(__file__).resolve().parents[2]
    / "scripts/operations/evacuation_maintenance.py"
)
SPEC = importlib.util.spec_from_file_location("evacuation_maintenance", PATH)
m = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(m)


class FakeNative:
    def __init__(self, second_active=True):
        self.states = {
            name: {
                "Id": name,
                "LoadState": "loaded",
                "ActiveState": "active",
                "SubState": "waiting",
                "Job": "",
                "UnitFileState": "enabled",
                "FragmentPath": "/usr/lib/systemd/system/" + name,
                "DropInPaths": "",
            }
            for name in m.TIMERS
        }
        if not second_active:
            self.states[m.TIMERS[1]]["ActiveState"] = "inactive"
        self.actions = []
        self.busy = False
        self.fail = None
        self.after_action = None
        self.machine = {
            "hostname": "cloud-server-10643982",
            "machineId": "m",
            "bootId": "b",
        }

    def identity(self):
        return self.machine.copy()

    def state(self, name):
        return self.states[name].copy()

    def configuration(self, state):
        return {"enabled": state["UnitFileState"], "files": [state["FragmentPath"]]}

    def idle(self):
        m.require(not self.busy, "Package service active or queued")
        return {"packageProcessesAbsent": True, "packageLocksUnheld": True}

    def action(self, action, name):
        self.actions.append((action, name))
        self.states[name]["ActiveState"] = "inactive" if action == "stop" else "active"
        if self.after_action:
            self.after_action(action, name)
        if self.fail == (action, name):
            raise ValueError("Injected interrupted command")


class FakeReceipt:
    def __init__(self):
        self.last = None
        self.history = []

    def write(self, record):
        self.last = copy.deepcopy(record)
        self.history.append(copy.deepcopy(record))


class MaintenanceTests(unittest.TestCase):
    def test_restores_only_originally_active_timer_without_changing_enablement(self):
        native, receipt = FakeNative(False), FakeReceipt()
        record = m.pause(native, receipt, 1800)
        m.restore(native, receipt, record)
        self.assertEqual(
            native.actions, [("stop", m.TIMERS[0]), ("start", m.TIMERS[0])]
        )
        self.assertTrue(
            all(s["UnitFileState"] == "enabled" for s in native.states.values())
        )

    def test_active_package_work_prevents_any_timer_mutation(self):
        native, receipt = FakeNative(), FakeReceipt()
        native.busy = True
        with self.assertRaisesRegex(ValueError, "Package service"):
            m.pause(native, receipt, 1800)
        self.assertEqual(native.actions, [])

    def test_timer_firing_during_pause_never_produces_safe_window(self):
        native, receipt = FakeNative(), FakeReceipt()
        native.after_action = lambda action, name: setattr(native, "busy", True)
        with self.assertRaisesRegex(ValueError, "Package service"):
            m.pause(native, receipt, 1800)
        self.assertEqual(receipt.last["status"], "pausing")
        native.after_action = None
        m.restore(native, receipt, receipt.last)
        self.assertEqual(receipt.last["status"], "restored")

    def test_interruption_after_stop_intent_is_recoverable(self):
        native, receipt = FakeNative(), FakeReceipt()
        native.fail = ("stop", m.TIMERS[0])
        with self.assertRaisesRegex(ValueError, "Injected"):
            m.pause(native, receipt, 1800)
        self.assertTrue(receipt.last["timers"][m.TIMERS[0]]["stopIntent"])
        self.assertFalse(receipt.last["timers"][m.TIMERS[0]]["stopped"])
        native.fail = None
        m.restore(native, receipt, receipt.last)
        self.assertEqual(native.actions[-1], ("start", m.TIMERS[0]))

    def test_configuration_drift_prevents_restoration(self):
        native, receipt = FakeNative(), FakeReceipt()
        record = m.pause(native, receipt, 1800)
        native.states[m.TIMERS[0]]["UnitFileState"] = "disabled"
        before = native.actions.copy()
        with self.assertRaisesRegex(ValueError, "configuration"):
            m.restore(native, receipt, record)
        self.assertEqual(native.actions, before)

    def test_partial_restore_can_retry_without_restarting_completed_timer(self):
        native, receipt = FakeNative(), FakeReceipt()
        record = m.pause(native, receipt, 1800)
        native.fail = ("start", m.TIMERS[1])
        with self.assertRaisesRegex(ValueError, "Injected"):
            m.restore(native, receipt, record)
        native.fail = None
        m.restore(native, receipt, receipt.last)
        self.assertEqual(native.actions.count(("start", m.TIMERS[0])), 1)
        self.assertEqual(native.actions.count(("start", m.TIMERS[1])), 1)

    def test_restarted_persistent_timer_may_launch_package_work(self):
        native, receipt = FakeNative(), FakeReceipt()
        record = m.pause(native, receipt, 1800)
        native.after_action = lambda action, name: setattr(
            native, "busy", action == "start"
        )
        result = m.restore(native, receipt, record)
        self.assertEqual(result["status"], "restored")

    def test_expired_window_fails_verification_but_can_restore(self):
        native, receipt = FakeNative(), FakeReceipt()
        record = m.pause(native, receipt, 1800)
        record["expiresMonotonic"] = time.monotonic() - 1
        with self.assertRaisesRegex(ValueError, "expired"):
            m.verify(native, record)
        self.assertEqual(m.restore(native, receipt, record)["status"], "restored")

    def test_changed_boot_refuses_actions(self):
        native, receipt = FakeNative(), FakeReceipt()
        record = m.pause(native, receipt, 1800)
        native.machine["bootId"] = "different"
        with self.assertRaisesRegex(ValueError, "identity"):
            m.restore(native, receipt, record)
        self.assertEqual(len(native.actions), 2)

    def test_already_restored_timer_drift_is_not_repaired(self):
        native, receipt = FakeNative(), FakeReceipt()
        record = m.restore(native, receipt, m.pause(native, receipt, 1800))
        native.states[m.TIMERS[0]]["ActiveState"] = "inactive"
        with self.assertRaisesRegex(ValueError, "Restored timer changed"):
            m.restore(native, receipt, record)
        self.assertEqual(len(native.actions), 4)

    def test_all_native_mutations_are_exact_timer_start_stop(self):
        native = m.Native()
        for action, name in [
            ("restart", m.TIMERS[0]),
            ("stop", m.SERVICES[0]),
            ("disable", m.TIMERS[0]),
        ]:
            with self.assertRaisesRegex(ValueError, "Only timer"):
                native.action(action, name)

    def test_native_empty_job_output_is_valid_but_matching_queue_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            proc = pathlib.Path(tmp)
            (proc / "locks").write_text("")
            native = m.Native()
            with (
                mock.patch.object(m, "PROC", proc),
                mock.patch.object(m, "LOCKS", ()),
                mock.patch.object(
                    native, "state", return_value={"ActiveState": "inactive", "Job": ""}
                ),
                mock.patch.object(native, "run", return_value="") as run,
            ):
                self.assertTrue(native.idle()["packageLocksUnheld"])
                run.return_value = json.dumps([{"unit": m.SERVICES[0]}])
                with self.assertRaisesRegex(ValueError, "job queued"):
                    native.idle()

    def test_native_monitor_is_ignored_but_actual_upgrader_blocks(self):
        with tempfile.TemporaryDirectory() as tmp:
            proc = pathlib.Path(tmp)
            (proc / "locks").write_text("")
            process = proc / "123"
            process.mkdir()
            (process / "comm").write_text("unattended-upgr")
            (process / "cmdline").write_bytes(
                b"python3\0/usr/share/unattended-upgrades/unattended-upgrade-shutdown\0--wait-for-signal\0"
            )
            native = m.Native()
            with (
                mock.patch.object(m, "PROC", proc),
                mock.patch.object(m, "LOCKS", ()),
                mock.patch.object(
                    native, "state", return_value={"ActiveState": "inactive", "Job": ""}
                ),
                mock.patch.object(native, "run", return_value="[]"),
            ):
                self.assertTrue(native.idle()["packageProcessesAbsent"])
                (process / "comm").write_text("python3")
                (process / "cmdline").write_bytes(
                    b"python3\0/usr/bin/unattended-upgrade\0"
                )
                with self.assertRaisesRegex(ValueError, "Package process"):
                    native.idle()

    def test_native_held_dpkg_lock_blocks(self):
        with tempfile.TemporaryDirectory() as tmp:
            proc = pathlib.Path(tmp)
            lock = proc / "dpkg-lock"
            lock.write_text("")
            st = lock.stat()
            (proc / "locks").write_text(
                f"1: POSIX ADVISORY WRITE 123 {os.major(st.st_dev):x}:{os.minor(st.st_dev):x}:{st.st_ino} 0 EOF\n"
            )
            native = m.Native()
            with (
                mock.patch.object(m, "PROC", proc),
                mock.patch.object(m, "LOCKS", (str(lock),)),
                mock.patch.object(
                    native, "state", return_value={"ActiveState": "inactive", "Job": ""}
                ),
                mock.patch.object(native, "run", return_value="[]"),
                self.assertRaisesRegex(ValueError, "Package lock"),
            ):
                native.idle()

    def test_orphaned_prior_window_prevents_new_baseline(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = pathlib.Path(tmp)
            old = base / ("1" * 16)
            old.mkdir(mode=0o700)
            path = old / "receipt.json"
            path.write_text(
                json.dumps(
                    {
                        "schemaVersion": 1,
                        "kind": "evacuation-maintenance-window",
                        "status": "paused",
                    }
                )
            )
            path.chmod(0o600)
            real_fstat = os.fstat

            def root_metadata(fd):
                st = real_fstat(fd)
                return SimpleNamespace(
                    st_mode=st.st_mode,
                    st_uid=0,
                    st_nlink=st.st_nlink,
                    st_size=st.st_size,
                )

            with (
                mock.patch.object(m, "BASE", base),
                mock.patch.object(m.Receipt, "check_directory"),
                mock.patch.object(m.os, "fstat", side_effect=root_metadata),
            ):
                with self.assertRaisesRegex(ValueError, "previous maintenance"):
                    m.Receipt.require_no_open_window()
                path.write_text(
                    json.dumps(
                        {
                            "schemaVersion": 1,
                            "kind": "evacuation-maintenance-window",
                            "status": "restored",
                        }
                    )
                )
                m.Receipt.require_no_open_window()

    def test_enable_drift_during_last_restart_is_not_reported_as_success(self):
        native, receipt = FakeNative(), FakeReceipt()
        record = m.pause(native, receipt, 1800)

        def drift(action, name):
            if action == "start" and name == m.TIMERS[1]:
                native.states[m.TIMERS[0]]["UnitFileState"] = "disabled"

        native.after_action = drift
        with self.assertRaisesRegex(ValueError, "configuration"):
            m.restore(native, receipt, record)
        self.assertEqual(receipt.last["status"], "restoring")


if __name__ == "__main__":
    unittest.main()
