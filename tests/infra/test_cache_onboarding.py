import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import yaml
from infra.cli import main

ROOT = Path(__file__).resolve().parents[2]
OVERLAY = """apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: ci-example
resources:
- ../base
patches:
- target:
    kind: HelmRelease
    name: buildkit
  patch: |
    - op: replace
      path: /spec/values/githubConfigUrl
      value: https://github.com/fredrir/example
"""


class CacheOnboardingTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name) / "infra"
        identity = Path(self.directory.name) / "age.txt"
        subprocess.run(["age-keygen", "-o", str(identity)], check=True, capture_output=True)
        self.environment = {**os.environ, "SOPS_AGE_KEY_FILE": str(identity)}
        recipient = subprocess.run(["age-keygen", "-y", str(identity)], check=True, capture_output=True, text=True).stdout.strip()
        self.runners = self.root / "platform/components/runners"
        for name in ["ci-namespace", "base", "buildkit-cache"]:
            shutil.copytree(ROOT / "platform/components/runners" / name, self.runners / name)
        self.overlay = self.runners / "example"
        self.overlay.mkdir()
        (self.overlay / "kustomization.yaml").write_text(OVERLAY)
        self.projects = self.root / "platform/components/build-cache/projects"
        self.projects.mkdir(parents=True)
        (self.projects / "kustomization.yaml").write_text("resources: []\n")
        (self.root / "platform/components/cache").mkdir(parents=True)
        (self.root / "platform/components/cache/tunnel.secret.sops.yaml").write_text(f"sops:\n  age:\n  - recipient: {recipient}\n")

    def onboard(self, project="example"):
        with patch.dict(os.environ, self.environment):
            return main(["onboard-cache", "--project", project, "--root", str(self.root)])

    def decrypt(self, path):
        return yaml.safe_load(subprocess.run(["sops", "decrypt", str(path)], env=self.environment,
                                             check=True, capture_output=True, text=True).stdout)

    def test_overlay_gains_bound_credentials_bucket_and_cache_egress(self):
        self.assertEqual(self.onboard(), 0)
        rendered = list(yaml.safe_load_all(subprocess.run(["kubectl", "kustomize", str(self.overlay)],
                                                         check=True, capture_output=True, text=True).stdout))
        release = next(r for r in rendered if r["kind"] == "HelmRelease")
        env = {e["name"]: e for e in release["spec"]["values"]["template"]["spec"]["containers"][0]["env"]}
        self.assertEqual(env["BUILDKIT_CACHE_BUCKET"]["value"], "$(CI_NAMESPACE)-main")
        self.assertEqual(env["AWS_SECRET_ACCESS_KEY"]["valueFrom"]["secretKeyRef"],
                         {"name": "buildkit-cache", "key": "AWS_SECRET_ACCESS_KEY"})
        self.assertIn("build-cache", {r["metadata"]["name"] for r in rendered if r["kind"] == "NetworkPolicy"})
        stored = yaml.safe_load((self.overlay / "buildkit-cache.secret.sops.yaml").read_text())["stringData"]
        self.assertTrue(all(str(value).startswith("ENC[AES256_GCM,") for value in stored.values()))
        runner = self.decrypt(self.overlay / "buildkit-cache.secret.sops.yaml")
        self.assertEqual(runner["metadata"]["namespace"], "ci-example")
        keys = self.decrypt(self.projects / "example.secret.sops.yaml")
        self.assertEqual(keys["metadata"]["labels"], {"infra.fredrir.com/build-cache-project": "example"})
        self.assertEqual(keys["stringData"]["rw_id"], runner["stringData"]["AWS_ACCESS_KEY_ID"])
        self.assertEqual(keys["stringData"]["rw_secret"], runner["stringData"]["AWS_SECRET_ACCESS_KEY"])
        self.assertEqual(yaml.safe_load((self.projects / "kustomization.yaml").read_text())["resources"], ["example.secret.sops.yaml"])

    def test_repeated_foreign_and_renamed_pools_refuse(self):
        self.assertEqual(self.onboard(), 0)
        for project, prepare in [("example", lambda: None), ("missing", lambda: None),
                                 ("renamed", lambda: self.write("renamed", OVERLAY.replace("ci-example", "ci-renamed").replace(
                                     "githubConfigUrl", "runnerScaleSetName"))),
                                 ("foreign", lambda: self.write("foreign", OVERLAY))]:
            with self.subTest(project=project):
                prepare()
                with self.assertRaises(SystemExit):
                    self.onboard(project)

    def write(self, project, text):
        (self.runners / project).mkdir()
        (self.runners / project / "kustomization.yaml").write_text(text)


if __name__ == "__main__":
    unittest.main()
