import argparse
import json
import re
import subprocess
from pathlib import Path

import yaml

OWNER_ID = 114402558
WORKFLOW = "fredrir/infra/.github/workflows/build-image.yml"


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
            "repository": identity["full_name"], "images": {args.image.split("@")[0]: {
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
            }, "permissions": {"contents": "write", "pull_requests": "write"},
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


def verify(args):
    immutable_image(args.image)
    revision(args.source_revision)
    revision(args.workflow_ref)
    identity = repository(args.repository)
    if identity.get("private", False):
        subprocess.run([
            "cosign", "verify", args.image,
            "--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
            "--certificate-identity", "https://github.com/" + WORKFLOW + "@" + args.workflow_ref,
            "--certificate-github-workflow-repository", args.repository,
            "--certificate-github-workflow-sha", args.source_revision,
            "--certificate-github-workflow-ref", "refs/heads/main",
            "--certificate-github-workflow-trigger", "push",
            "--annotation", "source-repository=" + args.repository,
            "--annotation", "source-revision=" + args.source_revision,
            "--annotation", "workflow-revision=" + args.workflow_ref,
        ], check=True)
        return
    subprocess.run([
        "gh", "attestation", "verify", "oci://" + args.image,
        "--repo", args.repository, "--signer-workflow", WORKFLOW,
        "--signer-digest", args.workflow_ref, "--source-ref", "refs/heads/main",
        "--source-digest", args.source_revision,
    ], check=True)


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
    verify_parser = commands.add_parser("verify-release")
    verify_parser.add_argument("image")
    verify_parser.add_argument("--repository", required=True)
    verify_parser.add_argument("--source-revision", required=True)
    verify_parser.add_argument("--workflow-ref", required=True)
    args = parser.parse_args(argv)
    try:
        onboarding(args) if args.command == "onboard" else verify(args)
    except (ValueError, subprocess.CalledProcessError) as error:
        message = str(error) if isinstance(error, ValueError) else "Native command failed"
        parser.exit(1, message + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
