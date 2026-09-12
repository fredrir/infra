#!/usr/bin/env python3
import argparse
import base64
import hashlib
import json
import os
import platform
import signal
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

from policy import (
    CONFIG_SHA,
    DIGEST,
    INFRA_ID,
    ROOT,
    SHA,
    PolicyError,
    build_configuration,
    component_image,
    load_catalog,
    output,
    relative_path,
    release_path,
    resolve,
)

PROVENANCE_TYPE = "https://slsa.dev/provenance/v1"
WORKFLOW_PATH = ".github/workflows/project-ci.yml"


def run(arguments, **kwargs):
    return subprocess.run(arguments, check=True, text=True, **kwargs)


def capture(arguments, **kwargs):
    return run(arguments, stdout=subprocess.PIPE, **kwargs).stdout.strip()


def sha256(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def context():
    result = {
        "repositoryId": int(os.environ["GITHUB_REPOSITORY_ID"]),
        "repository": os.environ["GITHUB_REPOSITORY"],
        "sourceRevision": os.environ["GITHUB_SHA"],
        "workflowRevision": os.environ["CI_WORKFLOW_REVISION"],
        "workflowRepository": os.environ["CI_WORKFLOW_REPOSITORY"],
        "workflowRef": os.environ["CI_WORKFLOW_REF"],
        "runId": int(os.environ["GITHUB_RUN_ID"]),
        "buildConfigSha256": os.environ["CI_BUILD_CONFIG_SHA256"],
    }
    if not SHA.fullmatch(result["sourceRevision"]) or not SHA.fullmatch(
        result["workflowRevision"]
    ):
        raise PolicyError("invalid source or workflow revision")
    if result["workflowRepository"] != "fredrir/llunde-infra":
        raise PolicyError("unapproved workflow repository")
    if not CONFIG_SHA.fullmatch(result["buildConfigSha256"]):
        raise PolicyError("validated build configuration digest is required")
    validate_workflow_identity(result)
    return result


def validate_workflow_identity(ctx):
    prefix = f"fredrir/llunde-infra/{WORKFLOW_PATH}@"
    pinned = prefix + ctx["workflowRevision"]
    local = (
        ctx["repositoryId"] == INFRA_ID
        and ctx["repository"] == "fredrir/llunde-infra"
        and ctx["sourceRevision"] == ctx["workflowRevision"]
        and ctx["workflowRef"] == prefix + "refs/heads/main"
    )
    if ctx["workflowRef"] != pinned and not local:
        raise PolicyError("consumer must pin the shared workflow to its exact commit")


def project_entry(project, platforms, component="app"):
    ctx = context()
    entry = resolve(load_catalog(), project, ctx["repositoryId"], platforms)
    if entry["repository"] != ctx["repository"]:
        raise PolicyError("repository name/identity mismatch")
    component_image(entry, component)
    ctx["component"] = component
    return ctx, entry


def native_platform():
    architecture = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    if platform.system() != "Linux" or architecture is None:
        raise PolicyError("a native supported Linux runner is required")
    return f"linux/{architecture}"


def build(args):
    import cache

    ctx, _ = project_entry(args.project, [f"linux/{args.architecture}"], args.component)
    with cache.read_environment(ctx["repositoryId"]) as environment:
        build_source(args, environment)


def build_source(args, environment):
    import cache

    selected = f"linux/{args.architecture}"
    ctx, entry = project_entry(args.project, [selected], args.component)
    configuration, config_hash = build_configuration(
        entry,
        args.component,
        args.context,
        args.dockerfile,
        args.nix_target,
        os.environ.get("CI_TEST_COMMAND", ""),
        os.environ.get("CI_BUILD_ARGUMENTS", "{}"),
    )
    if config_hash != ctx["buildConfigSha256"]:
        raise PolicyError("build configuration differs from the approved preparation")
    if native_platform() != selected:
        raise PolicyError("emulated or mismatched build architecture")
    source = Path(args.source).resolve()
    build_context = relative_path(source, args.context)
    dockerfile = relative_path(source, args.dockerfile)
    if not build_context.is_dir() or not dockerfile.is_file():
        raise PolicyError("build context or Dockerfile missing")
    test_command = os.environ.get("CI_TEST_COMMAND", "").strip()
    if not test_command:
        raise PolicyError("project test command is required")
    settings = json.loads(capture(["nix", "config", "show", "--json"], env=environment))
    if (
        settings["sandbox"]["value"] is not True
        or settings["sandbox-fallback"]["value"] is not False
    ):
        raise PolicyError("Nix sandboxing must be enabled without fallback")
    run(["bash", "-euo", "pipefail", "-c", test_command], cwd=source, env=environment)
    destination = Path(args.output).resolve()
    destination.mkdir(parents=True, exist_ok=False)
    closure = {}
    if args.nix_target:
        if not args.nix_target.startswith(".#") or any(
            c in args.nix_target for c in "\r\n"
        ):
            raise PolicyError("Nix target must select the source checkout")
        builds = json.loads(
            capture(
                [
                    "nix",
                    "build",
                    args.nix_target,
                    "--no-link",
                    "--json",
                    "--no-update-lock-file",
                ],
                cwd=source,
                env=environment,
            )
        )
        cache_entry = cache.configuration(ctx["repositoryId"])
        if cache_entry and cache_entry["uploadEnabled"]:
            paths = sorted(
                {path for result in builds for path in result["outputs"].values()}
            )
            closure = cache.export_closure(paths, destination, environment)
    with tempfile.TemporaryDirectory(prefix="infra-buildkit-") as work:
        socket = f"unix://{work}/buildkit.sock"
        with open(f"{work}/daemon.log", "w+") as log:
            daemon = subprocess.Popen(
                [
                    "rootlesskit",
                    "--net=slirp4netns",
                    "--copy-up=/etc",
                    "--copy-up=/run",
                    "buildkitd",
                    "--rootless",
                    "--addr",
                    socket,
                    "--root",
                    f"{work}/state",
                    "--oci-worker-snapshotter=native",
                    "--containerd-worker=false",
                ],
                stdout=log,
                stderr=log,
                start_new_session=True,
                env=environment,
            )
            try:
                for attempt in range(30):
                    if daemon.poll() is not None:
                        raise PolicyError(
                            "rootless BuildKit failed under the approved sandbox profile"
                        )
                    probe = subprocess.run(
                        ["buildctl", "--addr", socket, "debug", "workers"],
                        stdout=subprocess.DEVNULL,
                        stderr=subprocess.DEVNULL,
                        env=environment,
                    )
                    if probe.returncode == 0:
                        break
                    time.sleep(1)
                else:
                    raise PolicyError("BuildKit startup timed out")
                run(
                    buildkit_arguments(
                        socket,
                        build_context,
                        dockerfile,
                        selected,
                        destination,
                        configuration["publicBuildArguments"],
                    ),
                    env=environment,
                )
            finally:
                if daemon.poll() is None:
                    os.killpg(daemon.pid, signal.SIGTERM)
                    try:
                        daemon.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        os.killpg(daemon.pid, signal.SIGKILL)
                        daemon.wait()
    run(
        [
            "trivy",
            "image",
            "--input",
            str(destination / "image.tar"),
            "--scanners",
            "vuln,secret",
            "--severity",
            "HIGH,CRITICAL",
            "--exit-code",
            "1",
        ],
        env=environment,
    )
    manifest = {
        "schemaVersion": 1,
        "component": args.component,
        "repositoryId": ctx["repositoryId"],
        "sourceRevision": ctx["sourceRevision"],
        "workflowRevision": ctx["workflowRevision"],
        "buildConfigSha256": ctx["buildConfigSha256"],
        "runId": ctx["runId"],
        "platform": selected,
        "archiveSha256": sha256(destination / "image.tar"),
        **closure,
    }
    (destination / "build.json").write_text(json.dumps(manifest, indent=2) + "\n")


def buildkit_arguments(
    socket, build_context, dockerfile, selected, destination, public_arguments
):
    arguments = [
        "buildctl",
        "--addr",
        socket,
        "build",
        "--frontend=dockerfile.v0",
        "--local",
        f"context={build_context}",
        "--local",
        f"dockerfile={dockerfile.parent}",
        "--opt",
        f"filename={dockerfile.name}",
        "--opt",
        f"platform={selected}",
        "--output",
        f"type=oci,dest={destination / 'image.tar'}",
    ]
    for name, value in sorted(public_arguments.items()):
        arguments.extend(["--opt", f"build-arg:{name}={value}"])
    return arguments


def validate_artifacts(directory, platforms, ctx):
    artifacts = {}
    for path in sorted(directory.glob("build-*/build.json")):
        if path.stat().st_size > 4096:
            raise PolicyError("oversized build manifest")
        manifest = json.loads(path.read_text())
        selected = manifest.get("platform")
        if selected not in platforms or selected in artifacts:
            raise PolicyError("unexpected or duplicate build platform")
        if manifest.get("schemaVersion") != 1 or manifest.get(
            "component", "app"
        ) != ctx.get("component", "app"):
            raise PolicyError("build schema or component mismatch")
        for key in (
            "repositoryId",
            "sourceRevision",
            "workflowRevision",
            "buildConfigSha256",
            "runId",
        ):
            if manifest.get(key) != ctx.get(key):
                raise PolicyError(f"build {key} mismatch")
        archive = path.parent / "image.tar"
        if (
            archive.is_symlink()
            or not archive.is_file()
            or sha256(archive) != manifest.get("archiveSha256")
        ):
            raise PolicyError("build archive digest mismatch")
        artifacts[selected] = archive
    if set(artifacts) != set(platforms):
        raise PolicyError("native build artifacts are incomplete")
    return artifacts


def registry_auth(directory):
    token = os.environ["GHCR_TOKEN"]
    auth = base64.b64encode(f"{os.environ['GITHUB_ACTOR']}:{token}".encode()).decode()
    path = Path(directory) / "config.json"
    path.write_text(json.dumps({"auths": {"ghcr.io": {"auth": auth}}}))
    path.chmod(0o600)
    return path


def signer(ctx):
    ref = ctx.get(
        "workflowRef",
        f"{ctx['workflowRepository']}/{WORKFLOW_PATH}@{ctx['workflowRevision']}",
    )
    return f"https://github.com/{ref}"


def predicate(ctx, platforms):
    return {
        "buildDefinition": {
            "buildType": "https://github.com/fredrir/llunde-infra/native-container/v1",
            "externalParameters": {
                "repositoryId": ctx["repositoryId"],
                "repository": ctx["repository"],
                "sourceRevision": ctx["sourceRevision"],
                "platforms": platforms,
                "component": ctx.get("component", "app"),
                **(
                    {"buildConfigSha256": ctx["buildConfigSha256"]}
                    if "buildConfigSha256" in ctx
                    else {}
                ),
            },
            "internalParameters": {"workflowRevision": ctx["workflowRevision"]},
            "resolvedDependencies": [
                {
                    "uri": f"git+https://github.com/{ctx['repository']}",
                    "digest": {"gitCommit": ctx["sourceRevision"]},
                }
            ],
        },
        "runDetails": {
            "builder": {"id": signer(ctx)},
            "metadata": {
                "invocationId": f"https://github.com/{ctx['repository']}/actions/runs/{ctx['runId']}"
            },
        },
    }


def publish(args):
    platforms = json.loads(args.platforms)
    ctx, entry = project_entry(args.project, platforms, args.component)
    image = component_image(entry, args.component)
    artifacts = validate_artifacts(Path(args.artifacts), platforms, ctx)
    with tempfile.TemporaryDirectory(prefix="infra-publish-") as work:
        authfile = registry_auth(work)
        environment = {**os.environ, "DOCKER_CONFIG": work}
        references = []
        for selected, archive in artifacts.items():
            architecture = selected.split("/")[1]
            tag = f"{image}:{ctx['sourceRevision']}-{architecture}"
            digestfile = Path(work) / f"{architecture}.digest"
            run(
                [
                    "skopeo",
                    "copy",
                    "--authfile",
                    str(authfile),
                    "--digestfile",
                    str(digestfile),
                    f"oci-archive:{archive}",
                    f"docker://{tag}",
                ]
            )
            digest = digestfile.read_text().strip()
            if not DIGEST.fullmatch(digest):
                raise PolicyError("registry returned an invalid digest")
            references.append(f"{image}@{digest}")
        tag = f"{image}:{ctx['sourceRevision']}"
        run(
            ["docker", "buildx", "imagetools", "create", "--tag", tag, *references],
            env=environment,
        )
        digest = capture(
            [
                "docker",
                "buildx",
                "imagetools",
                "inspect",
                tag,
                "--format",
                "{{.Manifest.Digest}}",
            ],
            env=environment,
        )
        if not DIGEST.fullmatch(digest):
            raise PolicyError("registry returned an invalid index digest")
        reference = f"{image}@{digest}"
        statement = Path(work) / "predicate.json"
        statement.write_text(json.dumps(predicate(ctx, platforms)))
        run(["cosign", "sign", "--yes", reference], env=environment)
        run(
            [
                "cosign",
                "attest",
                "--yes",
                "--type",
                PROVENANCE_TYPE,
                "--predicate",
                str(statement),
                reference,
            ],
            env=environment,
        )
        verify_image(reference, ctx, platforms, environment)
        output("image", reference)


def upload_cache(args):
    import cache

    selected = f"linux/{args.architecture}"
    ctx, _ = project_entry(args.project, [selected], args.component)
    if native_platform() != selected:
        raise PolicyError("cache import requires the corresponding native runner")
    artifacts = validate_artifacts(Path(args.artifacts), [selected], ctx)
    directory = artifacts[selected].parent
    manifest = json.loads((directory / "build.json").read_text())
    cache.upload(directory, manifest, ctx["repositoryId"])


def parse_records(text):
    decoder = json.JSONDecoder()
    records = []
    while text.strip():
        record, offset = decoder.raw_decode(text.lstrip())
        text = text.lstrip()[offset:]
        records.extend(record if isinstance(record, list) else [record])
    return records


def validate_attestations(records, reference, ctx, platforms):
    expected = predicate(ctx, platforms)
    digest = reference.split("@", 1)[1].removeprefix("sha256:")
    for record in records:
        payload = record.get("payload", "")
        if len(payload) > 1_048_576:
            raise PolicyError("oversized attestation")
        statement = json.loads(base64.b64decode(payload, validate=True))
        matching_subject = any(
            subject.get("digest", {}).get("sha256") == digest
            for subject in statement.get("subject", [])
        )
        if (
            matching_subject
            and statement.get("predicateType") == PROVENANCE_TYPE
            and statement.get("predicate") == expected
        ):
            return
    raise PolicyError(
        "verified attestation does not match repository, revision, workflow, run and platforms"
    )


def verify_image(reference, ctx, platforms, environment=None):
    if "@" not in reference or not DIGEST.fullmatch(reference.split("@", 1)[1]):
        raise PolicyError("immutable image digest required")
    flags = [
        "--certificate-identity",
        signer(ctx),
        "--certificate-oidc-issuer",
        "https://token.actions.githubusercontent.com",
        "--certificate-github-workflow-repository",
        ctx["repository"],
        "--certificate-github-workflow-sha",
        ctx["sourceRevision"],
        "--certificate-github-workflow-trigger",
        "push",
        "--certificate-github-workflow-ref",
        "refs/heads/main",
    ]
    run(
        ["cosign", "verify", *flags, reference],
        env=environment,
        stdout=subprocess.DEVNULL,
    )
    records = parse_records(
        capture(
            [
                "cosign",
                "verify-attestation",
                *flags,
                "--type",
                PROVENANCE_TYPE,
                reference,
            ],
            env=environment,
        )
    )
    validate_attestations(records, reference, ctx, platforms)


def release_document(args):
    platforms = json.loads(args.platforms)
    ctx, entry = project_entry(args.project, platforms, args.component)
    if args.image.split("@", 1)[0] != component_image(entry, args.component):
        raise PolicyError("release image is not approved for this repository")
    with tempfile.TemporaryDirectory(prefix="infra-verify-") as work:
        registry_auth(work)
        verify_image(args.image, ctx, platforms, {**os.environ, "DOCKER_CONFIG": work})
    document = {
        "schemaVersion": 1,
        "project": args.project,
        "environment": "production",
        "component": args.component,
        "image": args.image,
        "sourceRevision": ctx["sourceRevision"],
        "provenance": {
            "workflowRepository": ctx["workflowRepository"],
            "workflowRevision": ctx["workflowRevision"],
            "workflowPath": WORKFLOW_PATH,
            "runId": ctx["runId"],
            "repositoryId": ctx["repositoryId"],
            "platforms": platforms,
            "buildConfigSha256": ctx["buildConfigSha256"],
        },
    }
    sys.path.insert(0, str(ROOT))
    from tools.infra.contracts import validate_release

    validate_release(ROOT, document, args.project)
    Path(args.output).write_text(json.dumps(document, indent=2) + "\n")


def api(method, endpoint, data=None):
    request = urllib.request.Request(
        f"https://api.github.com/{endpoint}",
        data=json.dumps(data).encode() if data is not None else None,
        method=method,
        headers={
            "Authorization": f"Bearer {os.environ['GH_TOKEN']}",
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2026-03-10",
            "Content-Type": "application/json",
        },
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


def update_release_branch(repository, branch, document):
    for attempt in range(5):
        exists = True
        try:
            base = api("GET", f"repos/{repository}/git/ref/heads/{branch}")["object"][
                "sha"
            ]
        except urllib.error.HTTPError as error:
            if error.code != 404:
                raise
            error.close()
            exists = False
            base = api("GET", f"repos/{repository}/git/ref/heads/main")["object"]["sha"]
        base_tree = api("GET", f"repos/{repository}/git/commits/{base}")["tree"]["sha"]
        tree = api(
            "POST",
            f"repos/{repository}/git/trees",
            {
                "base_tree": base_tree,
                "tree": [
                    {
                        "path": release_path(
                            document["project"], document.get("component", "app")
                        ),
                        "mode": "100644",
                        "type": "blob",
                        "content": json.dumps(document, indent=2) + "\n",
                    }
                ],
            },
        )
        commit = api(
            "POST",
            f"repos/{repository}/git/commits",
            {
                "message": f"Release {document['project']}/{document.get('component', 'app')} at {document['sourceRevision']}",
                "tree": tree["sha"],
                "parents": [base],
            },
        )
        try:
            if exists:
                api(
                    "PATCH",
                    f"repos/{repository}/git/refs/heads/{branch}",
                    {"sha": commit["sha"], "force": False},
                )
            else:
                api(
                    "POST",
                    f"repos/{repository}/git/refs",
                    {"ref": f"refs/heads/{branch}", "sha": commit["sha"]},
                )
            return
        except urllib.error.HTTPError as error:
            if error.code not in (409, 422) or attempt == 4:
                raise
            error.close()
    raise PolicyError("release branch update did not complete")


def request_release(args):
    document = json.loads(Path(args.document).read_text())
    sys.path.insert(0, str(ROOT))
    from tools.infra.contracts import validate_release

    validate_release(ROOT, document)
    ctx = context()
    project = document["project"]
    if (
        document["sourceRevision"] != ctx["sourceRevision"]
        or document["provenance"]["repositoryId"] != ctx["repositoryId"]
    ):
        raise PolicyError("release context mismatch")
    if document["provenance"].get("buildConfigSha256") != ctx["buildConfigSha256"]:
        raise PolicyError("release build configuration mismatch")
    repository = load_catalog()["infrastructureRepository"]
    branch = f"release/{project}/{ctx['sourceRevision']}-{ctx['runId']}-{os.environ.get('GITHUB_RUN_ATTEMPT', '1')}"
    update_release_branch(repository, branch, document)
    query = urllib.parse.urlencode(
        {"head": "fredrir:" + branch, "base": "main", "state": "open"}
    )
    pulls = api("GET", f"repos/{repository}/pulls?{query}")
    if pulls:
        print(pulls[0]["html_url"])
        return
    try:
        pull = api(
            "POST",
            f"repos/{repository}/pulls",
            {
                "title": f"Release {project}",
                "head": branch,
                "base": "main",
                "body": f"Collect verified component images from `{ctx['sourceRevision']}` in source run {ctx['runId']}.\n\nEach component is added after native builds, tests, scanning, signature and provenance verification.\n\nReview after every required component succeeds; infrastructure validation and health gates apply before activation.",
            },
        )
    except urllib.error.HTTPError as error:
        if error.code != 422:
            raise
        error.close()
        pulls = api("GET", f"repos/{repository}/pulls?{query}")
        if not pulls:
            raise
        pull = pulls[0]
    print(pull["html_url"])


def verify_release_file(path):
    document = json.loads(path.read_text())
    sys.path.insert(0, str(ROOT))
    from tools.infra.contracts import validate_release

    component = document.get("component", "app")
    if (
        path.resolve()
        != (ROOT / release_path(document["project"], component)).resolve()
    ):
        raise PolicyError("release project or component path mismatch")
    entry = validate_release(ROOT, document, document["project"])
    proof = document["provenance"]
    import yaml

    policy = yaml.safe_load(
        (
            ROOT / f".github/chainguard/{document['project']}-release.sts.yaml"
        ).read_text()
    )
    if (
        policy["claim_pattern"]["job_workflow_sha"]
        != "^" + proof["workflowRevision"] + "$"
    ):
        raise PolicyError("release workflow revision is not approved")
    ctx = {
        "repositoryId": proof["repositoryId"],
        "repository": entry["repository"],
        "sourceRevision": document["sourceRevision"],
        "workflowRevision": proof["workflowRevision"],
        "workflowRepository": proof["workflowRepository"],
        "runId": proof["runId"],
        "component": component,
    }
    if "buildConfigSha256" in proof:
        ctx["buildConfigSha256"] = proof["buildConfigSha256"]
    verify_image(document["image"], ctx, proof["platforms"])


def main():
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)
    builder = sub.add_parser("build")
    for name, default in (
        ("source", "source"),
        ("context", "."),
        ("dockerfile", "Dockerfile"),
        ("nix-target", ""),
        ("output", "artifacts"),
    ):
        builder.add_argument(f"--{name}", default=default)
    builder.add_argument("--project", required=True)
    builder.add_argument("--component", default="app")
    builder.add_argument("--architecture", choices=["amd64", "arm64"], required=True)
    publisher = sub.add_parser("publish")
    publisher.add_argument("--project", required=True)
    publisher.add_argument("--component", default="app")
    publisher.add_argument("--platforms", required=True)
    publisher.add_argument("--artifacts", default="artifacts")
    cache = sub.add_parser("upload-cache")
    cache.add_argument("--project", required=True)
    cache.add_argument("--component", default="app")
    cache.add_argument("--architecture", choices=["amd64", "arm64"], required=True)
    cache.add_argument("--artifacts", default="artifacts")
    release = sub.add_parser("release-document")
    release.add_argument("--project", required=True)
    release.add_argument("--component", default="app")
    release.add_argument("--platforms", required=True)
    release.add_argument("--image", required=True)
    release.add_argument("--output", default="release.json")
    request = sub.add_parser("request-release")
    request.add_argument("document")
    verify = sub.add_parser("verify-release")
    verify.add_argument("path", type=Path)
    args = parser.parse_args()
    {
        "build": build,
        "publish": publish,
        "upload-cache": upload_cache,
        "release-document": release_document,
        "request-release": request_release,
        "verify-release": lambda value: verify_release_file(value.path),
    }[args.command](args)


if __name__ == "__main__":
    try:
        main()
    except (
        PolicyError,
        ValueError,
        KeyError,
        subprocess.CalledProcessError,
        urllib.error.URLError,
    ) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
