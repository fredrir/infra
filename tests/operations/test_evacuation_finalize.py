import copy
import hashlib
import json
import sys
import tempfile
import unittest
from datetime import UTC, datetime
from pathlib import Path
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
import evacuation_finalize as finalize
from test_evacuation_execution import inactive


def provider(host="fredrir-05"):
    return {
        "schemaVersion": 1,
        "kind": "evacuation-cloudflare-origin-observation",
        "tunnelId": finalize.TUNNEL,
        "expectedHost": host,
        "expectedOriginIP": finalize.ORIGINS[host],
        "expectedConnectorId": "12345678-1234-1234-1234-123456789abc",
        "providerInventoryMatches": True,
        "samples": [
            {
                "observedAt": datetime.fromtimestamp(timestamp, UTC).isoformat(),
                "matches": True,
                "connectors": [
                    {
                        "id": "12345678-1234-1234-1234-123456789abc",
                        "connections": [{"originIP": finalize.ORIGINS[host]}],
                    }
                ],
            }
            for timestamp in (100, 102, 104)
        ],
    }


class FinalizationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def test_provider_acceptance_rejects_other_origins_old_samples_and_extra_clients(
        self,
    ):
        self.assertEqual(
            finalize.provider_authority(provider(), "fredrir-05", now=105)["samples"], 3
        )
        for change in [
            lambda value: value.update(expectedHost="fredrir-09"),
            lambda value: value.update(providerInventoryMatches=False),
            lambda value: value["samples"][1]["connectors"].append(
                copy.deepcopy(value["samples"][1]["connectors"][0])
            ),
            lambda value: value["samples"][2]["connectors"][0]["connections"][0].update(
                originIP="192.0.2.3"
            ),
            lambda value: value["samples"][1].update(matches=False),
        ]:
            value = provider()
            change(value)
            with self.assertRaises(ValueError):
                finalize.provider_authority(value, "fredrir-05", now=105)
        for now in (103, 195):
            with self.assertRaises(ValueError):
                finalize.provider_authority(provider(), "fredrir-05", now=now)

    def test_boot_order_requires_exact_files_and_loaded_systemd_dependencies(self):
        expected = b"[Unit]\nRequires=infra-evacuation-secrets.service\nAfter=infra-evacuation-secrets.service\n"
        filesystem = Mock()
        filesystem.read.return_value = ({"mode": "0644"}, expected)

        def properties(commands, unit, names):
            if unit == "infra-evacuation-secrets.service":
                return {
                    "LoadState": "loaded",
                    "ActiveState": "active",
                    "SubState": "exited",
                    "Before": "user@2000.service user@2001.service user@2002.service",
                }
            return {
                "LoadState": "loaded",
                "ActiveState": "active",
                "Requires": "infra-evacuation-secrets.service",
                "After": "infra-evacuation-secrets.service",
                "DropInPaths": "/etc/systemd/system/"
                + unit
                + ".d/infra-evacuation.conf",
            }

        with patch.object(finalize, "properties", side_effect=properties):
            self.assertEqual(
                set(finalize.boot_dependencies(Mock(), filesystem)),
                {"2000", "2001", "2002"},
            )
            filesystem.read.return_value = (
                {"mode": "0644"},
                expected.replace(b"Requires=", b"Wants="),
            )
            with self.assertRaisesRegex(ValueError, "Exact user-manager"):
                finalize.boot_dependencies(Mock(), filesystem)
        filesystem.read.return_value = ({"mode": "0644"}, expected)
        with (
            patch.object(
                finalize,
                "properties",
                return_value={
                    "LoadState": "loaded",
                    "ActiveState": "inactive",
                    "SubState": "dead",
                    "Before": "",
                },
            ),
            self.assertRaisesRegex(ValueError, "renderer"),
        ):
            finalize.boot_dependencies(Mock(), filesystem)

    def test_partial_linger_activation_retains_receipt_without_restarting_apps(self):
        from contextlib import ExitStack

        manifest = {"candidateSHA256": "a" * 64}
        digest = hashlib.sha256(finalize.canonical(manifest)).hexdigest()
        writer = {
            "kind": "evacuation-target-writer-attempt",
            "privateAcceptancePassed": True,
            "pairManifestSHA256": digest,
        }
        connector = {
            "kind": "evacuation-connector-handoff",
            "connectorActive": True,
            "pairManifestSHA256": digest,
        }
        enabled = set()
        commands = Mock()

        def run(arguments):
            if arguments[1] == "enable-linger":
                if arguments[2] == "2001":
                    raise ValueError("fixture linger failure")
                enabled.add(arguments[2])
            return 0, b"yes\n" if arguments[2] in enabled else b"no\n"

        commands.run.side_effect = run
        output = self.root / "linger.json"
        with ExitStack() as stack:
            for name, value in [
                ("host_identity", None),
                ("verify_fresh_source", manifest),
                ("marker_value", {"pairManifestSHA256": digest}),
                ("guard_state", {}),
                ("verify_unit_files", None),
                ("checkpoint_proof", {}),
                ("provider_authority", {}),
                ("private_acceptance", {}),
                ("public_acceptance", {}),
                ("boot_dependencies", {"verified": True}),
            ]:
                stack.enter_context(patch.object(finalize, name, return_value=value))
            stack.enter_context(
                patch.object(finalize, "MARKERS", self.root / "markers")
            )
            with self.assertRaisesRegex(ValueError, "linger failure"):
                finalize.persist_target(
                    self.root,
                    {},
                    {"candidateSHA256": "a" * 64},
                    writer,
                    connector,
                    {},
                    {},
                    output,
                    commands,
                )
        record = json.loads(output.read_text())
        self.assertEqual(record["lingerEnabled"], [2000])
        self.assertFalse(record["completed"])
        self.assertFalse(record["rebootVerified"])
        self.assertFalse(record["sourceRetirementAuthorized"])
        self.assertTrue(
            all(call.args[0][0] == "loginctl" for call in commands.run.call_args_list)
        )
        commands.user.assert_not_called()

    def timer_fixture(self):
        marker = {"candidateSHA256": "a" * 64, "run": "fixture", "direction": "forward"}
        execution = {
            "host": "fredrir-05",
            "direction": "forward",
            "candidateSHA256": "a" * 64,
            "marker": marker,
            "timersBefore": {
                user + "/" + unit: {
                    "activeState": "active"
                    if user == "root" and unit == "gitops-pull.timer"
                    else "inactive",
                    "loadState": "loaded",
                    "unitFileState": "enabled" if user == "root" else "disabled",
                }
                for user, unit in finalize.TIMERS
            },
        }
        writer = {
            "kind": "evacuation-source-writer-resume",
            "privateAcceptancePassed": True,
            "candidateSHA256": "a" * 64,
        }
        connector = {
            "kind": "evacuation-source-connector-resume",
            "connectorActive": True,
            "candidateSHA256": "a" * 64,
        }
        return execution, writer, connector

    def timer_context(self, execution, state):
        from contextlib import ExitStack

        stack = ExitStack()
        for name, value in [
            ("host_identity", None),
            ("validate_destination", False),
            ("marker_value", execution["marker"]),
            ("provider_authority", {"host": "fredrir-05"}),
            ("private_acceptance", {}),
            ("public_acceptance", {}),
        ]:
            stack.enter_context(patch.object(finalize, name, return_value=value))
        stack.enter_context(patch.object(finalize, "MARKERS", self.root / "markers"))
        stack.enter_context(patch.object(finalize, "timer_state", side_effect=state))
        return stack

    def test_reconciliation_release_restarts_only_captured_active_timer_without_enablement(
        self,
    ):
        execution, writer, connector = self.timer_fixture()
        commands, events = Mock(), []
        started = set()

        def run(arguments):
            events.append(arguments)
            started.add(arguments[-1])
            return 0, b""

        commands.run.side_effect = run

        def state(commands, user, unit):
            return inactive(True) | {
                "ActiveState": "active" if unit in started else "inactive"
            }, execution["timersBefore"][user + "/" + unit]["unitFileState"]

        with (
            self.timer_context(execution, state),
            patch.object(finalize, "remove_owned_marker") as remove,
        ):
            result = finalize.release_source_reconciliation(
                execution,
                writer,
                connector,
                {},
                {},
                self.root / "release.json",
                commands,
            )
        remove.assert_called_once_with("reconciliation-locked", execution["marker"])
        self.assertEqual(events, [["systemctl", "start", "gitops-pull.timer"]])
        commands.user.assert_not_called()
        self.assertFalse(result["enabledStatesChanged"])
        self.assertTrue(result["completed"])

    def test_changed_enabled_state_refuses_marker_removal_and_timer_actions(self):
        execution, writer, connector = self.timer_fixture()
        commands = Mock()
        with (
            self.timer_context(execution, lambda *_: (inactive(True), "masked")),
            patch.object(finalize, "remove_owned_marker") as remove,
        ):
            with self.assertRaisesRegex(ValueError, "enabled state changed"):
                finalize.release_source_reconciliation(
                    execution,
                    writer,
                    connector,
                    {},
                    {},
                    self.root / "release.json",
                    commands,
                )
        remove.assert_not_called()
        commands.run.assert_not_called()
        commands.user.assert_not_called()

    def test_failed_timer_resume_reinstates_fence_before_stopping_only_attempted_timer(
        self,
    ):
        execution, writer, connector = self.timer_fixture()
        commands, events = Mock(), []

        def run(arguments):
            events.append(arguments)
            if "start" in arguments:
                raise ValueError("fixture start failed")
            return 0, b""

        commands.run.side_effect = run

        def state(commands, user, unit):
            return inactive(True), execution["timersBefore"][user + "/" + unit][
                "unitFileState"
            ]

        cleanup = Mock()
        cleanup.run.side_effect = lambda args: events.append(args) or (0, b"")
        with (
            self.timer_context(execution, state),
            patch.object(finalize, "remove_owned_marker"),
            patch.object(
                finalize,
                "private_marker",
                side_effect=lambda path, value: events.append(["fence", path.name]),
            ),
            patch.object(finalize, "Commands", return_value=cleanup) as budget,
            patch.object(finalize, "unit_state", return_value=inactive()),
        ):
            with self.assertRaisesRegex(ValueError, "start failed"):
                finalize.release_source_reconciliation(
                    execution,
                    writer,
                    connector,
                    {},
                    {},
                    self.root / "release.json",
                    commands,
                )
        budget.assert_called_once_with(30)
        self.assertEqual(
            events,
            [
                ["systemctl", "start", "gitops-pull.timer"],
                ["fence", "reconciliation-locked"],
                ["systemctl", "stop", "gitops-pull.timer"],
            ],
        )
        self.assertFalse(
            json.loads((self.root / "release.json").read_text())["completed"]
        )

    def test_refence_and_cleanup_failures_still_preserve_a_failure_receipt(self):
        execution, writer, connector = self.timer_fixture()
        commands = Mock()
        commands.run.side_effect = ValueError("expired execution budget")
        cleanup = Mock()
        cleanup.run.side_effect = ValueError("cleanup unavailable")

        def state(commands, user, unit):
            return inactive(True), execution["timersBefore"][user + "/" + unit][
                "unitFileState"
            ]

        with (
            self.timer_context(execution, state),
            patch.object(finalize, "remove_owned_marker"),
            patch.object(
                finalize, "private_marker", side_effect=OSError("marker failure")
            ),
            patch.object(finalize, "Commands", return_value=cleanup),
            patch.object(
                finalize,
                "unit_state",
                return_value=inactive() | {"ActiveState": "active", "MainPID": "42"},
            ),
            self.assertRaisesRegex(ValueError, "expired"),
        ):
            finalize.release_source_reconciliation(
                execution,
                writer,
                connector,
                {},
                {},
                self.root / "release.json",
                commands,
            )
        record = json.loads((self.root / "release.json").read_text())
        self.assertFalse(record["cleanup"]["refenced"])
        self.assertEqual(len(record["cleanup"]["errors"]), 2)
        self.assertEqual(
            record["cleanup"]["services"]["root/gitops-pull.service"]["MainPID"], "42"
        )
        self.assertFalse(record["cleanup"]["alreadyTriggeredServicesCancelled"])


if __name__ == "__main__":
    unittest.main()
