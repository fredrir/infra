import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import yaml
from infra.cli import main

ROOT = Path(__file__).resolve().parents[2]
IDENTITY = {"id": 123, "full_name": "fredrir/example", "owner": {"id": 114402558}}
IMAGE = "ghcr.io/fredrir/example@sha256:" + "a" * 64


class OnboardingTests(unittest.TestCase):
    def arguments(self, output):
        return ["onboard", "fredrir/example", "--project", "example", "--image", IMAGE,
                "--source-revision", "b" * 40, "--workflow-ref", "c" * 40,
                "--domain", "example.fredrir.com", "--test-command", "npm test", "--output", str(output)]

    def test_generated_project_renders_with_native_helm_and_kustomize(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "new"
            with patch("infra.cli.repository", return_value=IDENTITY):
                self.assertEqual(main(self.arguments(output)), 0)
            release = yaml.safe_load((output / "project/release.yaml").read_text())
            values = output / "values.json"
            values.write_text(json.dumps(release["spec"]["values"]))
            rendered = subprocess.check_output(["helm", "template", "example", str(ROOT / "charts/project"),
                                                "--namespace", "project-example", "-f", str(values)], text=True)
            documents = list(yaml.safe_load_all(rendered))
            deployment = next(d for d in documents if d and d["kind"] == "Deployment")
            self.assertEqual(deployment["spec"]["replicas"], 0)
            container = deployment["spec"]["template"]["spec"]["containers"][0]
            self.assertEqual(container["image"], IMAGE)
            self.assertFalse(container["securityContext"]["allowPrivilegeEscalation"])
            self.assertEqual(container["lifecycle"]["preStop"]["sleep"]["seconds"], 5)
            self.assertTrue(any(d and d["kind"] == "Ingress" and d["spec"]["ingressClassName"] == "platform" for d in documents))
            subprocess.run(["kubectl", "kustomize", str(output / "project")], check=True, capture_output=True)
            caller = yaml.safe_load((output / "caller/.github/workflows/build.yaml").read_text())
            self.assertEqual(caller["on"], {"push": {"branches": ["main"]}})
            self.assertTrue(caller["jobs"]["build"]["uses"].endswith("@" + "c" * 40))

    def test_refuses_existing_output_without_overwriting(self):
        with tempfile.TemporaryDirectory() as directory:
            sentinel = Path(directory) / "keep"
            sentinel.write_text("unchanged")
            with patch("infra.cli.repository") as remote, self.assertRaises(SystemExit):
                main(self.arguments(directory))
            remote.assert_not_called()
            self.assertEqual(sentinel.read_text(), "unchanged")

    def test_unqualified_architecture_and_mutable_image_fail_before_network(self):
        for extra in [["--architecture", "arm64"], ["--image", "ghcr.io/fredrir/example:latest"], ["--project", "../escape"]]:
            with self.subTest(extra=extra), tempfile.TemporaryDirectory() as directory:
                with patch("infra.cli.repository") as remote, self.assertRaises(SystemExit):
                    main(self.arguments(Path(directory) / "new") + extra)
                remote.assert_not_called()


if __name__ == "__main__":
    unittest.main()
