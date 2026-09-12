import copy
import importlib.util
from pathlib import Path

import yaml

from .contracts import ContractError, load_document, validate_project, validate_release
from .resources import RESOURCE_CLASSES, VOLUME_CLASSES, project_quota


def resource(kind, name, namespace=None, api="v1", **fields):
    metadata = {"name": name}
    if namespace:
        metadata["namespace"] = namespace
    return {"apiVersion": api, "kind": kind, "metadata": metadata, **fields}


def namespace_resources(project, quota):
    namespace = project["namespace"]
    ns = resource("Namespace", namespace)
    ns["metadata"]["labels"] = {
        "pod-security.kubernetes.io/enforce": "restricted",
        "pod-security.kubernetes.io/enforce-version": "v1.36",
        "pod-security.kubernetes.io/audit": "restricted",
        "pod-security.kubernetes.io/warn": "restricted",
        "infra.fredrir.com/tier": "project",
        "infra.fredrir.com/telemetry": "true",
    }
    sa = resource(
        "ServiceAccount",
        "project-reconciler",
        namespace,
        automountServiceAccountToken=False,
    )
    role = resource(
        "Role",
        "project-reconciler",
        namespace,
        "rbac.authorization.k8s.io/v1",
        rules=[
            {
                "apiGroups": [""],
                "resources": [
                    "configmaps",
                    "secrets",
                    "services",
                    "serviceaccounts",
                    "persistentvolumeclaims",
                ],
                "verbs": [
                    "get",
                    "list",
                    "watch",
                    "create",
                    "update",
                    "patch",
                    "delete",
                ],
            },
            {
                "apiGroups": ["apps"],
                "resources": ["deployments"],
                "verbs": [
                    "get",
                    "list",
                    "watch",
                    "create",
                    "update",
                    "patch",
                    "delete",
                ],
            },
            {
                "apiGroups": ["batch"],
                "resources": ["jobs", "cronjobs"],
                "verbs": [
                    "get",
                    "list",
                    "watch",
                    "create",
                    "update",
                    "patch",
                    "delete",
                ],
            },
            {
                "apiGroups": ["networking.k8s.io"],
                "resources": ["ingresses", "networkpolicies"],
                "verbs": [
                    "get",
                    "list",
                    "watch",
                    "create",
                    "update",
                    "patch",
                    "delete",
                ],
            },
            {
                "apiGroups": ["policy"],
                "resources": ["poddisruptionbudgets"],
                "verbs": [
                    "get",
                    "list",
                    "watch",
                    "create",
                    "update",
                    "patch",
                    "delete",
                ],
            },
            {
                "apiGroups": [""],
                "resources": ["pods", "events"],
                "verbs": ["get", "list", "watch"],
            },
        ],
    )
    binding = resource(
        "RoleBinding",
        "project-reconciler",
        namespace,
        "rbac.authorization.k8s.io/v1",
        roleRef={
            "apiGroup": "rbac.authorization.k8s.io",
            "kind": "Role",
            "name": "project-reconciler",
        },
        subjects=[
            {
                "kind": "ServiceAccount",
                "name": "project-reconciler",
                "namespace": namespace,
            }
        ],
    )
    data_reconciler = resource(
        "ServiceAccount",
        "project-data-reconciler",
        namespace,
        automountServiceAccountToken=False,
    )
    data_role = resource(
        "Role",
        "project-data-reconciler",
        namespace,
        "rbac.authorization.k8s.io/v1",
        rules=[
            {
                "apiGroups": [""],
                "resources": ["secrets", "services", "persistentvolumeclaims"],
                "verbs": [
                    "get",
                    "list",
                    "watch",
                    "create",
                    "update",
                    "patch",
                    "delete",
                ],
            },
            {
                "apiGroups": ["apps"],
                "resources": ["statefulsets"],
                "verbs": [
                    "get",
                    "list",
                    "watch",
                    "create",
                    "update",
                    "patch",
                    "delete",
                ],
            },
            {
                "apiGroups": ["networking.k8s.io"],
                "resources": ["networkpolicies"],
                "verbs": [
                    "get",
                    "list",
                    "watch",
                    "create",
                    "update",
                    "patch",
                    "delete",
                ],
            },
            {
                "apiGroups": [""],
                "resources": ["pods", "events"],
                "verbs": ["get", "list", "watch"],
            },
        ],
    )
    data_binding = resource(
        "RoleBinding",
        "project-data-reconciler",
        namespace,
        "rbac.authorization.k8s.io/v1",
        roleRef={
            "apiGroup": "rbac.authorization.k8s.io",
            "kind": "Role",
            "name": "project-data-reconciler",
        },
        subjects=[
            {
                "kind": "ServiceAccount",
                "name": "project-data-reconciler",
                "namespace": namespace,
            }
        ],
    )
    data_runtime = resource(
        "ServiceAccount", "project-data", namespace, automountServiceAccountToken=False
    )
    migration_runtime = resource(
        "ServiceAccount",
        "project-migration",
        namespace,
        automountServiceAccountToken=False,
    )
    public_headers = resource(
        "Middleware",
        "project-public-headers",
        namespace,
        "traefik.io/v1alpha1",
        spec={"headers": {"customRequestHeaders": {"X-Admin-Origin": ""}}},
    )
    quota = resource("ResourceQuota", "project", namespace, spec={"hard": quota})
    deny = resource(
        "NetworkPolicy",
        "default-deny",
        namespace,
        "networking.k8s.io/v1",
        spec={"podSelector": {}, "policyTypes": ["Ingress", "Egress"]},
    )
    dns = resource(
        "NetworkPolicy",
        "dns",
        namespace,
        "networking.k8s.io/v1",
        spec={
            "podSelector": {},
            "policyTypes": ["Egress"],
            "egress": [
                {
                    "to": [
                        {
                            "namespaceSelector": {
                                "matchLabels": {
                                    "kubernetes.io/metadata.name": "kube-system"
                                }
                            },
                            "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}},
                        }
                    ],
                    "ports": [
                        {"protocol": "UDP", "port": 53},
                        {"protocol": "TCP", "port": 53},
                    ],
                }
            ],
        },
    )
    telemetry = resource(
        "NetworkPolicy",
        "telemetry",
        namespace,
        "networking.k8s.io/v1",
        spec={
            "podSelector": {},
            "policyTypes": ["Egress"],
            "egress": [
                {
                    "to": [
                        {
                            "namespaceSelector": {
                                "matchLabels": {
                                    "kubernetes.io/metadata.name": "observability"
                                }
                            },
                            "podSelector": {
                                "matchLabels": {"app.kubernetes.io/name": "alloy"}
                            },
                        }
                    ],
                    "ports": [
                        {"protocol": "TCP", "port": 4317},
                        {"protocol": "TCP", "port": 4318},
                    ],
                }
            ],
        },
    )
    store = resource(
        "SecretStore",
        "runtime",
        namespace,
        "external-secrets.io/v1",
        spec={
            "provider": {
                "doppler": {
                    "auth": {
                        "secretRef": {
                            "dopplerToken": {
                                "name": "doppler-token",
                                "key": "dopplerToken",
                            }
                        }
                    }
                }
            }
        },
    )
    registry = resource(
        "ExternalSecret",
        "project-registry",
        namespace,
        "external-secrets.io/v1",
        spec={
            "refreshInterval": "1h",
            "secretStoreRef": {"kind": "SecretStore", "name": "runtime"},
            "target": {
                "name": "project-registry",
                "creationPolicy": "Owner",
                "template": {"type": "kubernetes.io/dockerconfigjson"},
            },
            "data": [
                {
                    "secretKey": ".dockerconfigjson",
                    "remoteRef": {"key": "GHCR_DOCKER_CONFIG_JSON"},
                }
            ],
        },
    )
    return [
        ns,
        sa,
        role,
        binding,
        data_reconciler,
        data_role,
        data_binding,
        data_runtime,
        migration_runtime,
        public_headers,
        quota,
        deny,
        dns,
        telemetry,
        store,
        registry,
    ]


def chart_values(root, document, release=None, releases=None):
    project = validate_project(root, document)
    releases = dict(releases or {})
    if release:
        releases[release.get("component", "app")] = release
    for component, item in releases.items():
        validate_release(root, item, document["project"])
        if component != item.get("component", "app"):
            raise ContractError("release component mismatch")
    required_components = {
        value.get("component", "app") for value in document["workloads"].values()
    }
    if document.get("migration"):
        required_components.add(document["migration"]["component"])
    selected_releases = [
        item for name, item in releases.items() if name in required_components
    ]
    identities = {
        (
            item["sourceRevision"],
            item["provenance"]["runId"],
            item["provenance"]["workflowRevision"],
        )
        for item in selected_releases
    }
    if len(identities) > 1:
        raise ContractError(
            "component releases must come from the same source and workflow run"
        )
    workloads = {}
    for name, definition in document["workloads"].items():
        component = definition.get("component", "app")
        if component not in releases:
            raise ContractError(f"{name}: component release required: {component}")
        component_release = releases[component]
        platforms = {
            p.removeprefix("linux/")
            for p in component_release["provenance"]["platforms"]
        }
        architecture = definition.get("architecture", "amd64")
        required = {"amd64", "arm64"} if architecture == "multi" else {architecture}
        if not required.issubset(platforms):
            raise ContractError(f"{name}: release lacks required architecture")
        workload = dict(definition)
        workload.pop("component", None)
        workload["image"] = component_release["image"]
        workload["sourceRevision"] = component_release["sourceRevision"]
        workload["resources"] = copy.deepcopy(
            RESOURCE_CLASSES[workload.pop("resourceClass", "small")]
        )
        workload["architectures"] = sorted(required)
        workload.pop("architecture", None)
        workload.setdefault(
            "replicas",
            2
            if workload["kind"] == "web"
            and not (workload.get("volume") or workload.get("sharedVolume"))
            else 1,
        )
        if workload.get("volume"):
            workload["volume"] = {
                "mountPath": workload["volume"]["mountPath"],
                "size": VOLUME_CLASSES[workload["volume"]["sizeClass"]],
            }
        workloads[name] = workload
    for volume in document.get("sharedVolumes", {}):
        consumers = [
            workload
            for workload in workloads.values()
            if workload.get("sharedVolume", {}).get("name") == volume
        ]
        architectures = sorted(
            set.intersection(
                *(set(workload["architectures"]) for workload in consumers)
            )
        )
        for workload in consumers:
            workload["architectures"] = architectures
    data_images = load_document(Path(root) / "platform/catalog/data-images.json")
    data = {
        name: {
            "size": VOLUME_CLASSES[config["sizeClass"]],
            "image": data_images[name]["image"],
            "recovery": config["recovery"],
        }
        for name, config in document.get("data", {}).items()
    }
    shared_volumes = {
        name: {"size": VOLUME_CLASSES[value["sizeClass"]]}
        for name, value in document.get("sharedVolumes", {}).items()
    }
    values = {
        "project": document["project"],
        "workloads": workloads,
        "data": data,
        "sharedVolumes": shared_volumes,
    }
    if document.get("migration"):
        migration = copy.deepcopy(document["migration"])
        component = migration.pop("component")
        if component not in releases:
            raise ContractError(f"migration component release required: {component}")
        component_release = releases[component]
        migration.update(
            kind="cron",
            image=component_release["image"],
            sourceRevision=component_release["sourceRevision"],
            resources=copy.deepcopy(
                RESOURCE_CLASSES[migration.pop("resourceClass", "small")]
            ),
            architectures=sorted(
                platform.removeprefix("linux/")
                for platform in component_release["provenance"]["platforms"]
            ),
            serviceAccountName="project-migration",
        )
        values["migration"] = migration
    return values


def render_project(root, document, release=None, releases=None):
    project = validate_project(root, document)
    objects = namespace_resources(project, project_quota(root, project))
    secret_keys = sorted(
        {
            key
            for workload in document["workloads"].values()
            for key in workload.get("secretKeys", [])
        }
    )
    secret_keys += document.get("migration", {}).get("secretKeys", [])
    if "postgres" in document.get("data", {}):
        secret_keys += ["POSTGRES_PASSWORD", "POSTGRES_USER", "POSTGRES_DB"]
    if "valkey" in document.get("data", {}):
        secret_keys += ["VALKEY_PASSWORD"]
    if secret_keys:
        objects.append(
            resource(
                "ExternalSecret",
                "project-runtime",
                project["namespace"],
                "external-secrets.io/v1",
                spec={
                    "refreshInterval": "1h",
                    "secretStoreRef": {"kind": "SecretStore", "name": "runtime"},
                    "target": {"name": "project-runtime", "creationPolicy": "Owner"},
                    "data": [
                        {"secretKey": key, "remoteRef": {"key": key}}
                        for key in sorted(set(secret_keys))
                    ],
                },
            )
        )
    if document.get("data"):
        path = Path(root) / "scripts/operations/backup_jobs.py"
        spec = importlib.util.spec_from_file_location("infra_backup_jobs", path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        objects.extend(module.generate(Path(root), document))
    if release or releases:
        values = chart_values(root, document, release, releases)
        data = values.pop("data")
        if data:
            objects.append(
                resource(
                    "HelmRelease",
                    document["project"] + "-data",
                    project["namespace"],
                    "helm.toolkit.fluxcd.io/v2",
                    spec={
                        "interval": "5m",
                        "timeout": "10m",
                        "serviceAccountName": "project-data-reconciler",
                        "releaseName": document["project"] + "-data",
                        "chart": {
                            "spec": {
                                "chart": "./charts/project-data",
                                "reconcileStrategy": "Revision",
                                "sourceRef": {
                                    "kind": "GitRepository",
                                    "name": "infra",
                                    "namespace": "flux-system",
                                },
                            }
                        },
                        "install": {
                            "strategy": {
                                "name": "RetryOnFailure",
                                "retryInterval": "5m",
                            }
                        },
                        "upgrade": {
                            "strategy": {
                                "name": "RetryOnFailure",
                                "retryInterval": "5m",
                            }
                        },
                        "values": {"project": document["project"], "data": data},
                    },
                )
            )
        application = resource(
            "HelmRelease",
            document["project"],
            project["namespace"],
            "helm.toolkit.fluxcd.io/v2",
            spec={
                "interval": "5m",
                "timeout": "10m",
                "serviceAccountName": "project-reconciler",
                "releaseName": document["project"],
                "chart": {
                    "spec": {
                        "chart": "./charts/project",
                        "reconcileStrategy": "Revision",
                        "sourceRef": {
                            "kind": "GitRepository",
                            "name": "infra",
                            "namespace": "flux-system",
                        },
                    }
                },
                "install": {
                    "strategy": {"name": "RemediateOnFailure"},
                    "remediation": {"retries": 1},
                },
                "upgrade": {
                    "strategy": {"name": "RemediateOnFailure"},
                    "remediation": {"retries": 1, "strategy": "rollback"},
                },
                "rollback": {"disableHooks": True},
                "values": values,
            },
        )
        if data:
            application["spec"]["dependsOn"] = [{"name": document["project"] + "-data"}]
        objects.append(application)
    return objects


def write_manifests(path, objects):
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    Path(path).write_text(yaml.safe_dump_all(objects, sort_keys=False))


def render_all(root, output):
    root, output = Path(root), Path(output)
    entries = []
    for project_file in sorted((root / "platform/projects").glob("*/project.yaml")):
        document = load_document(project_file)
        if project_file.parent.name != document.get("project"):
            raise ContractError("project directory mismatch")
        release_file = project_file.with_name("release.json")
        release = load_document(release_file) if release_file.exists() else None
        releases = {
            path.stem: load_document(path)
            for path in sorted((project_file.parent / "releases").glob("*.json"))
        }
        if release and release.get("component", "app") in releases:
            raise ContractError("duplicate component release")
        destination = Path(document["project"]) / "resources.yaml"
        write_manifests(
            output / destination, render_project(root, document, release, releases)
        )
        entries.append(destination.as_posix())
    (output / "kustomization.yaml").parent.mkdir(parents=True, exist_ok=True)
    (output / "kustomization.yaml").write_text(
        yaml.safe_dump(
            {
                "apiVersion": "kustomize.config.k8s.io/v1beta1",
                "kind": "Kustomization",
                "resources": entries,
            },
            sort_keys=False,
        )
    )
    return entries
