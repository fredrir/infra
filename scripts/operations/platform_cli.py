from __future__ import annotations

import argparse
import hashlib
import importlib.util
import ipaddress
import json
import re
import sys
from datetime import UTC, datetime, timedelta
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
DIGEST = re.compile(r"[a-z0-9][a-z0-9./_-]*@sha256:[0-9a-f]{64}")
CI_IMAGE = re.compile(r"ghcr\.io/fredrir/infra-ci@sha256:[0-9a-f]{64}")
BASE = {"sources", "policy", "controllers"}
DEPENDENCIES = {
    "sources": set(),
    "policy": set(),
    "controllers": {"sources", "policy"},
    "secrets": {"controllers"},
    "ingress": {"secrets"},
    "observability": {"secrets"},
    "runners": {"controllers", "secrets"},
    "cache": {"secrets"},
    "backups": {"secrets"},
    "projects": {"policy", "ingress", "observability"},
}


class PlatformError(ValueError):
    pass


def documents(root: Path):
    for path in sorted((root / "platform/components").rglob("*.yaml")):
        for item in yaml.safe_load_all(path.read_text()):
            if item:
                yield path, item


def check(root: Path) -> list[str]:
    errors = []
    versions = yaml.safe_load((root / "platform/versions.yaml").read_text())
    catalog = json.loads((root / "platform/catalog/projects.json").read_text())
    expected_runners = {
        (
            f"infra-{project['repositoryId']}-{arch}",
            f"https://github.com/{project['repository']}",
        )
        for project in catalog["projects"].values()
        for arch in project["architectures"]
    }
    actual_runners = set()
    for path, item in documents(root):
        kind = item["kind"]
        if "labels" in item:
            errors.append(f"{path}: labels must be under metadata")
        if kind == "HelmRelease":
            spec = item["spec"]
            chart = spec["chart"]["spec"]
            if (
                chart["chart"] not in versions["charts"]
                or chart["version"] != versions["charts"][chart["chart"]]["version"]
            ):
                errors.append(f"{path}: chart version drift")
            if chart["sourceRef"].get("namespace") != "flux-system":
                errors.append(f"{path}: chart source must be centrally owned")
            if chart["chart"] == "gha-runner-scale-set":
                values = spec["values"]
                actual_runners.add(
                    (values["runnerScaleSetName"], values["githubConfigUrl"])
                )
                if (
                    spec.get("suspend") is not True
                    or values.get("maxRunners") != 0
                    or values.get("minRunners") != 0
                ):
                    errors.append(
                        f"{path}: runner activation must be a reviewed overlay"
                    )
                if values["template"]["spec"].get("runtimeClassName") != "gvisor":
                    errors.append(f"{path}: runner sandbox is mandatory")
                runner = next(
                    container
                    for container in values["template"]["spec"]["containers"]
                    if container["name"] == "runner"
                )
                environment = {
                    entry["name"]: entry.get("value") for entry in runner.get("env", [])
                }
                if (
                    runner["image"] != versions["images"]["runner"]
                    or values.get("containerMode", {}).get("type") != "kubernetes"
                ):
                    errors.append(
                        f"{path}: runner image and PVC hook mode must match the reviewed contract"
                    )
                if any(
                    environment.get(key) != "true"
                    for key in [
                        "ACTIONS_RUNNER_REQUIRE_JOB_CONTAINER",
                        "ACTIONS_RUNNER_USE_KUBE_SCHEDULER",
                    ]
                ):
                    errors.append(
                        f"{path}: isolated job containers must use the scheduler"
                    )
        if (
            kind == "ClusterRoleBinding"
            and item["metadata"]["name"] != "platform-reconciler"
        ):
            errors.append(f"{path}: unexpected platform-wide role binding")
        if (
            kind == "ValidatingAdmissionPolicy"
            and item["spec"].get("failurePolicy") != "Fail"
        ):
            errors.append(f"{path}: admission must fail closed")
    if actual_runners != expected_runners:
        errors.append(
            "ARC registrations differ from the repository and architecture catalog"
        )
    module_spec = importlib.util.spec_from_file_location(
        "platform_runners", Path(__file__).with_name("runners.py")
    )
    generator = importlib.util.module_from_spec(module_spec)
    module_spec.loader.exec_module(generator)
    if (
        root / "platform/components/runners/scalesets.yaml"
    ).read_text() != generator.render(root):
        errors.append("ARC resources differ from the catalog and canonical template")
    root_path = root / "platform/clusters/production/root.yaml"
    for item in yaml.safe_load_all(root_path.read_text()):
        if item["spec"].get("suspend") is not True:
            errors.append(f"{root_path}: activation must use a reviewed overlay")
        referenced = (
            root / item["spec"]["path"].removeprefix("./") / "kustomization.yaml"
        )
        if not referenced.is_file():
            errors.append(f"{referenced}: missing deployment entrypoint")
    return errors


def evidence(config: dict, key: str, *, now: datetime | None = None) -> dict:
    record = config.get("evidence", {}).get(key, {})
    if not re.fullmatch(r"[0-9a-f]{64}", record.get("reportSha256", "")):
        raise PlatformError(f"{key}: a reviewed evidence report digest is required")
    try:
        verified = datetime.fromisoformat(record["verifiedAt"].replace("Z", "+00:00"))
        current = now or datetime.now(UTC)
        if (
            verified.tzinfo is None
            or verified > current
            or current - verified > timedelta(days=30)
        ):
            raise ValueError()
    except (KeyError, ValueError):
        raise PlatformError(
            f"{key}: evidence must be timestamped within the last 30 days"
        ) from None
    return record


def secret_contracts(
    root: Path, layers: set[str], runner_repositories=None, runner_architectures=None
) -> set[tuple[str, str, str]]:
    required = {("flux-system", "sops-age", "age.agekey")}
    if "secrets" in layers:
        for _, item in documents(root):
            if item["kind"] == "SecretStore":
                required.add(
                    (item["metadata"]["namespace"], "doppler-token", "dopplerToken")
                )
    if "runners" in layers:
        for _, item in documents(root):
            if (
                item["kind"] == "HelmRelease"
                and item["spec"]["chart"]["spec"]["chart"] == "gha-runner-scale-set"
            ):
                scale_name = item["spec"]["values"]["runnerScaleSetName"]
                if (
                    runner_repositories is not None
                    and int(scale_name.split("-")[1]) not in runner_repositories
                ):
                    continue
                if (
                    runner_architectures is not None
                    and scale_name.rsplit("-", 1)[1] not in runner_architectures
                ):
                    continue
                for key in [
                    "github_app_id",
                    "github_app_installation_id",
                    "github_app_private_key",
                ]:
                    required.add((item["metadata"]["namespace"], "github-app", key))
                required.add(
                    (item["metadata"]["namespace"], "ci-registry", ".dockerconfigjson")
                )
    if "projects" in layers:
        for path in (root / "platform/projects").glob("*/resources.yaml"):
            for item in yaml.safe_load_all(path.read_text()):
                if item and item["kind"] == "SecretStore":
                    reference = item["spec"]["provider"]["doppler"]["auth"][
                        "secretRef"
                    ]["dopplerToken"]
                    required.add(
                        (
                            item["metadata"]["namespace"],
                            reference["name"],
                            reference["key"],
                        )
                    )
    return required


def host_contract(root: Path) -> dict:
    inventory = json.loads((root / "platform/inventory/nodes.json").read_text())
    active = [node for node in inventory["nodes"] if node["enrollment"] is not None]
    hashes = {
        node["id"]: hashlib.sha256(
            json.dumps(
                {"cluster": inventory["cluster"], "node": node},
                sort_keys=True,
                separators=(",", ":"),
            ).encode()
        ).hexdigest()
        for node in active
    }
    adapter = hashlib.sha256()
    files = [root / "flake.lock"]
    for directory in ["modules/platform", "ansible"]:
        files.extend(
            path
            for path in (root / directory).rglob("*")
            if path.is_file()
            and path.suffix in {".nix", ".py", ".sh", ".yml", ".yaml", ".json"}
            and "__pycache__" not in path.parts
        )
    for path in sorted(files):
        adapter.update(
            str(path.relative_to(root)).encode() + b"\0" + path.read_bytes() + b"\0"
        )
    servers = [
        node["enrollment"]["privateIP"]
        for node in active
        if node["desiredRole"] == "server"
    ]
    service_ip = (
        ipaddress.ip_network(inventory["cluster"]["serviceCIDR"]).network_address + 1
    )
    endpoints = servers + [str(service_ip)]
    ci_architectures = sorted(
        {
            node["architecture"]
            for node in active
            if node["desiredRole"] == "worker"
            and node.get("capabilities", {}).get("verified") is True
            and node["capabilities"].get("ci") is True
        }
    )
    return {
        "inventoryHashes": hashes,
        "adapterHash": adapter.hexdigest(),
        "apiCIDRs": sorted(str(ipaddress.ip_network(address)) for address in endpoints),
        "serverCount": len(set(servers)),
        "ciArchitectures": ci_architectures,
    }


def activation_plan(root: Path, config: dict, *, now: datetime | None = None) -> dict:
    if config.get("schemaVersion") != 1 or set(config) - {
        "schemaVersion",
        "layers",
        "settings",
        "secrets",
        "evidence",
        "runnerRepositories",
        "runnerArchitectures",
    }:
        raise PlatformError("Unsupported activation document or unexpected fields")
    layers = set(config.get("layers", []))
    if not layers or layers - set(DEPENDENCIES):
        raise PlatformError("Explicit known activation layers are required")
    for layer in layers:
        if DEPENDENCIES[layer] - layers:
            raise PlatformError(f"{layer}: dependency layers are missing")
    settings = config.get("settings", {})
    defaults = yaml.safe_load(
        (root / "platform/clusters/production/settings.yaml").read_text()
    )["data"]
    if set(settings) - set(defaults) or any(
        not isinstance(value, str) for value in settings.values()
    ):
        raise PlatformError("Settings must use the declared nonsecret string keys")
    merged = defaults | settings
    host = evidence(config, "hostNetwork", now=now)
    contract = host_contract(root)
    if contract["serverCount"] != 3 or not contract["inventoryHashes"]:
        raise PlatformError("Activation requires three enrolled control planes")
    if (
        host.get("inventoryHashes") != contract["inventoryHashes"]
        or host.get("adapterHash") != contract["adapterHash"]
    ):
        raise PlatformError(
            "Host evidence does not match the enrolled inventory and adapter sources"
        )
    if (
        host.get("k3sVersion")
        != "v"
        + yaml.safe_load((root / "platform/versions.yaml").read_text())["kubernetes"]
        + "+k3s1"
    ):
        raise PlatformError("Host evidence does not match the pinned K3s version")
    if set(host.get("nodeConfigurationHashes", {})) != set(
        contract["inventoryHashes"]
    ) or not all(
        re.fullmatch(r"[0-9a-f]{64}", value)
        for value in host["nodeConfigurationHashes"].values()
    ):
        raise PlatformError("Host evidence must bind each verified node configuration")
    declared = {
        (entry["namespace"], entry["name"], key)
        for entry in config.get("secrets", [])
        for key in entry.get("keys", [])
    }
    missing = (
        secret_contracts(
            root,
            layers,
            config.get("runnerRepositories", []),
            config.get("runnerArchitectures", []),
        )
        - declared
    )
    if missing:
        raise PlatformError(
            "Missing bootstrap secret metadata: "
            + ", ".join("/".join(item) for item in sorted(missing))
        )
    if "ingress" in layers:
        if not re.fullmatch(
            r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}",
            merged["TUNNEL_ID"],
        ):
            raise PlatformError("A concrete Cloudflare tunnel identifier is required")
        evidence(config, "ingressTLS", now=now)
    if "observability" in layers:
        for key in [
            "S3_ENDPOINT",
            "S3_REGION",
            "LOKI_BUCKET",
            "TEMPO_BUCKET",
            "GRAFANA_HOST",
            "HOST_SCRAPE_CIDR",
            "LOKI_CLUSTER_IP",
        ]:
            if not merged[key] or "${" in merged[key]:
                raise PlatformError(f"{key}: a concrete telemetry setting is required")
        if not re.fullmatch(r"[a-zA-Z0-9.-]+(?::[0-9]{1,5})?", merged["S3_ENDPOINT"]):
            raise PlatformError("S3_ENDPOINT must be a TLS hostname with optional port")
        ipaddress.ip_network(merged["HOST_SCRAPE_CIDR"])
        ipaddress.ip_address(merged["LOKI_CLUSTER_IP"])
        if merged["LOKI_BUCKET"] == merged["TEMPO_BUCKET"]:
            raise PlatformError("Loki and Tempo require separate buckets")
        evidence(config, "telemetryStorage", now=now)
    if "runners" in layers:
        if not CI_IMAGE.fullmatch(merged["CI_IMAGE"]):
            raise PlatformError(
                "CI_IMAGE must be the approved infrastructure image digest"
            )
        network = ipaddress.ip_network(merged["KUBERNETES_API_CIDR"])
        if str(network) not in contract["apiCIDRs"]:
            raise PlatformError(
                "The runner API route must be an exact enrolled API endpoint"
            )
        selected_repositories = config.get("runnerRepositories", [])
        selected_architectures = config.get("runnerArchitectures", [])
        if (
            "amd64" not in selected_architectures
            or len(set(selected_architectures)) != len(selected_architectures)
            or not set(selected_architectures) <= set(contract["ciArchitectures"])
        ):
            raise PlatformError(
                "Runner architectures require explicitly verified enrolled workers"
            )
        catalog = json.loads((root / "platform/catalog/projects.json").read_text())
        allowed_repositories = {
            project["repositoryId"] for project in catalog["projects"].values()
        }
        if (
            not selected_repositories
            or any(type(item) is not int for item in selected_repositories)
            or len(set(selected_repositories)) != len(selected_repositories)
            or not set(selected_repositories) <= allowed_repositories
        ):
            raise PlatformError(
                "An explicit approved runner repository selection is required"
            )
        if len(selected_repositories) > 1:
            capacity = evidence(config, "aggregateCapacity", now=now)
            if (
                capacity.get("repositoryIds") != sorted(selected_repositories)
                or capacity.get("architectures") != sorted(selected_architectures)
                or capacity.get("maxConcurrentJobs")
                != len(selected_repositories) * len(selected_architectures)
                or capacity.get("nodeConfigurationHashes")
                != host["nodeConfigurationHashes"]
                or any(
                    capacity.get("passed", {}).get(test) is not True
                    for test in ["productionContention", "diskPressure", "nodeLoss"]
                )
            ):
                raise PlatformError(
                    "Multiple repositories require matching aggregate capacity evidence"
                )
        pilot = evidence(config, "ciSandbox", now=now)
        if (
            pilot.get("image") != merged["CI_IMAGE"]
            or pilot.get("nodeConfigurationHashes") != host["nodeConfigurationHashes"]
        ):
            raise PlatformError(
                "CI evidence must bind the approved image and current host configurations"
            )
        for test in selected_architectures + [
            "buildkitProcessSandbox",
            "nixSandbox",
            "noApiToken",
            "productionNetworkDenied",
            "privilegedPodDenied",
        ]:
            if pilot.get("passed", {}).get(test) is not True:
                raise PlatformError(f"CI pilot has not passed {test}")
    if "cache" in layers:
        if not DIGEST.fullmatch(merged["ATTIC_IMAGE"]):
            raise PlatformError("Attic requires a reviewed immutable image")
        pilot = evidence(config, "cacheRecovery", now=now)
        if (
            pilot.get("image") != merged["ATTIC_IMAGE"]
            or not pilot.get("privateCacheVerified")
            or not pilot.get("signingKeyRecoveryVerified")
        ):
            raise PlatformError(
                "Cache authentication and signing-key recovery must pass"
            )
    if "backups" in layers:
        evidence(config, "alternateProviderRestore", now=now)
    merged["PLATFORM_ACTIVATED"] = "true"
    patches = [
        {
            "apiVersion": "kustomize.toolkit.fluxcd.io/v1",
            "kind": "Kustomization",
            "metadata": {"name": "platform", "namespace": "flux-system"},
            "spec": {"suspend": False},
        }
    ]
    for layer in sorted(layers):
        patches.append(
            {
                "apiVersion": "kustomize.toolkit.fluxcd.io/v1",
                "kind": "Kustomization",
                "metadata": {"name": f"platform-{layer}", "namespace": "flux-system"},
                "spec": {"suspend": False},
            }
        )
    if "projects" in layers:
        handoff = evidence(config, "sourceHandoff", now=now)
        if handoff.get("branch") != "deploy" or not re.fullmatch(
            r"[0-9a-f]{40}", handoff.get("verifiedRevision", "")
        ):
            raise PlatformError(
                "Source handoff requires the validated deploy branch revision"
            )
        patches.append(
            {
                "apiVersion": "source.toolkit.fluxcd.io/v1",
                "kind": "GitRepository",
                "metadata": {"name": "infra", "namespace": "flux-system"},
                "spec": {"ref": {"commit": None, "branch": "deploy"}},
            }
        )
    if "runners" in layers:
        for _, item in documents(root):
            if (
                item["kind"] == "HelmRelease"
                and item["spec"]["chart"]["spec"]["chart"] == "gha-runner-scale-set"
            ):
                scale_name = item["spec"]["values"]["runnerScaleSetName"]
                if (
                    int(scale_name.split("-")[1]) in selected_repositories
                    and scale_name.rsplit("-", 1)[1] in selected_architectures
                ):
                    patches.append(
                        {
                            "apiVersion": item["apiVersion"],
                            "kind": item["kind"],
                            "metadata": item["metadata"],
                            "spec": {"suspend": False, "values": {"maxRunners": 1}},
                        }
                    )
                    patches.append(
                        {
                            "apiVersion": "networking.k8s.io/v1",
                            "kind": "NetworkPolicy",
                            "metadata": {
                                "name": "runner-api",
                                "namespace": item["metadata"]["namespace"],
                            },
                            "spec": {
                                "egress": [
                                    {
                                        "to": [
                                            {"ipBlock": {"cidr": cidr}}
                                            for cidr in contract["apiCIDRs"]
                                        ],
                                        "ports": [
                                            {"protocol": "TCP", "port": 443},
                                            {"protocol": "TCP", "port": 6443},
                                        ],
                                    }
                                ]
                            },
                        }
                    )
    if "cache" in layers:
        patches.append(
            {
                "apiVersion": "apps/v1",
                "kind": "StatefulSet",
                "metadata": {"name": "attic", "namespace": "nix-cache"},
                "spec": {"replicas": 1},
            }
        )
    if "backups" in layers:
        patches.append(
            {
                "apiVersion": "batch/v1",
                "kind": "CronJob",
                "metadata": {
                    "name": "repository-integrity",
                    "namespace": "platform-backups",
                },
                "spec": {"suspend": False},
            }
        )
    variables = (
        {
            "INFRA_CI_IMAGE": merged["CI_IMAGE"],
            "INFRA_CI_ARCHITECTURES": json.dumps(
                selected_architectures, separators=(",", ":")
            ),
        }
        if "runners" in layers
        else {}
    )
    return {
        "schemaVersion": 1,
        "kind": "review-only-platform-activation",
        "settings": merged,
        "patches": patches,
        "githubVariables": variables,
        "evidence": config.get("evidence", {}),
        "notes": [
            "Apply patches in the corresponding Flux-owned overlays; direct cluster patches are reverted by reconciliation",
            "Install actual namespace-scoped secrets separately; this file contains metadata only",
            "Set GitHub variables only on the selected repository registrations after the pilot is approved",
            "Project database backups require their own reviewed credentials and restore evidence before unsuspending",
            "Revalidate host and sandbox evidence after OS, kernel, runtime, network or pipeline-image changes",
        ],
    }


def bootstrap_plan(root: Path, repository: str, revision: str) -> dict:
    expected = json.loads((root / "platform/catalog/projects.json").read_text())[
        "infrastructureRepository"
    ]
    if repository != f"https://github.com/{expected}.git" or not re.fullmatch(
        r"[0-9a-f]{40}", revision
    ):
        raise PlatformError(
            "Bootstrap requires the catalogued infrastructure URL and a full commit SHA"
        )
    version = yaml.safe_load((root / "platform/versions.yaml").read_text())["flux"]
    return {
        "schemaVersion": 1,
        "kind": "review-only-bootstrap",
        "commands": [
            [
                "flux",
                "install",
                "--export",
                f"--version=v{version}",
                "--namespace=flux-system",
                "--watch-all-namespaces=true",
                "--network-policy=true",
            ]
        ],
        "resources": [
            {
                "apiVersion": "source.toolkit.fluxcd.io/v1",
                "kind": "GitRepository",
                "metadata": {"name": "infra", "namespace": "flux-system"},
                "spec": {
                    "interval": "1m",
                    "url": repository,
                    "ref": {"commit": revision},
                    "secretRef": {"name": "infra-git-readonly"},
                },
            },
            {
                "apiVersion": "kustomize.toolkit.fluxcd.io/v1",
                "kind": "Kustomization",
                "metadata": {"name": "platform", "namespace": "flux-system"},
                "spec": {
                    "interval": "10m",
                    "prune": True,
                    "suspend": True,
                    "sourceRef": {"kind": "GitRepository", "name": "infra"},
                    "path": "./platform/clusters/production",
                },
            },
        ],
        "secretMetadata": [
            {
                "namespace": "flux-system",
                "name": "infra-git-readonly",
                "keys": ["username", "password"],
            },
            {"namespace": "flux-system", "name": "sops-age", "keys": ["age.agekey"]},
        ],
        "notes": [
            "Read-only Git credential must be scoped to the infrastructure repository",
            "Exported Flux controllers must be reviewed and schema validated before cluster installation",
            "Keep encrypted bootstrap recovery material outside this cluster and repository",
            "Cross-namespace source references are required for the centrally owned infra source; project writes remain scoped by reconciliation service accounts",
        ],
    }


def main(argv=None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", type=Path, default=ROOT)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("check")
    sub.add_parser("host-contract")
    activate = sub.add_parser("activation-plan")
    activate.add_argument("config", type=Path)
    bootstrap = sub.add_parser("bootstrap-plan")
    bootstrap.add_argument("--repository", required=True)
    bootstrap.add_argument("--revision", required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == "check":
            errors = check(args.root)
            if errors:
                raise PlatformError("; ".join(errors))
            print(
                "Platform catalog, version, source, runner and activation contracts passed"
            )
        elif args.command == "host-contract":
            print(json.dumps(host_contract(args.root), indent=2))
        elif args.command == "activation-plan":
            print(
                yaml.safe_dump(
                    activation_plan(args.root, json.loads(args.config.read_text())),
                    sort_keys=False,
                )
            )
        else:
            print(
                yaml.safe_dump(
                    bootstrap_plan(args.root, args.repository, args.revision),
                    sort_keys=False,
                )
            )
        return 0
    except (PlatformError, KeyError, OSError, ValueError) as exc:
        print(f"Platform operation refused: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
