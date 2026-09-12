import copy
import importlib.util
import json
import shlex
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import jinja2
import yaml

ROOT = Path(__file__).resolve().parents[2]
ROLE = ROOT / "ansible/roles/platform"
spec = importlib.util.spec_from_file_location(
    "fleet_inventory", ROOT / "ansible/inventory/fleet.py"
)
fleet = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fleet)


class HostContractTests(unittest.TestCase):
    def setUp(self):
        self.values = yaml.safe_load((ROLE / "defaults/main.yml").read_text())
        self.values.update(
            platform_node_name="rehearsal-node",
            platform_node_ip="192.0.2.10",
            platform_private_ip="192.0.2.10",
            platform_private_interface="ens7",
            platform_api_backends=["192.0.2.10", "192.0.2.11", "192.0.2.12"],
        )
        self.environment = jinja2.Environment(
            loader=jinja2.FileSystemLoader(ROLE / "templates"),
            undefined=jinja2.StrictUndefined,
        )
        self.environment.filters.update(
            to_json=json.dumps, bool=bool, quote=shlex.quote
        )

    def render(self, name, **values):
        return self.environment.get_template(name).render(self.values | values)

    def test_worker_does_not_receive_server_credential(self):
        config = yaml.safe_load(self.render("k3s.yaml.j2"))
        self.assertEqual(config["token-file"], self.values["platform_agent_token_file"])
        self.assertNotIn("agent-token-file", config)
        self.assertNotIn("cluster-init", config)
        self.assertNotIn("node-taint", config)

    def test_control_plane_encrypts_secrets_and_excludes_workloads(self):
        config = yaml.safe_load(self.render("k3s.yaml.j2", platform_role="server"))
        self.assertTrue(config["secrets-encryption"])
        self.assertIn(
            "node-role.kubernetes.io/control-plane=true:NoSchedule",
            config["node-taint"],
        )
        self.assertEqual(set(config["disable"]), {"traefik", "servicelb"})
        self.assertNotEqual(config["token-file"], config["agent-token-file"])

    def test_first_server_does_not_wait_for_its_own_api(self):
        config = yaml.safe_load(
            self.render("k3s.yaml.j2", platform_role="server", platform_initialize=True)
        )
        self.assertTrue(config["cluster-init"])
        self.assertNotIn("server", config)

    def test_api_proxy_is_local_and_checks_all_backends(self):
        config = self.render("haproxy.cfg.j2")
        self.assertIn("bind 127.0.0.1:7443", config)
        for address in self.values["platform_api_backends"]:
            self.assertIn(f"{address}:6443 check", config)

    def test_containerd_template_preserves_upstream_base(self):
        config = self.render("containerd-v3.toml.j2")
        self.assertIn('{{ template "base" . }}', config)
        self.assertIn('runtime_type = "io.containerd.runsc.v1"', config)

    def test_transport_keeps_control_plane_routes_private_and_role_tagged(self):
        tasks = yaml.safe_load((ROLE / "tasks/main.yml").read_text())
        for name in [
            "Enroll transport with runtime credential",
            "Configure transport routes",
        ]:
            task = next(task for task in tasks if task["name"] == name)
            for role, tag, accepts in [
                ("server", "control", "false"),
                ("agent", "worker", "true"),
            ]:
                values = self.values | {"platform_role": role}
                argv = [
                    self.environment.from_string(value).render(values)
                    for value in task["ansible.builtin.command"]["argv"]
                ]
                self.assertIn(f"--accept-routes={accepts}", argv)
                if "up" in argv:
                    self.assertIn(f"--advertise-tags=tag:platform-{tag}", argv)
                else:
                    self.assertFalse(
                        any(value.startswith("--advertise-tags=") for value in argv)
                    )
                self.assertIn("--advertise-exit-node=false", argv)
                self.assertIn("--ssh=false", argv)

    def test_only_control_planes_can_advertise_the_exact_backend_routes(self):
        contract = yaml.safe_load((ROLE / "tasks/main.yml").read_text())[0]
        routes = [f"{address}/32" for address in self.values["platform_api_backends"]]
        values = self.values | {
            "ansible_facts": {
                "service_mgr": "systemd",
                "distribution": "Ubuntu",
                "distribution_version": "26.04",
                "architecture": "x86_64",
            },
            "platform_preflight_approved": True,
            "platform_architecture": "amd64",
            "platform_admin_keys": ["rehearsal-key"],
        }
        cases = [
            ("server", [], True),
            ("agent", [], True),
            ("server", routes, True),
            ("agent", routes, False),
            ("server", ["192.0.2.0/24"], False),
            ("server", routes[:2], False),
            ("server", routes + routes[:1], False),
        ]
        with tempfile.TemporaryDirectory() as directory:
            playbook = Path(directory) / "contract.yml"
            for role, advertisements, expected in cases:
                with self.subTest(role=role, advertisements=advertisements):
                    playbook.write_text(
                        yaml.safe_dump(
                            [
                                {
                                    "hosts": "all",
                                    "gather_facts": False,
                                    "vars": values
                                    | {
                                        "platform_role": role,
                                        "platform_advertise_routes": advertisements,
                                    },
                                    "tasks": [contract],
                                }
                            ]
                        )
                    )
                    result = subprocess.run(
                        [
                            str(Path(sys.executable).parent / "ansible-playbook"),
                            "-i",
                            "localhost,",
                            "--connection",
                            "local",
                            str(playbook),
                        ],
                        text=True,
                        capture_output=True,
                        timeout=30,
                    )
                    self.assertEqual(
                        result.returncode == 0, expected, result.stdout + result.stderr
                    )

    def test_preflight_shell_parses_for_server_and_worker(self):
        for role in ["server", "agent"]:
            script = self.render("preflight.sh.j2", platform_role=role)
            result = subprocess.run(
                ["bash", "-n"], input=script, text=True, capture_output=True
            )
            self.assertEqual(result.returncode, 0, result.stderr)

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


if __name__ == "__main__":
    unittest.main()
