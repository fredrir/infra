import argparse
import json
import re
import secrets
import subprocess
import tempfile
from pathlib import Path

import yaml

OWNER_ID = 114402558
WORKFLOW = "fredrir/infra/.github/workflows/build-image.yml"
ROOT = Path(__file__).resolve().parents[2]
RUNNERS = Path("platform/components/runners")
CACHE_PROJECTS = Path("platform/components/build-cache/projects")
REGISTRY = Path(".github/rust-projects.yaml")
SHARED_WORKFLOW = r"^fredrir/infra/\.github/workflows/{}\.yml@[0-9a-f]{{40}}$"
APP_CREDENTIALS = {
    "github_app_id": "ARC_GITHUB_APP_ID",
    "github_app_installation_id": "ARC_GITHUB_APP_INSTALLATION_ID",
    "github_app_private_key": "ARC_GITHUB_APP_PRIVATE_KEY",
}


def checked(value, pattern, label):
    if not re.fullmatch(pattern, value):
        raise ValueError(f"Invalid {label}")
    return value


def repository(value):
    checked(value, r"fredrir/[A-Za-z0-9_.-]+", "repository")
    result = subprocess.run(["gh", "api", f"repos/{value}"], check=True, capture_output=True, text=True)
    identity = json.loads(result.stdout)
    if identity["owner"]["id"] != OWNER_ID or identity["full_name"].lower() != value.lower():
        raise ValueError("Repository identity does not match")
    return identity


def immutable_image(value):
    return checked(value, r"ghcr\.io/fredrir/[a-z0-9][a-z0-9._/-]*@sha256:[a-f0-9]{64}", "image")


def revision(value):
    return checked(value, r"[a-f0-9]{40}", "revision")


def onboarding(args):
    project = checked(args.project, r"[a-z][a-z0-9-]{0,29}", "project")
    immutable_image(args.image)
    revision(args.source_revision)
    revision(args.workflow_ref)
    checked(args.domain, r"[a-z0-9][a-z0-9-]*\.fredrir\.com", "shared gateway domain")
    checked(args.health_path, r"/[^\s]{0,255}", "health path")
    if not 1024 <= args.port <= 65535:
        raise ValueError("Port must be between 1024 and 65535")
    if args.architecture != "amd64":
        raise ValueError("Native ARM runners and deployment images are not qualified")
    output = Path(args.output)
    if output.exists():
        raise ValueError("Onboarding output must be a fresh directory")
    identity = repository(args.repository)
    namespace = f"project-{project}"
    workload = {
        "kind": "web", "replicas": 0, "image": args.image,
        "sourceRevision": args.source_revision, "architectures": ["amd64"],
        "port": args.port, "healthPath": args.health_path, "domains": [args.domain],
        "resources": {
            "requests": {"cpu": "100m", "memory": "128Mi", "ephemeral-storage": "256Mi"},
            "limits": {"cpu": "500m", "memory": "512Mi", "ephemeral-storage": "1Gi"},
        },
    }
    release = {
        "apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
        "metadata": {"name": project, "namespace": namespace},
        "spec": {
            "interval": "10m", "releaseName": project,
            "chart": {"spec": {
                "chart": "./charts/project", "reconcileStrategy": "Revision",
                "sourceRef": {"kind": "GitRepository", "name": "flux-system", "namespace": "flux-system"},
            }},
            "values": {"project": project, "workloads": {"web": workload}},
        },
    }
    resources = [{
        "apiVersion": "v1", "kind": "Namespace", "metadata": {
            "name": namespace, "labels": {
                "infra.fredrir.com/tier": "project", "infra.fredrir.com/managed": "true",
                "pod-security.kubernetes.io/enforce": "restricted",
                "pod-security.kubernetes.io/enforce-version": "v1.36",
            },
        },
    }]
    for name, egress in [("default-deny", None), ("dns", [{
        "to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}},
                "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}}],
        "ports": [{"port": 53, "protocol": protocol} for protocol in ["UDP", "TCP"]],
    }])]:
        spec = {"podSelector": {}, "policyTypes": ["Ingress", "Egress"] if egress is None else ["Egress"]}
        if egress is not None:
            spec["egress"] = egress
        resources.append({"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
                          "metadata": {"name": name, "namespace": namespace}, "spec": spec})
    caller = {
        "name": "Build", "on": {"push": {"branches": ["main"]}},
        "permissions": {"contents": "read", "packages": "write", "id-token": "write", "attestations": "write"},
        "jobs": {"build": {
            "if": f"github.event_name == 'push' && github.ref_protected && github.repository_id == '{identity['id']}' && github.repository_owner_id == '{OWNER_ID}'",
            "uses": f"{WORKFLOW}@{args.workflow_ref}",
            "with": {"image": args.image.split("@")[0], "test-command": args.test_command},
        }},
    }
    output.mkdir(parents=True)
    (output / "project").mkdir()
    (output / "runner").mkdir()
    (output / "infrastructure/.github/deployments").mkdir(parents=True)
    (output / "infrastructure/.github/chainguard").mkdir(parents=True)
    (output / "caller/.github/workflows").mkdir(parents=True)
    files = {
        "project/baseline.yaml": yaml.safe_dump_all(resources, sort_keys=False),
        "project/release.yaml": yaml.safe_dump(release, sort_keys=False),
        "project/kustomization.yaml": yaml.safe_dump({"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "namespace": namespace, "resources": ["baseline.yaml", "release.yaml"]}, sort_keys=False),
        "caller/.github/workflows/build.yaml": yaml.safe_dump(caller, sort_keys=False),
        f"infrastructure/.github/deployments/{identity['id']}.yaml": yaml.safe_dump({
            "repository": identity["full_name"], "visibility": "private" if identity.get("private", False) else "public",
            "images": {args.image.split("@")[0]: {
                "path": f"platform/projects/{project}", "mode": "helmrelease", "workload": "web",
            }},
        }, sort_keys=False),
        f"infrastructure/.github/chainguard/deploy-{identity['id']}.sts.yaml": yaml.safe_dump({
            "issuer": "https://token.actions.githubusercontent.com",
            "subject_pattern": f"^repo:fredrir(@{OWNER_ID})?/{re.escape(identity['full_name'].split('/')[1])}(@{identity['id']})?:ref:refs/heads/main$",
            "claim_pattern": {
                "repository_id": f"^{identity['id']}$", "repository_owner_id": f"^{OWNER_ID}$",
                "event_name": "^push$", "ref": "^refs/heads/main$", "runner_environment": "^self-hosted$",
                "job_workflow_ref": "^" + re.escape(WORKFLOW + "@" + args.workflow_ref) + "$",
                "job_workflow_sha": "^" + args.workflow_ref + "$",
            }, "permissions": {"actions": "write"},
        }, sort_keys=False),
        "runner/kustomization.yaml": yaml.safe_dump({
            "apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization",
            "namespace": f"ci-{project}", "resources": ["../base"],
            "patches": [{"target": {"kind": "HelmRelease", "name": "buildkit"},
                         "patch": yaml.safe_dump({"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
                                                 "metadata": {"name": "buildkit"}, "spec": {"values": {
                                                     "githubConfigUrl": "https://github.com/" + identity["full_name"],
                                                 }}}, sort_keys=False)}],
        }, sort_keys=False),
    }
    for path, content in files.items():
        (output / path).write_text(content)
    print(f"Created {output}; add encrypted project-registry/project-runtime Secrets before starting workloads")


def platform_recipients(root):
    found = {tuple(sorted(re.findall(r"recipient: (age1[0-9a-z]+)", path.read_text())))
             for path in (root / "platform").rglob("*.secret.sops.yaml")}
    if len(found) != 1:
        raise ValueError("Platform secrets disagree on their age recipients")
    return list(found.pop())


def app_credentials():
    result = subprocess.run(["doppler", "secrets", "get", *APP_CREDENTIALS.values(), "--project", "infra",
                             "--config", "ops", "--json"], check=True, capture_output=True, text=True)
    values = json.loads(result.stdout)
    return {key: values[name]["computed"] for key, name in APP_CREDENTIALS.items()}


def garage_key():
    return {"id": "GK" + secrets.token_hex(12), "secret": secrets.token_hex(32)}


def encrypt(document, destination, recipients):
    with tempfile.TemporaryDirectory() as directory:
        plaintext = Path(directory) / destination.name
        plaintext.touch(mode=0o600)
        plaintext.write_text(yaml.safe_dump(document, sort_keys=False))
        subprocess.run(["sops", "encrypt", "--encrypted-regex", "^(data|stringData)$", "--age", ",".join(recipients),
                        "--input-type", "yaml", "--output-type", "yaml", "--output", str(destination.resolve()), str(plaintext)],
                       check=True, capture_output=True, cwd=directory)


def secret(name, namespace, data, labels=None):
    metadata = {"name": name, "namespace": namespace} | ({"labels": labels} if labels else {})
    return {"apiVersion": "v1", "kind": "Secret", "metadata": metadata, "type": "Opaque", "stringData": data}


def append_entry(kustomization, field, entry):
    text = kustomization.read_text()
    if entry in (yaml.safe_load(text).get(field) or []):
        raise ValueError(f"{entry} is already listed in {kustomization}")
    line = yaml.safe_dump([entry])
    if re.search(rf"^{field}: \[\]$", text, re.M):
        text = re.sub(rf"^{field}: \[\]$", f"{field}:\n" + line.rstrip("\n"), text, count=1, flags=re.M)
    elif re.search(rf"^{field}:\n(- .*\n)*", text, re.M):
        text = re.sub(rf"^({field}:\n(?:- .*\n)*)", lambda m: m.group(1) + line, text, count=1, flags=re.M)
    elif field == "components" and re.search(r"^resources:\n(- .*\n)*", text, re.M):
        text = re.sub(r"^(resources:\n(?:- .*\n)*)", lambda m: m.group(1) + "components:\n" + line, text, count=1, flags=re.M)
    else:
        raise ValueError(f"{kustomization} has no top-level {field} list")
    kustomization.write_text(text)


def append_resource(kustomization, entry):
    append_entry(kustomization, "resources", entry)


def cache_pools():
    return {"ro": garage_key(), "rw": garage_key(), "release": garage_key()}


def runner_credentials(key):
    return {"AWS_ACCESS_KEY_ID": key["id"], "AWS_SECRET_ACCESS_KEY": key["secret"]}


def encrypt_provisioner_keys(project, pools, destination, recipients):
    encrypt(secret(f"build-cache-{project}", "build-cache",
                   {f"{pool}_{field}": key[field] for pool, key in pools.items() for field in ["id", "secret"]},
                   {"infra.fredrir.com/build-cache-project": project}), destination, recipients)


def trust_policy(identity, subject, claims, permissions):
    name = re.escape(identity["full_name"].split("/")[1])
    return {
        "issuer": "https://token.actions.githubusercontent.com",
        "subject_pattern": f"^repo:fredrir(@{OWNER_ID})?/{name}(@{identity['id']})?:{subject}$",
        "claim_pattern": {"repository_id": f"^{identity['id']}$", "repository_owner_id": f"^{OWNER_ID}$",
                          "runner_environment": "^self-hosted$"} | claims,
        "permissions": permissions,
    }


def rust_callers(identity, reference):
    uses = f"fredrir/infra/.github/workflows/{{}}.yml@{reference}"
    files = {"project/.github/workflows/ci.yml": {
        "name": "CI", "on": {"push": {"branches": ["main"]}, "pull_request": {}},
        "permissions": {"contents": "read"},
        "concurrency": {"group": "ci-${{ github.ref }}", "cancel-in-progress": "${{ github.event_name == 'pull_request' }}"},
        "jobs": {"rust": {"uses": uses.format("rust-ci")}},
    }}
    if identity.get("private", False):
        return files
    return files | {
        "project/.github/workflows/auto-tag.yml": {
            "name": "Tag release",
            "on": {"push": {"branches": ["main"], "paths-ignore": [".github/**", "**.md", "cliff.toml"]}, "workflow_dispatch": {}},
            "permissions": {"contents": "read", "id-token": "write"},
            "concurrency": {"group": "auto-tag", "cancel-in-progress": False},
            "jobs": {"tag": {"uses": uses.format("rust-auto-tag")}},
        },
        "project/.github/workflows/release.yml": {
            "name": "Release", "on": {"push": {"tags": ["v*"]}, "workflow_dispatch": {}},
            "permissions": {"contents": "write", "id-token": "write", "attestations": "write"},
            "concurrency": {"group": "release-${{ github.ref }}", "cancel-in-progress": False},
            "jobs": {"release": {"uses": uses.format("rust-release")}},
        },
        "project/.github/chainguard/auto-tag.sts.yaml": trust_policy(identity, "ref:refs/heads/main", {
            "ref": "^refs/heads/main$", "event_name": "^(push|workflow_dispatch)$",
            "job_workflow_ref": SHARED_WORKFLOW.format("rust-auto-tag"),
        }, {"contents": "write"}),
        f"packages/.github/chainguard/dispatch-{identity['id']}.sts.yaml": trust_policy(identity, "environment:release", {
            "ref": r"^refs/tags/v[0-9A-Za-z.+-]+$", "event_name": "^push$", "environment": "^release$",
            "job_workflow_ref": SHARED_WORKFLOW.format("rust-release"),
        }, {"actions": "write"}),
    }


def rust_onboarding(args):
    project = checked(args.project, r"[a-z][a-z0-9-]{0,24}", "project")
    revision(args.workflow_ref)
    root = Path(args.root)
    output = Path(args.output)
    overlay = root / RUNNERS / project
    keys = root / CACHE_PROJECTS / f"{project}.secret.sops.yaml"
    registry = yaml.safe_load((root / REGISTRY).read_text())
    if output.exists() or overlay.exists() or keys.exists() or any(p["project"] == project for p in registry["projects"]):
        raise ValueError("Project is already onboarded or the output directory exists")
    identity = repository(args.repository)
    if any(p["id"] == identity["id"] for p in registry["projects"]):
        raise ValueError("Repository is already onboarded")
    recipients = platform_recipients(root)
    credentials = app_credentials()
    namespace = f"ci-{project}"
    pools = cache_pools()
    overlay.mkdir(parents=True)
    resources = ["../ci-namespace", "github-app.secret.sops.yaml"]
    encrypt(secret("github-app", namespace, credentials), overlay / "github-app.secret.sops.yaml", recipients)
    for pool, key in pools.items():
        name = f"sccache-{pool}"
        encrypt(secret(name, namespace, runner_credentials(key)), overlay / f"{name}.secret.sops.yaml", recipients)
        resources.append(f"{name}.secret.sops.yaml")
    encrypt_provisioner_keys(project, pools, keys, recipients)
    url = [{"op": "replace", "path": "/spec/values/githubConfigUrl", "value": "https://github.com/" + identity["full_name"]}]
    (overlay / "kustomization.yaml").write_text(yaml.safe_dump({
        "apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "namespace": namespace,
        "resources": resources, "components": ["../rust"],
        "patches": [{"target": {"kind": "HelmRelease"}, "patch": yaml.safe_dump(url, sort_keys=False)}],
    }, sort_keys=False))
    append_resource(root / RUNNERS / "kustomization.yaml", project)
    append_resource(root / CACHE_PROJECTS / "kustomization.yaml", keys.name)
    registry["projects"].append({"project": project, "repository": identity["full_name"], "id": identity["id"],
                                 "visibility": "private" if identity.get("private", False) else "public"})
    (root / REGISTRY).write_text(yaml.safe_dump(registry, sort_keys=False))
    for path, content in rust_callers(identity, args.workflow_ref).items():
        (output / path).parent.mkdir(parents=True, exist_ok=True)
        (output / path).write_text(yaml.safe_dump(content, sort_keys=False))
    print(f"Onboarded {identity['full_name']} as {namespace}; copy {output}/project into the repository")


def cache_onboarding(args):
    project = checked(args.project, r"[a-z][a-z0-9-]{0,24}", "project")
    root = Path(args.root)
    overlay = root / RUNNERS / project
    kustomization = overlay / "kustomization.yaml"
    credentials = overlay / "buildkit-cache.secret.sops.yaml"
    keys = root / CACHE_PROJECTS / f"{project}.secret.sops.yaml"
    if not kustomization.exists():
        raise ValueError("Project has no runner overlay")
    text = kustomization.read_text()
    settings = yaml.safe_load(text)
    if "../base" not in (settings.get("resources") or []) or "runnerScaleSetName" in text:
        raise ValueError("Only buildkit-amd64 pools support the layer cache")
    if settings.get("namespace") != f"ci-{project}":
        raise ValueError("Runner namespace does not match the project")
    if credentials.exists() or keys.exists():
        raise ValueError("Project already has a build cache")
    recipients = platform_recipients(root)
    pools = cache_pools()
    encrypt(secret("buildkit-cache", f"ci-{project}", runner_credentials(pools["rw"])), credentials, recipients)
    encrypt_provisioner_keys(project, pools, keys, recipients)
    append_resource(kustomization, credentials.name)
    append_entry(kustomization, "components", "../buildkit-cache")
    append_resource(root / CACHE_PROJECTS / "kustomization.yaml", keys.name)
    print(f"Enabled the layer cache for ci-{project}")


def main(argv=None):
    parser = argparse.ArgumentParser(prog="infra")
    commands = parser.add_subparsers(dest="command", required=True)
    onboard = commands.add_parser("onboard")
    onboard.add_argument("repository")
    onboard.add_argument("--project", required=True)
    onboard.add_argument("--image", required=True)
    onboard.add_argument("--source-revision", required=True)
    onboard.add_argument("--workflow-ref", required=True)
    onboard.add_argument("--port", type=int, default=8080)
    onboard.add_argument("--health-path", default="/healthz")
    onboard.add_argument("--domain", required=True)
    onboard.add_argument("--architecture", choices=["amd64", "arm64"], default="amd64")
    onboard.add_argument("--test-command", required=True)
    onboard.add_argument("--output", required=True)
    rust = commands.add_parser("onboard-rust")
    rust.add_argument("repository")
    rust.add_argument("--project", required=True)
    rust.add_argument("--workflow-ref", required=True)
    rust.add_argument("--output", required=True)
    rust.add_argument("--root", default=str(ROOT))
    cache = commands.add_parser("onboard-cache")
    cache.add_argument("--project", required=True)
    cache.add_argument("--root", default=str(ROOT))
    args = parser.parse_args(argv)
    try:
        {"onboard": onboarding, "onboard-rust": rust_onboarding, "onboard-cache": cache_onboarding}[args.command](args)
    except (ValueError, subprocess.CalledProcessError) as error:
        message = str(error) if isinstance(error, ValueError) else "Native command failed"
        parser.exit(1, message + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
