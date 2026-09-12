import json
import unittest
from pathlib import Path

import jinja2
import yaml

ROOT = Path(__file__).resolve().parents[1]


class K3sConfigurationTests(unittest.TestCase):
    def setUp(self):
        inventory = yaml.safe_load((ROOT / "inventory/production.yml").read_text())
        cluster = inventory["all"]["children"]["ubuntu"]["children"]["k3s_cluster"]
        self.groups = {
            name: list(group["hosts"]) for name, group in cluster["children"].items()
        }
        self.hostvars = {
            name: values
            for group in cluster["children"].values()
            for name, values in group["hosts"].items()
        }
        self.defaults = yaml.safe_load(
            (ROOT / "roles/k3s/defaults/main.yml").read_text()
        )
        environment = jinja2.Environment(undefined=jinja2.StrictUndefined)
        environment.filters["to_json"] = json.dumps
        self.template = environment.from_string(
            (ROOT / "roles/k3s/templates/config.yaml.j2").read_text()
        )

    def render(self, name, role):
        return yaml.safe_load(
            self.template.render(
                self.defaults
                | self.hostvars[name]
                | {
                    "inventory_hostname": name,
                    "groups": self.groups,
                    "hostvars": self.hostvars,
                    "k3s_role": role,
                    "k3s_api_endpoint": self.hostvars["fredrir-07"]["private_ip"],
                }
            )
        )

    def test_only_first_server_initializes_the_cluster(self):
        self.assertEqual(
            self.groups["server"], ["fredrir-07", "fredrir-08", "fredrir-05"]
        )
        for name in self.groups["server"]:
            config = self.render(name, "server")
            self.assertEqual(config.get("cluster-init", False), name == "fredrir-07")
            self.assertEqual("server" in config, name != "fredrir-07")
            self.assertEqual(config["node-ip"], self.hostvars[name]["private_ip"])
            self.assertEqual(config["advertise-address"], config["node-ip"])

    def test_servers_share_network_and_security_configuration(self):
        configs = [self.render(name, "server") for name in self.groups["server"]]
        for key in [
            "cluster-cidr",
            "service-cidr",
            "secrets-encryption",
            "flannel-backend",
            "flannel-external-ip",
            "disable",
            "tls-san",
        ]:
            self.assertTrue(all(config[key] == configs[0][key] for config in configs))
        for config in configs:
            self.assertTrue(config["secrets-encryption"])
            self.assertIn(
                "node-role.kubernetes.io/control-plane=true:NoSchedule",
                config["node-taint"],
            )
            self.assertEqual(config["flannel-iface"], "tailscale0")
            self.assertNotIn("token", config)
            self.assertNotIn("agent-token", config)
            self.assertNotEqual(config["token-file"], config["agent-token-file"])

    def test_workers_use_only_the_agent_credential_and_tailnet_address(self):
        for name in self.groups["agent"]:
            config = self.render(name, "agent")
            self.assertEqual(config["node-ip"], self.hostvars[name]["tailscale_ip"])
            self.assertEqual(
                config["token-file"], self.defaults["k3s_agent_token_file"]
            )
            self.assertNotIn("agent-token-file", config)
            self.assertNotIn("cluster-init", config)
            self.assertNotIn("token", config)


if __name__ == "__main__":
    unittest.main()
