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
IDENTITY = {"id": 456, "full_name": "fredrir/example", "owner": {"id": 114402558}, "private": False}
CREDENTIALS = {"github_app_id": "1", "github_app_installation_id": "2", "github_app_private_key": "key"}


class RustOnboardingTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        base = Path(self.directory.name)
        self.root = base / "infra"
        self.output = base / "callers"
        identity = base / "age.txt"
        subprocess.run(["age-keygen", "-o", str(identity)], check=True, capture_output=True)
        self.environment = {**os.environ, "SOPS_AGE_KEY_FILE": str(identity)}
        recipient = subprocess.run(["age-keygen", "-y", str(identity)], check=True, capture_output=True, text=True).stdout.strip()
        runners = self.root / "platform/components/runners"
        for name in ["ci-namespace", "rust"]:
            shutil.copytree(ROOT / "platform/components/runners" / name, runners / name)
        (runners / "kustomization.yaml").write_text("apiVersion: v1\nresources:\n- existing\n- 'y'\npatches: []\n")
        (self.root / "platform/components/build-cache/projects").mkdir(parents=True)
        (self.root / "platform/components/build-cache/projects/kustomization.yaml").write_text("resources: []\n")
        (self.root / "platform/components/cache").mkdir(parents=True)
        (self.root / "platform/components/cache/tunnel.secret.sops.yaml").write_text(f"sops:\n  age:\n  - recipient: {recipient}\n")
        (self.root / ".github").mkdir()
        (self.root / ".github/rust-projects.yaml").write_text("projects: []\n")

    def tearDown(self):
        self.directory.cleanup()

    def onboard(self, identity=IDENTITY, project="example"):
        with patch("infra.cli.repository", return_value=identity), \
                patch("infra.cli.app_credentials", return_value=CREDENTIALS), \
                patch.dict(os.environ, self.environment):
            return main(["onboard-rust", identity["full_name"], "--project", project, "--workflow-ref", "c" * 40,
                         "--output", str(self.output), "--root", str(self.root)])

    def decrypt(self, path):
        return yaml.safe_load(subprocess.run(["sops", "decrypt", str(path)], env=self.environment,
                                             check=True, capture_output=True, text=True).stdout)

    def test_overlay_renders_three_pools_with_encrypted_bound_credentials(self):
        self.assertEqual(self.onboard(), 0)
        overlay = self.root / "platform/components/runners/example"
        rendered = list(yaml.safe_load_all(subprocess.run(["kubectl", "kustomize", str(overlay)],
                                                         check=True, capture_output=True, text=True).stdout))
        releases = {r["spec"]["values"]["runnerScaleSetName"]: r for r in rendered if r["kind"] == "HelmRelease"}
        self.assertEqual(set(releases), {"rust-amd64", "rust-pr-amd64", "rust-release-amd64"})
        for release in releases.values():
            self.assertEqual(release["metadata"]["namespace"], "ci-example")
            self.assertEqual(release["spec"]["values"]["githubConfigUrl"], "https://github.com/fredrir/example")
        secrets = {r["metadata"]["name"]: r for r in rendered if r["kind"] == "Secret"}
        self.assertEqual(set(secrets), {"github-app", "sccache-ro", "sccache-rw", "sccache-release"})
        for name in secrets:
            path = overlay / f"{name}.secret.sops.yaml"
            stored = yaml.safe_load(path.read_text())["stringData"]
            self.assertTrue(all(str(value).startswith("ENC[AES256_GCM,") for value in stored.values()))
            self.assertEqual(self.decrypt(path)["metadata"]["namespace"], "ci-example")
        self.assertEqual(self.decrypt(overlay / "github-app.secret.sops.yaml")["stringData"], CREDENTIALS)
        runner = self.decrypt(overlay / "sccache-rw.secret.sops.yaml")["stringData"]
        self.assertRegex(runner["AWS_ACCESS_KEY_ID"], r"^GK[0-9a-f]{24}$")
        self.assertRegex(runner["AWS_SECRET_ACCESS_KEY"], r"^[0-9a-f]{64}$")
        keys = self.decrypt(self.root / "platform/components/build-cache/projects/example.secret.sops.yaml")
        self.assertEqual(keys["metadata"]["labels"], {"infra.fredrir.com/build-cache-project": "example"})
        self.assertEqual(keys["stringData"]["rw_id"], runner["AWS_ACCESS_KEY_ID"])
        self.assertEqual(len({keys["stringData"][f"{pool}_id"] for pool in ["ro", "rw", "release"]}), 3)

    def test_registries_list_the_project_once(self):
        self.onboard()
        runners = (self.root / "platform/components/runners/kustomization.yaml").read_text()
        self.assertEqual(runners, "apiVersion: v1\nresources:\n- existing\n- 'y'\n- example\npatches: []\n")
        projects = yaml.safe_load((self.root / "platform/components/build-cache/projects/kustomization.yaml").read_text())
        self.assertEqual(projects["resources"], ["example.secret.sops.yaml"])
        registry = yaml.safe_load((self.root / ".github/rust-projects.yaml").read_text())
        self.assertEqual(registry["projects"], [{"project": "example", "repository": "fredrir/example", "id": 456, "visibility": "public"}])
        self.output = self.output.with_name("again")
        with self.assertRaises(SystemExit):
            self.onboard()
        with self.assertRaises(SystemExit):
            self.onboard(project="other")

    def test_public_callers_pin_the_shared_workflows_and_scope_trust(self):
        self.onboard()
        project = self.output / "project/.github"
        for name, workflow in [("ci", "rust-ci"), ("auto-tag", "rust-auto-tag"), ("release", "rust-release")]:
            caller = yaml.safe_load((project / f"workflows/{name}.yml").read_text())
            self.assertEqual([job["uses"] for job in caller["jobs"].values()],
                             [f"fredrir/infra/.github/workflows/{workflow}.yml@" + "c" * 40])
        tag = yaml.safe_load((project / "chainguard/auto-tag.sts.yaml").read_text())
        self.assertEqual(tag["permissions"], {"contents": "write"})
        self.assertRegex("fredrir/infra/.github/workflows/rust-auto-tag.yml@" + "a" * 40, tag["claim_pattern"]["job_workflow_ref"])
        self.assertNotRegex("fredrir/infra/.github/workflows/rust-release.yml@" + "a" * 40, tag["claim_pattern"]["job_workflow_ref"])
        self.assertRegex("repo:fredrir/example:ref:refs/heads/main", tag["subject_pattern"])
        self.assertNotRegex("repo:fredrir/example:ref:refs/heads/feature", tag["subject_pattern"])
        dispatch = yaml.safe_load((self.output / "packages/.github/chainguard/dispatch-456.sts.yaml").read_text())
        self.assertEqual(dispatch["permissions"], {"actions": "write"})
        self.assertRegex("repo:fredrir/example:environment:release", dispatch["subject_pattern"])
        self.assertRegex("refs/tags/v1.2.3", dispatch["claim_pattern"]["ref"])
        self.assertNotRegex("refs/heads/main", dispatch["claim_pattern"]["ref"])

    def test_private_repositories_only_get_ci(self):
        self.onboard(identity=IDENTITY | {"private": True})
        files = sorted(str(p.relative_to(self.output)) for p in self.output.rglob("*") if p.is_file())
        self.assertEqual(files, ["project/.github/workflows/ci.yml"])
        registry = yaml.safe_load((self.root / ".github/rust-projects.yaml").read_text())
        self.assertEqual(registry["projects"][0]["visibility"], "private")

    def test_invalid_input_fails_before_network(self):
        for project in ["../escape", "Example", "a" * 30]:
            with self.subTest(project=project), patch("infra.cli.repository") as remote, self.assertRaises(SystemExit):
                main(["onboard-rust", "fredrir/example", "--project", project, "--workflow-ref", "c" * 40,
                      "--output", str(self.output), "--root", str(self.root)])
            remote.assert_not_called()


if __name__ == "__main__":
    unittest.main()
