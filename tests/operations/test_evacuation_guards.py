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
import evacuation_guards as guards


class FixtureManager:
    def __init__(self, filesystem, host):
        self.fs, self.host = filesystem, host
        self.loaded = host == "fredrir-05"
        self.events = []
        self.probes = {}
        self.missing_path = False
        self.wrong_negation = False
        self.ignore_marker = False
        self.baseline = [
            "ConditionPathExists",
            False,
            False,
            "/existing-prerequisite",
            0,
        ]

    def root(self, manager):
        return guards.ROOTS[self.host]["system" if manager == "system" else "user"]

    def paths(self, manager):
        return [] if self.missing_path else [self.root(manager)]

    def properties(self, manager, unit):
        probe = unit.startswith("infra-evacuation-guard-probe-")
        fragment = self.fs.path(self.root(manager) + "/" + unit)
        loaded = fragment.exists() if probe else self.loaded
        suffix = "95-evacuation-probe.conf" if probe else "95-evacuation-fence.conf"
        dropin = self.root(manager) + "/" + unit + ".d/" + suffix
        active = "inactive" if probe or not self.loaded else "active"
        result = {
            "LoadState": "loaded" if loaded else "not-found",
            "ActiveState": active,
            "SubState": "dead" if active == "inactive" else "running",
            "MainPID": "0" if active == "inactive" else "123",
            "FragmentPath": str(fragment.relative_to(self.fs.root)).join(["/", ""])
            if loaded and probe
            else "/existing/unit",
            "DropInPaths": dropin if self.fs.path(dropin).exists() else "",
            "ConditionResult": "yes",
            "ExecMainStatus": "0",
            "ExecMainStartTimestampMonotonic": "0",
        }
        return result | self.probes.get((manager, unit), {})

    def conditions(self, manager, unit):
        values = (
            [] if unit.startswith("infra-evacuation-guard-probe-") else [self.baseline]
        )
        dropin = self.properties(manager, unit)["DropInPaths"]
        if dropin:
            text = (
                self.fs.path(dropin)
                .read_text()
                .split("ConditionPathExists=", 1)[1]
                .strip()
            )
            values += [
                [
                    "ConditionPathExists",
                    False,
                    text.startswith("!") and not self.wrong_negation,
                    text.lstrip("!"),
                    0,
                ]
            ]
        return values

    def reload(self):
        self.events.append(("daemon-reload",))

    def probe_start(self, manager, unit):
        if unit.endswith(".target"):
            self.events.append(("start", manager, unit))
            self.probes[(manager, unit)] = {
                "ActiveState": "active",
                "SubState": "active",
            }
            self.probe_start(manager, unit.removesuffix(".target") + ".service")
            return
        self.events.append(("start", manager, unit))
        self.fs.path(self.root(manager) + "/" + unit).read_text()
        marker = self.conditions(manager, unit)[0][3]
        skipped = self.fs.path(marker).exists() and not self.ignore_marker
        previous = self.probes.get((manager, unit), {}).get(
            "ExecMainStartTimestampMonotonic", "0"
        )
        self.probes[(manager, unit)] = {
            "ConditionResult": "no" if skipped else "yes",
            "ExecMainStartTimestampMonotonic": previous
            if skipped
            else str(int(previous) + 100),
        }

    def probe_stop(self, manager, unit):
        self.events.append(("stop", manager, unit))
        self.probes[(manager, unit)] = {"ActiveState": "inactive", "SubState": "dead"}
        service = unit.removesuffix(".target") + ".service"
        self.probes.pop((manager, service), None)


class GuardTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.fs = guards.Filesystem(self.root, os.geteuid())
        self.value = guards.plan("fredrir-05", "a" * 64)
        self.manager = FixtureManager(self.fs, "fredrir-05")

    def install(self):
        return guards.install(self.value, _filesystem=self.fs, _manager=self.manager)

    def receipt(self):
        return self.fs.read_json(guards.RECEIPT)

    def verify(self, receipt=None, require_loaded=True):
        return guards.verify_installation(
            receipt or self.receipt(),
            require_loaded,
            _filesystem=self.fs,
            _manager=self.manager,
        )

    def rollback(self, receipt=None):
        return guards.rollback(
            receipt or self.receipt(), _filesystem=self.fs, _manager=self.manager
        )

    def test_inert_installation_verifies_each_manager_and_rollback_preserves_app_state(
        self,
    ):
        before = guards.states(self.manager, self.value["host"])
        result = self.install()
        self.assertTrue(result["exactUnitMergeVerified"])
        self.assertEqual(len(result["files"]), 12)
        self.assertEqual(len(self.receipt()["conditionProbes"]), 4)
        self.assertFalse(any(value["exists"] for value in result["markers"].values()))
        starts = [event for event in self.manager.events if event[0] == "start"]
        self.assertEqual(len(starts), 12)
        self.assertTrue(
            all(
                event[2].startswith("infra-evacuation-guard-probe-") for event in starts
            )
        )
        self.assertEqual(self.install()["guardFilesSHA256"], result["guardFilesSHA256"])
        self.assertEqual(
            len([event for event in self.manager.events if event[0] == "start"]), 12
        )
        self.assertEqual(self.rollback()["status"], "rolled-back")
        self.assertEqual(guards.states(self.manager, self.value["host"]), before)
        self.assertTrue(
            all(not self.fs.path(path).exists() for path in self.value["files"])
        )

    def test_target_is_not_activation_ready_until_real_application_units_merge(self):
        self.value = guards.plan("fredrir-09", "a" * 64)
        self.manager = FixtureManager(self.fs, "fredrir-09")
        self.assertFalse(self.install()["exactUnitMergeVerified"])
        with self.assertRaisesRegex(guards.GuardError, "merge remains unverified"):
            self.verify()
        self.manager.loaded = True
        self.assertTrue(self.verify()["exactUnitMergeVerified"])

    def test_manager_search_path_mismatch_refuses_before_any_guard_write(self):
        self.manager.missing_path = True
        with self.assertRaisesRegex(guards.GuardError, "search path"):
            self.install()
        self.assertFalse(self.fs.path(guards.RECEIPT).exists())
        self.assertTrue(
            all(not self.fs.path(path).exists() for path in self.value["files"])
        )

    def test_non_negated_condition_cannot_be_reported_as_merged(self):
        self.manager.wrong_negation = True
        with self.assertRaisesRegex(guards.GuardError, "negated condition"):
            self.install()
        self.assertEqual(self.receipt()["status"], "recovery-required")
        self.assertEqual(self.rollback()["status"], "rolled-back")

    def test_probe_execution_with_present_marker_refuses_and_retains_recoverable_journal(
        self,
    ):
        self.manager.ignore_marker = True
        with self.assertRaisesRegex(guards.GuardError, "inhibit execution"):
            self.install()
        receipt = self.receipt()
        self.assertEqual(receipt["status"], "recovery-required")
        self.assertIsNotNone(receipt["probeMarker"])
        self.assertFalse(
            any(value["exists"] for value in guards.marker_state(self.fs).values())
        )
        self.rollback()
        self.assertFalse(
            self.fs.path(guards.BASE + "/probe-" + receipt["probeNonce"]).exists()
        )

    def test_later_marker_is_accepted_by_verifier_and_blocks_rollback(self):
        self.install()
        self.fs.write(guards.MARKERS["source-locked"], b"fixed fence observation\n")
        self.assertTrue(self.verify()["markers"]["source-locked"]["exists"])
        with self.assertRaisesRegex(guards.GuardError, "must remain absent"):
            self.rollback()
        self.assertTrue(
            all(self.fs.path(path).exists() for path in self.value["files"])
        )

    def test_changed_guard_prevents_verification_and_any_rollback_deletion(self):
        self.install()
        path = next(iter(self.value["files"]))
        self.fs.path(path).write_text("[Unit]\nConditionPathExists=/different\n")
        for action in [self.verify, self.rollback]:
            with self.assertRaises(guards.GuardError):
                action()
        self.assertTrue(
            all(self.fs.path(path).exists() for path in self.value["files"])
        )

    def test_crafted_receipt_cannot_remove_an_outside_file_or_directory(self):
        self.install()
        self.fs.write("/outside", b"preserve\n")
        for field, payload in [
            ("files", {"/outside": self.fs.metadata("/outside")}),
            ("probeFiles", {"/outside": self.fs.metadata("/outside")}),
            (
                "pendingFile",
                {
                    "path": "/outside",
                    "sha256": self.fs.metadata("/outside")["sha256"],
                    "kind": "guard",
                },
            ),
            (
                "directoriesCreated",
                {
                    "/outside-directory": {
                        "uid": os.geteuid(),
                        "mode": "0755",
                        "inode": 1,
                        "device": 1,
                    }
                },
            ),
        ]:
            receipt = copy.deepcopy(self.receipt())
            receipt[field] = payload
            with self.subTest(field=field), self.assertRaises(guards.GuardError):
                self.rollback(receipt)
            self.assertEqual(self.fs.path("/outside").read_bytes(), b"preserve\n")
            self.assertTrue(
                all(self.fs.path(path).exists() for path in self.value["files"])
            )

    def test_parent_symlink_never_redirects_guard_writes(self):
        (self.root / "etc").mkdir()
        outside = self.root / "outside"
        outside.mkdir()
        (self.root / "etc/systemd").symlink_to(outside)
        with self.assertRaises((guards.GuardError, OSError)):
            self.install()
        self.assertEqual(list(outside.iterdir()), [])

    def test_group_writable_parent_is_rejected(self):
        path = self.root / "etc"
        path.mkdir(mode=0o755)
        path.chmod(0o775)
        with self.assertRaisesRegex(guards.GuardError, "Unsafe guard"):
            self.install()

    def test_restrictive_umask_creates_exact_public_guards_and_private_receipt(self):
        previous = os.umask(0o077)
        try:
            self.install()
        finally:
            os.umask(previous)
        self.assertEqual(self.fs.path(guards.BASE).stat().st_mode & 0o777, 0o755)
        self.assertEqual(self.fs.path(guards.RECEIPT).stat().st_mode & 0o777, 0o600)
        self.assertTrue(
            all(
                self.fs.path(path).stat().st_mode & 0o777 == 0o644
                for path in self.value["files"]
            )
        )

    def test_marker_parent_must_remain_searchable_by_application_users(self):
        self.install()
        self.fs.path(guards.BASE).chmod(0o700)
        with self.assertRaisesRegex(guards.GuardError, "readable by application"):
            self.verify()

    def test_crash_after_guard_write_is_recoverable_from_pending_record(self):
        original = self.fs.write
        crashed = False

        def interrupt(path, data, mode=0o644):
            nonlocal crashed
            result = original(path, data, mode)
            if not crashed:
                crashed = True
                raise RuntimeError("simulated interrupted writer")
            return result

        with (
            patch.object(self.fs, "write", side_effect=interrupt),
            self.assertRaises(RuntimeError),
        ):
            self.install()
        receipt = self.receipt()
        self.assertIsNotNone(receipt["pendingFile"])
        self.assertEqual(self.rollback()["status"], "rolled-back")
        self.assertTrue(
            all(not self.fs.path(path).exists() for path in self.value["files"])
        )

    def test_rollback_preserves_unrelated_file_in_owned_dropin_directory(self):
        self.install()
        directory = str(Path(next(iter(self.value["files"]))).parent)
        self.fs.write(directory + "/99-unrelated.conf", b"[Unit]\n")
        result = self.rollback()
        self.assertIn(directory, result["retainedDirectories"])
        self.assertEqual(
            self.fs.path(directory + "/99-unrelated.conf").read_bytes(), b"[Unit]\n"
        )

    def test_manager_start_api_rejects_application_names(self):
        manager = guards.Manager()
        with (
            patch.object(manager, "invoke") as invoke,
            self.assertRaises(guards.GuardError),
        ):
            manager.probe_start("system", "gitops-pull.service")
        invoke.assert_not_called()
        for unit in [
            "gitops-pull.service",
            "infra-evacuation-guard-probe-" + "a" * 16 + ".service",
        ]:
            with (
                patch.object(manager, "invoke") as invoke,
                self.assertRaises(guards.GuardError),
            ):
                manager.probe_stop("system", unit)
            invoke.assert_not_called()

    def test_typed_condition_lookup_autoloads_without_a_racy_getunit_call(self):
        manager = guards.Manager()
        row = ["ConditionPathExists", False, True, guards.MARKERS["source-locked"], 0]
        with patch.object(
            manager,
            "invoke",
            return_value=json.dumps({"type": "a(sbbsi)", "data": [row]}),
        ) as invoke:
            self.assertEqual(manager.conditions("system", "caddy.service"), [row])
        arguments = invoke.call_args.args[1]
        self.assertIn("get-property", arguments)
        self.assertIn("/org/freedesktop/systemd1/unit/caddy_2eservice", arguments)
        self.assertNotIn("GetUnit", arguments)

    def test_active_reference_target_preserves_execution_evidence_and_is_cleaned(self):
        self.install()
        events = self.manager.events
        for manager in ["system", *guards.USERS]:
            starts = [event[2] for event in events if event[:2] == ("start", manager)]
            self.assertTrue(starts[0].endswith(".target"))
            self.assertTrue(all(unit.endswith(".service") for unit in starts[1:]))
            self.assertEqual(
                len([event for event in events if event[:2] == ("stop", manager)]), 1
            )
        self.assertTrue(
            all(
                value.get("ActiveState", "inactive") == "inactive"
                for value in self.manager.probes.values()
            )
        )

    def test_dbus_condition_parser_preserves_negation_and_refuses_ambiguous_types(self):
        row = ["ConditionPathExists", False, True, guards.MARKERS["source-locked"], 0]
        self.assertEqual(
            guards.condition_rows({"type": "a(sbbsi)", "data": [row]}), [row]
        )
        for changed in [row[:2] + [1] + row[3:], [row], row[:4] + [False]]:
            with self.assertRaises(guards.GuardError):
                guards.condition_rows({"type": "a(sbbsi)", "data": [changed]})


if __name__ == "__main__":
    unittest.main()
