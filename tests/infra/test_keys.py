import subprocess
import unittest
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]


class KeysTests(unittest.TestCase):
    def test_admin_keys_valid_openssh_public_keys(self):
        content = (ROOT / "keys/admin_keys").read_text()
        lines = [line.strip() for line in content.strip().splitlines() if line.strip()]
        self.assertGreater(len(lines), 0)
        for line in lines:
            parts = line.split()
            self.assertGreaterEqual(len(parts), 2)
            self.assertTrue(parts[0].startswith("ssh-") or parts[0].startswith("ecdsa-"))

    def test_ingress_kustomize_renders_keys_service_and_routes(self):
        output = subprocess.check_output(
            ["kubectl", "kustomize", str(ROOT / "platform/components/ingress")],
            text=True,
        )
        documents = list(yaml.safe_load_all(output))

        configmaps = [d for d in documents if d and d.get("kind") == "ConfigMap"]
        keys_cm = next(
            d for d in configmaps
            if d.get("metadata", {}).get("name", "").startswith("keys-") and "admin_keys" in d.get("data", {})
        )
        self.assertEqual(keys_cm["data"]["admin_keys"], (ROOT / "keys/admin_keys").read_text())

        caddy_cm = next(d for d in configmaps if d.get("metadata", {}).get("name") == "keys-caddy")
        self.assertIn("try_files {path} /admin_keys", caddy_cm["data"]["Caddyfile"])

        deployment = next(
            d for d in documents
            if d and d.get("kind") == "Deployment" and d.get("metadata", {}).get("name") == "keys"
        )
        self.assertEqual(deployment["metadata"]["namespace"], "ingress-system")
        self.assertEqual(deployment["spec"]["template"]["spec"]["securityContext"]["runAsNonRoot"], True)

        ingressroute = next(
            d for d in documents
            if d and d.get("kind") == "IngressRoute" and d.get("metadata", {}).get("name") == "keys"
        )
        self.assertIn("Host(`keys.fredrir.com`)", ingressroute["spec"]["routes"][0]["match"])
