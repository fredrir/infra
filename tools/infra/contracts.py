import json
import re
from pathlib import Path

import yaml
from jsonschema import Draft202012Validator


class ContractError(ValueError):
    pass


class StrictLoader(yaml.SafeLoader):
    pass


def _mapping(loader, node, deep=False):
    result = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node, deep=deep)
        if not isinstance(key, str) or key in result:
            raise ContractError("mapping keys must be unique strings")
        result[key] = loader.construct_object(value_node, deep=deep)
    return result


StrictLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping)


def load_document(path):
    path = Path(path)
    if path.stat().st_size > 1_048_576:
        raise ContractError("configuration exceeds 1 MiB")
    try:
        return yaml.load(path.read_text(), Loader=StrictLoader)
    except yaml.YAMLError as error:
        raise ContractError(f"invalid YAML: {path}") from error


def validate_schema(root, name, document):
    schema = json.loads((Path(root) / "schemas" / f"{name}.schema.json").read_text())
    errors = list(Draft202012Validator(schema).iter_errors(document))
    if errors:
        error = min(errors, key=lambda item: str(list(item.absolute_path)))
        location = ".".join(str(part) for part in error.absolute_path) or name
        raise ContractError(f"{location}: {error.message}")


def catalog_project(root, project):
    if not re.fullmatch(r"[a-z][a-z0-9-]{0,39}", project):
        raise ContractError("invalid project name")
    catalog = load_document(Path(root) / "platform/catalog/projects.json")
    try:
        return catalog, catalog["projects"][project]
    except KeyError as error:
        raise ContractError(f"project requires catalog approval: {project}") from error


def validate_schedule(schedule):
    fields = schedule.split()
    bounds = [(0, 59), (0, 23), (1, 31), (1, 12), (0, 6)]
    if len(fields) != len(bounds):
        raise ContractError("cron schedule requires five fields")
    for field, (minimum, maximum) in zip(fields, bounds):
        for part in field.split(","):
            match = re.fullmatch(r"(\*|\d+(?:-\d+)?)(?:/(\d+))?", part)
            if not match:
                raise ContractError("invalid cron field")
            value, step = match.groups()
            if step is not None and not 1 <= int(step) <= maximum - minimum + 1:
                raise ContractError("cron step outside field bounds")
            if value != "*":
                values = [int(item) for item in value.split("-")]
                if not all(minimum <= item <= maximum for item in values) or values != sorted(values):
                    raise ContractError("cron value outside field bounds")


def validate_project(root, document):
    validate_schema(root, "project", document)
    _, project = catalog_project(root, document["project"])
    for dependency in document.get("data", {}):
        if dependency not in project["capabilities"]:
            raise ContractError(f"data capability not approved: {dependency}")
    claimed_domains = set()
    shared_volumes = document.get("sharedVolumes", {})
    if shared_volumes and "shared-volume" not in project["capabilities"]:
        raise ContractError("shared-volume capability not approved")
    shared_architectures = {}
    independent_claims = {f"{name}-data" for name, value in document["workloads"].items() if value.get("volume")}
    migration = document.get("migration")
    if migration:
        if "postgres" not in document.get("data", {}):
            raise ContractError("migration requires postgres data")
        if "migration" in document["workloads"]:
            raise ContractError("reserved workload name: migration")
        if migration["component"] not in (project.get("images") or {"app": project["image"]}):
            raise ContractError("migration component not approved")
        if set(migration.get("env", {})) & set(migration.get("secretKeys", [])):
            raise ContractError("migration has duplicate environment and secret keys")
        if migration.get("secretKeys") and "secrets" not in project["capabilities"]:
            raise ContractError("migration secrets capability not approved")
    for name, workload in document["workloads"].items():
        if workload["kind"] == "cron":
            validate_schedule(workload["schedule"])
        if workload.get("component", "app") not in (project.get("images") or {"app": project["image"]}):
            raise ContractError(f"{name}: component not approved")
        if name in {"postgres", "valkey"}:
            raise ContractError(f"reserved workload name: {name}")
        if set(workload.get("env", {})) & set(workload.get("secretKeys", [])):
            raise ContractError(f"{name}: duplicate environment and secret keys")
        if workload["kind"] not in project["capabilities"]:
            raise ContractError(f"{name}: workload capability not approved")
        architecture = workload.get("architecture", "amd64")
        selected = {"amd64", "arm64"} if architecture == "multi" else {architecture}
        if not selected.issubset(project["architectures"]):
            raise ContractError(f"{name}: architecture not approved")
        for domain in workload.get("domains", []):
            if domain not in project["domains"]:
                raise ContractError(f"{name}: domain not approved: {domain}")
            if domain in claimed_domains:
                raise ContractError(f"duplicate domain: {domain}")
            claimed_domains.add(domain)
        for field, capability in (("volume", "volume"), ("sharedVolume", "shared-volume"), ("secretKeys", "secrets")):
            if workload.get(field) and capability not in project["capabilities"]:
                raise ContractError(f"{name}: {capability} capability not approved")
        if workload.get("egress", "none") == "internet" and "internet" not in project["capabilities"]:
            raise ContractError(f"{name}: internet egress not approved")
        if (workload.get("volume") or workload.get("sharedVolume")) and workload.get("replicas", 1) != 1:
            raise ContractError(f"{name}: local storage requires one replica")
        if workload.get("volume") and workload["kind"] == "cron":
            raise ContractError(f"{name}: persistent cron storage requires a platform extension")
        if workload.get("sharedVolume"):
            volume = workload["sharedVolume"]["name"]
            if volume not in shared_volumes:
                raise ContractError(f"{name}: shared volume not declared: {volume}")
            if f"shared-{volume}-data" in independent_claims:
                raise ContractError(f"shared volume claim conflicts with workload: {volume}")
            shared_architectures[volume] = shared_architectures.get(volume, selected) & selected
            if not shared_architectures[volume]:
                raise ContractError(f"shared volume consumers require a common architecture: {volume}")
    unused = set(shared_volumes) - set(shared_architectures)
    if unused:
        raise ContractError(f"shared volumes are unused: {', '.join(sorted(unused))}")
    from .resources import validate_resource_budget
    validate_resource_budget(root, project, document)
    return project


def validate_release(root, document, project_name=None):
    validate_schema(root, "release", document)
    if project_name is not None and document["project"] != project_name:
        raise ContractError("release project mismatch")
    catalog, project = catalog_project(root, document["project"])
    if document["environment"] not in project["environments"]:
        raise ContractError("release environment not approved")
    image = (project.get("images") or {"app": project["image"]}).get(document.get("component", "app"))
    if document["image"].split("@", 1)[0] != image:
        raise ContractError("release image not approved")
    provenance = document["provenance"]
    if provenance["repositoryId"] != project["repositoryId"]:
        raise ContractError("release repository identity mismatch")
    if provenance["workflowRepository"] != catalog["infrastructureRepository"]:
        raise ContractError("release workflow repository mismatch")
    if provenance["workflowPath"] != ".github/workflows/project-ci.yml":
        raise ContractError("release workflow path mismatch")
    if not {p.removeprefix("linux/") for p in provenance["platforms"]}.issubset(project["architectures"]):
        raise ContractError("release architecture not approved")
    return project
