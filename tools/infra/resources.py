import copy
from decimal import Decimal
import json
from pathlib import Path
import re

from .contracts import ContractError


RESOURCE_CLASSES = {
    "small": {"requests": {"cpu": "100m", "memory": "128Mi", "ephemeral-storage": "256Mi"}, "limits": {"cpu": "500m", "memory": "512Mi", "ephemeral-storage": "1Gi"}},
    "medium": {"requests": {"cpu": "500m", "memory": "512Mi", "ephemeral-storage": "1Gi"}, "limits": {"cpu": "2", "memory": "2Gi", "ephemeral-storage": "4Gi"}},
    "large": {"requests": {"cpu": "1", "memory": "2Gi", "ephemeral-storage": "2Gi"}, "limits": {"cpu": "4", "memory": "4Gi", "ephemeral-storage": "8Gi"}},
    "compute": {"requests": {"cpu": "2", "memory": "4Gi", "ephemeral-storage": "2Gi"}, "limits": {"cpu": "4", "memory": "8Gi", "ephemeral-storage": "8Gi"}},
}
VOLUME_CLASSES = {"small": "10Gi", "medium": "40Gi", "large": "100Gi"}
DATA_RESOURCES = {"requests": {"cpu": "250m", "memory": "512Mi", "ephemeral-storage": "256Mi"}, "limits": {"cpu": "2", "memory": "1Gi", "ephemeral-storage": "1Gi"}}
BACKUP_RESOURCES = {"requests": {"cpu": "100m", "memory": "256Mi", "ephemeral-storage": "20Gi"}, "limits": {"cpu": "1", "memory": "1Gi", "ephemeral-storage": "20Gi"}}
QUOTA_KEYS = {"requests.cpu", "requests.memory", "requests.ephemeral-storage", "limits.cpu", "limits.memory", "limits.ephemeral-storage", "requests.storage", "persistentvolumeclaims", "pods", "services", "count/jobs.batch"}


def quantity(value):
    match = re.fullmatch(r"([0-9]+(?:\.[0-9]+)?)(m|Ki|Mi|Gi|Ti)?", str(value))
    if not match:
        raise ContractError(f"unsupported resource quantity: {value}")
    number, suffix = match.groups()
    return Decimal(number) * {None: 1, "m": Decimal("0.001"), "Ki": 1024, "Mi": 1024**2, "Gi": 1024**3, "Ti": 1024**4}[suffix]


def project_quota(root, project):
    profiles = json.loads((Path(root) / "platform/catalog/resource-quotas.json").read_text())
    profile = project.get("quotaProfile", "standard")
    if profile not in profiles or set(profiles[profile]) != QUOTA_KEYS:
        raise ContractError("unknown or incomplete catalog quota profile")
    quota = profiles[profile]
    if any(quantity(value) <= 0 for value in quota.values()):
        raise ContractError("catalog quota quantities must be positive")
    return copy.deepcopy(quota)


def resource_budget(document):
    steady = {key: Decimal(0) for key in QUOTA_KEYS}
    batch = {key: Decimal(0) for key in QUOTA_KEYS}
    surge = {key: Decimal(0) for key in QUOTA_KEYS}
    terminating = {key: Decimal(0) for key in QUOTA_KEYS}

    def add(target, resources, count=1):
        target["pods"] += count
        for scope, values in resources.items():
            for name, value in values.items():
                target[f"{scope}.{name}"] += quantity(value) * count

    for workload in document["workloads"].values():
        resources = RESOURCE_CLASSES[workload.get("resourceClass", "small")]
        if workload["kind"] == "cron":
            add(batch, resources)
            steady["count/jobs.batch"] += 4
            continue
        local = workload.get("volume") or workload.get("sharedVolume")
        replicas = workload.get("replicas", 2 if workload["kind"] == "web" and not local else 1)
        add(steady, resources, replicas)
        if not local:
            add(surge, resources)
            add(terminating, resources, replicas - 1)
        if workload["kind"] == "web":
            steady["services"] += 1
        if workload.get("volume"):
            steady["requests.storage"] += quantity(VOLUME_CLASSES[workload["volume"]["sizeClass"]])
            steady["persistentvolumeclaims"] += 1
    for data in document.get("data", {}).values():
        add(steady, DATA_RESOURCES)
        add(batch, BACKUP_RESOURCES)
        steady["services"] += 1
        steady["requests.storage"] += quantity(VOLUME_CLASSES[data["sizeClass"]])
        steady["persistentvolumeclaims"] += 1
        steady["count/jobs.batch"] += 5
    for volume in document.get("sharedVolumes", {}).values():
        steady["requests.storage"] += quantity(VOLUME_CLASSES[volume["sizeClass"]])
        steady["persistentvolumeclaims"] += 1
    if document.get("migration"):
        steady["count/jobs.batch"] += 1
    jobs = {key: value + batch[key] for key, value in steady.items()}
    phases = {"steady-state": steady, "concurrent-jobs": jobs}
    if document.get("migration"):
        migration = copy.deepcopy(jobs)
        add(migration, RESOURCE_CLASSES[document["migration"].get("resourceClass", "small")])
        phases["migration"] = migration
    phases["rollout-surge"] = {key: value + surge[key] for key, value in jobs.items()}
    phases["rollout-termination"] = {key: value + surge[key] + terminating[key] for key, value in jobs.items()}
    return phases


def validate_resource_budget(root, project, document):
    quota = project_quota(root, project)
    for phase, resources in resource_budget(document).items():
        for name, amount in sorted(resources.items()):
            if amount > quantity(quota[name]):
                raise ContractError(f"{phase} exceeds project quota {name} ({quota[name]}); reduce demand or request catalog quota approval")
    return quota
