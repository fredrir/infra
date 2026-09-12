import argparse
import copy
import gzip
import hashlib
import json
import os
import pwd
import re
import shlex
import socket
import stat
import subprocess
import tarfile
import tempfile
import time
from pathlib import Path

MAX_ARCHIVE_BYTES = 2 * 1024**3
MAX_JSON_BYTES = 4 * 1024**2
MAX_LAYER_BYTES = 2 * 1024**3
MAX_UNCOMPRESSED_BYTES = 4 * 1024**3
USERS = {"edge": 2000, "llunde-backend": 2001, "llunde-frontend": 2002}
SERVICE_USERS = {
    "llunde-postgres": "llunde-backend",
    "llunde-valkey": "llunde-backend",
    "llunde-backend": "llunde-backend",
    "llunde-frontend": "llunde-frontend",
    "caddy": "edge",
    "cloudflared": "edge",
}
CONTAINERS = {
    "caddy": "systemd-caddy",
    "cloudflared": "llunde-cloudflared",
    "llunde-frontend": "systemd-llunde-frontend",
}
DIGEST = re.compile(r"sha256:[a-f0-9]{64}")


class ImageError(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise ImageError(message)


def private_directory(path, owner, create=False):
    path = Path(path)
    require(not path.is_symlink(), "Private directory symlink forbidden")
    if create:
        path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.stat()
    require(
        stat.S_ISDIR(info.st_mode)
        and info.st_uid == owner
        and stat.S_IMODE(info.st_mode) == 0o700,
        "Private directory required",
    )
    return path.resolve()


def open_private(path, owner, maximum):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        require(
            stat.S_ISREG(info.st_mode)
            and info.st_uid == owner
            and info.st_nlink == 1
            and stat.S_IMODE(info.st_mode) in (0o400, 0o600),
            "Private regular file required",
        )
        require(0 < info.st_size <= maximum, "Private file exceeds size budget")
        return os.fdopen(fd, "rb")
    except BaseException:
        os.close(fd)
        raise


def read_json(path, owner):
    with open_private(path, owner, MAX_JSON_BYTES) as stream:
        return json.load(stream)


def write_json(path, document):
    with os.fdopen(
        os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), "w"
    ) as stream:
        json.dump(document, stream, indent=2)
        stream.write("\n")


def validate_plan(plan, service):
    require(service in SERVICE_USERS, "Unexpected service")
    require(
        plan["source"]["id"] == "fredrir-05"
        and plan["source"]["providerId"] == 132168416
        and plan["source"]["hostname"] == "llunde-01",
        "Source identity mismatch",
    )
    require(
        plan["target"]["id"] == "fredrir-09"
        and plan["target"]["hostname"] == "cloud-server-10643982"
        and plan["target"]["architecture"] == "amd64",
        "Target identity mismatch",
    )
    require(
        plan["authorization"]
        == {"targetInstall": True, "sourceStop": False, "cutover": False},
        "Dormant preparation scope required",
    )
    item = plan["services"][service]
    require(item["user"] == SERVICE_USERS[service], "Service user mismatch")
    require(
        re.fullmatch(
            r"(?:docker\.io|ghcr\.io)/[a-z0-9/-]+@sha256:[a-f0-9]{64}", item["image"]
        ),
        "Pinned source image required",
    )
    user = plan["users"][item["user"]]
    require(user["uid"] == user["gid"] == USERS[item["user"]], "Service UID mismatch")
    return item


def podman_command(user, arguments):
    require(user in USERS, "Unexpected service user")
    uid = USERS[user]
    return [
        "sudo",
        "-n",
        "-u",
        user,
        "env",
        f"XDG_RUNTIME_DIR=/run/user/{uid}",
        f"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/{uid}/bus",
        "podman",
        *arguments,
    ]


def child_environment():
    result = {"PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "LANG": "C", "LC_ALL": "C"}
    if "HOME" in os.environ:
        result["HOME"] = os.environ["HOME"]
    agent = os.environ.get("SSH_AUTH_SOCK")
    if agent:
        info = os.stat(agent)
        require(
            stat.S_ISSOCK(info.st_mode) and info.st_uid == os.geteuid(),
            "Private SSH agent socket required",
        )
        result["SSH_AUTH_SOCK"] = agent
    return result


def source_ssh(arguments, seconds=25):
    return [
        "ssh",
        "-o",
        "BatchMode=yes",
        "-o",
        "ConnectTimeout=10",
        "-o",
        "StrictHostKeyChecking=yes",
        "-o",
        "ForwardAgent=no",
        "-o",
        "ClearAllForwardings=yes",
        "-o",
        "RequestTTY=no",
        "fredrir-05",
        "cd /tmp && exec "
        + shlex.join(
            ["timeout", "--signal=TERM", "--kill-after=5s", f"{seconds}s", *arguments]
        ),
    ]


def source_command(user, arguments):
    return source_ssh(
        podman_command(user, arguments), seconds=210 if arguments[0] == "save" else 25
    )


def checked_run(arguments, *, input=None, timeout=30):
    result = subprocess.run(
        arguments,
        stdin=input,
        capture_output=True,
        timeout=timeout,
        cwd="/tmp",
        env=child_environment(),
    )
    require(result.returncode == 0, "Image command failed")
    return result.stdout


def source_identity(service, item):
    require(
        checked_run(source_ssh(["hostname"])).decode().strip() == "llunde-01",
        "Source host mismatch",
    )
    container = CONTAINERS.get(service, service)
    config = (
        checked_run(
            source_command(
                item["user"],
                ["container", "inspect", "--format", "{{.Image}}", container],
            )
        )
        .decode()
        .strip()
    )
    config = "sha256:" + config.removeprefix("sha256:")
    require(DIGEST.fullmatch(config), "Invalid source image identity")
    digests = json.loads(
        checked_run(
            source_command(
                item["user"],
                ["image", "inspect", "--format", "{{json .RepoDigests}}", config],
            )
        )
    )
    require(
        item["image"] in digests, "Running source image differs from reviewed digest"
    )
    return config


def verify_oci(stream, expected_config):
    require(DIGEST.fullmatch(expected_config), "Invalid config identity")
    stream.seek(0)
    archive_sha = hashlib.file_digest(stream, "sha256").hexdigest()
    stream.seek(0)
    with tarfile.open(fileobj=stream, mode="r:") as archive:
        members, blobs = {}, {}
        for member in archive:
            require(
                len(members) < 512 and member.name not in members,
                "Duplicate or excessive archive entries",
            )
            members[member.name] = member
            if member.isdir():
                require(
                    member.name in (".", "blobs", "blobs/sha256") and member.size == 0,
                    "Unexpected archive directory",
                )
                continue
            require(
                member.isfile()
                and not member.sparse
                and 0 <= member.size <= MAX_ARCHIVE_BYTES,
                "Archive links or unsupported files forbidden",
            )
            if member.name in ("oci-layout", "index.json"):
                require(member.size <= MAX_JSON_BYTES, "OCI metadata too large")
                continue
            require(
                re.fullmatch(r"blobs/sha256/[a-f0-9]{64}", member.name),
                "Unexpected archive path",
            )
            with archive.extractfile(member) as data:
                actual = hashlib.file_digest(data, "sha256").hexdigest()
            require(actual == member.name.rsplit("/", 1)[1], "OCI blob digest mismatch")
            blobs["sha256:" + actual] = member

        def document(name):
            require(
                name in members
                and members[name].isfile()
                and members[name].size <= MAX_JSON_BYTES,
                "Missing or oversized OCI metadata",
            )
            with archive.extractfile(members[name]) as data:
                return json.load(data)

        def descriptor(entry):
            digest = entry["digest"]
            require(
                DIGEST.fullmatch(digest)
                and digest in blobs
                and type(entry["size"]) is int
                and entry["size"] == blobs[digest].size,
                "OCI descriptor mismatch",
            )
            return digest

        require(
            document("oci-layout") == {"imageLayoutVersion": "1.0.0"},
            "Unsupported OCI layout",
        )
        index = document("index.json")
        require(
            index["schemaVersion"] == 2 and len(index["manifests"]) == 1,
            "One OCI image required",
        )
        manifest_digest = descriptor(index["manifests"][0])
        manifest = document("blobs/sha256/" + manifest_digest.removeprefix("sha256:"))
        require(manifest["schemaVersion"] == 2, "Unsupported image manifest")
        config = descriptor(manifest["config"])
        require(
            config == expected_config, "Archive config differs from source container"
        )
        configuration = document("blobs/sha256/" + config.removeprefix("sha256:"))
        require(
            configuration["architecture"] == "amd64" and configuration["os"] == "linux",
            "Image platform mismatch",
        )
        layers = manifest["layers"]
        rootfs = configuration["rootfs"]
        require(
            rootfs["type"] == "layers"
            and 0 < len(layers) <= 128
            and len(rootfs["diff_ids"]) == len(layers),
            "Image layer count mismatch",
        )
        referenced = {manifest_digest, config} | {descriptor(layer) for layer in layers}
        require(referenced == set(blobs), "Unreferenced or missing OCI blobs")
        total, deadline = 0, time.monotonic() + 120
        for layer, expected in zip(layers, rootfs["diff_ids"], strict=True):
            require(DIGEST.fullmatch(expected), "Invalid layer diffID")
            media = layer["mediaType"]
            require(
                media
                in (
                    "application/vnd.oci.image.layer.v1.tar",
                    "application/vnd.oci.image.layer.v1.tar+gzip",
                    "application/vnd.docker.image.rootfs.diff.tar.gzip",
                ),
                "Unsupported image layer encoding",
            )
            with archive.extractfile(blobs[layer["digest"]]) as raw:
                data = gzip.GzipFile(fileobj=raw) if media.endswith("gzip") else raw
                checksum, size = hashlib.sha256(), 0
                try:
                    while chunk := data.read(1024**2):
                        size += len(chunk)
                        total += len(chunk)
                        require(
                            size <= MAX_LAYER_BYTES
                            and total <= MAX_UNCOMPRESSED_BYTES
                            and time.monotonic() < deadline,
                            "Uncompressed image exceeds time or size budget",
                        )
                        checksum.update(chunk)
                finally:
                    if data is not raw:
                        data.close()
            require(
                "sha256:" + checksum.hexdigest() == expected,
                "Image layer diffID mismatch",
            )
    return archive_sha


def docker_members(archive, expected_config, *, compatibility=False):
    members = {}
    for member in archive:
        require(
            len(members) < 512
            and member.name not in members
            and 0 <= member.size <= MAX_ARCHIVE_BYTES,
            "Duplicate or excessive Docker archive entries",
        )
        members[member.name] = member

    def read(name):
        require(
            name in members
            and members[name].isfile()
            and not members[name].sparse
            and members[name].size <= MAX_JSON_BYTES,
            "Missing or oversized Docker metadata",
        )
        with archive.extractfile(members[name]) as data:
            return data.read()

    manifest = json.loads(read("manifest.json"))
    require(
        isinstance(manifest, list)
        and len(manifest) == 1
        and set(manifest[0]) == {"Config", "RepoTags", "Layers"},
        "One Docker image required",
    )
    record = manifest[0]
    require(record["RepoTags"] in (None, []), "Archive must not assign registry tags")
    require(
        DIGEST.fullmatch(expected_config)
        and record["Config"] == expected_config.removeprefix("sha256:") + ".json",
        "Docker config identity mismatch",
    )
    raw = read(record["Config"])
    require(
        "sha256:" + hashlib.sha256(raw).hexdigest() == expected_config,
        "Docker config checksum mismatch",
    )
    config = json.loads(raw)
    require(
        config["architecture"] == "amd64"
        and config["os"] == "linux"
        and config["rootfs"]["type"] == "layers",
        "Docker image platform mismatch",
    )
    diff_ids, layers = config["rootfs"]["diff_ids"], record["Layers"]
    require(
        0 < len(layers) <= 128 and len(layers) == len(diff_ids),
        "Docker layer count mismatch",
    )
    total, deadline = 0, time.monotonic() + 120
    for name, expected in zip(layers, diff_ids, strict=True):
        require(
            DIGEST.fullmatch(expected)
            and name == expected.removeprefix("sha256:") + ".tar",
            "Docker layer name mismatch",
        )
        require(
            name in members
            and members[name].isfile()
            and not members[name].sparse
            and members[name].size <= MAX_LAYER_BYTES,
            "Docker layer file mismatch",
        )
        total += members[name].size
        require(
            total <= MAX_UNCOMPRESSED_BYTES and time.monotonic() < deadline,
            "Docker layer budget exceeded",
        )
        with archive.extractfile(members[name]) as data:
            require(
                "sha256:" + hashlib.file_digest(data, "sha256").hexdigest() == expected,
                "Docker layer diffID mismatch",
            )
    selected = {"manifest.json", record["Config"], *layers}
    for name, member in members.items():
        if name in selected:
            continue
        require(compatibility, "Unexpected Docker archive entry")
        if member.issym():
            require(
                re.fullmatch(r"[a-f0-9]{64}/layer\.tar", name)
                and member.linkname in {"../" + layer for layer in layers},
                "Unexpected Docker compatibility link",
            )
        else:
            require(
                member.isfile()
                and not member.sparse
                and member.size <= MAX_JSON_BYTES
                and (
                    name == "repositories"
                    or re.fullmatch(r"[a-f0-9]{64}/(?:VERSION|json)", name)
                ),
                "Unexpected Docker compatibility metadata",
            )
    return {name: members[name] for name in sorted(selected)}


def normalize_docker(source, destination, expected_config, owner):
    with (
        open_private(source, owner, MAX_ARCHIVE_BYTES) as stream,
        tarfile.open(fileobj=stream, mode="r:") as archive,
    ):
        selected = docker_members(archive, expected_config, compatibility=True)
        with (
            os.fdopen(
                os.open(
                    destination,
                    os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                    0o600,
                ),
                "wb",
            ) as output,
            tarfile.open(fileobj=output, mode="w", format=tarfile.PAX_FORMAT) as target,
        ):
            for name, member in selected.items():
                clean = tarfile.TarInfo(name)
                clean.size, clean.mode = member.size, 0o600
                with archive.extractfile(member) as data:
                    target.addfile(clean, data)


def verify_docker(stream, expected_config):
    stream.seek(0)
    checksum = hashlib.file_digest(stream, "sha256").hexdigest()
    stream.seek(0)
    with tarfile.open(fileobj=stream, mode="r:") as archive:
        docker_members(archive, expected_config)
    return checksum


def verify_bundle(directory, plan, service, owner=None):
    owner = os.geteuid() if owner is None else owner
    directory = private_directory(directory, owner)
    manifest = read_json(directory / "manifest.json", owner)
    filename = manifest.get("archiveFile", "image.oci.tar")
    format = manifest.get("archiveFormat", "oci-archive")
    require(
        (filename, format)
        in (("image.oci.tar", "oci-archive"), ("image.tar", "docker-archive")),
        "Unsupported archive format",
    )
    require(
        {p.name for p in directory.iterdir()} == {filename, "manifest.json"},
        "Unexpected bundle files",
    )
    item = validate_plan(plan, service)
    require(
        manifest["schemaVersion"] == 1
        and manifest["kind"] == "standalone-image-archive"
        and manifest["service"] == service
        and manifest["user"] == item["user"]
        and manifest["source"] == "fredrir-05"
        and manifest["target"] == "fredrir-09"
        and manifest["sourceImage"] == item["image"],
        "Image bundle identity mismatch",
    )
    with open_private(directory / filename, owner, MAX_ARCHIVE_BYTES) as stream:
        require(
            os.fstat(stream.fileno()).st_size == manifest["archiveBytes"],
            "Image archive size mismatch",
        )
        verifier = verify_docker if format == "docker-archive" else verify_oci
        require(
            verifier(stream, manifest["configID"]) == manifest["archiveSHA256"],
            "Image archive checksum mismatch",
        )
    return manifest


def export_image(directory, service):
    root = private_directory(directory, os.geteuid())
    plan = read_json(root / "staging.json", os.geteuid())
    item = validate_plan(plan, service)
    config = source_identity(service, item)
    images = private_directory(root / "images", os.geteuid(), create=True)
    destination = images / service
    if destination.exists() or destination.is_symlink():
        manifest = verify_bundle(destination, plan, service)
        require(
            manifest["configID"] == config,
            "Existing archive has different source identity",
        )
        return {"service": service, "configID": config, "changed": False}
    source_type = (
        checked_run(
            source_command(
                item["user"],
                ["image", "inspect", "--format", "{{.ManifestType}}", config],
            )
        )
        .decode()
        .strip()
    )
    formats = {
        "application/vnd.oci.image.manifest.v1+json": "oci-archive",
        "application/vnd.docker.distribution.manifest.v2+json": "docker-archive",
    }
    require(source_type in formats, "Unsupported source manifest type")
    format = formats[source_type]
    filename = "image.oci.tar" if format == "oci-archive" else "image.tar"
    with tempfile.TemporaryDirectory(dir=images) as temporary:
        target = Path(temporary)
        source_archive, archive = target / "source.tar", target / filename
        arguments = ["save", "--quiet", "--format", format]
        if format == "oci-archive":
            arguments.append("--uncompressed")
        arguments.append(config)
        with (
            os.fdopen(
                os.open(source_archive, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600),
                "wb",
            ) as output,
            tempfile.TemporaryFile() as errors,
        ):
            process = subprocess.Popen(
                source_command(item["user"], arguments),
                stdin=subprocess.DEVNULL,
                stdout=output,
                stderr=errors,
                env=child_environment(),
            )
            deadline = time.monotonic() + 240
            try:
                while True:
                    require(
                        time.monotonic() < deadline
                        and source_archive.stat().st_size <= MAX_ARCHIVE_BYTES,
                        "Image export exceeds time or size budget",
                    )
                    try:
                        require(
                            process.wait(timeout=0.5) == 0, "Source image export failed"
                        )
                        break
                    except subprocess.TimeoutExpired:
                        continue
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()
        require(
            source_identity(service, item) == config,
            "Source changed during image export",
        )
        if format == "docker-archive":
            normalize_docker(source_archive, archive, config, os.geteuid())
            source_archive.unlink()
        else:
            source_archive.rename(archive)
        with open_private(archive, os.geteuid(), MAX_ARCHIVE_BYTES) as stream:
            checksum = (verify_docker if format == "docker-archive" else verify_oci)(
                stream, config
            )
        manifest = {
            "schemaVersion": 1,
            "kind": "standalone-image-archive",
            "source": "fredrir-05",
            "target": "fredrir-09",
            "service": service,
            "user": item["user"],
            "sourceImage": item["image"],
            "configID": config,
            "sourceManifestType": source_type,
            "archiveFile": filename,
            "archiveFormat": format,
            "archiveBytes": archive.stat().st_size,
            "archiveSHA256": checksum,
            "createdAt": int(time.time()),
        }
        write_json(target / "manifest.json", manifest)
        verify_bundle(target, plan, service)
        os.rename(target, destination)
    return {
        "service": service,
        "configID": config,
        "archiveBytes": manifest["archiveBytes"],
        "changed": True,
    }


def load_image(directory, plan_path, service):
    require(
        os.geteuid() == 0 and socket.gethostname() == "cloud-server-10643982",
        "Verified target root required",
    )
    plan_path = Path(plan_path)
    require(
        plan_path == Path("/var/lib/infra-evacuation/llunde/staging.json"),
        "Installed staging metadata required",
    )
    plan = read_json(plan_path, 0)
    item = validate_plan(plan, service)
    for marker in (
        "stage-approved",
        "restore-approved",
        "source-fenced",
        "edge-approved",
    ):
        path = plan_path.parent / marker
        require(
            not path.exists() and not path.is_symlink(),
            "Image preload requires dormant applications",
        )
    account = pwd.getpwnam(item["user"])
    require(
        account.pw_uid == account.pw_gid == USERS[item["user"]]
        and account.pw_dir == f"/home/{item['user']}",
        "Service account mismatch",
    )
    runtime = Path(f"/run/user/{account.pw_uid}")
    private_directory(runtime, account.pw_uid)
    bus = (runtime / "bus").stat()
    require(
        stat.S_ISSOCK(bus.st_mode) and bus.st_uid == account.pw_uid,
        "Explicit prepared user session required",
    )
    manifest = verify_bundle(directory, plan, service, owner=0)
    require(
        not checked_run(
            podman_command(item["user"], ["ps", "--all", "--format", "{{.Names}}"])
        ).strip(),
        "Reserved image store contains containers",
    )
    present = subprocess.run(
        podman_command(item["user"], ["image", "exists", manifest["configID"]]),
        capture_output=True,
        timeout=30,
        cwd="/tmp",
        env=child_environment(),
    )
    require(present.returncode in (0, 1), "Image store unavailable")
    if present.returncode == 1:
        with open_private(
            Path(directory) / manifest.get("archiveFile", "image.oci.tar"),
            0,
            MAX_ARCHIVE_BYTES,
        ) as stream:
            checked_run(
                podman_command(item["user"], ["load", "--quiet"]),
                input=stream,
                timeout=180,
            )
    observed = json.loads(
        checked_run(
            podman_command(
                item["user"],
                [
                    "image",
                    "inspect",
                    "--format",
                    '{"id":"{{.Id}}","architecture":"{{.Architecture}}","os":"{{.Os}}"}',
                    manifest["configID"],
                ],
            )
        )
    )
    require(
        "sha256:" + observed["id"].removeprefix("sha256:") == manifest["configID"]
        and observed["architecture"] == "amd64"
        and observed["os"] == "linux",
        "Loaded image identity mismatch",
    )
    return {
        "service": service,
        "configID": manifest["configID"],
        "changed": present.returncode == 1,
        "applicationStarted": False,
    }


def translate_candidates(directory, destination):
    root = private_directory(directory, os.geteuid())
    plan = read_json(root / "staging.json", os.geteuid())
    require(set(plan["services"]) == set(SERVICE_USERS), "All six services required")
    receipts = {
        service: verify_bundle(root / "images" / service, plan, service)
        for service in SERVICE_USERS
    }
    expected = {
        f"units/{user}/{service}.container" for service, user in SERVICE_USERS.items()
    } | {"units/Caddyfile", "units/llunde-backend/llunde-backend-data.network"}
    require(set(plan["unitSHA256"]) == expected, "Unexpected candidate inventory")
    require(
        not any(path.is_symlink() for path in (root / "units").rglob("*")),
        "Candidate symlink forbidden",
    )
    require(
        {
            str(path.relative_to(root))
            for path in (root / "units").rglob("*")
            if not path.is_dir()
        }
        == expected,
        "Unexpected candidate files",
    )
    translated, contents = copy.deepcopy(plan), {}
    for name in sorted(expected):
        path = root / name
        require(path.resolve().is_relative_to(root), "Candidate path escaped")
        with open_private(path, os.geteuid(), MAX_JSON_BYTES) as stream:
            data = stream.read()
        require(
            hashlib.sha256(data).hexdigest() == plan["unitSHA256"][name],
            "Candidate checksum mismatch",
        )
        if path.suffix == ".container":
            service = path.stem
            image = plan["services"][service]["image"]
            text = data.decode()
            require(
                text.count(f"Image={image}\n") == 1 and "\nPull=" not in text,
                "Expected source image reference required",
            )
            config = receipts[service]["configID"]
            text = text.replace(f"Image={image}\n", f"Image={config}\nPull=never\n")
            text = "\n".join(
                line
                for line in text.split("\n")
                if not line.startswith(
                    "Environment=REGISTRY_AUTH_FILE=/run/infra-evacuation/llunde/"
                )
            )
            require("REGISTRY_AUTH_FILE" not in text, "Unexpected registry authority")
            data = text.encode()
            translated["services"][service]["runtimeImage"] = config
        contents[name] = data
        translated["unitSHA256"][name] = hashlib.sha256(data).hexdigest()
    translated["imageArchives"] = receipts
    translated["sourceCandidateSHA256"] = hashlib.sha256(
        (root / "staging.json").read_bytes()
    ).hexdigest()
    destination = Path(destination)
    require(
        not destination.exists() and not destination.is_symlink(),
        "Fresh candidate destination required",
    )
    parent = private_directory(destination.parent, os.geteuid())
    with tempfile.TemporaryDirectory(dir=parent) as temporary:
        candidate = Path(temporary)
        for name, data in contents.items():
            output = candidate / name
            output.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            with os.fdopen(
                os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb"
            ) as stream:
                stream.write(data)
        write_json(candidate / "staging.json", translated)
        os.rename(candidate, destination)
    return {
        "translatedServices": len(receipts),
        "registryCredentialsRequired": False,
        "applicationStarted": False,
        "sourceCandidateSHA256": translated["sourceCandidateSHA256"],
    }


def main(argv=None):
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    export = commands.add_parser("export")
    export.add_argument("directory")
    export.add_argument("service", choices=SERVICE_USERS)
    verify = commands.add_parser("verify")
    verify.add_argument("directory")
    verify.add_argument("plan")
    verify.add_argument("service", choices=SERVICE_USERS)
    load = commands.add_parser("load")
    load.add_argument("directory")
    load.add_argument("service", choices=SERVICE_USERS)
    translate = commands.add_parser("translate")
    translate.add_argument("directory")
    translate.add_argument("destination")
    args = parser.parse_args(argv)
    try:
        if args.command == "export":
            result = export_image(args.directory, args.service)
        elif args.command == "verify":
            result = verify_bundle(
                args.directory, read_json(args.plan, os.geteuid()), args.service
            )
        elif args.command == "translate":
            result = translate_candidates(args.directory, args.destination)
        else:
            result = load_image(
                args.directory,
                "/var/lib/infra-evacuation/llunde/staging.json",
                args.service,
            )
        print(json.dumps(result))
        return 0
    except (
        ImageError,
        OSError,
        ValueError,
        KeyError,
        TypeError,
        EOFError,
        tarfile.TarError,
        subprocess.SubprocessError,
    ):
        print(
            json.dumps(
                {"error": "Image identity, archive, or command verification failed"}
            )
        )
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
