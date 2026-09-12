#!/usr/bin/env python3
import argparse
import hashlib
import json
import os
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
OWNER_ID = 114402558
INFRA_ID = 1328085692
SHA = re.compile(r"[0-9a-f]{40}\Z")
DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")
IMAGE = re.compile(r"ghcr\.io/fredrir/infra-ci@sha256:[0-9a-f]{64}\Z")
COMPONENT = re.compile(r"[a-z][a-z0-9-]{0,19}\Z")
PROJECT = re.compile(r"[a-z][a-z0-9-]{0,29}\Z")
CONFIG_SHA = re.compile(r"[0-9a-f]{64}\Z")
PUBLIC_ARGUMENT = re.compile(r"[A-Z][A-Z0-9_]{0,63}\Z")
SECRET_ARGUMENT = re.compile(
    r"SECRET|PASSWORD|TOKEN|PRIVATE|CREDENTIAL|ACCESS_KEY|AUTH|DOPPLER", re.IGNORECASE
)


class PolicyError(ValueError):
    pass


def guard(infrastructure=False):
    identity = (
        f"github.repository_id == '{INFRA_ID}'"
        if infrastructure
        else (
            "contains(fromJSON('["
            + ",".join(f'"{value}"' for value in repository_ids())
            + "]'), github.repository_id)"
        )
    )
    return (
        f"github.repository_owner_id == '{OWNER_ID}' && {identity} && "
        "github.event_name == 'push' && github.ref == 'refs/heads/main' && "
        "github.ref_protected == true && startsWith(vars.INFRA_CI_IMAGE, "
        "'ghcr.io/fredrir/infra-ci@sha256:') && "
        "contains(fromJSON(vars.INFRA_CI_ARCHITECTURES || '[]'), 'amd64')"
    )


def authorize(context, infrastructure=False):
    allowed = (INFRA_ID,) if infrastructure else repository_ids()
    return (
        str(context.get("repository_owner_id")) == str(OWNER_ID)
        and str(context.get("repository_id")) in {str(value) for value in allowed}
        and context.get("event_name") == "push"
        and context.get("ref") == "refs/heads/main"
        and context.get("ref_protected") is True
    )


def load_catalog(root=ROOT):
    catalog = json.loads((root / "platform/catalog/projects.json").read_text())
    if (
        catalog["ownerId"] != OWNER_ID
        or catalog["infrastructureRepositoryId"] != INFRA_ID
    ):
        raise PolicyError("catalog infrastructure identity mismatch")
    return catalog


def repository_ids():
    identifiers = {
        entry["repositoryId"] for entry in load_catalog()["projects"].values()
    } | {INFRA_ID}
    if any(type(value) is not int or value <= 0 for value in identifiers):
        raise PolicyError("catalog repository IDs must be positive integers")
    return sorted(identifiers)


def available_architectures(value):
    architectures = json.loads(value)
    if (
        not isinstance(architectures, list)
        or not architectures
        or any(not isinstance(item, str) for item in architectures)
    ):
        raise PolicyError("explicit runner architectures are required")
    if (
        len(architectures) != len(set(architectures))
        or not set(architectures) <= {"amd64", "arm64"}
        or "amd64" not in architectures
    ):
        raise PolicyError(
            "approved native architectures must include amd64 for control jobs"
        )
    return architectures


def job_guard(path, name, infrastructure):
    expected = guard(infrastructure)
    if path.name == "project-ci.yml" and name == "cache":
        expected += " && needs.prepare.outputs.cache == 'true'"
    if path.name == "project-ci.yml" and name == "publish":
        expected += " && !cancelled() && needs.build.result == 'success' && (needs.cache.result == 'success' || needs.cache.result == 'skipped')"
    return expected


def write_guards():
    import yaml

    for path in workflow_paths(ROOT):
        workflow = yaml.safe_load(path.read_text())
        triggers = workflow.get("on", workflow.get(True, {}))
        for name, job in workflow.get("jobs", {}).items():
            job["if"] = job_guard(path, name, "workflow_call" not in triggers)
        path.write_text(yaml.safe_dump(workflow, sort_keys=False, width=110))


def workflow_paths(root):
    return sorted(
        path
        for path in (root / ".github/workflows").iterdir()
        if path.suffix in {".yml", ".yaml"}
    )


def resolve(catalog, project, repository_id, platforms):
    if project not in catalog["projects"]:
        raise PolicyError("project is not approved")
    entry = catalog["projects"][project]
    if str(entry["repositoryId"]) != str(repository_id):
        raise PolicyError("project repository identity mismatch")
    if (
        not isinstance(platforms, list)
        or not platforms
        or len(platforms) != len(set(platforms))
    ):
        raise PolicyError("platforms must be a nonempty unique list")
    approved = {f"linux/{arch}" for arch in entry["architectures"]}
    if not set(platforms) <= approved or not set(platforms) <= {
        "linux/amd64",
        "linux/arm64",
    }:
        raise PolicyError("platform is not approved")
    return entry


def component_image(entry, component="app"):
    if not COMPONENT.fullmatch(component):
        raise PolicyError("invalid component name")
    images = entry.get("images", {"app": entry.get("image")})
    if component not in images or not images[component]:
        raise PolicyError("component is not approved")
    return images[component]


def release_path(project, component="app"):
    if not PROJECT.fullmatch(project) or not COMPONENT.fullmatch(component):
        raise PolicyError("invalid release project or component")
    suffix = "release.json" if component == "app" else f"releases/{component}.json"
    return f"platform/projects/{project}/{suffix}"


def public_build_arguments(entry, component, value):
    component_image(entry, component)
    if len(value) > 16384:
        raise PolicyError("public build arguments exceed the size limit")

    def unique_object(pairs):
        result = {}
        for key, item in pairs:
            if key in result:
                raise PolicyError("duplicate public build argument")
            result[key] = item
        return result

    arguments = json.loads(value, object_pairs_hook=unique_object)
    if not isinstance(arguments, dict):
        raise PolicyError("public build arguments must be a JSON object")
    approved = entry.get("publicBuildArguments", {}).get(component, [])
    for name, item in arguments.items():
        if (
            not PUBLIC_ARGUMENT.fullmatch(name)
            or SECRET_ARGUMENT.search(name)
            or name not in approved
        ):
            raise PolicyError("build argument is not approved as public configuration")
        if (
            not isinstance(item, str)
            or len(item) > 2048
            or any(ord(character) < 32 or ord(character) == 127 for character in item)
        ):
            raise PolicyError(
                "public build argument values must be bounded single-line strings"
            )
    return dict(sorted(arguments.items()))


def build_configuration(
    entry, component, build_context, dockerfile, nix_target, test_command, arguments
):
    document = {
        "context": build_context,
        "dockerfile": dockerfile,
        "nixTarget": nix_target,
        "testCommand": test_command.strip(),
        "publicBuildArguments": public_build_arguments(entry, component, arguments),
    }
    if not document["testCommand"]:
        raise PolicyError("project test command is required")
    encoded = json.dumps(
        document, sort_keys=True, separators=(",", ":"), ensure_ascii=True
    ).encode()
    return document, hashlib.sha256(encoded).hexdigest()


def relative_path(root, value):
    path = Path(value)
    if path.is_absolute() or ".." in path.parts:
        raise PolicyError("path must remain inside the source checkout")
    resolved = (root / path).resolve()
    if not resolved.is_relative_to(root.resolve()):
        raise PolicyError("path escapes the source checkout")
    return resolved


def output(name, value):
    if "\n" in str(value) or "\r" in str(value):
        raise PolicyError("invalid workflow output")
    destination = os.environ.get("GITHUB_OUTPUT")
    if destination:
        with open(destination, "a") as stream:
            stream.write(f"{name}={value}\n")
    else:
        print(f"{name}={value}")


def validate_workflows(root=ROOT):
    import yaml

    errors = []
    for path in workflow_paths(root):
        workflow = yaml.safe_load(path.read_text())
        triggers = workflow.get("on", workflow.get(True, {}))
        if not isinstance(triggers, dict) or not set(triggers) <= {
            "push",
            "workflow_call",
        }:
            errors.append(f"{path.name}: forbidden workflow trigger")
        if "push" in triggers and triggers["push"].get("branches") != ["main"]:
            errors.append(f"{path.name}: push must select main only")
        infrastructure = "workflow_call" not in triggers
        for name, job in workflow.get("jobs", {}).items():
            expected = job_guard(path, name, infrastructure)
            if job.get("if") != expected:
                errors.append(
                    f"{path.name}/{name}: missing pre-allocation identity guard"
                )
            if "runs-on" in job:
                if not str(job["runs-on"]).startswith("${{ format('infra-"):
                    errors.append(f"{path.name}/{name}: forbidden runner selector")
                if (
                    job.get("container", {}).get("image")
                    != "${{ vars.INFRA_CI_IMAGE }}"
                ):
                    errors.append(
                        f"{path.name}/{name}: required pipeline container missing"
                    )
            elif job.get("uses") != "./.github/workflows/project-ci.yml":
                errors.append(f"{path.name}/{name}: unapproved reusable workflow")
            for step in job.get("steps", []):
                action = step.get("uses", "")
                if action and not re.fullmatch(
                    r"[\w.-]+/[\w./-]+@[0-9a-f]{40}", action
                ):
                    errors.append(f"{path.name}/{name}: action is not pinned")
    if errors:
        raise PolicyError("\n".join(errors))


def sts_policy(entry, revision):
    if not SHA.fullmatch(revision):
        raise PolicyError("workflow revision must be a full commit SHA")
    return {
        "issuer": "https://token.actions.githubusercontent.com",
        "claim_pattern": {
            "repository_id": f"^{entry['repositoryId']}$",
            "repository_owner_id": f"^{OWNER_ID}$",
            "event_name": "^push$",
            "ref": "^refs/heads/main$",
            "job_workflow_ref": "^fredrir/llunde-infra/\\.github/workflows/project-ci\\.yml@"
            + revision
            + "$",
            "job_workflow_sha": "^" + revision + "$",
        },
        "permissions": {"contents": "write", "pull_requests": "write"},
    }


def settings_plan(repository):
    if not re.fullmatch(r"fredrir/[A-Za-z0-9_.-]+", repository):
        raise PolicyError("repository is not owned by fredrir")
    return [
        {
            "method": "PUT",
            "path": f"repos/{repository}/actions/permissions/fork-pr-contributor-approval",
            "body": {"approval_policy": "all_external_contributors"},
        },
        {
            "method": "PUT",
            "path": f"repos/{repository}/actions/permissions/workflow",
            "body": {
                "default_workflow_permissions": "read",
                "can_approve_pull_request_reviews": False,
            },
        },
    ]


def main():
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("check")
    sub.add_parser("write-guards")
    sub.add_parser("available")
    resolve_parser = sub.add_parser("resolve")
    resolve_parser.add_argument("--project", required=True)
    resolve_parser.add_argument("--component", default="app")
    resolve_parser.add_argument("--platforms", required=True)
    resolve_parser.add_argument("--nix-target", default="")
    settings = sub.add_parser("settings-plan")
    settings.add_argument("repository")
    sts = sub.add_parser("sts-plan")
    sts.add_argument("--revision", required=True)
    args = parser.parse_args()
    if args.command == "check":
        validate_workflows()
    elif args.command == "write-guards":
        write_guards()
    elif args.command == "available":
        output(
            "architectures",
            json.dumps(
                available_architectures(
                    os.environ.get("CI_AVAILABLE_ARCHITECTURES", "[]")
                )
            ),
        )
    elif args.command == "settings-plan":
        print(json.dumps(settings_plan(args.repository), indent=2))
    elif args.command == "sts-plan":
        print(
            json.dumps(
                [
                    {
                        "path": f".github/chainguard/{project}-release.sts.yaml",
                        "policy": sts_policy(entry, args.revision),
                    }
                    for project, entry in load_catalog()["projects"].items()
                    if entry["capabilities"]
                ],
                indent=2,
            )
        )
    elif args.command == "resolve":
        context = json.loads(os.environ["CI_GITHUB_CONTEXT"])
        if not authorize(context) or not IMAGE.fullmatch(
            os.environ.get("CI_PIPELINE_IMAGE", "")
        ):
            raise PolicyError("CI execution is not authorized")
        platforms = json.loads(args.platforms)
        available = available_architectures(
            os.environ.get("CI_AVAILABLE_ARCHITECTURES", "[]")
        )
        if not set(platforms) <= {f"linux/{arch}" for arch in available}:
            raise PolicyError(
                "requested native platform has not been activated; no build was queued"
            )
        entry = resolve(
            load_catalog(), args.project, context["repository_id"], platforms
        )
        _, config_hash = build_configuration(
            entry,
            args.component,
            os.environ["CI_CONTEXT"],
            os.environ["CI_DOCKERFILE"],
            args.nix_target,
            os.environ["CI_TEST_COMMAND"],
            os.environ.get("CI_BUILD_ARGUMENTS", "{}"),
        )
        output("build-config-sha256", config_hash)
        output(
            "matrix",
            json.dumps(
                {
                    "include": [
                        {"architecture": value.split("/")[1]} for value in platforms
                    ]
                },
                separators=(",", ":"),
            ),
        )
        output("image", component_image(entry, args.component))
        import cache

        configuration = cache.configuration(context["repository_id"])
        output(
            "cache",
            "true"
            if configuration and configuration["uploadEnabled"] and args.nix_target
            else "false",
        )


if __name__ == "__main__":
    try:
        main()
    except (PolicyError, KeyError, ValueError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
