import copy
import hashlib
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
import evacuation_reset_inventory as inventory
from test_evacuation_finalize import provider


class ResetInventoryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.root.chmod(0o700)
        self.reset = object.__new__(inventory.Reset)
        self.reset.fs = inventory.guards.Filesystem(self.root, os.geteuid())

    def test_static_backup_service_is_allowed_but_static_timers_are_refused(self):
        state = {
            "ActiveState": "inactive",
            "SubState": "dead",
            "MainPID": "0",
            "LoadState": "loaded",
        }
        properties = {"UnitFileState": "static", "Job": ""}
        result = inventory.reconciler_policy(
            "restic-backups-llunde-backend.service", state, properties
        )
        self.assertEqual(result["UnitFileState"], "static")
        for unit in (
            "restic-backups-llunde-backend.timer",
            "gitops-pull.timer",
            "gitops-pull.service",
        ):
            with self.subTest(unit=unit), self.assertRaises(ValueError):
                inventory.reconciler_policy(unit, state, properties)
        with self.assertRaises(ValueError):
            inventory.reconciler_policy(
                "restic-backups-llunde-backend.service",
                state,
                {**properties, "Job": "1"},
            )

    def test_exact_candidate_tree_rejects_additional_files_links_and_directories(self):
        candidate = self.root / "candidate"
        candidate.mkdir(mode=0o700)
        (candidate / "backend").mkdir(mode=0o700)
        (candidate / "backend/unit.container").write_bytes(b"unit")
        (candidate / "backend/unit.container").chmod(0o600)
        expected = {"backend/unit.container": hashlib.sha256(b"unit").hexdigest()}
        self.assertEqual(
            set(self.reset.tree_snapshot("/candidate", expected)),
            {"backend", "backend/unit.container"},
        )
        for kind in ("file", "directory", "symlink"):
            with self.subTest(kind=kind):
                extra = candidate / "extra"
                if kind == "directory":
                    extra.mkdir(mode=0o700)
                elif kind == "symlink":
                    extra.symlink_to(candidate / "backend")
                else:
                    extra.write_bytes(b"extra")
                with self.assertRaises(ValueError):
                    self.reset.tree_snapshot("/candidate", expected)
                extra.rmdir() if kind == "directory" else extra.unlink()

    def test_fresh_source_and_provider_are_both_required(self):
        source = {
            "schemaVersion": 1,
            "kind": "evacuation-reset-source-authority",
            "host": "fredrir-05",
            "planSHA256": inventory.PLAN_SHA,
            "observedAt": 104,
            "sourceFenceAbsent": True,
            "reconciliationFenceAbsent": True,
            "unitStates": {
                name: {"ActiveState": "active", "MainPID": "100"}
                for name in inventory.target.SERVICE_USERS
            },
            "privateAcceptance": {
                "connectorActive": True,
                "checks": {
                    name: {"status": 200}
                    for name in ("health", "ready", "frontend", "proxy")
                },
            },
        }
        self.reset.fs = Mock()
        with patch.object(inventory.time, "time", return_value=105):
            self.reset.fs.read_json.side_effect = [source, provider()]
            self.assertEqual(
                self.reset.source_authority("/source", "/provider")["provider"]["host"],
                "fredrir-05",
            )
            for key, value in (
                ("observedAt", 0),
                ("observedAt", float("nan")),
                ("sourceFenceAbsent", False),
                ("host", "fredrir-09"),
            ):
                changed = copy.deepcopy(source)
                changed[key] = value
                self.reset.fs.read_json.side_effect = [changed, provider()]
                with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                    self.reset.source_authority("/source", "/provider")
            self.reset.fs.read_json.side_effect = [source, provider("fredrir-09")]
            with self.assertRaises(ValueError):
                self.reset.source_authority("/source", "/provider")
        with patch.object(inventory.time, "time", return_value=135):
            source["observedAt"] = 134
            self.reset.fs.read_json.side_effect = [source, provider()]
            with self.assertRaisesRegex(ValueError, "thirty"):
                self.reset.source_authority("/source", "/provider")

    def test_quadlet_inventory_rejects_hidden_extra_search_root(self):
        root = self.root / "quadlets"
        root.mkdir(mode=0o755)
        expected = "/quadlets/unit.container"
        (root / "unit.container").write_bytes(b"unit")
        (root / "unit.container").chmod(0o644)
        with patch.object(inventory, "quadlet_paths", return_value=["/quadlets"]):
            self.reset.quadlet_inventory({expected: {}})
            (root / "unknown.network").write_bytes(b"unknown")
            (root / "unknown.network").chmod(0o644)
            with self.assertRaises(ValueError):
                self.reset.quadlet_inventory({expected: {}})


if __name__ == "__main__":
    unittest.main()
