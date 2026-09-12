import copy
from decimal import Decimal
import importlib.util
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import yaml

from infra.contracts import ContractError, catalog_project, load_document, validate_project
from infra.render import chart_values, render_project
from infra.resources import BACKUP_RESOURCES, DATA_RESOURCES, project_quota, quantity, resource_budget, validate_resource_budget


ROOT = Path(__file__).resolve().parents[2]
HELM = os.environ.get("HELM") or shutil.which("helm")


def parser_project():
    return {
        "schemaVersion": 1, "project": "llunde-pyparser",
        "workloads": {
            "review": {"kind": "web", "port": 8081, "healthPath": "/healthz", "resourceClass": "medium", "sharedVolume": {"name": "files", "mountPath": "/app/.local/files"}},
            "extract": {"kind": "worker", "resourceClass": "compute", "terminationGracePeriodSeconds": 90, "sharedVolume": {"name": "files", "mountPath": "/app/.local/files"}},
            "light": {"kind": "worker", "resourceClass": "medium", "terminationGracePeriodSeconds": 45, "sharedVolume": {"name": "files", "mountPath": "/app/.local/files"}},
        },
        "sharedVolumes": {"files": {"sizeClass": "large"}},
        "data": {name: {"sizeClass": "small", "recovery": {"rpoHours": 24, "rtoHours": 4}} for name in ["postgres", "valkey"]},
        "migration": {"component": "app", "command": ["alembic"], "args": ["upgrade", "head"], "compatibility": "backward-compatible", "retrySafe": True},
    }


class ProjectResources(unittest.TestCase):
    def test_parser_catalog_budget_covers_full_data_backup_and_migration_set(self):
        document = parser_project()
        validate_project(ROOT, document)
        budget = resource_budget(document)
        self.assertEqual(budget["steady-state"]["requests.cpu"], Decimal("3.5"))
        self.assertEqual(budget["steady-state"]["requests.memory"], quantity("6Gi"))
        self.assertEqual(budget["steady-state"]["limits.cpu"], 12)
        self.assertEqual(budget["concurrent-jobs"]["requests.cpu"], Decimal("3.7"))
        self.assertEqual(budget["concurrent-jobs"]["limits.cpu"], 14)
        self.assertEqual(budget["concurrent-jobs"]["limits.memory"], quantity("16Gi"))
        self.assertEqual(budget["concurrent-jobs"]["requests.ephemeral-storage"], quantity("44.5Gi"))
        self.assertEqual(budget["migration"]["requests.cpu"], Decimal("3.8"))
        self.assertEqual(budget["migration"]["limits.cpu"], Decimal("14.5"))
        self.assertEqual(budget["migration"]["limits.memory"], quantity("16.5Gi"))
        self.assertEqual(budget["rollout-surge"], budget["concurrent-jobs"])
        self.assertEqual(budget["rollout-termination"], budget["concurrent-jobs"])
        self.assertEqual(budget["steady-state"]["requests.storage"], quantity("120Gi"))
        self.assertEqual(budget["steady-state"]["persistentvolumeclaims"], 3)
        quota = next(item for item in render_project(ROOT, document) if item["kind"] == "ResourceQuota")
        self.assertEqual(quota["spec"]["hard"]["limits.cpu"], "16")
        self.assertEqual(quota["spec"]["hard"]["limits.memory"], "20Gi")

    def test_parser_profile_is_platform_owned_and_standard_profile_refuses_overage(self):
        _, approved = catalog_project(ROOT, "llunde-pyparser")
        standard = copy.deepcopy(approved)
        standard.pop("quotaProfile")
        with self.assertRaisesRegex(ContractError, "concurrent-jobs.*limits.cpu"):
            validate_resource_budget(ROOT, standard, parser_project())
        document = parser_project()
        document["quotaProfile"] = "parser"
        with self.assertRaises(ContractError):
            validate_project(ROOT, document)

    def test_steady_state_fit_does_not_hide_rollout_surge_or_terminating_pods(self):
        document = {"schemaVersion": 1, "project": "portfolio", "workloads": {"web": {"kind": "web", "port": 3000, "healthPath": "/", "resourceClass": "compute", "replicas": 2}}}
        with self.assertRaisesRegex(ContractError, "rollout-surge"):
            validate_project(ROOT, document)
        document["workloads"]["web"].update(resourceClass="medium", replicas=4)
        budget = resource_budget(document)
        self.assertEqual(budget["steady-state"]["pods"], 4)
        self.assertEqual(budget["rollout-surge"]["pods"], 5)
        self.assertEqual(budget["rollout-termination"]["pods"], 8)
        with self.assertRaisesRegex(ContractError, "rollout-termination"):
            validate_project(ROOT, document)

    def test_retained_claims_and_job_history_are_accounted_once_per_object(self):
        document = parser_project()
        document["sharedVolumes"]["other"] = {"sizeClass": "large"}
        document["workloads"]["light"]["sharedVolume"]["name"] = "other"
        with self.assertRaisesRegex(ContractError, "requests.storage"):
            validate_project(ROOT, document)
        document = {"schemaVersion": 1, "project": "llunde-pyparser", "workloads": {f"cron-{index}": {"kind": "cron", "schedule": "0 * * * *"} for index in range(8)}}
        with self.assertRaisesRegex(ContractError, "count/jobs.batch"):
            validate_project(ROOT, document)

    def test_migration_and_rollout_are_separate_phases(self):
        document = parser_project()
        document["workloads"]["review"].pop("sharedVolume")
        document["workloads"]["review"]["replicas"] = 1
        budget = resource_budget(document)
        self.assertEqual(budget["migration"]["limits.cpu"], Decimal("14.5"))
        self.assertEqual(budget["rollout-surge"]["limits.cpu"], 16)
        validate_project(ROOT, document)

    def test_backup_accounting_matches_effective_init_and_upload_resources(self):
        spec = importlib.util.spec_from_file_location("backup_jobs", ROOT / "scripts/operations/backup_jobs.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        jobs = [item for item in module.generate(ROOT, parser_project()) if item["kind"] == "CronJob"]
        self.assertEqual(len(jobs), 2)
        for job in jobs:
            pod = job["spec"]["jobTemplate"]["spec"]["template"]["spec"]
            effective = {}
            for scope in ["requests", "limits"]:
                effective[scope] = {}
                for resource in ["cpu", "memory", "ephemeral-storage"]:
                    running = sum(quantity(item["resources"][scope][resource]) for item in pod["containers"])
                    initializing = max(quantity(item["resources"][scope][resource]) for item in pod["initContainers"])
                    effective[scope][resource] = max(running, initializing)
                    self.assertEqual(effective[scope][resource], quantity(BACKUP_RESOURCES[scope][resource]))

    @unittest.skipUnless(HELM, "Helm required for data chart accounting verification")
    def test_data_accounting_matches_rendered_statefulsets(self):
        output = subprocess.check_output([HELM, "template", "parser-data", str(ROOT / "charts/project-data"), "-f", str(ROOT / "tests/infra/fixtures/data-values.yaml")], text=True)
        data = [item for item in yaml.safe_load_all(output) if item["kind"] == "StatefulSet"]
        self.assertEqual(len(data), 2)
        for item in data:
            self.assertEqual(item["spec"]["template"]["spec"]["containers"][0]["resources"], DATA_RESOURCES)

    @unittest.skipUnless(HELM, "Helm required for rollout accounting verification")
    def test_phase_accounting_matches_rendered_replicas_rollout_and_job_history(self):
        document = load_document(ROOT / "tests/infra/fixtures/project.yaml")
        release = load_document(ROOT / "tests/infra/fixtures/release.json")
        values = chart_values(ROOT, document, release)
        data = values.pop("data")
        objects = []
        with tempfile.TemporaryDirectory() as directory:
            for chart, compiled in [("project", values), ("project-data", {"project": document["project"], "data": data})]:
                path = Path(directory) / f"{chart}.yaml"
                path.write_text(yaml.safe_dump(compiled))
                output = subprocess.check_output([HELM, "template", document["project"], str(ROOT / "charts" / chart), "-f", str(path)], text=True)
                objects.extend(yaml.safe_load_all(output))
        objects.extend(item for item in render_project(ROOT, document) if item["kind"] == "CronJob")
        keys = [f"{scope}.{name}" for scope in ["requests", "limits"] for name in ["cpu", "memory", "ephemeral-storage"]] + ["pods"]
        steady, jobs, migration, surge, terminating = [{key: Decimal(0) for key in keys} for _ in range(5)]

        def add(target, pod, count=1):
            target["pods"] += count
            for key in keys[:-1]:
                scope, name = key.split(".")
                running = sum(quantity(container["resources"][scope][name]) for container in pod["containers"])
                initializing = max([quantity(container["resources"][scope][name]) for container in pod.get("initContainers", [])] or [0])
                target[key] += max(running, initializing) * count

        expected_history = 0
        for item in objects:
            spec = item.get("spec", {})
            if item["kind"] in {"Deployment", "StatefulSet"}:
                replicas = spec["replicas"]
                pod = spec["template"]["spec"]
                add(steady, pod, replicas)
                if item["kind"] == "Deployment" and spec["strategy"]["type"] == "RollingUpdate":
                    self.assertEqual(spec["strategy"]["rollingUpdate"]["maxUnavailable"], 0)
                    count = min(replicas, spec["strategy"]["rollingUpdate"]["maxSurge"])
                    add(surge, pod, count)
                    add(terminating, pod, replicas - count)
            elif item["kind"] == "CronJob":
                self.assertEqual(spec["concurrencyPolicy"], "Forbid")
                add(jobs, spec["jobTemplate"]["spec"]["template"]["spec"])
                expected_history += spec["successfulJobsHistoryLimit"] + spec["failedJobsHistoryLimit"] + 1
            elif item["kind"] == "Job":
                add(migration, spec["template"]["spec"])
                expected_history += 1
        budget = resource_budget(document)
        for phase, increments in {
            "steady-state": [], "concurrent-jobs": [jobs], "migration": [jobs, migration],
            "rollout-surge": [jobs, surge], "rollout-termination": [jobs, surge, terminating],
        }.items():
            for key in keys:
                self.assertEqual(budget[phase][key], steady[key] + sum(item[key] for item in increments), (phase, key))
        self.assertEqual(budget["steady-state"]["count/jobs.batch"], expected_history)

    def test_unknown_catalog_quota_profile_fails_closed(self):
        with self.assertRaises(ContractError):
            project_quota(ROOT, {"quotaProfile": "unbounded"})


if __name__ == "__main__":
    unittest.main()
