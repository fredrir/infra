import copy
import importlib.util
import json
import subprocess
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location(
    "fleet_inventory", ROOT / "ansible/inventory/fleet.py"
)
fleet = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fleet)


class InventoryTests(unittest.TestCase):
    def test_proposals_and_legacy_hosts_are_not_enrolled(self):
        result = subprocess.run(
            ["python3", str(ROOT / "ansible/inventory/fleet.py"), "--list"],
            text=True,
            capture_output=True,
            check=True,
        )
        inventory = json.loads(result.stdout)
        source = json.loads((ROOT / "platform/inventory/nodes.json").read_text())
        expected = [
            n["id"]
            for n in source["nodes"]
            if n["osAdapter"] == "ansible"
            and n["enrollment"] is not None
            and n["desiredRole"] in ("server", "worker")
        ]
        self.assertEqual(inventory["platform"]["hosts"], expected)
        for candidate in source["candidates"]:
            self.assertNotIn(candidate["key"], inventory["platform"]["hosts"])

    def test_enrollment_requires_three_control_planes_and_verified_hardware(self):
        source = json.loads((ROOT / "platform/inventory/nodes.json").read_text())
        control = next(
            node for node in source["nodes"] if node["desiredRole"] == "server"
        )
        control["capabilities"]["verified"] = True
        control["enrollment"] = {
            "sshAddress": "rehearsal-control",
            "hostname": "rehearsal-control",
            "nodeIP": "192.0.2.10",
            "privateIP": "192.0.2.10",
            "privateInterface": "ens7",
            "adminKeys": ["ssh-ed25519 AAAA rehearsal"],
        }
        with self.assertRaisesRegex(ValueError, "three distinct"):
            fleet.build_inventory(source)
        for number in [11, 12]:
            additional = copy.deepcopy(control)
            additional["id"] = f"fredrir-{number}"
            additional["enrollment"].update(
                hostname=f"rehearsal-control-{number}",
                nodeIP=f"192.0.2.{number}",
                privateIP=f"192.0.2.{number}",
            )
            source["nodes"].append(additional)
        worker = next(
            node for node in source["nodes"] if node["desiredRole"] == "worker"
        )
        worker["osAdapter"] = "ansible"
        worker["enrollment"] = {
            "sshAddress": "rehearsal-worker",
            "hostname": "rehearsal-worker",
            "nodeIP": "192.0.2.20",
            "adminKeys": ["ssh-ed25519 AAAA rehearsal"],
        }
        with self.assertRaisesRegex(ValueError, "verified"):
            fleet.build_inventory(source)
        worker["capabilities"]["verified"] = True
        result = fleet.build_inventory(source)
        self.assertEqual(result["platform"]["hosts"], [worker["id"]])
        self.assertFalse(
            result["_meta"]["hostvars"][worker["id"]]["platform_sandbox_enabled"]
        )
        control["capabilities"]["ci"] = True
        with self.assertRaisesRegex(ValueError, "workload capabilities"):
            fleet.build_inventory(source)
