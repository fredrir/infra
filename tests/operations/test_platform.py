import importlib.util
import json
import shutil
import tempfile
import unittest
from datetime import UTC, datetime
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]


def module(name):
    spec = importlib.util.spec_from_file_location(
        name, ROOT / f"scripts/operations/{name}.py"
    )
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


platform = module("platform_cli")
backups = module("backup_jobs")
NOW = datetime(2026, 9, 12, tzinfo=UTC)


class PlatformTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        shutil.copytree(ROOT / "platform", self.root / "platform")
        shutil.copy2(ROOT / "flake.lock", self.root / "flake.lock")
        inventory = json.loads(
            (self.root / "platform/inventory/nodes.json").read_text()
        )
        inventory["nodes"] = [
            {
                "id": f"control-{n}",
                "desiredRole": "server",
                "architecture": "amd64",
                "enrollment": {"privateIP": f"10.0.0.{n}", "nodeIP": f"10.0.0.{n}"},
            }
            for n in range(1, 4)
        ] + [
            {
                "id": "worker-1",
                "desiredRole": "worker",
                "architecture": "amd64",
                "enrollment": {"nodeIP": "10.0.0.4"},
                "capabilities": {"verified": True, "ci": True},
            }
        ]
        (self.root / "platform/inventory/nodes.json").write_text(json.dumps(inventory))

    def tearDown(self):
        self.temp.cleanup()

    def config(self):
        layers = {"sources", "policy", "controllers", "secrets", "runners"}
        contract = platform.host_contract(self.root)
        hashes = {key: "b" * 64 for key in contract["inventoryHashes"]}
        metadata = {"verifiedAt": NOW.isoformat(), "reportSha256": "c" * 64}
        image = "ghcr.io/fredrir/infra-ci@sha256:" + "a" * 64
        required = platform.secret_contracts(self.root, layers, [773018612], ["amd64"])
        return {
            "schemaVersion": 1,
            "layers": sorted(layers),
            "runnerRepositories": [773018612],
            "runnerArchitectures": ["amd64"],
            "settings": {"CI_IMAGE": image, "KUBERNETES_API_CIDR": "10.0.0.1/32"},
            "secrets": [
                {"namespace": ns, "name": name, "keys": [key]}
                for ns, name, key in required
            ],
            "evidence": {
                "hostNetwork": {
                    **metadata,
                    **contract,
                    "k3sVersion": "v1.36.3+k3s1",
                    "nodeConfigurationHashes": hashes,
                },
                "ciSandbox": {
                    **metadata,
                    "image": image,
                    "nodeConfigurationHashes": hashes,
                    "passed": {
                        key: True
                        for key in [
                            "amd64",
                            "buildkitProcessSandbox",
                            "nixSandbox",
                            "noApiToken",
                            "productionNetworkDenied",
                            "privilegedPodDenied",
                        ]
                    },
                },
            },
        }

    def test_pilot_only_enables_selected_repository_and_architecture(self):
        result = platform.activation_plan(self.root, self.config(), now=NOW)
        runners = [item for item in result["patches"] if item["kind"] == "HelmRelease"]
        self.assertEqual(
            [item["metadata"]["name"] for item in runners], ["infra-773018612-amd64"]
        )
        self.assertEqual(result["patches"][0]["metadata"]["name"], "platform")
        self.assertEqual(
            result["githubVariables"]["INFRA_CI_ARCHITECTURES"], '["amd64"]'
        )

    def test_unpurchased_arm_and_arbitrary_node_evidence_are_refused(self):
        config = self.config()
        config["runnerArchitectures"] = ["arm64"]
        with self.assertRaises(platform.PlatformError):
            platform.activation_plan(self.root, config, now=NOW)
        config = self.config()
        config["evidence"]["hostNetwork"]["nodeConfigurationHashes"]["invented"] = (
            "d" * 64
        )
        with self.assertRaisesRegex(platform.PlatformError, "each verified node"):
            platform.activation_plan(self.root, config, now=NOW)

    def test_broad_api_route_and_failed_sandbox_are_refused(self):
        config = self.config()
        config["settings"]["KUBERNETES_API_CIDR"] = "10.0.0.0/8"
        with self.assertRaisesRegex(platform.PlatformError, "exact enrolled"):
            platform.activation_plan(self.root, config, now=NOW)
        config = self.config()
        config["evidence"]["ciSandbox"]["passed"]["nixSandbox"] = False
        with self.assertRaisesRegex(platform.PlatformError, "nixSandbox"):
            platform.activation_plan(self.root, config, now=NOW)

    def test_multiple_repository_activation_requires_contention_evidence(self):
        config = self.config()
        config["runnerRepositories"].append(1327945675)
        required = platform.secret_contracts(
            self.root, set(config["layers"]), config["runnerRepositories"], ["amd64"]
        )
        config["secrets"] = [
            {"namespace": ns, "name": name, "keys": [key]} for ns, name, key in required
        ]
        with self.assertRaisesRegex(platform.PlatformError, "aggregateCapacity"):
            platform.activation_plan(self.root, config, now=NOW)

    def test_version_drift_and_unscoped_source_are_detected(self):
        path = self.root / "platform/components/ingress/traefik.yaml"
        item = yaml.safe_load(path.read_text())
        item["spec"]["chart"]["spec"]["version"] = "*"
        item["spec"]["chart"]["spec"]["sourceRef"]["namespace"] = "portfolio"
        path.write_text(yaml.safe_dump(item))
        errors = platform.check(self.root)
        self.assertTrue(any("version drift" in error for error in errors))
        self.assertTrue(any("centrally owned" in error for error in errors))

    def test_runner_hook_cannot_bypass_scheduler(self):
        path = self.root / "platform/components/runners/scalesets.yaml"
        resources = list(yaml.safe_load_all(path.read_text()))
        release = next(item for item in resources if item["kind"] == "HelmRelease")
        runner = release["spec"]["values"]["template"]["spec"]["containers"][0]
        next(
            item
            for item in runner["env"]
            if item["name"] == "ACTIONS_RUNNER_USE_KUBE_SCHEDULER"
        )["value"] = "false"
        path.write_text(yaml.safe_dump_all(resources))
        self.assertTrue(
            any(
                "must use the scheduler" in error for error in platform.check(self.root)
            )
        )

    def test_hourly_rpo_reserves_dump_disk_and_runs_every_half_hour(self):
        doc = {
            "project": "llunde-backend",
            "data": {
                "postgres": {
                    "sizeClass": "small",
                    "recovery": {"rpoHours": 1, "rtoHours": 2},
                }
            },
        }
        result = backups.generate(ROOT, doc)
        job = next(item for item in result if item["kind"] == "CronJob")
        self.assertTrue(job["spec"]["suspend"])
        self.assertEqual(job["spec"]["schedule"], "*/30 * * * *")
        pod = job["spec"]["jobTemplate"]["spec"]["template"]["spec"]
        self.assertEqual(
            pod["containers"][0]["resources"]["requests"]["ephemeral-storage"], "20Gi"
        )
        self.assertIn("pg_restore --list", pod["initContainers"][0]["args"][0])
        self.assertNotIn("forget", json.dumps(pod))


if __name__ == "__main__":
    unittest.main()
