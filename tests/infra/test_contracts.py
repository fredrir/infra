import copy
import json
import shutil
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from jsonschema import Draft7Validator

from infra.cli import main
from infra.contracts import ContractError, catalog_project, load_document, validate_project, validate_release
from infra.render import RESOURCE_CLASSES, chart_values, render_project


ROOT = Path(__file__).resolve().parents[2]


def project():
    return {"schemaVersion": 1, "project": "portfolio", "workloads": {"web": {"kind": "web", "port": 3000, "healthPath": "/", "domains": ["hansteen.dev"]}}}


def release():
    return {"schemaVersion": 1, "project": "portfolio", "environment": "production", "image": "ghcr.io/fredrir/portfolio@sha256:" + "a" * 64, "sourceRevision": "b" * 40,
        "provenance": {"workflowRepository": "fredrir/llunde-infra", "workflowRevision": "c" * 40, "workflowPath": ".github/workflows/project-ci.yml", "repositoryId": 773018612, "runId": 123, "platforms": ["linux/amd64"]}}


def application_values(document, record):
    values = chart_values(ROOT, document, record)
    values.pop("data")
    return values


class ProjectContracts(unittest.TestCase):
    def test_rejects_foreign_domain_and_privilege_injection(self):
        for mutate in [
            lambda p: p["workloads"]["web"].update(domains=["grafana.fredrir.com"]),
            lambda p: p.update(namespace="kube-system"),
            lambda p: p["workloads"]["web"].update(securityContext={"privileged": True}),
            lambda p: p.update(extraObjects=[{"kind": "ClusterRoleBinding"}]),
        ]:
            with self.subTest(mutate=mutate):
                value = project()
                mutate(value)
                with self.assertRaises(ContractError):
                    validate_project(ROOT, value)

    def test_rejects_duplicate_yaml_keys(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "project.yaml"
            path.write_text("project: portfolio\nproject: caddy\n")
            with self.assertRaises(ContractError):
                load_document(path)

    def test_rejects_data_without_catalog_capability(self):
        value = project()
        value["data"] = {"valkey": {"sizeClass": "small", "recovery": {"rpoHours": 24, "rtoHours": 4}}}
        with self.assertRaisesRegex(ContractError, "data capability"):
            validate_project(ROOT, value)

    def test_local_volume_requires_single_writer(self):
        value = project()
        value["project"] = "llunde-pyparser"
        value["workloads"]["web"].pop("domains")
        value["workloads"]["web"].update(volume={"mountPath": "/data", "sizeClass": "small"}, replicas=2)
        with self.assertRaisesRegex(ContractError, "one replica"):
            validate_project(ROOT, value)

    def test_release_identity_and_mutable_images_rejected(self):
        for field, bad in [("image", "ghcr.io/fredrir/portfolio:latest"), ("image", "ghcr.io/fredrir/llunde-backend@sha256:" + "a" * 64), ("project", "llunde-backend")]:
            with self.subTest(field=field, bad=bad):
                value = release()
                value[field] = bad
                with self.assertRaises(ContractError):
                    validate_release(ROOT, value, "portfolio")
        value = release()
        value["provenance"]["repositoryId"] = 1327945675
        with self.assertRaisesRegex(ContractError, "identity"):
            validate_release(ROOT, value)

    def test_missing_native_variant_blocks_deployment(self):
        value = project()
        value["workloads"]["web"]["architecture"] = "multi"
        with self.assertRaisesRegex(ContractError, "lacks required architecture"):
            chart_values(ROOT, value, release())

    def test_each_component_requires_its_own_verified_image(self):
        value = project()
        value["workloads"]["web"]["component"] = "web"
        value["workloads"]["worker"] = {"kind": "worker", "component": "worker"}
        web = release()
        web.update(component="web", image="ghcr.io/fredrir/portfolio-web@sha256:" + "d" * 64)
        worker = release()
        worker.update(component="worker", image="ghcr.io/fredrir/portfolio-worker@sha256:" + "e" * 64)
        with self.assertRaisesRegex(ContractError, "component release required"):
            chart_values(ROOT, value, releases={"web": web})
        rendered = chart_values(ROOT, value, releases={"web": web, "worker": worker})
        self.assertEqual(rendered["workloads"]["worker"]["image"], worker["image"])
        wrong = copy.deepcopy(worker)
        wrong["image"] = web["image"]
        with self.assertRaisesRegex(ContractError, "image not approved"):
            chart_values(ROOT, value, releases={"web": web, "worker": wrong})
        stale = copy.deepcopy(worker)
        stale["sourceRevision"] = "f" * 40
        with self.assertRaisesRegex(ContractError, "same source"):
            chart_values(ROOT, value, releases={"web": web, "worker": stale})

    def test_no_release_creates_no_application(self):
        resources = render_project(ROOT, project())
        self.assertNotIn("HelmRelease", {item["kind"] for item in resources})
        namespace = next(item for item in resources if item["kind"] == "Namespace")
        self.assertEqual(namespace["metadata"]["name"], "portfolio")
        role = next(item for item in resources if item["kind"] == "Role")
        self.assertTrue(all("roles" not in rule["resources"] and "rolebindings" not in rule["resources"] for rule in role["rules"]))

    def test_component_catalog_does_not_require_legacy_image(self):
        catalog, approved = catalog_project(ROOT, "portfolio")
        approved = copy.deepcopy(approved)
        approved.pop("image")
        with patch("infra.contracts.catalog_project", return_value=(catalog, approved)):
            validate_project(ROOT, project())
            validate_release(ROOT, release())

    def test_chart_rejects_unsafe_local_volume_combinations(self):
        schema = json.loads((ROOT / "charts/project/values.schema.json").read_text())
        validator = Draft7Validator(schema)
        value = application_values(project(), release())
        workload = value["workloads"]["web"]
        workload["volume"] = {"mountPath": "/data", "size": "10Gi"}
        self.assertFalse(validator.is_valid(value))
        workload["replicas"] = 1
        self.assertTrue(validator.is_valid(value))
        workload.update(kind="cron", schedule="0 * * * *")
        for key in ["domains", "port", "healthPath"]:
            workload.pop(key)
        self.assertFalse(validator.is_valid(value))

    def test_compiled_resource_classes_satisfy_the_chart_boundary(self):
        schema = json.loads((ROOT / "charts/project/values.schema.json").read_text())
        validator = Draft7Validator(schema)
        for name in RESOURCE_CLASSES:
            with self.subTest(resource_class=name):
                value = project()
                value["workloads"]["web"]["resourceClass"] = name
                value["workloads"]["web"]["replicas"] = 1
                validator.validate(application_values(value, release()))
        value = application_values(project(), release())
        value["workloads"]["web"]["resources"]["limits"]["memory"] = "128Gi"
        self.assertFalse(validator.is_valid(value))
        validator.validate(application_values(project(), release()))

    def test_validate_rejects_incomplete_component_release(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            shutil.copytree(ROOT / "schemas", root / "schemas")
            shutil.copytree(ROOT / "platform/catalog", root / "platform/catalog")
            location = root / "platform/projects/portfolio"
            (location / "releases").mkdir(parents=True)
            value = project()
            value["workloads"]["web"]["component"] = "web"
            value["workloads"]["worker"] = {"kind": "worker", "component": "worker"}
            record = release()
            record.update(component="web", image="ghcr.io/fredrir/portfolio-web@sha256:" + "d" * 64)
            (location / "project.yaml").write_text(json.dumps(value))
            (location / "releases/web.json").write_text(json.dumps(record))
            self.assertEqual(main(["--root", str(root), "validate"]), 1)

    def test_release_gets_scoped_reconciliation_and_recovery(self):
        resources = render_project(ROOT, project(), release())
        helm = next(item for item in resources if item["kind"] == "HelmRelease")
        self.assertEqual(helm["metadata"]["namespace"], "portfolio")
        self.assertEqual(helm["spec"]["serviceAccountName"], "project-reconciler")
        self.assertEqual(helm["spec"]["values"]["workloads"]["web"]["replicas"], 2)
        self.assertEqual(helm["spec"]["upgrade"]["remediation"]["strategy"], "rollback")

    def test_cron_requires_schedule(self):
        value = {"schemaVersion": 1, "project": "portfolio", "workloads": {"task": {"kind": "cron"}}}
        with self.assertRaises(ContractError):
            validate_project(ROOT, value)

    def test_cron_rejects_invalid_time_fields(self):
        value = {"schemaVersion": 1, "project": "portfolio", "workloads": {"task": {"kind": "cron"}}}
        for schedule in ["65 * * * *", "0 24 * * *", "*/0 * * * *", "* * * * * *", "0 0 0 * *", "0 0 * 13 *", "0 0 * * 6-1"]:
            with self.subTest(schedule=schedule):
                value["workloads"]["task"]["schedule"] = schedule
                with self.assertRaises(ContractError):
                    validate_project(ROOT, value)
        value["workloads"]["task"]["schedule"] = "*/15 2-4 * * 1,3,5"
        validate_project(ROOT, value)

    def test_onboarding_verifies_repository_and_pins_workflow(self):
        identity = {"id": 773018612, "name": "portfolio", "full_name": "fredrir/portfolio", "owner": {"id": 114402558}}
        with tempfile.TemporaryDirectory() as directory, patch("infra.cli.subprocess.check_output", return_value=json.dumps(identity)):
            code = main(["--root", str(ROOT), "onboard", "fredrir/portfolio", "--domain", "hansteen.dev", "--readiness-path", "/ready", "--termination-grace-period-seconds", "45", "--workflow-ref", "d" * 40, "--test-command", "npm test", "--output", directory])
            self.assertEqual(code, 0)
            caller = load_document(Path(directory) / ".github/workflows/ci.yml")
            self.assertEqual(caller["on"], {"push": {"branches": ["main"]}})
            self.assertTrue(caller["jobs"]["project"]["uses"].endswith("@" + "d" * 40))
            self.assertNotIn("secrets", caller["jobs"]["project"])
            generated = load_document(Path(directory) / "project.yaml")
            self.assertEqual(generated["workloads"]["app"]["readinessPath"], "/ready")
            self.assertEqual(generated["workloads"]["app"]["terminationGracePeriodSeconds"], 45)


if __name__ == "__main__":
    unittest.main()
