import argparse
import difflib
import json
import re
import subprocess
import sys
import tempfile
from pathlib import Path

import yaml

from .contracts import ContractError, load_document, validate_project, validate_release
from .render import chart_values, render_all


def repository_root(value=None):
    if value:
        root = Path(value).resolve()
    else:
        root = Path(
            subprocess.check_output(
                ["git", "rev-parse", "--show-toplevel"], text=True
            ).strip()
        )
    if not (root / "platform/catalog/projects.json").is_file():
        raise ContractError("infrastructure checkout required; use --root")
    return root


def validate(root):
    count = 0
    for path in sorted((root / "platform/projects").glob("*/project.yaml")):
        document = load_document(path)
        validate_project(root, document)
        if document["project"] != path.parent.name:
            raise ContractError("project directory mismatch")
        release_file = path.with_name("release.json")
        release = load_document(release_file) if release_file.exists() else None
        releases = {}
        for component_release in sorted((path.parent / "releases").glob("*.json")):
            record = load_document(component_release)
            validate_release(root, record, document["project"])
            if component_release.stem != record.get("component", "app"):
                raise ContractError("release component filename mismatch")
            releases[component_release.stem] = record
        if release and release.get("component", "app") in releases:
            raise ContractError("duplicate component release")
        if release or releases:
            chart_values(root, document, release, releases)
        count += 1
    return count


def onboarding(root, args):
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", args.repository):
        raise ContractError("repository must be owner/name")
    if not re.fullmatch(r"[a-f0-9]{40}", args.workflow_ref):
        raise ContractError("workflow-ref must be a full commit SHA")
    identity = json.loads(
        subprocess.check_output(["gh", "api", f"repos/{args.repository}"], text=True)
    )
    catalog = load_document(root / "platform/catalog/projects.json")
    if identity["owner"]["id"] != catalog["ownerId"]:
        raise ContractError("repository owner not approved")
    project_name = args.project or identity["name"].lower()
    if not re.fullmatch(r"[a-z][a-z0-9-]{0,29}", project_name):
        raise ContractError(
            "project must be a lowercase DNS name of at most 30 characters"
        )
    workload = {"kind": args.kind, "architecture": args.architecture}
    if args.kind == "web":
        workload.update(port=args.port, healthPath=args.health_path)
        if args.readiness_path:
            workload["readinessPath"] = args.readiness_path
        if args.domain:
            workload["domains"] = args.domain
    elif args.readiness_path:
        raise ContractError("readiness-path requires a web workload")
    if args.termination_grace_period_seconds is not None:
        workload["terminationGracePeriodSeconds"] = (
            args.termination_grace_period_seconds
        )
    if args.kind == "cron":
        if not args.schedule:
            raise ContractError("cron schedule required")
        workload["schedule"] = args.schedule
    document = {
        "schemaVersion": 1,
        "project": project_name,
        "workloads": {"app": workload},
    }
    approved = catalog["projects"].get(project_name)
    if approved:
        if (
            approved["repositoryId"] != identity["id"]
            or approved["repository"] != identity["full_name"]
        ):
            raise ContractError("repository identity conflicts with catalog")
        validate_project(root, document)
    else:
        from .contracts import validate_schema

        validate_schema(root, "project", document)
    architectures = (
        ["amd64", "arm64"] if args.architecture == "multi" else [args.architecture]
    )
    caller = {
        "name": "Project CI",
        "on": {"push": {"branches": ["main"]}},
        "permissions": {
            "contents": "read",
            "packages": "write",
            "id-token": "write",
            "attestations": "write",
        },
        "jobs": {
            "project": {
                "if": f"github.event_name == 'push' && github.ref == 'refs/heads/main' && github.ref_protected && github.repository_id == '{identity['id']}' && github.repository_owner_id == '{catalog['ownerId']}'",
                "uses": f"{catalog['infrastructureRepository']}/.github/workflows/project-ci.yml@{args.workflow_ref}",
                "with": {
                    "project": project_name,
                    "platforms": json.dumps(
                        ["linux/" + arch for arch in architectures]
                    ),
                    "test-command": args.test_command,
                },
            }
        },
    }
    output = Path(args.output).resolve()
    paths = [
        output / "project.yaml",
        output / ".github/workflows/ci.yml",
        output / "onboarding-request.json",
    ]
    if any(path.exists() for path in paths):
        raise ContractError("onboarding output already exists")
    request = {
        "schemaVersion": 1,
        "project": project_name,
        "repository": identity["full_name"],
        "repositoryId": identity["id"],
        "ownerId": identity["owner"]["id"],
        "domains": args.domain or [],
        "architectures": architectures,
        "catalogApproved": bool(approved),
    }
    for path, content in zip(
        paths,
        [
            yaml.safe_dump(document, sort_keys=False),
            yaml.safe_dump(caller, sort_keys=False),
            json.dumps(request, indent=2) + "\n",
        ],
    ):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)
    print(f"Generated onboarding files: {output}")
    if not approved:
        print("Catalog approval required before deployment.")


def check_render(root):
    with tempfile.TemporaryDirectory(prefix="infra-render-") as directory:
        temporary = Path(directory)
        render_all(root, temporary)
        paths = sorted(temporary.rglob("*.yaml"))
        mismatch = False
        for path in paths:
            relative = path.relative_to(temporary)
            target = root / "platform/projects" / relative
            expected = path.read_text()
            current = target.read_text() if target.exists() else ""
            if current != expected:
                mismatch = True
                print(
                    "".join(
                        difflib.unified_diff(
                            current.splitlines(True),
                            expected.splitlines(True),
                            fromfile=str(target),
                            tofile=str(relative),
                        )
                    ),
                    end="",
                )
        if mismatch:
            raise ContractError("generated resources differ; run infra render")


def main(argv=None):
    parser = argparse.ArgumentParser(prog="infra")
    parser.add_argument("--root")
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("validate")
    render = commands.add_parser("render")
    render.add_argument("--output")
    render.add_argument("--check", action="store_true")
    onboard = commands.add_parser("onboard")
    onboard.add_argument("repository")
    onboard.add_argument("--project")
    onboard.add_argument("--kind", choices=["web", "worker", "cron"], default="web")
    onboard.add_argument("--port", type=int, default=3000)
    onboard.add_argument("--health-path", default="/")
    onboard.add_argument("--readiness-path")
    onboard.add_argument("--termination-grace-period-seconds", type=int)
    onboard.add_argument("--domain", action="append")
    onboard.add_argument(
        "--architecture", choices=["amd64", "arm64", "multi"], default="amd64"
    )
    onboard.add_argument("--schedule")
    onboard.add_argument("--test-command", required=True)
    onboard.add_argument("--workflow-ref", required=True)
    onboard.add_argument("--output", required=True)
    release = commands.add_parser("validate-release")
    release.add_argument("file")
    release.add_argument("--project")
    args = parser.parse_args(argv)
    try:
        root = repository_root(args.root)
        if args.command == "validate":
            print(f"Validated {validate(root)} project configurations.")
        elif args.command == "render":
            if args.check:
                check_render(root)
                print("Generated resources match.")
            else:
                entries = render_all(
                    root,
                    Path(args.output) if args.output else root / "platform/projects",
                )
                print(f"Rendered {len(entries)} project configurations.")
        elif args.command == "onboard":
            onboarding(root, args)
        elif args.command == "validate-release":
            validate_release(root, load_document(args.file), args.project)
            print(
                "Release contract valid; signature verification is a separate promotion gate."
            )
        return 0
    except (
        ContractError,
        OSError,
        subprocess.CalledProcessError,
        KeyError,
        json.JSONDecodeError,
    ) as error:
        print(f"infra: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
