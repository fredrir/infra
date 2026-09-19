import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
DIGEST = "sha256:" + "a" * 64
IMAGE = "ghcr.io/fredrir/example@" + DIGEST
WORKFLOW_REVISION = "d" * 40
TOOL = '#!/usr/bin/env python3\nimport json,os,sys\nfrom pathlib import Path\n' \
       'if sys.argv[1:3] != ["auth", "git-credential"]:\n' \
       '    Path(os.environ["VERIFY_CAPTURE"]).write_text(json.dumps(sys.argv))\n' \
       '    sys.exit(int(os.environ.get("VERIFY_EXIT", "0")))\n'


class DeployTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        tools = tempfile.TemporaryDirectory()
        cls.addClassCleanup(tools.cleanup)
        cls.binary = Path(tools.name)
        for name in ["gh", "cosign"]:
            (cls.binary / name).write_text(TOOL)
            (cls.binary / name).chmod(0o755)

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
        (self.repo / ".github/chainguard").mkdir()
        (self.repo / ".github/chainguard/deploy-123.sts.yaml").write_text(yaml.safe_dump({
            "claim_pattern": {"job_workflow_sha": f"^{WORKFLOW_REVISION}$"}, "permissions": {"actions": "write"}}))
        self.environment = os.environ | {
            "PATH": str(self.binary) + os.pathsep + os.environ["PATH"],
            "SOURCE_REPOSITORY_ID": "123", "SOURCE_REVISION": "b" * 40, "IMAGE_NAME": IMAGE.split("@")[0],
            "IMAGE_DIGEST": DIGEST, "DEPLOY_TOKEN": "deploy-token", "GH_TOKEN": "read-token",
            "VERIFY_CAPTURE": str(self.area / "verify.json"),
        }

    def git(self, *args, cwd=None):
        return subprocess.check_output(["git", *args], cwd=cwd or self.repo, stderr=subprocess.DEVNULL, text=True).strip()

    def publish(self):
        self.git("add", ".")
        self.git("commit", "-m", "Initial deployment")
        self.git("push", "origin", "HEAD:main")

    def deployed(self):
        return self.git("rev-parse", "refs/heads/main", cwd=self.remote)

    def prepare(self, mode="kustomize", path="platform/projects/example", visibility="public"):
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
            "repository": "fredrir/example", "visibility": visibility, "images": {"ghcr.io/fredrir/example": target}}))
        self.publish()

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
            "repository": "fredrir/example", "visibility": "public",
            "images": {"ghcr.io/fredrir/example": {"path": "platform/projects/example", "mode": "kustomize"}}}))
        self.publish()

    def run_release(self):
        return subprocess.run(["bash", str(ROOT / "scripts/ci/deploy.sh"), str(self.repo)],
                              env=self.environment, capture_output=True, text=True, check=False)

    def test_verified_deployment_pushes_only_the_pinned_image_change_to_main_once(self):
        self.prepare()
        result = self.run_release()
        self.assertEqual(result.returncode, 0, result.stderr)
        rendered = yaml.safe_load(subprocess.check_output(["kustomize", "build", str(self.project)], text=True))
        self.assertEqual(rendered["spec"]["template"]["spec"]["containers"][0]["image"], IMAGE)
        self.assertEqual(rendered["spec"]["replicas"], 1)
        self.assertEqual(self.git("diff", "--name-only", "HEAD^"), "platform/projects/example/kustomization.yaml")
        self.assertEqual(self.git("rev-parse", "HEAD"), self.deployed())
        args = json.loads((self.area / "verify.json").read_text())
        self.assertEqual(args[1:4], ["attestation", "verify", "oci://" + IMAGE])
        for flag, value in [("--repo", "fredrir/example"), ("--signer-digest", WORKFLOW_REVISION),
                            ("--source-ref", "refs/heads/main"), ("--source-digest", "b" * 40),
                            ("--signer-workflow", "fredrir/infra/.github/workflows/build-image.yml")]:
            self.assertEqual(args[args.index(flag) + 1], value)
        self.assertNotIn("deploy-token", " ".join(args))
        deployed = self.deployed()
        result = self.run_release()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.deployed(), deployed)

    def test_private_images_verify_the_keyless_signature_of_the_pinned_workflow(self):
        self.prepare(visibility="private")
        result = self.run_release()
        self.assertEqual(result.returncode, 0, result.stderr)
        args = json.loads((self.area / "verify.json").read_text())
        self.assertTrue(args[0].endswith("/cosign"))
        self.assertEqual(args[-1], IMAGE)
        for flag, value in [("--certificate-identity", "https://github.com/fredrir/infra/.github/workflows/build-image.yml@" + WORKFLOW_REVISION),
                            ("--certificate-github-workflow-repository", "fredrir/example"),
                            ("--certificate-github-workflow-sha", "b" * 40),
                            ("--certificate-github-workflow-ref", "refs/heads/main")]:
            self.assertEqual(args[args.index(flag) + 1], value)

    def test_unverified_or_unclassified_images_never_reach_main(self):
        for visibility, environment in [("public", {"VERIFY_EXIT": "1"}), ("private", {"VERIFY_EXIT": "1"}), ("internal", {})]:
            with self.subTest(visibility=visibility):
                self.setUp()
                self.prepare(visibility=visibility)
                before = self.deployed()
                self.environment |= environment
                self.assertNotEqual(self.run_release().returncode, 0)
                self.assertEqual(self.deployed(), before)
                self.assertEqual(self.git("status", "--porcelain"), "")

    def test_deployment_rebases_onto_a_main_that_moved(self):
        self.prepare()
        other = self.area / "other"
        self.git("clone", "--quiet", "--branch", "main", str(self.remote), str(other), cwd=self.area)
        (other / "README").write_text("moved\n")
        for arguments in [["add", "README"], ["-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "Move main"],
                          ["push", "--quiet", "origin", "HEAD:main"]]:
            self.git(*arguments, cwd=other)
        result = self.run_release()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.git("rev-parse", "HEAD"), self.deployed())
        self.assertEqual(self.git("log", "--format=%s", "-2"), "Deploy example " + "b" * 12 + "\nMove main")

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

    def test_generic_release_updates_revision_without_starting_workloads(self):
        self.prepare("helmrelease")
        result = self.run_release()
        self.assertEqual(result.returncode, 0, result.stderr)
        web = yaml.safe_load((self.project / "release.yaml").read_text())["spec"]["values"]["workloads"]["web"]
        self.assertEqual(web, {"image": IMAGE, "sourceRevision": "b" * 40, "replicas": 0})

    def test_unknown_image_cannot_change_or_push_deployment(self):
        self.prepare()
        self.environment["IMAGE_NAME"] = "ghcr.io/fredrir/other"
        result = self.run_release()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.git("status", "--porcelain"), "")
        self.assertEqual(self.git("for-each-ref", "--format=%(refname)", "refs/heads", cwd=self.remote), "refs/heads/main")

    def test_mapping_cannot_escape_project_directory(self):
        self.prepare(path="platform/projects/example/../../../.github")
        result = self.run_release()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.git("status", "--porcelain"), "")
