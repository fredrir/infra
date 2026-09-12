import argparse
import hashlib
import http.client
import ipaddress
import json
import os
import pwd
import re
import resource
import select
import shlex
import shutil
import socket
import stat
import subprocess
import time
from pathlib import Path

HOSTS = {"fredrir-05": "llunde-01", "fredrir-09": "cloud-server-10643982"}
SOURCE_PYTHON = (
    "/nix/store/vm6nxpp97fxgczw00bwkxdqdm2an3n95-python3-3.13.12/bin/python3"
)
USERS = {"edge": 2000, "llunde-backend": 2001, "llunde-frontend": 2002}
SERVICES = {
    "edge": {"caddy": "systemd-caddy", "cloudflared": "llunde-cloudflared"},
    "llunde-backend": {
        "llunde-backend": "llunde-backend",
        "llunde-postgres": "llunde-postgres",
        "llunde-valkey": "llunde-valkey",
    },
    "llunde-frontend": {"llunde-frontend": "systemd-llunde-frontend"},
}
ROOT_UNITS = (
    "gitops-pull.service",
    "gitops-pull.timer",
    "gitops-deadman.service",
    "gitops-deadman.timer",
    "restic-backups-llunde-backend.service",
    "restic-backups-llunde-backend.timer",
    "infra-evacuation-secrets.service",
)
UNIT_FIELDS = {
    "Id",
    "LoadState",
    "ActiveState",
    "SubState",
    "UnitFileState",
    "FragmentPath",
    "DropInPaths",
    "MainPID",
    "Result",
    "ExecMainStatus",
    "MemoryCurrent",
    "MemoryMax",
    "ConditionResult",
    "NextElapseUSecRealtime",
    "LastTriggerUSec",
    "Triggers",
    "User",
}
ENDPOINT_KEYS = {
    "DB_HOST",
    "DB_PORT",
    "DB_NAME",
    "DB_USER",
    "VALKEY_HOST",
    "VALKEY_PORT",
}
PORTS = {80, 443, 2019, 8080, 8081, 8085, 9101, 5432, 6379}
MAX_OUTPUT = 524288
PATH = "/run/current-system/sw/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
STAGE = "initialization"


class PreflightError(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise PreflightError(message)


def bounded_file(path, maximum=1048576):
    with Path(path).open("rb") as stream:
        value = stream.read(maximum + 1)
    require(len(value) <= maximum, "Observed file exceeds budget")
    return value


def metadata(path, *, content_hash=False, resolve_path=True):
    path = Path(path)
    try:
        info = path.lstat()
    except FileNotFoundError:
        return {"exists": False}
    value = {
        "exists": True,
        "uid": info.st_uid,
        "gid": info.st_gid,
        "mode": format(stat.S_IMODE(info.st_mode), "04o"),
        "inode": info.st_ino,
        "device": info.st_dev,
        "bytes": info.st_size,
        "kind": "symlink"
        if stat.S_ISLNK(info.st_mode)
        else "directory"
        if stat.S_ISDIR(info.st_mode)
        else "file"
        if stat.S_ISREG(info.st_mode)
        else "special",
    }
    if stat.S_ISLNK(info.st_mode):
        value["linkTarget"] = os.readlink(path)
    if content_hash and path.is_file():
        raw = bounded_file(path)
        value["sha256"] = hashlib.sha256(raw).hexdigest()
        value["resolvedPath"] = str(path.resolve(strict=True)) if resolve_path else None
        after = path.lstat()
        require(
            all(
                getattr(info, name) == getattr(after, name)
                for name in [
                    "st_dev",
                    "st_ino",
                    "st_mode",
                    "st_uid",
                    "st_gid",
                    "st_size",
                    "st_mtime_ns",
                    "st_ctime_ns",
                ]
            ),
            "Observed file changed",
        )
    return value


def capture(arguments, *, timeout, environment, source=None):
    require(
        source is None or isinstance(source, bytes) and len(source) <= 1048576,
        "Observation source exceeds budget",
    )
    process = subprocess.Popen(
        arguments,
        stdin=subprocess.PIPE if source is not None else subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        env=environment,
        cwd="/",
    )
    deadline, chunks, size, position = time.monotonic() + timeout, [], 0, 0
    input_open = source is not None
    try:
        if input_open:
            os.set_blocking(process.stdin.fileno(), False)
        while True:
            require(time.monotonic() < deadline, "Observation time budget exceeded")
            if input_open and position == len(source):
                process.stdin.close()
                input_open = False
            readable, writable, _ = select.select(
                [process.stdout],
                [process.stdin] if input_open else [],
                [],
                max(0, min(0.2, deadline - time.monotonic())),
            )
            if writable:
                try:
                    position += os.write(
                        process.stdin.fileno(), source[position : position + 65536]
                    )
                except BlockingIOError:
                    pass
            if not readable:
                continue
            chunk = os.read(process.stdout.fileno(), 65536)
            if not chunk:
                break
            size += len(chunk)
            require(size <= MAX_OUTPUT, "Observed command exceeds output budget")
            chunks.append(chunk)
        require(
            source is None or position == len(source),
            "Observation source transfer incomplete",
        )
        status = process.wait(timeout=max(0.1, deadline - time.monotonic()))
        return status, b"".join(chunks)
    finally:
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=2)
        process.stdout.close()
        if process.stdin is not None and not process.stdin.closed:
            process.stdin.close()


def command(arguments, *, timeout=10, optional=False):
    status, output = capture(
        arguments,
        timeout=timeout,
        environment={
            "PATH": PATH,
            "LC_ALL": "C",
            "SYSTEMD_PAGER": "cat",
            "SYSTEMD_COLORS": "0",
        },
    )
    if status:
        if optional:
            return None
        raise PreflightError("Read-only observation command failed")
    return output.decode()


def as_user(user, arguments):
    uid = USERS[user]
    return [
        "runuser",
        "-u",
        user,
        "--",
        "env",
        "-i",
        "PATH=" + PATH,
        "LC_ALL=C",
        "HOME=/home/" + user,
        "USER=" + user,
        "LOGNAME=" + user,
        f"XDG_RUNTIME_DIR=/run/user/{uid}",
        f"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/{uid}/bus",
        *arguments,
    ]


def unit_projection(raw):
    result = {}
    for line in raw.splitlines():
        require("=" in line, "Malformed unit observation")
        key, value = line.split("=", 1)
        require(
            key in UNIT_FIELDS and key not in result and len(value) <= 8192,
            "Unapproved unit property",
        )
        result[key] = value
    return result


def observe_unit(unit, user=None):
    require(
        re.fullmatch(r"[a-z0-9@.-]+[.](?:service|timer|slice)", unit),
        "Invalid unit name",
    )
    arguments = [
        "systemctl",
        *(["--user"] if user else []),
        "show",
        unit,
        "--property=" + ",".join(sorted(UNIT_FIELDS)),
        "--no-pager",
    ]
    raw = command(as_user(user, arguments) if user else arguments, optional=True)
    if raw is None:
        return {"available": False}
    result = unit_projection(raw)
    paths = [
        result.get("FragmentPath", ""),
        *shlex.split(result.get("DropInPaths", "")),
    ]
    require(len(paths) <= 33, "Too many unit fragments")
    result["files"] = {
        path: metadata(path, content_hash=True)
        for path in paths
        if path.startswith("/")
    }
    return result


def endpoint_projection(raw):
    require(len(raw) <= 1048576, "Process environment exceeds budget")
    result = {}
    for record in raw.split(b"\0"):
        key, separator, value = record.partition(b"=")
        if separator and key.decode(errors="replace") in ENDPOINT_KEYS:
            name, text = key.decode(), value.decode()
            require(
                name not in result and re.fullmatch(r"[A-Za-z0-9_.:-]{1,253}", text),
                "Invalid nonsecret endpoint value",
            )
            result[name] = text
    return result


def process_endpoints(pid, proc=Path("/proc")):
    require(type(pid) is int and pid > 0, "Container process required")
    pending, seen, result = [pid], set(), []
    while pending:
        current = pending.pop()
        if current in seen:
            continue
        seen.add(current)
        require(len(seen) <= 64, "Container process tree exceeds budget")
        base = proc / str(current)
        before = bounded_file(base / "stat", 8192)
        tasks = list((base / "task").iterdir())
        require(len(tasks) <= 512, "Container thread inventory exceeds budget")
        for task in tasks:
            try:
                children = bounded_file(task / "children", 8192).split()
                pending.extend(int(value) for value in children)
            except FileNotFoundError:
                continue
        if bounded_file(base / "comm", 256).strip() == b"java":
            values = endpoint_projection(bounded_file(base / "environ"))
            require(
                before.rsplit(b")", 1)[1].split()[19]
                == bounded_file(base / "stat", 8192).rsplit(b")", 1)[1].split()[19],
                "Application process identity changed",
            )
            result.append({"pid": current, "values": values})
    require(len(result) == 1, "Exactly one running Java application required")
    return result[0]


def container_projection(raw):
    value = json.loads(raw)
    require(
        set(value) == {"image", "running", "pid"}
        and re.fullmatch(r"(?:sha256:)?[a-f0-9]{64}", value["image"])
        and type(value["running"]) is bool
        and type(value["pid"]) is int,
        "Container identity projection differs",
    )
    value["image"] = "sha256:" + value["image"].removeprefix("sha256:")
    return value


def observe_container(user, container):
    require(container in SERVICES[user].values(), "Unapproved container")
    template = '{"image":{{json .Image}},"running":{{json .State.Running}},"pid":{{json .State.Pid}}}'
    raw = command(
        as_user(
            user, ["podman", "container", "inspect", "--format", template, container]
        )
    )
    return container_projection(raw)


def native_tools(pid, names):
    root = Path("/proc") / str(pid) / "root"
    return {
        name: next(
            (
                str(Path(directory) / name)
                for directory in ["/usr/local/bin", "/usr/bin", "/bin"]
                if (root / directory.lstrip("/") / name).is_file()
                and os.access(root / directory.lstrip("/") / name, os.X_OK)
            ),
            None,
        )
        for name in names
    }


def directory_state(path):
    value = metadata(path)
    if value["exists"] and value["kind"] == "directory":
        names = []
        for entry in Path(path).iterdir():
            names.append(entry.name)
            require(len(names) <= 1024, "Data directory inventory exceeds budget")
        names.sort()
        value.update(empty=not names, immediateEntries=names)
    else:
        value["empty"] = not value["exists"]
    return value


def writable_parent(path):
    candidate = Path(path)
    existing = candidate
    while not existing.exists() and not existing.is_symlink():
        require(existing != existing.parent, "Existing unit path ancestor required")
        existing = existing.parent
    try:
        resolved = existing.resolve(strict=True)
    except FileNotFoundError:
        return {
            "path": str(candidate),
            "metadata": metadata(candidate),
            "existingAncestor": str(existing),
            "ancestorMetadata": metadata(existing),
            "unresolvedAncestor": True,
            "rootOwnedWritableCandidate": False,
            "futureManagerMergeVerified": False,
        }
    info = resolved.stat()
    readonly = bool(os.statvfs(resolved).f_flag & os.ST_RDONLY)
    managed_store = (
        resolved == Path("/nix/store") or Path("/nix/store") in resolved.parents
    )
    writable = (
        stat.S_ISDIR(info.st_mode)
        and os.access(resolved, os.W_OK | os.X_OK)
        and not readonly
        and not managed_store
    )
    return {
        "path": str(candidate),
        "metadata": metadata(candidate),
        "existingAncestor": str(existing),
        "resolvedAncestor": str(resolved),
        "ancestorOwner": info.st_uid,
        "ancestorMode": format(stat.S_IMODE(info.st_mode), "04o"),
        "filesystemReadOnly": readonly,
        "managedNixStore": managed_store,
        "rootOwnedWritableCandidate": writable
        and info.st_uid == 0
        and info.st_mode & 0o022 == 0,
        "futureManagerMergeVerified": False,
    }


def unit_search_paths(units, user=None, active=True):
    args = (
        [
            "systemctl",
            *(["--user"] if user else []),
            "show",
            "--property=UnitPath",
            "--value",
        ]
        if active
        else ["systemd-analyze", "--user", "unit-paths"]
    )
    raw = command(as_user(user, args) if user else args, optional=True)
    if raw is None:
        return {
            "source": "unavailable",
            "paths": [],
            "futureManagerMergeVerified": False,
        }
    paths = shlex.split(raw)
    require(
        len(paths) <= 64
        and all(path.startswith("/") and len(path) <= 1024 for path in paths),
        "Invalid unit search paths",
    )
    return {
        "source": "running-manager-UnitPath"
        if active
        else "computed-defaults-no-manager-start",
        "paths": [writable_parent(path) for path in paths],
        "dropInParents": {
            str(Path(path) / (unit + ".d")): writable_parent(Path(path) / (unit + ".d"))
            for path in paths
            for unit in units
            if path.startswith(("/etc/", "/usr/local/"))
        },
        "futureManagerMergeVerified": False,
    }


def listen_projection(raw, ipv6=False):
    result = []
    for line in raw.splitlines()[1:]:
        fields = line.split()
        require(len(fields) >= 4, "Malformed TCP observation")
        address, port = fields[1].split(":")
        port = int(port, 16)
        if fields[3] != "0A" or port not in PORTS:
            continue
        encoded = bytes.fromhex(address)
        packed = b"".join(
            encoded[index : index + 4][::-1] for index in range(0, len(encoded), 4)
        )
        require(len(packed) == (16 if ipv6 else 4), "Address family differs")
        result.append({"address": str(ipaddress.ip_address(packed)), "port": port})
    return result


def health(port, path, headers=None):
    require(
        (port, path)
        in {(8080, "/health"), (8080, "/ready"), (8081, "/"), (8085, "/ready")},
        "Unapproved local health endpoint",
    )
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=3)
    try:
        connection.request("GET", path, headers=headers or {})
        response = connection.getresponse()
        body = response.read(4097)
        require(len(body) <= 4096, "Health response exceeds budget")
        return {
            "status": response.status,
            "bytes": len(body),
            "sha256": hashlib.sha256(body).hexdigest(),
        }
    except (OSError, http.client.HTTPException):
        return {"status": None, "unreachable": True}
    finally:
        connection.close()


def lock_observation(path):
    info = metadata(path)
    if not info["exists"]:
        return info
    matches = []
    for line in bounded_file("/proc/locks").decode().splitlines():
        fields = line.split()
        identity = fields[5].split(":") if len(fields) >= 6 else []
        if len(identity) == 3 and (
            int(identity[0], 16),
            int(identity[1], 16),
            int(identity[2]),
        ) == (os.major(info["device"]), os.minor(info["device"]), info["inode"]):
            matches.append({"kind": fields[1], "mode": fields[3], "pid": fields[4]})
    return info | {"observedLocks": matches, "lockAcquiredByCollector": False}


def service_processes(users, proc=Path("/proc")):
    ranges = [
        (int(start), int(start) + int(count))
        for user in users.values()
        for start, count in user["subordinateIDs"]["subuid"]
    ]
    result = []
    for entry in proc.iterdir():
        if not entry.name.isdecimal():
            continue
        try:
            fields = dict(
                line.split(":", 1)
                for line in bounded_file(entry / "status", 16384).decode().splitlines()
                if ":" in line
            )
            uid = int(fields["Uid"].split()[0])
            if uid in USERS.values() or any(
                start <= uid < end for start, end in ranges
            ):
                result.append(
                    {"pid": int(entry.name), "uid": uid, "name": fields["Name"].strip()}
                )
                require(len(result) <= 1024, "Service process inventory exceeds budget")
        except (FileNotFoundError, ProcessLookupError):
            continue
    return result


def target_dormant(result):
    return (
        all(not value["exists"] for value in result["markers"].values())
        and all(value["empty"] for value in result["data"].values())
        and all(value["empty"] for value in result["activeQuadletPaths"].values())
        and not result["listeners"]
        and not result["serviceProcesses"]
        and all(
            not value["managerContacted"] and not value["linger"]["exists"]
            for value in result["users"].values()
        )
    )


def quadlet_paths():
    paths = {"/etc/containers/systemd/users"}
    for user, uid in USERS.items():
        paths.update(
            {
                f"/run/user/{uid}/containers/systemd",
                f"/home/{user}/.config/containers/systemd",
                f"/etc/containers/systemd/users/{uid}",
            }
        )
    return sorted(paths)


def collect_host(host, expected):
    global STAGE
    STAGE = "host-identity"
    require(
        host in HOSTS and os.geteuid() == 0 and socket.gethostname() == HOSTS[host],
        "Fixed root host identity required",
    )
    require(
        set(expected["images"])
        == {service for values in SERVICES.values() for service in values}
        and all(
            re.fullmatch(r"sha256:[a-f0-9]{64}", image)
            for image in expected["images"].values()
        ),
        "Approved image identities required",
    )
    started = time.time()
    boot = bounded_file("/proc/sys/kernel/random/boot_id", 128).decode().strip()
    result = {
        "schemaVersion": 1,
        "kind": "evacuation-readonly-preflight",
        "host": host,
        "hostname": socket.gethostname(),
        "bootId": boot,
        "startedAt": started,
        "candidateSHA256": expected["candidateSHA256"],
        "fenceObservation": False,
        "writerStopPerformed": False,
        "guardInstallationPerformed": False,
        "applicationStartPerformed": False,
    }
    STAGE = "root-units"
    result["rootUnits"] = {unit: observe_unit(unit) for unit in ROOT_UNITS}
    STAGE = "root-search-paths"
    result["rootUnitSearchPaths"] = unit_search_paths(ROOT_UNITS)
    STAGE = "root-jobs"
    jobs = command(["systemctl", "list-jobs", "--no-legend", "--no-pager", "--plain"])
    result["rootJobs"] = [
        line.split()[:4] for line in jobs.splitlines() if line.strip()
    ]
    result["users"] = {}
    for user, uid in USERS.items():
        STAGE = "user-" + user
        account = pwd.getpwnam(user)
        require(account.pw_uid == uid, "Service account identity differs")
        manager = observe_unit(f"user@{uid}.service")
        bus = Path(f"/run/user/{uid}/bus")
        active = (
            manager.get("ActiveState") == "active"
            and bus.exists()
            and stat.S_ISSOCK(bus.stat().st_mode)
        )
        units = [service + ".service" for service in SERVICES[user]] + [
            "llunde-auto-update.service",
            "llunde-auto-update.timer",
        ]
        result["users"][user] = {
            "uid": uid,
            "gid": account.pw_gid,
            "home": account.pw_dir,
            "shell": account.pw_shell,
            "manager": manager,
            "linger": metadata("/var/lib/systemd/linger/" + user),
            "units": {unit: observe_unit(unit, user) for unit in units}
            if active
            else {},
            "managerContacted": active,
            "slice": observe_unit(f"user-{uid}.slice"),
            "subordinateIDs": {
                name: [
                    line.split(":")[1:]
                    for line in bounded_file("/etc/" + name).decode().splitlines()
                    if line.startswith(user + ":")
                ]
                for name in ["subuid", "subgid"]
            },
        }
        result["users"][user]["unitSearchPaths"] = unit_search_paths(
            units, user, active
        )
    STAGE = "guard-metadata"
    result["guardPaths"] = {
        path: metadata(path)
        for path in [
            "/etc/systemd/system",
            "/etc/systemd/user",
            "/var/lib",
            "/var/lib/platform-evacuation",
            "/var/lib/platform-evacuation/source-locked",
            "/var/lib/platform-evacuation/reconciliation-locked",
        ]
    }
    result["guardRequirement"] = {
        "owner": "root",
        "directoryMode": "0755",
        "markerMode": "0644",
        "actualUserManagerConditionEvaluation": "pending-fence-procedure",
    }
    result["guardParentAccess"] = {
        user: {
            path: command(as_user(user, ["test", "-x", path]), optional=True)
            is not None
            for path in ["/var/lib", "/etc/systemd/user"]
        }
        for user in USERS
    }
    STAGE = "lock-metadata"
    result["gitopsLock"] = lock_observation("/var/lib/gitops-pull/lock")
    STAGE = "data-metadata"
    result["data"] = {
        name: directory_state("/home/llunde-backend/data/" + name)
        for name in ["postgres", "valkey"]
    }
    STAGE = "listener-metadata"
    result["listeners"] = listen_projection(
        bounded_file("/proc/net/tcp").decode()
    ) + listen_projection(bounded_file("/proc/net/tcp6").decode(), True)
    STAGE = "capacity-metadata"
    result["freeDiskBytes"] = shutil.disk_usage("/home/llunde-backend").free
    memory = dict(
        line.split(":", 1)
        for line in bounded_file("/proc/meminfo").decode().splitlines()
    )
    result["memoryAvailableKiB"] = int(memory["MemAvailable"].split()[0])
    result["hostTools"] = {
        name: shutil.which(name, path=PATH)
        for name in [
            "timeout",
            "tar",
            "runuser",
            "flock",
            "podman",
            "systemctl",
            "age",
            "restic",
        ]
    }
    STAGE = "service-processes"
    result["serviceProcesses"] = service_processes(result["users"])
    if host == "fredrir-05":
        STAGE = "source-containers"
        result["containers"] = {
            service: observe_container(user, container)
            for user, services in SERVICES.items()
            for service, container in services.items()
        }
        result["imageMatches"] = {
            service: value["image"] == expected["images"][service]
            for service, value in result["containers"].items()
        }
        STAGE = "backend-endpoints"
        result["effectiveBackendEndpoints"] = process_endpoints(
            result["containers"]["llunde-backend"]["pid"]
        )
        STAGE = "native-tools"
        result["nativeTools"] = {
            "postgres": native_tools(
                result["containers"]["llunde-postgres"]["pid"],
                ["timeout", "pg_dump", "pg_restore", "psql"],
            ),
            "valkey": native_tools(
                result["containers"]["llunde-valkey"]["pid"],
                ["valkey-cli", "valkey-check-aof", "valkey-check-rdb"],
            ),
        }
        STAGE = "health-probes"
        result["health"] = {
            "backendHealth": health(8080, "/health"),
            "backendReady": health(8080, "/ready"),
            "frontend": health(8081, "/"),
            "tunnelApiReady": health(
                8085,
                "/ready",
                {"Host": "api.llunde.no", "CF-Connecting-IP": "203.0.113.1"},
            ),
        }
        STAGE = "tunnel-metadata"
        result["tunnelIdentity"] = {
            "container": "llunde-cloudflared",
            "image": result["containers"]["cloudflared"]["image"],
            "unitFiles": result["users"]["edge"]["units"]
            .get("cloudflared.service", {})
            .get("files", {}),
            "caddyConfig": metadata(
                f"/proc/{result['containers']['caddy']['pid']}/root/etc/caddy/Caddyfile",
                content_hash=True,
                resolve_path=False,
            ),
            "remoteConfigurationVerified": False,
            "tokenRead": False,
        }
    else:
        STAGE = "target-dormancy"
        base = "/var/lib/infra-evacuation/llunde/"
        result["markers"] = {
            name: metadata(base + name)
            for name in [
                "stage-approved",
                "restore-approved",
                "source-fenced",
                "edge-approved",
                "target-writer-start-attempted",
            ]
        }
        result["activeQuadletPaths"] = {
            path: directory_state(path) for path in quadlet_paths()
        }
        result["dormant"] = target_dormant(result)
        result["nativeRestoreTools"] = (
            "Previously preloaded exact images; no container started by collector"
        )
    require(
        boot == bounded_file("/proc/sys/kernel/random/boot_id", 128).decode().strip(),
        "Host rebooted during observation",
    )
    result["finishedAt"] = time.time()
    return result


def validate_result(result, host, expected):
    require(
        result.get("schemaVersion") == 1
        and result.get("kind") == "evacuation-readonly-preflight"
        and result.get("host") == host
        and result.get("hostname") == HOSTS[host]
        and result.get("candidateSHA256") == expected["candidateSHA256"],
        "Remote observation identity differs",
    )
    require(
        all(
            result.get(key) is False
            for key in [
                "fenceObservation",
                "writerStopPerformed",
                "guardInstallationPerformed",
                "applicationStartPerformed",
            ]
        ),
        "Preflight cannot claim a live fence or mutation",
    )
    require(
        type(result["startedAt"]) in (int, float)
        and type(result["finishedAt"]) in (int, float)
        and 0 <= result["finishedAt"] - result["startedAt"] <= 150,
        "Observation window exceeds budget",
    )
    return result


def error_diagnostic(error):
    reason = str(error) if isinstance(error, PreflightError) else type(error).__name__
    return {
        "error": "Read-only preflight failed; sensitive output withheld",
        "stage": STAGE,
        "exceptionType": type(error).__name__,
        "reasonCode": hashlib.sha256(reason.encode()).hexdigest()[:12],
    }


def collect(candidate, destination, hosts):
    from evacuation_staging import validate_plan

    plan = validate_plan(candidate)
    expected = {
        "candidateSHA256": hashlib.sha256(
            (candidate / "staging.json").read_bytes()
        ).hexdigest(),
        "images": {
            name: value["runtimeImage"] for name, value in plan["services"].items()
        },
    }
    require(
        hosts and len(set(hosts)) == len(hosts) and set(hosts) <= set(HOSTS),
        "Fixed unique collection hosts required",
    )
    require(
        not destination.exists() and not destination.is_symlink(),
        "Fresh observation directory required",
    )
    parent = destination.parent.lstat()
    require(
        stat.S_ISDIR(parent.st_mode)
        and parent.st_uid == os.geteuid()
        and stat.S_IMODE(parent.st_mode) == 0o700,
        "Private observation parent required",
    )
    destination.mkdir(mode=0o700)
    source = Path(__file__).read_bytes()
    try:
        for host in hosts:
            interpreter = SOURCE_PYTHON if host == "fredrir-05" else "/usr/bin/python3"
            invocation = [
                "env",
                "-i",
                "PATH=" + PATH,
                "timeout",
                "--signal=TERM",
                "--kill-after=5s",
                "145s",
                interpreter,
                "-I",
                "-",
                "--remote",
                host,
                "--expected",
                json.dumps(expected),
            ]
            remote = (
                'if [ "$(id -u)" = 0 ]; then exec '
                + shlex.join(invocation)
                + "; else exec sudo -n "
                + shlex.join(invocation)
                + "; fi"
            )
            args = [
                "ssh",
                "-T",
                "-o",
                "StrictHostKeyChecking=yes",
                "-o",
                "BatchMode=yes",
                "-o",
                "ForwardAgent=no",
                "-o",
                "ClearAllForwardings=yes",
                "-o",
                "ConnectTimeout=10",
                host,
                remote,
            ]
            environment = {
                key: os.environ[key]
                for key in ["PATH", "HOME", "SSH_AUTH_SOCK"]
                if key in os.environ
            }
            status, output = capture(
                args, source=source, timeout=155, environment=environment
            )
            if status:
                diagnostic = json.loads(output)
                require(
                    set(diagnostic) == {"error", "stage", "exceptionType", "reasonCode"}
                    and re.fullmatch(r"[a-z-]{1,40}", diagnostic["stage"])
                    and re.fullmatch(r"[A-Za-z]{1,40}", diagnostic["exceptionType"])
                    and re.fullmatch(r"[a-f0-9]{12}", diagnostic["reasonCode"]),
                    "Invalid observation diagnostic",
                )
                print(
                    json.dumps(
                        {
                            "host": host,
                            "stage": diagnostic["stage"],
                            "exceptionType": diagnostic["exceptionType"],
                            "reasonCode": diagnostic["reasonCode"],
                        }
                    )
                )
                raise PreflightError(
                    "Remote read-only collection failed; output withheld"
                )
            result = validate_result(json.loads(output), host, expected)
            with os.fdopen(
                os.open(
                    destination / (host + ".json"),
                    os.O_WRONLY | os.O_CREAT | os.O_EXCL,
                    0o600,
                ),
                "w",
            ) as stream:
                json.dump(result, stream, indent=2)
                stream.write("\n")
    except BaseException:
        if not any(destination.iterdir()):
            destination.rmdir()
        raise
    return {
        "directory": str(destination),
        "hosts": hosts,
        "fenceObservation": False,
        "liveMutations": False,
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--remote", choices=tuple(HOSTS))
    parser.add_argument("--expected")
    parser.add_argument("--candidate", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--host", action="append", choices=tuple(HOSTS))
    args = parser.parse_args()
    os.umask(0o077)
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    try:
        if args.remote:
            resource.setrlimit(resource.RLIMIT_CPU, (30, 30))
            result = collect_host(args.remote, json.loads(args.expected))
        else:
            require(
                args.candidate is not None and args.output is not None,
                "Candidate and private output required",
            )
            result = collect(args.candidate, args.output, args.host or list(HOSTS))
        print(json.dumps(result))
        return 0
    except (
        OSError,
        ValueError,
        KeyError,
        TypeError,
        subprocess.SubprocessError,
    ) as error:
        print(json.dumps(error_diagnostic(error)))
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
