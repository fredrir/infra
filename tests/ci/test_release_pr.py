import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
IMAGE = "ghcr.io/fredrir/example@sha256:" + "a" * 64


class ReleasePRTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.area = Path(self.directory.name)
        self.repo = self.area / "infra"
        self.repo.mkdir()
        self.remote = self.area / "origin.git"
        self.git("init", "--bare", str(self.remote), cwd=self.area)
        self.git("init", "--initial-branch=main")
        self.git("config", "user.name", "Test")
        self.git("config", "user.email", "test@example.invalid")
        self.git("remote", "add", "origin", "https://github.com/fredrir/infra")
        self.git("config", f"url.{self.remote}.insteadOf", "https://github.com/fredrir/infra")
        self.project = self.repo / "platform/projects/example"
        self.project.mkdir(parents=True)
        (self.repo / ".github/deployments").mkdir(parents=True)
        self.binary = self.area / "bin"
        self.binary.mkdir()
        gh = self.binary / "gh"
        gh.write_text('#!/usr/bin/env python3\nimport json,os,sys\nfrom pathlib import Path\nPath(os.environ["PR_CAPTURE"]).write_text(json.dumps(sys.argv[1:]))\n')
        gh.chmod(0o755)
        self.environment = os.environ | {
            "PATH": str(self.binary) + os.pathsep + os.environ["PATH"],
            "SOURCE_REPOSITORY_ID": "123", "GITHUB_REPOSITORY": "fredrir/example",
            "GITHUB_SHA": "b" * 40, "IMAGE": IMAGE, "GITHUB_RUN_ID": "456",
            "GITHUB_RUN_ATTEMPT": "1", "RUNNER_TEMP": str(self.area),
            "PR_CAPTURE": str(self.area / "pr.json"), "GH_TOKEN": "test-token",
        }

    def git(self, *args, cwd=None):
        return subprocess.check_output(["git", *args], cwd=cwd or self.repo, stderr=subprocess.DEVNULL, text=True).strip()

    def prepare(self, mode="kustomize", path="platform/projects/example"):
        target = {"path": path, "mode": mode}
        if mode == "helmrelease":
            target["workload"] = "web"
            resource = {"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
                        "metadata": {"name": "example"}, "spec": {"values": {"workloads": {
                            "web": {"image": "ghcr.io/fredrir/example@sha256:" + "c" * 64, "replicas": 0}}}}}
            filename = "release.yaml"
        else:
            resource = {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "example"},
                        "spec": {"replicas": 1, "template": {"spec": {"containers": [
                            {"name": "web", "image": "ghcr.io/fredrir/example@sha256:" + "c" * 64}]}}}}
            filename = "application.yaml"
        (self.project / filename).write_text(yaml.safe_dump(resource))
        self.write_kustomization(self.project, [filename], pinned=mode == "kustomize")
        (self.repo / ".github/deployments/123.yaml").write_text(yaml.safe_dump({
            "repository": "fredrir/example", "images": {"ghcr.io/fredrir/example": target}}))
        self.git("add", ".")
        self.git("commit", "-m", "Initial deployment")

    def write_kustomization(self, directory, resources, pinned):
        kustomization = {"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "resources": resources}
        if pinned:
            kustomization["images"] = [{"name": "ghcr.io/fredrir/example", "newName": "ghcr.io/fredrir/example",
                                        "digest": "sha256:" + "c" * 64}]
        (directory / "kustomization.yaml").write_text(yaml.safe_dump(kustomization))

    def prepare_nested(self):
        for stage, kind in [("migration", "Job"), ("application", "Deployment")]:
            directory = self.project / stage
            directory.mkdir()
            resource = {"apiVersion": "batch/v1" if kind == "Job" else "apps/v1", "kind": kind, "metadata": {"name": stage},
                        "spec": {"template": {"spec": {"containers": [{"name": "app", "image": "ghcr.io/fredrir/example:latest"}]}}}}
            (directory / f"{stage}.yaml").write_text(yaml.safe_dump(resource))
            self.write_kustomization(directory, [f"{stage}.yaml"], pinned=True)
        (self.project / "namespace.yaml").write_text(yaml.safe_dump({"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "example"}}))
        self.write_kustomization(self.project, ["namespace.yaml"], pinned=False)
        (self.repo / ".github/deployments/123.yaml").write_text(yaml.safe_dump({
            "repository": "fredrir/example", "images": {"ghcr.io/fredrir/example": {"path": "platform/projects/example", "mode": "kustomize"}}}))
        self.git("add", ".")
        self.git("commit", "-m", "Initial deployment")

    def run_release(self):
        return subprocess.run(["bash", str(ROOT / "scripts/ci/release-pr.sh"), str(self.repo)],
                              env=self.environment, capture_output=True, text=True, check=False)

    def test_static_deployment_pushes_only_pinned_image_change_and_opens_pr(self):
        self.prepare()
        result = self.run_release()
        self.assertEqual(result.returncode, 0, result.stderr)
        rendered = yaml.safe_load(subprocess.check_output(["kustomize", "build", str(self.project)], text=True))
        self.assertEqual(rendered["spec"]["template"]["spec"]["containers"][0]["image"], IMAGE)
        self.assertEqual(rendered["spec"]["replicas"], 1)
        self.assertEqual(self.git("diff", "--name-only", "HEAD^"), "platform/projects/example/kustomization.yaml")
        self.assertEqual(self.git("rev-parse", "HEAD"), self.git("rev-parse", "refs/heads/deploy/123/456-1-example", cwd=self.remote))
        args = json.loads((self.area / "pr.json").read_text())
        self.assertEqual(args[:4], ["pr", "create", "--repo", "fredrir/infra"])
        self.assertNotIn("test-token", " ".join(args))

    def test_every_nested_pin_receives_the_digest_and_nothing_else_changes(self):
        self.prepare_nested()
        result = self.run_release()
        self.assertEqual(result.returncode, 0, result.stderr)
        for stage in ["migration", "application"]:
            rendered = yaml.safe_load(subprocess.check_output(["kustomize", "build", str(self.project / stage)], text=True))
            self.assertEqual(rendered["spec"]["template"]["spec"]["containers"][0]["image"], IMAGE)
        self.assertEqual(self.git("diff", "--name-only", "HEAD^").splitlines(),
                         ["platform/projects/example/application/kustomization.yaml",
                          "platform/projects/example/migration/kustomization.yaml"])

    def test_project_without_a_pin_cannot_receive_a_deployment(self):
        self.prepare()
        self.write_kustomization(self.project, ["application.yaml"], pinned=False)
        self.git("commit", "-am", "Drop the pin")
        result = self.run_release()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.git("status", "--porcelain"), "")
        self.assertFalse((self.area / "pr.json").exists())

    def test_generic_release_updates_revision_without_starting_workloads(self):
        self.prepare("helmrelease")
        result = self.run_release()
        self.assertEqual(result.returncode, 0, result.stderr)
        web = yaml.safe_load((self.project / "release.yaml").read_text())["spec"]["values"]["workloads"]["web"]
        self.assertEqual(web, {"image": IMAGE, "sourceRevision": "b" * 40, "replicas": 0})

    def test_unknown_image_cannot_change_or_push_deployment(self):
        self.prepare()
        self.environment["IMAGE"] = "ghcr.io/fredrir/other@sha256:" + "a" * 64
        result = self.run_release()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.git("status", "--porcelain"), "")
        self.assertFalse((self.area / "pr.json").exists())
        self.assertEqual(self.git("for-each-ref", "--format=%(refname)", "refs/heads", cwd=self.remote), "")

    def test_mapping_cannot_escape_project_directory(self):
        self.prepare(path="platform/projects/example/../../../.github")
        result = self.run_release()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.git("status", "--porcelain"), "")
        self.assertFalse((self.area / "pr.json").exists())
