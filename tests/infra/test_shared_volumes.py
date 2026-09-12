import copy
import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

import yaml
from infra.contracts import ContractError, catalog_project, validate_project
from infra.render import chart_values
from jsonschema import Draft7Validator

ROOT = Path(__file__).resolve().parents[2]
HELM = os.environ.get("HELM") or shutil.which("helm")


def project():
    return {
        "schemaVersion": 1,
        "project": "llunde-pyparser",
        "sharedVolumes": {"files": {"sizeClass": "large"}},
        "workloads": {
            "review": {
                "kind": "web",
                "port": 8000,
                "healthPath": "/healthz",
                "readinessPath": "/readyz",
                "sharedVolume": {"name": "files", "mountPath": "/app/.local/files"},
            },
            "extract": {
                "kind": "worker",
                "terminationGracePeriodSeconds": 90,
                "sharedVolume": {"name": "files", "mountPath": "/app/.local/files"},
            },
            "light": {
                "kind": "worker",
                "terminationGracePeriodSeconds": 45,
                "sharedVolume": {"name": "files", "mountPath": "/data"},
            },
        },
    }


def values(document=None, platforms=None):
    catalog, approved = catalog_project(ROOT, "llunde-pyparser")
    release = {
        "schemaVersion": 1,
        "project": "llunde-pyparser",
        "environment": "production",
        "image": approved["images"]["app"] + "@sha256:" + "a" * 64,
        "sourceRevision": "b" * 40,
        "provenance": {
            "workflowRepository": catalog["infrastructureRepository"],
            "workflowRevision": "c" * 40,
            "workflowPath": ".github/workflows/project-ci.yml",
            "repositoryId": approved["repositoryId"],
            "runId": 123,
            "platforms": platforms or ["linux/amd64"],
        },
    }
    compiled = chart_values(ROOT, document or project(), release)
    compiled.pop("data")
    return compiled


class SharedVolumeContract(unittest.TestCase):
    def test_only_parser_has_shared_volume_approval(self):
        catalog, _ = catalog_project(ROOT, "llunde-pyparser")
        approved = {
            name
            for name, item in catalog["projects"].items()
            if "shared-volume" in item["capabilities"]
        }
        self.assertEqual(approved, {"llunde-pyparser"})
        value = project()
        value["project"] = "portfolio"
        with self.assertRaisesRegex(ContractError, "shared-volume capability"):
            validate_project(ROOT, value)

    def test_references_require_declared_used_and_compatible_volumes(self):
        mutations = [
            (lambda p: p["sharedVolumes"].clear(), "not declared"),
            (
                lambda p: p["sharedVolumes"].update(unused={"sizeClass": "small"}),
                "unused",
            ),
            (
                lambda p: p["workloads"]["light"].update(architecture="arm64"),
                "common architecture",
            ),
            (
                lambda p: p["workloads"].update(
                    {
                        "shared-files": {
                            "kind": "worker",
                            "volume": {"mountPath": "/data", "sizeClass": "small"},
                        }
                    }
                ),
                "claim conflicts",
            ),
        ]
        for mutation, message in mutations:
            with self.subTest(message=message):
                value = project()
                mutation(value)
                with self.assertRaisesRegex(ContractError, message):
                    validate_project(ROOT, value)

    def test_source_contract_rejects_unsafe_shared_consumers(self):
        for changes in [
            {"replicas": 2},
            {"kind": "cron", "schedule": "0 * * * *"},
            {"volume": {"mountPath": "/data", "sizeClass": "small"}},
            {
                "sharedVolume": {
                    "name": "files",
                    "mountPath": "/app/.local/files/../../etc",
                }
            },
            {"terminationGracePeriodSeconds": 121},
            {"readinessPath": "/ready"},
        ]:
            with self.subTest(changes=changes):
                value = project()
                value["workloads"]["extract"].update(changes)
                with self.assertRaises(ContractError):
                    validate_project(ROOT, value)

    def test_chart_schema_keeps_shared_consumers_bounded(self):
        validator = Draft7Validator(
            json.loads((ROOT / "charts/project/values.schema.json").read_text())
        )
        compiled = values()
        validator.validate(compiled)
        self.assertEqual(compiled["sharedVolumes"], {"files": {"size": "100Gi"}})
        self.assertTrue(
            all(
                workload["replicas"] == 1 for workload in compiled["workloads"].values()
            )
        )
        for changes in [
            {"replicas": 2},
            {"kind": "cron", "schedule": "0 * * * *"},
            {"volume": {"mountPath": "/data", "size": "10Gi"}},
            {"sharedVolume": {"name": "files", "mountPath": "/etc"}},
            {"readinessPath": "/ready"},
            {"terminationGracePeriodSeconds": 0},
        ]:
            with self.subTest(changes=changes):
                candidate = copy.deepcopy(compiled)
                candidate["workloads"]["extract"].update(changes)
                self.assertFalse(validator.is_valid(candidate))

    def test_shared_consumers_are_narrowed_to_their_common_architecture(self):
        document = project()
        document["workloads"]["review"]["architecture"] = "multi"
        compiled = values(document, ["linux/amd64", "linux/arm64"])
        self.assertTrue(
            all(
                workload["architectures"] == ["amd64"]
                for workload in compiled["workloads"].values()
            )
        )

    @unittest.skipUnless(HELM, "Helm required for chart render verification")
    def test_chart_uses_one_retained_claim_and_colocated_recreate_consumers(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "values.yaml"
            compiled = values()
            compiled["workloads"]["review"]["architectures"] = ["amd64", "arm64"]
            path.write_text(yaml.safe_dump(compiled))
            output = subprocess.check_output(
                [
                    HELM,
                    "template",
                    "llunde-pyparser",
                    str(ROOT / "charts/project"),
                    "-f",
                    str(path),
                ],
                text=True,
            )
        documents = list(yaml.safe_load_all(output))
        claims = [item for item in documents if item["kind"] == "PersistentVolumeClaim"]
        self.assertEqual(len(claims), 1)
        claim = claims[0]
        self.assertEqual(claim["spec"]["accessModes"], ["ReadWriteOnce"])
        self.assertEqual(claim["spec"]["storageClassName"], "local-retain")
        self.assertEqual(
            claim["metadata"]["annotations"]["helm.sh/resource-policy"], "keep"
        )
        deployments = [item for item in documents if item["kind"] == "Deployment"]
        self.assertEqual(len(deployments), 3)
        for deployment in deployments:
            self.assertEqual(deployment["spec"]["strategy"], {"type": "Recreate"})
            pod = deployment["spec"]["template"]["spec"]
            self.assertNotIn("topologySpreadConstraints", pod)
            self.assertEqual(
                next(
                    volume
                    for volume in pod["volumes"]
                    if volume["name"] == "shared-data"
                )["persistentVolumeClaim"]["claimName"],
                claim["metadata"]["name"],
            )
            requirements = pod["affinity"]["nodeAffinity"][
                "requiredDuringSchedulingIgnoredDuringExecution"
            ]["nodeSelectorTerms"][0]["matchExpressions"]
            self.assertTrue(
                any(
                    item["key"] == "node-restriction.kubernetes.io/stateful"
                    and item["values"] == ["true"]
                    for item in requirements
                )
            )
            self.assertTrue(
                any(
                    item["key"] == "kubernetes.io/arch" and item["values"] == ["amd64"]
                    for item in requirements
                )
            )
        pods = {
            item["metadata"]["name"].removeprefix("llunde-pyparser-"): item["spec"][
                "template"
            ]["spec"]
            for item in deployments
        }
        self.assertEqual(pods["extract"]["terminationGracePeriodSeconds"], 90)
        self.assertEqual(pods["light"]["terminationGracePeriodSeconds"], 45)
        self.assertEqual(pods["review"]["terminationGracePeriodSeconds"], 30)
        container = pods["review"]["containers"][0]
        self.assertEqual(container["livenessProbe"]["httpGet"]["path"], "/healthz")
        self.assertEqual(container["readinessProbe"]["httpGet"]["path"], "/readyz")

    @unittest.skipUnless(HELM, "Helm required for chart render verification")
    def test_chart_rejects_missing_unused_or_incompatible_claims(self):
        for mutation, message in [
            (lambda v: v["sharedVolumes"].clear(), "not declared"),
            (lambda v: v["sharedVolumes"].update(unused={"size": "10Gi"}), "unused"),
            (
                lambda v: v["workloads"]["light"].update(architectures=["arm64"]),
                "common architecture",
            ),
        ]:
            with (
                self.subTest(message=message),
                tempfile.TemporaryDirectory() as directory,
            ):
                compiled = values()
                mutation(compiled)
                path = Path(directory) / "values.yaml"
                path.write_text(yaml.safe_dump(compiled))
                result = subprocess.run(
                    [
                        HELM,
                        "template",
                        "llunde-pyparser",
                        str(ROOT / "charts/project"),
                        "-f",
                        str(path),
                    ],
                    text=True,
                    capture_output=True,
                )
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)


if __name__ == "__main__":
    unittest.main()
