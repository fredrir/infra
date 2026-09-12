import argparse
import copy
import grp
import hashlib
import json
import os
import re
import stat
import subprocess
import tempfile
from pathlib import Path


class StagingError(ValueError):
    pass


USERS = {"edge": 2000, "llunde-backend": 2001, "llunde-frontend": 2002}
SERVICE_USERS = {
    "llunde-postgres": "llunde-backend",
    "llunde-valkey": "llunde-backend",
    "llunde-backend": "llunde-backend",
    "llunde-frontend": "llunde-frontend",
    "caddy": "edge",
    "cloudflared": "edge",
}
SECRETS = {
    "doppler": (
        "llunde-backend",
        "doppler.env",
        {"DOPPLER_TOKEN"},
        "doppler.yaml",
        "doppler_token",
    ),
    "database": (
        "llunde-backend",
        "db.env",
        {"POSTGRES_PASSWORD", "DB_PASSWORD"},
        "llunde-backend-db.yaml",
        "env",
    ),
    "tunnel": ("edge", "tunnel.env", {"TUNNEL_TOKEN"}, "llunde-tunnel.yaml", "env"),
}
DATA_NETWORK = "llunde-backend-data.network"
EGRESS_NETWORK = "llunde-backend-egress.network"
EGRESS_CONTENT = "[Network]\nDriver=bridge\nInternal=false\nDisableDNS=false\n"


def require(condition, message):
    if not condition:
        raise StagingError(message)


def read_private(path, owner=0):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        require(
            stat.S_ISREG(info.st_mode) and info.st_uid == owner and info.st_nlink == 1,
            "Private regular file required",
        )
        require(
            stat.S_IMODE(info.st_mode) in (0o400, 0o600),
            "Private file permissions required",
        )
        require(info.st_size <= 65536, "Private file too large")
        with os.fdopen(fd, "rb", closefd=False) as handle:
            return handle.read()
    finally:
        os.close(fd)


def candidate_files(*, legacy_networks=False):
    return (
        {f"units/{user}/{name}.container" for name, user in SERVICE_USERS.items()}
        | {"units/Caddyfile", f"units/llunde-backend/{DATA_NETWORK}"}
        | (set() if legacy_networks else {f"units/llunde-backend/{EGRESS_NETWORK}"})
    )


def unit_fields(text, section):
    current, fields = None, {}
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith(("#", ";")):
            continue
        require(not line.endswith("\\"), "Continued network settings forbidden")
        if line.startswith("["):
            current = line[1:-1] if line.endswith("]") else None
        elif current == section or section is None:
            key, separator, value = line.partition("=")
            require(separator, "Invalid unit setting")
            fields.setdefault(key.strip(), []).append(value.strip())
    return fields


def validate_networks(root, *, legacy_networks=False):
    root = Path(root)
    for name, internal in ((DATA_NETWORK, "true"), (EGRESS_NETWORK, "false")):
        if legacy_networks and name == EGRESS_NETWORK:
            continue
        text = (root / "units/llunde-backend" / name).read_text()
        require(
            not {"PodmanArgs", "GlobalArgs"} & set(unit_fields(text, None)),
            "Unvalidated bridge overrides forbidden",
        )
        fields = unit_fields(text, "Network")
        require(
            set(fields) <= {"Driver", "Internal", "DisableDNS"}
            and fields.get("Driver", ["bridge"]) == ["bridge"]
            and fields.get("Internal") == [internal]
            and fields.get("DisableDNS", ["false"]) == ["false"],
            "Bridge isolation and enabled DNS are required",
        )
        if name == EGRESS_NETWORK:
            require(
                fields.get("DisableDNS") == ["false"], "Explicit egress DNS required"
            )
    for service, user in SERVICE_USERS.items():
        text = (root / f"units/{user}/{service}.container").read_text()
        require(
            not {"PodmanArgs", "GlobalArgs", "Pod"} & set(unit_fields(text, None)),
            "Unvalidated container network overrides forbidden",
        )
        fields = unit_fields(text, "Container")
        networks = fields.get("Network", [])
        if service == "llunde-backend":
            expected = [DATA_NETWORK, "podman" if legacy_networks else EGRESS_NETWORK]
            require(
                networks == expected,
                "Backend requires internal data and DNS-enabled egress networks",
            )
            require(
                not {"DNS", "DNSOption", "DNSSearch"} & set(fields),
                "Backend must use its registered network resolver",
            )
        elif service in ("llunde-postgres", "llunde-valkey"):
            require(
                networks == [DATA_NETWORK],
                "Datastores must remain on the internal network only",
            )
        else:
            require(
                not {DATA_NETWORK, EGRESS_NETWORK, "podman"} & set(networks),
                "Backend networks cannot be shared with other services",
            )


def validate_plan(directory, *, legacy_networks=False):
    root = Path(directory).resolve()
    plan = json.loads((root / "staging.json").read_text())
    require(
        plan["source"]["id"] == "fredrir-05"
        and plan["source"]["providerId"] == 132168416,
        "Source identity mismatch",
    )
    require(
        plan["target"]["id"] == "fredrir-09"
        and plan["target"]["architecture"] == "amd64",
        "Target identity mismatch",
    )
    require(
        plan["authorization"]["targetInstall"] is True,
        "Dormant preparation scope missing",
    )
    require(
        not plan["authorization"]["sourceStop"]
        and not plan["authorization"]["cutover"],
        "Dormant adapter cannot perform cutover",
    )
    require(set(plan["users"]) == set(USERS), "Unexpected service users")
    ranges = [(100000, 165536)]
    for name, uid in USERS.items():
        user = plan["users"][name]
        require(
            user["uid"] == uid and user["gid"] == uid and user["subIdCount"] == 65536,
            "Service identity mismatch",
        )
        bounds = (
            user["targetSubIdStart"],
            user["targetSubIdStart"] + user["subIdCount"],
        )
        require(
            bounds[0] >= 165536
            and not any(bounds[0] < end and start < bounds[1] for start, end in ranges),
            "Overlapping subordinate ranges",
        )
        ranges.append(bounds)
    require(set(plan["services"]) == set(SERVICE_USERS), "Six pinned services required")
    runtime_images = any(
        "runtimeImage" in service for service in plan["services"].values()
    )
    if runtime_images:
        require(
            set(plan["imageArchives"]) == set(SERVICE_USERS),
            "Six verified image receipts required",
        )
    for name, service in plan["services"].items():
        require(service["user"] == SERVICE_USERS[name], "Service ownership mismatch")
        require(
            type(service["memoryMaxBytes"]) is int
            and 0 < service["memoryMaxBytes"] <= 1024**3,
            "Invalid service memory cap",
        )
        if runtime_images:
            receipt = plan["imageArchives"][name]
            require(
                re.fullmatch(r"sha256:[a-f0-9]{64}", service.get("runtimeImage", ""))
                is not None,
                "Pinned runtime configID required",
            )
            require(
                receipt["schemaVersion"] == 1
                and receipt["kind"] == "standalone-image-archive"
                and receipt["source"] == "fredrir-05"
                and receipt["target"] == "fredrir-09"
                and receipt["service"] == name
                and receipt["user"] == service["user"]
                and receipt["sourceImage"] == service["image"]
                and receipt["configID"] == service["runtimeImage"],
                "Runtime image receipt mismatch",
            )
            require(
                re.fullmatch(r"[a-f0-9]{64}", receipt["archiveSHA256"]) is not None,
                "Archive checksum required",
            )
    total = sum(service["memoryMaxBytes"] for service in plan["services"].values())
    require(
        total == plan["capacity"]["preservedAggregateServiceMemoryMaxBytes"]
        and total <= 4 * 1024**3,
        "Memory budget mismatch",
    )
    for service in plan["services"].values():
        require(
            re.fullmatch(
                r"(?:docker\.io|ghcr\.io)/[a-z0-9/-]+@sha256:[a-f0-9]{64}",
                service["image"],
            )
            is not None,
            "Pinned source image required",
        )
    expected_files = candidate_files(legacy_networks=legacy_networks)
    actual_files = {
        str(path.relative_to(root))
        for path in (root / "units").rglob("*")
        if not path.is_dir()
    }
    require(
        set(plan["unitSHA256"]) == expected_files == actual_files,
        "Unexpected candidate files",
    )
    require(
        not any(path.is_symlink() for path in (root / "units").rglob("*")),
        "Candidate symlink forbidden",
    )
    for relative, expected in plan["unitSHA256"].items():
        path = root / relative
        require(
            relative.startswith("units/")
            and path.resolve().is_relative_to(root)
            and not path.is_symlink(),
            "Candidate path escaped",
        )
        require(
            hashlib.sha256(path.read_bytes()).hexdigest() == expected,
            "Candidate digest mismatch",
        )
        if path.suffix == ".container":
            text = path.read_text()
            service = plan["services"][path.stem]
            require(
                f"Image={service.get('runtimeImage', service['image'])}\n" in text,
                "Candidate image mismatch",
            )
            if runtime_images:
                require(
                    "\nPull=never\n" in text and "REGISTRY_AUTH_FILE" not in text,
                    "Runtime image must use only preloaded content",
                )
            require(
                f"MemoryMax={service['memoryMaxBytes'] // 1024**2}M\n" in text,
                "Candidate memory cap mismatch",
            )
            require(
                "ConditionPathExists=/var/lib/infra-evacuation/llunde/stage-approved"
                in text,
                "Dormant checkpoint missing",
            )
            require(
                "AutoUpdate=" not in text and ":latest" not in text,
                "Automatic updates forbidden during relocation",
            )
    backend = (root / "units/llunde-backend/llunde-backend.container").read_text()
    validate_networks(root, legacy_networks=legacy_networks)
    require(
        "ConditionPathExists=/var/lib/infra-evacuation/llunde/source-fenced" in backend,
        "Source fence checkpoint missing",
    )
    slices = plan["capacity"]["userSliceMemoryMaxMiB"]
    require(
        slices == {"edge": 768, "llunde-backend": 2560, "llunde-frontend": 768},
        "Unexpected user memory budget",
    )
    return plan


def upgrade_network_contents(contents):
    require(
        set(contents) == candidate_files(legacy_networks=True),
        "Exact legacy candidate inventory required",
    )
    contents = dict(contents)
    backend = "units/llunde-backend/llunde-backend.container"
    require(
        contents[backend].count(b"\nNetwork=podman\n") == 1,
        "Exact legacy outbound binding required",
    )
    contents[backend] = contents[backend].replace(
        b"\nNetwork=podman\n", f"\nNetwork={EGRESS_NETWORK}\n".encode()
    )
    contents[f"units/llunde-backend/{EGRESS_NETWORK}"] = EGRESS_CONTENT.encode()
    return contents


def prepare_networks(directory, destination, source_sha256):
    root, destination = Path(directory).absolute(), Path(destination).absolute()
    require(
        re.fullmatch(r"[a-f0-9]{64}", source_sha256), "Pinned source candidate required"
    )
    source = read_private(root / "staging.json", os.geteuid())
    require(
        hashlib.sha256(source).hexdigest() == source_sha256, "Source candidate changed"
    )
    plan = validate_plan(root, legacy_networks=True)
    require(json.loads(source) == plan, "Source metadata changed during validation")
    contents = {
        name: read_private(root / name, os.geteuid())
        for name in candidate_files(legacy_networks=True)
    }
    require(
        all(
            hashlib.sha256(data).hexdigest() == plan["unitSHA256"][name]
            for name, data in contents.items()
        ),
        "Candidate bytes changed",
    )
    contents = upgrade_network_contents(contents)
    updated = copy.deepcopy(plan)
    updated["sourceNetworkCandidateSHA256"] = source_sha256
    updated["unitSHA256"] = {
        name: hashlib.sha256(data).hexdigest()
        for name, data in sorted(contents.items())
    }
    require(not os.path.lexists(destination), "Fresh network candidate required")
    parent = destination.parent.lstat()
    require(
        stat.S_ISDIR(parent.st_mode)
        and parent.st_uid == os.geteuid()
        and stat.S_IMODE(parent.st_mode) == 0o700,
        "Private candidate output parent required",
    )
    with tempfile.TemporaryDirectory(dir=destination.parent) as temporary:
        candidate = Path(temporary)
        for name, data in contents.items():
            output = candidate / name
            output.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            with os.fdopen(
                os.open(
                    output, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600
                ),
                "wb",
            ) as stream:
                stream.write(data)
        with os.fdopen(
            os.open(
                candidate / "staging.json", os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600
            ),
            "w",
        ) as stream:
            json.dump(updated, stream, indent=2, sort_keys=True)
            stream.write("\n")
        validate_plan(candidate)
        require(not os.path.lexists(destination), "Candidate destination appeared")
        os.rename(candidate, destination)
    return {
        "sourceCandidateSHA256": source_sha256,
        "unitCount": len(contents),
        "applicationStarted": False,
        "candidate": str(destination),
    }


def validate_accounts(plan, passwd_text, group_text, subuid_text, subgid_text):
    for text, id_index in ((passwd_text, 2), (group_text, 2)):
        for line in text.splitlines():
            fields = line.split(":")
            if len(fields) <= id_index:
                continue
            name, number = fields[0], int(fields[id_index])
            if name in USERS or number in USERS.values():
                require(
                    name in USERS and USERS[name] == number,
                    "Service UID or GID collision",
                )
                if id_index == 2 and len(fields) >= 7:
                    require(
                        int(fields[3]) == USERS[name] and fields[5] == f"/home/{name}",
                        "Service account ownership mismatch",
                    )
    for text in (subuid_text, subgid_text):
        entries = [line.split(":") for line in text.splitlines() if line.strip()]
        for name, user in plan["users"].items():
            start, end = (
                user["targetSubIdStart"],
                user["targetSubIdStart"] + user["subIdCount"],
            )
            own = [entry for entry in entries if entry[0] == name]
            require(
                len(own) <= 1
                and (not own or own[0][1:] == [str(start), str(end - start)]),
                "Existing subordinate mapping differs",
            )
            for other, other_start, count in entries:
                if other != name:
                    other_start, count = int(other_start), int(count)
                    require(
                        not (start < other_start + count and other_start < end),
                        "Subordinate range overlaps another account",
                    )


def run_private(argv, data=None):
    result = subprocess.run(argv, input=data, capture_output=True, timeout=30)
    require(result.returncode == 0, "Private command failed")
    return result.stdout


def owned_directory(path, owner, mode):
    path = Path(path)
    require(not path.is_symlink(), "Directory symlink forbidden")
    created = False
    try:
        path.mkdir(mode=mode)
        created = True
    except FileExistsError:
        pass
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        require(info.st_uid == owner, "Directory owner mismatch")
        if created:
            os.fchmod(fd, mode)
        info = os.fstat(fd)
        require(
            stat.S_ISDIR(info.st_mode) and stat.S_IMODE(info.st_mode) == mode,
            "Directory permissions mismatch",
        )
    finally:
        os.close(fd)
    return path


def prepare_data_child(parent_fd, owner, group):
    created = False
    try:
        os.mkdir("data", mode=0o700, dir_fd=parent_fd)
        created = True
    except FileExistsError:
        pass
    fd = os.open("data", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent_fd)
    try:
        info = os.fstat(fd)
        if created:
            require(
                info.st_uid == os.geteuid()
                and info.st_gid == os.getegid()
                and not os.listdir(fd),
                "New data parent ownership or contents changed",
            )
            os.fchmod(fd, 0o700)
            os.fchown(fd, owner, group)
            os.fsync(fd)
            os.fsync(parent_fd)
        info = os.fstat(fd)
        require(
            info.st_uid == owner
            and info.st_gid == group
            and stat.S_IMODE(info.st_mode) == 0o700,
            "Existing data parent ownership or mode mismatch",
        )
        current = os.stat("data", dir_fd=parent_fd, follow_symlinks=False)
        require(
            (current.st_dev, current.st_ino) == (info.st_dev, info.st_ino),
            "Data parent was replaced",
        )
        return {
            "created": created,
            "uid": info.st_uid,
            "gid": info.st_gid,
            "mode": "0700",
            "device": info.st_dev,
            "inode": info.st_ino,
        }
    finally:
        os.close(fd)


def prepare_data_parent():
    require(os.geteuid() == 0, "Root data preparation required")
    fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for name, owner in ((None, 0), ("home", 0), ("llunde-backend", 2001)):
            if name is not None:
                child = os.open(
                    name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd
                )
                os.close(fd)
                fd = child
            info = os.fstat(fd)
            require(
                info.st_uid == owner
                and info.st_gid == owner
                and not stat.S_IMODE(info.st_mode) & 0o022,
                "Data parent ancestry is untrusted",
            )
        return prepare_data_child(fd, 2001, 2001)
    finally:
        os.close(fd)


def write_private(path, content, owner):
    path = Path(path)
    if path.exists() or path.is_symlink():
        read_private(path, owner)
    fd, temporary = tempfile.mkstemp(dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    finally:
        Path(temporary).unlink(missing_ok=True)


def host_key(path):
    path = Path(path)
    require(os.geteuid() == 0, "Root required")
    owned_directory(path.parent, 0, 0o700)
    created = False
    if not path.exists() and not path.is_symlink():
        with tempfile.TemporaryDirectory(dir=path.parent) as directory:
            temporary = Path(directory) / "age.key"
            run_private(["age-keygen", "-o", str(temporary)])
            temporary.chmod(0o400)
            os.link(temporary, path)
            created = True
    read_private(path)
    recipient = run_private(["age-keygen", "-y", str(path)]).decode().strip()
    require(
        re.fullmatch(r"age1[0-9a-z]{58}", recipient) is not None,
        "Invalid host recipient",
    )
    return {"recipient": recipient, "created": created}


def validate_environment(data, expected):
    require(0 < len(data) <= 16384 and b"\0" not in data, "Invalid runtime environment")
    entries = {}
    for line in data.decode().splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        key, separator, value = line.partition("=")
        require(
            separator and key in expected and value.strip() and key not in entries,
            "Unexpected runtime environment keys",
        )
        entries[key] = value
    require(set(entries) == expected, "Required runtime environment keys missing")


def prepare_secrets(repository, recipient, destination):
    require(
        re.fullmatch(r"age1[0-9a-z]{58}", recipient) is not None,
        "Invalid host recipient",
    )
    root, output = (
        Path(repository).resolve(),
        owned_directory(destination, os.geteuid(), 0o700),
    )
    allowed = {f"{name}.age" for name in SECRETS} | {"manifest.json"}
    require(
        {path.name for path in output.iterdir()} <= allowed, "Unexpected bundle files"
    )
    existing = None
    if (output / "manifest.json").exists() or (output / "manifest.json").is_symlink():
        existing = json.loads(read_private(output / "manifest.json", os.geteuid()))
        require(
            set(existing["files"]) == allowed - {"manifest.json"},
            "Unexpected manifest files",
        )
    source_hashes = {
        f"{name}.age": hashlib.sha256(
            (root / "secrets" / source).read_bytes()
        ).hexdigest()
        for name, (_, _, _, source, _) in SECRETS.items()
    }
    if (
        existing
        and existing["recipient"] == recipient
        and all(
            existing["files"][name]["sourceSHA256"] == digest
            and existing["files"][name]["sha256"]
            == hashlib.sha256(read_private(output / name, os.geteuid())).hexdigest()
            for name, digest in source_hashes.items()
        )
    ):
        return {
            "encryptedFiles": len(SECRETS),
            "recipient": recipient,
            "changed": False,
        }
    manifest = {"recipient": recipient, "files": {}}
    encrypted = []
    for name, (_, _, expected, source, key) in SECRETS.items():
        path = root / "secrets" / source
        plaintext = run_private(
            ["sops", "--decrypt", "--extract", json.dumps([key]), str(path)]
        )
        validate_environment(plaintext, expected)
        ciphertext = run_private(["age", "--recipient", recipient], plaintext)
        target = output / f"{name}.age"
        if target.exists() or target.is_symlink():
            read_private(target, os.geteuid())
        encrypted.append((target, ciphertext))
        manifest["files"][target.name] = {
            "sha256": hashlib.sha256(ciphertext).hexdigest(),
            "sourceSHA256": hashlib.sha256(path.read_bytes()).hexdigest(),
        }
    for target, ciphertext in encrypted:
        write_private(target, ciphertext, os.geteuid())
    write_private(
        output / "manifest.json",
        (json.dumps(manifest, indent=2) + "\n").encode(),
        os.geteuid(),
    )
    return {
        "encryptedFiles": len(manifest["files"]),
        "recipient": recipient,
        "changed": True,
    }


def render_secrets(
    key, directory, runtime="/run/infra-evacuation/llunde", owner=0, group_id=None
):
    require(os.geteuid() == owner, "Secret renderer owner mismatch")
    read_private(key, owner)
    directory, runtime = owned_directory(directory, owner, 0o700), Path(runtime)
    manifest = json.loads(read_private(directory / "manifest.json", owner))
    recipient = run_private(["age-keygen", "-y", str(key)]).decode().strip()
    require(manifest["recipient"] == recipient, "Secret bundle recipient mismatch")
    require(
        set(manifest["files"]) == {f"{name}.age" for name in SECRETS},
        "Unexpected encrypted files",
    )
    contents = []
    for name, (user, filename, expected, _, _) in SECRETS.items():
        ciphertext = read_private(directory / f"{name}.age", owner)
        require(
            hashlib.sha256(ciphertext).hexdigest()
            == manifest["files"][f"{name}.age"]["sha256"],
            "Encrypted file digest mismatch",
        )
        plaintext = run_private(
            ["age", "--decrypt", "--identity", str(key)], ciphertext
        )
        validate_environment(plaintext, expected)
        contents.append((user, filename, plaintext))
    owned_directory(runtime.parent, owner, 0o755)
    owned_directory(runtime, owner, 0o755)
    for user, filename, plaintext in contents:
        destination = runtime / user
        gid = group_id if group_id is not None else grp.getgrnam(user).gr_gid
        destination.mkdir(mode=0o750, exist_ok=True)
        require(
            not destination.is_symlink() and destination.stat().st_uid == owner,
            "Runtime group directory ownership mismatch",
        )
        os.chown(destination, owner, gid)
        destination.chmod(0o750)
        fd, temporary = tempfile.mkstemp(dir=destination)
        try:
            os.fchown(fd, owner, gid)
            os.fchmod(fd, 0o640)
            with os.fdopen(fd, "wb") as handle:
                handle.write(plaintext)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temporary, destination / filename)
        finally:
            Path(temporary).unlink(missing_ok=True)
    return {"renderedFiles": len(contents)}


def main(argv=None):
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("check-plan").add_argument("directory")
    commands.add_parser("check-accounts").add_argument("plan")
    commands.add_parser("host-key").add_argument("path")
    commands.add_parser("prepare-data-parent")
    network = commands.add_parser("prepare-networks")
    network.add_argument("directory")
    network.add_argument("destination")
    network.add_argument("--source-sha256", required=True)
    prepare = commands.add_parser("prepare-secrets")
    prepare.add_argument("repository")
    prepare.add_argument("recipient")
    prepare.add_argument("destination")
    render = commands.add_parser("render-secrets")
    render.add_argument("key")
    render.add_argument("directory")
    args = parser.parse_args(argv)
    try:
        if args.command == "check-plan":
            validate_plan(args.directory)
            result = {"candidate": "verified", "applicationActivation": False}
        elif args.command == "check-accounts":
            plan = json.loads(Path(args.plan).read_text())
            validate_accounts(
                plan,
                *(
                    Path(f"/etc/{name}").read_text()
                    for name in ("passwd", "group", "subuid", "subgid")
                ),
            )
            result = {"accountMappings": "compatible"}
        elif args.command == "host-key":
            result = host_key(args.path)
        elif args.command == "prepare-data-parent":
            result = prepare_data_parent()
        elif args.command == "prepare-networks":
            result = prepare_networks(
                args.directory, args.destination, args.source_sha256
            )
        elif args.command == "prepare-secrets":
            result = prepare_secrets(args.repository, args.recipient, args.destination)
        else:
            result = render_secrets(args.key, args.directory)
        print(json.dumps(result))
        return 0
    except (StagingError, OSError, ValueError, KeyError, subprocess.SubprocessError):
        print(json.dumps({"error": "Staging validation or private operation failed"}))
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
