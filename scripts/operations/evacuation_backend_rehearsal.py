import argparse
import hashlib
import http.cookies
import json
import math
import os
import re
import signal
import stat
import subprocess
import tempfile
import time
from pathlib import Path

import evacuation_cutover as cutover
from evacuation_execution import (
    MARKERS,
    TARGET_BASE,
    Commands,
    canonical,
    durable_json,
    guard_state,
    host_identity,
    stopped,
    unit_state,
)
from evacuation_images import private_directory
from evacuation_staging import validate_plan
from evacuation_target import kernel_profile

IMAGES = {
    "postgres": "sha256:1edc8e87e53194e0cc8006c4e9df9b626c6c72cb43ad3385f0f34af7922065b1",
    "valkey": "sha256:de5319aecfe95f031945d6b9d19d845835f500377e8d03d719fe0db35752ce53",
    "backend": "sha256:2d4c1b7ca019564f6bae31ee91b747e2c9451a5fc28cbcd6c2afcd83ded8c2a1",
}
MEMORY = {"postgres": 768, "valkey": 384, "backend": 1024}
CPUS = {"postgres": 50000, "valkey": 25000, "backend": 100000}
PIDS = {"postgres": 128, "valkey": 64, "backend": 128}
TMPFS = {
    "postgres": {"/tmp": 64, "/var/lib/postgresql/data": 128, "/run/postgresql": 4},
    "valkey": {"/tmp": 64, "/data": 16},
    "backend": {"/tmp": 64},
}
MARKER_PATHS = [MARKERS / name for name in ("source-locked", "reconciliation-locked")]
MARKER_PATHS += [
    TARGET_BASE / name
    for name in (
        "stage-approved",
        "restore-approved",
        "source-fenced",
        "edge-approved",
        "target-writer-start-attempted",
    )
]
FORBIDDEN = re.compile(
    r"TOKEN|SECRET|PASSWORD|API_KEY|ACCESS_KEY|CREDENTIAL", re.IGNORECASE
)
ORIGIN = "https://llunde.no"
AUTH_PATHS = {
    "/health",
    "/ready",
    "/auth/register",
    "/auth/login",
    "/auth/me",
    "/auth/logout",
}
NATIVE_ERRORS = (
    OSError,
    ValueError,
    KeyError,
    TypeError,
    RuntimeError,
    subprocess.SubprocessError,
)
DEFAULT_NETWORK_ID = "2f259bab93aaaaa2542ba43ef33eb990d0999ee1b9924b557b7be53c0b7a1bb9"


def comparable_baseline(value):
    document = json.loads(json.dumps(value))
    for network in document["networks"]:
        if network.get("name") == "podman" and network.get("id") == DEFAULT_NETWORK_ID:
            network.pop("created", None)
    return document


def capacity(meminfo, limit, current):
    available = re.findall(r"^MemAvailable:\s+([0-9]+) kB$", meminfo, re.MULTILINE)
    require(
        len(available) == 1 and int(available[0]) * 1024 >= 2816 * 1024**2,
        "Host memory reserve insufficient",
    )
    require(
        limit == 2560 * 1024**2 and 0 <= current <= 256 * 1024**2,
        "Loaded service slice lacks rehearsal headroom",
    )
    return {
        "hostAvailableBytes": int(available[0]) * 1024,
        "serviceSliceLimitBytes": limit,
        "serviceSliceCurrentBytes": current,
        "containerMemoryMaxBytes": sum(MEMORY.values()) * 1024**2,
    }


class RehearsalError(ValueError):
    pass


def require(value, message):
    if not value:
        raise RehearsalError(message)


def inputs(candidate):
    new = validate_plan(candidate)
    for component, image in IMAGES.items():
        service = "llunde-" + component
        require(
            new["services"][service]["runtimeImage"] == image,
            "Exact reviewed application and datastore images required",
        )
        require(
            new["services"][service]["memoryMaxBytes"] == MEMORY[component] * 1024**2,
            "Reviewed service memory differs",
        )
    return new


def metadata(path):
    if not os.path.lexists(path):
        return None
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(descriptor)
        require(
            stat.S_ISREG(info.st_mode) and info.st_nlink == 1 and info.st_uid == 0,
            "Owned marker required",
        )
        require(info.st_size <= 65536, "Marker size exceeds bound")
        return {
            "device": info.st_dev,
            "inode": info.st_ino,
            "mode": stat.S_IMODE(info.st_mode),
            "sha256": hashlib.sha256(os.read(descriptor, 65537)).hexdigest(),
        }
    finally:
        os.close(descriptor)


def environment(values):
    result = {}
    require(
        isinstance(values, list) and len(values) <= 64,
        "Bounded image environment required",
    )
    for value in values:
        key, separator, content = value.partition("=")
        require(
            separator and re.fullmatch(r"[A-Z_][A-Z0-9_]{0,127}", key),
            "Image environment key differs",
        )
        require(
            key not in result and len(content) <= 4096 and "\x00" not in content,
            "Image environment value differs",
        )
        result[key] = content
    return result


def tmpfs_diagnostic(values):
    if not isinstance(values, dict) or len(values) > 16:
        return {"shapeValid": False, "mounts": []}
    mounts = []
    for path, raw in values.items():
        valid_path = (
            isinstance(path, str)
            and re.fullmatch(r"/[A-Za-z0-9_./-]{0,127}", path) is not None
            and ".." not in path.split("/")
        )
        valid_options = (
            isinstance(raw, str) and len(raw) <= 1024 and raw.count(",") < 32
        )
        options = raw.split(",") if valid_options else []
        mounts.append(
            {
                "destination": path if valid_path else None,
                "shapeValid": valid_path and valid_options,
                "options": [
                    option
                    if re.fullmatch(
                        r"(?:ro|rw|nosuid|suid|nodev|dev|noexec|exec|sync|async|"
                        r"noatime|relatime|strictatime|nodiratime|lazytime|"
                        r"private|rprivate|shared|rshared|slave|rslave|unbindable|"
                        r"runbindable|seclabel|tmpcopyup|notmpcopyup|"
                        r"(?:size|nr_inodes|uid|gid|mode)=[0-9]{1,20}[kKmMgG]?)",
                        option,
                    )
                    else "<unrecognized>"
                    for option in options
                ],
            }
        )
    return {
        "shapeValid": all(mount["shapeValid"] for mount in mounts),
        "mounts": mounts,
    }


def tmpfs_profile(values, component):
    require(
        isinstance(values, dict) and set(values) == set(TMPFS[component]),
        "Exact ephemeral filesystems required",
    )
    for path, size in TMPFS[component].items():
        options = set(values[path].split(","))
        require(
            {"rw", "nosuid", "nodev", "mode=1777"} <= options,
            "Tmpfs permission flags differ",
        )
        sizes = [option[5:] for option in options if option.startswith("size=")]
        require(len(sizes) == 1, "One tmpfs size required")
        match = re.fullmatch(r"([0-9]+)([kmg]?)", sizes[0].lower())
        require(
            match is not None
            and int(match[1]) * {"": 1, "k": 1024, "m": 1024**2, "g": 1024**3}[match[2]]
            == size * 1024**2,
            "Tmpfs size differs",
        )
        require(
            path == "/tmp" or "noexec" in options, "Data tmpfs must not execute files"
        )


def response(raw):
    require(len(raw) <= 32768, "HTTP response exceeds bound")
    head, separator, body = raw.partition(b"\r\n\r\n")
    require(
        separator and len(head) <= 8192 and len(body) <= 16384,
        "Bounded HTTP response required",
    )
    lines = head.decode("ascii").split("\r\n")
    match = re.fullmatch(
        r"HTTP/1\.[01] ([1-5][0-9]{2})(?: [\x20-\x7e]*)?", lines.pop(0)
    )
    require(match is not None, "HTTP status differs")
    headers = {}
    for line in lines:
        key, separator, value = line.partition(":")
        require(
            separator and re.fullmatch(r"[A-Za-z0-9-]+", key), "HTTP header differs"
        )
        headers.setdefault(key.lower(), []).append(value.strip())
    return int(match[1]), headers, body


def session_cookie(headers, *, diagnostics=None):
    values = headers.get("set-cookie", [])
    require(
        len(values) == 1 and len(values[0]) <= 4096,
        "One bounded session cookie required",
    )
    raw = values[0]
    parts = raw.split(";")
    require(
        len(parts) <= 16 and all(32 <= ord(char) <= 126 for char in raw),
        "Bounded printable session cookie required",
    )
    attributes = [part.strip().partition("=")[0] for part in parts[1:]]
    valid_names = [
        re.fullmatch(r"[$A-Za-z][A-Za-z0-9$-]{0,63}", name) is not None
        for name in attributes
    ]
    if diagnostics is not None:
        diagnostics["attributeNames"] = [
            name if valid else "<invalid>"
            for name, valid in zip(attributes, valid_names, strict=True)
        ]
    lowered = [name.lower() for name in attributes]
    require(
        all(valid_names) and len(set(lowered)) == len(lowered),
        "Unique session cookie attributes required",
    )
    require(
        all(not name.startswith("$") or name == "$x-enc" for name in lowered),
        "Unsupported session cookie extension",
    )
    normalized = [parts[0]]
    for name, part in zip(lowered, parts[1:], strict=True):
        if name == "$x-enc":
            require(
                part.strip() == "$x-enc=URI_ENCODING",
                "Session cookie encoding differs",
            )
        else:
            normalized.append(part)
    cookie = http.cookies.SimpleCookie()
    try:
        cookie.load(";".join(normalized))
    except http.cookies.CookieError:
        raise RehearsalError("Session cookie syntax differs") from None
    require(set(cookie) == {"llunde_session"}, "Session cookie name differs")
    value = cookie["llunde_session"]
    require(
        value["secure"]
        and value["httponly"]
        and value["samesite"].lower() == "lax"
        and value["path"] == "/"
        and not value["domain"]
        and re.fullmatch(r"[A-Za-z0-9_-]{20,512}", value.value),
        "Production session cookie attributes differ",
    )
    return "llunde_session=" + value.value


def auth_flow(request, nonce, password, *, cookie_diagnostics=None):
    email = "infra-rehearsal-" + nonce + "@example.invalid"
    payload = {"email": email, "password": password}
    require(
        request("GET", "/auth/me")[0] == 401, "Anonymous protected route must be denied"
    )
    require(
        request("POST", "/auth/login", payload, csrf=False)[0] == 403,
        "Missing CSRF must be denied",
    )
    require(
        request("POST", "/auth/register", payload)[0] == 201,
        "Synthetic registration failed",
    )
    require(
        request(
            "POST", "/auth/login", {"email": email, "password": password + "-wrong"}
        )[0]
        == 401,
        "Incorrect synthetic password must be denied",
    )
    code, headers, _ = request("POST", "/auth/login", payload)
    require(code == 200, "Synthetic login failed")
    token = session_cookie(headers, diagnostics=cookie_diagnostics)
    code, _, body = request("GET", "/auth/me", cookie=token)
    require(
        code == 200 and json.loads(body)["email"] == email,
        "Authenticated synthetic identity differs",
    )
    require(
        request("POST", "/auth/logout", cookie=token)[0] == 204,
        "Synthetic logout failed",
    )
    require(
        request("GET", "/auth/me", cookie=token)[0] == 401,
        "Revoked synthetic session remains usable",
    )
    return {
        "registrationVerified": True,
        "authenticatedIdentityVerified": True,
        "logoutInvalidationVerified": True,
        "anonymousDenied": True,
        "missingCsrfDenied": True,
        "wrongPasswordDenied": True,
        "cookieAttributesVerified": True,
        "browserCookieTransportVerified": False,
        "existingUserAccountVerified": False,
    }


class Rehearsal:
    def __init__(self, candidate, guard_receipt, output, nonce):
        require(re.fullmatch(r"[a-f0-9]{16}", nonce), "Fixed rehearsal nonce required")
        self.candidate, self.plan = Path(candidate), inputs(candidate)
        self.guard_receipt, self.output = guard_receipt, Path(output)
        self.nonce, self.prefix = nonce, "infra-backend-rehearsal-" + nonce
        self.commands, self.cleanup_commands = Commands(300), None
        self.initialized = False
        self.containers, self.networks, self.image_env = {}, {}, {}
        self.password = "synthetic-" + os.urandom(24).hex()
        self.record = {
            "schemaVersion": 1,
            "kind": "isolated-backend-rehearsal",
            "host": "fredrir-09",
            "nonce": nonce,
            "candidateSHA256": hashlib.sha256(
                (self.candidate / "staging.json").read_bytes()
            ).hexdigest(),
            "images": IMAGES,
            "data": "fresh-synthetic-tmpfs",
            "restoredProductionDataVerified": False,
            "productionCredentials": False,
            "productionApplicationStarted": False,
            "connectorStarted": False,
            "externalNetworkingEnabled": False,
            "dopplerRetrievalVerified": False,
            "systemdApplicationLifecycleVerified": False,
            "passed": False,
        }

    def save(self):
        self.record["ownedContainers"] = {
            name: {key: value for key, value in entry.items() if key != "environment"}
            for name, entry in self.containers.items()
        }
        durable_json(self.output, self.record, replace=self.initialized)
        self.initialized = True

    def call(self, args, *, cleanup=False, data=None, **kwargs):
        if cleanup and self.cleanup_commands is None:
            self.cleanup_commands = Commands(90)
        commands = self.cleanup_commands if cleanup else self.commands
        if data is None:
            return commands.user("llunde-backend", ["podman", *args], **kwargs)
        with tempfile.TemporaryFile() as source:
            source.write(data)
            source.seek(0)
            return commands.user(
                "llunde-backend", ["podman", *args], source=source, **kwargs
            )

    def baseline(self, *, cleanup=False):
        commands = self.cleanup_commands if cleanup else self.commands
        host_identity("fredrir-09")
        require(
            Path("/etc/machine-id").read_text().strip()
            == "98b6af13dea54f4081903e96458be24f",
            "Target machine differs",
        )
        guard = guard_state(self.guard_receipt)
        require(guard["host"] == "fredrir-09", "Target loaded guard differs")
        states, containers = {}, {}
        for user, units in cutover.APP_UNITS.items():
            for unit in units:
                value = unit_state(commands, unit, user)
                require(stopped(value), "Production services must remain stopped")
                states[user + "/" + unit] = value
            _, raw = commands.user(
                user, ["podman", "ps", "--all", "--format", "{{.ID}}"]
            )
            require(
                not raw.strip(), "Existing production or diagnostic container present"
            )
            containers[user] = []
        networks = json.loads(
            self.call(["network", "ls", "--format=json"], cleanup=cleanup)[1]
        )
        volumes = (
            self.call(["volume", "ls", "--format", "{{.Name}}"], cleanup=cleanup)[1]
            .decode()
            .splitlines()
        )
        return {
            "bootId": Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
            "guardFilesSHA256": guard["guardFilesSHA256"],
            "services": states,
            "containers": containers,
            "markers": {str(path): metadata(path) for path in MARKER_PATHS},
            "networks": sorted(networks, key=lambda item: item["name"]),
            "volumes": sorted(volumes),
        }

    def preflight(self):
        private_directory(self.output.parent, 0)
        require(not os.path.lexists(self.output), "Fresh rehearsal receipt required")
        self.save()
        self.record["before"] = self.baseline()
        self.save()
        user_slice = Path("/sys/fs/cgroup/user.slice/user-2001.slice")
        self.record["capacity"] = capacity(
            Path("/proc/meminfo").read_text(),
            int((user_slice / "memory.max").read_text()),
            int((user_slice / "memory.current").read_text()),
        )
        for kind in ("uid_map", "gid_map"):
            raw = self.call(["unshare", "cat", "/proc/self/" + kind])[1]
            mapping = [
                [int(value) for value in line.split()]
                for line in raw.decode().splitlines()
            ]
            require(
                mapping
                == [
                    [0, 2001, 1],
                    [
                        1,
                        self.plan["users"]["llunde-backend"]["targetSubIdStart"],
                        65536,
                    ],
                ],
                "Rootless namespace differs",
            )
        for component, image in IMAGES.items():
            self.call(["image", "exists", image])
            document = json.loads(self.call(["image", "inspect", image])[1])[0]
            require(
                document["Id"].removeprefix("sha256:") == image.removeprefix("sha256:"),
                "Cached image differs",
            )
            values = environment(document["Config"].get("Env") or [])
            require(
                not any(FORBIDDEN.search(key) for key in values),
                "Credential-bearing image environment forbidden",
            )
            self.image_env[component] = values
            if component == "backend":
                require(
                    document["Config"].get("Entrypoint") == ["/app/entrypoint.sh"],
                    "Original supported entrypoint required",
                )
        self.network("data")
        self.network("app")

    def network(self, role):
        name = self.prefix + "-" + role
        require(
            self.call(["network", "exists", name], check=False)[0] == 1,
            "Fresh network required",
        )
        self.networks[name] = None
        self.record["ownedNetworks"] = self.networks
        self.save()
        self.call(
            [
                "network",
                "create",
                "--internal",
                "--disable-dns=false",
                "--driver=bridge",
                "--label=infra.backend-rehearsal=" + self.nonce,
                name,
            ]
        )
        value = json.loads(self.call(["network", "inspect", name])[1])[0]
        self.network_owner(value, name)
        require(
            value["internal"] is True
            and value["dns_enabled"] is True
            and value["driver"] == "bridge",
            "Isolated DNS network required",
        )
        self.networks[name] = value["id"]
        self.save()

    def network_owner(self, value, name):
        require(
            value["name"] == name
            and re.fullmatch(r"[a-f0-9]{64}", value["id"])
            and value.get("labels", {}).get("infra.backend-rehearsal") == self.nonce,
            "Network ownership differs",
        )
        require(self.networks[name] in (None, value["id"]), "Network ID changed")

    def owner(self, value, name):
        entry = self.containers[name]
        require(
            value["Name"].lstrip("/") == name
            and re.fullmatch(r"[a-f0-9]{64}", value["Id"]),
            "Container identity differs",
        )
        require(
            entry["id"] in (None, value["Id"])
            and value["Config"].get("Labels", {}).get("infra.backend-rehearsal")
            == self.nonce,
            "Container ownership differs",
        )
        require(
            value["Image"].removeprefix("sha256:")
            == IMAGES[entry["component"]].removeprefix("sha256:"),
            "Container image differs",
        )

    def inspect(self, name, *, cleanup=False):
        reference = self.containers[name]["id"] or name
        value = json.loads(self.call(["inspect", reference], cleanup=cleanup)[1])[0]
        self.owner(value, name)
        state = value.get("State", {})
        observed = {
            key: state.get(key)
            for key in ("Status", "Running", "ExitCode", "OOMKilled")
        }
        require(
            observed["Status"] is None
            or isinstance(observed["Status"], str)
            and re.fullmatch(r"[a-z-]{1,24}", observed["Status"]),
            "Container status projection differs",
        )
        require(
            all(
                observed[key] is None or type(observed[key]) is bool
                for key in ("Running", "OOMKilled")
            )
            and (observed["ExitCode"] is None or type(observed["ExitCode"]) is int),
            "Container exit projection differs",
        )
        self.record.setdefault("containerStates", {})[name] = observed
        return value

    def profile(self, value, name):
        self.owner(value, name)
        entry, config, host = (
            self.containers[name],
            value["Config"],
            value["HostConfig"],
        )
        component = entry["component"]
        state = (
            "running" if value.get("State", {}).get("Running") is True else "notRunning"
        )
        self.record.setdefault("tmpfsProfiles", {}).setdefault(name, {})[state] = (
            tmpfs_diagnostic(host.get("Tmpfs"))
        )
        require(
            config["User"] == ("65534:65534" if component == "backend" else "999:999"),
            "Container UID differs",
        )
        require(
            host["Privileged"] is False and host["ReadonlyRootfs"] is True,
            "Container filesystem authority differs",
        )
        require(host.get("PortBindings") in (None, {}), "Host publication forbidden")
        require(
            host["Memory"] == host["MemorySwap"] == MEMORY[component] * 1024**2
            and host["PidsLimit"] == PIDS[component],
            "Container resource limits differ",
        )
        require(
            host.get("NanoCpus") == CPUS[component] * 10000
            or (
                host.get("CpuQuota") == CPUS[component]
                and host.get("CpuPeriod") == 100000
            ),
            "CPU quota differs",
        )
        require(
            config.get("Timeout") == entry["timeout"],
            "Independent container lifetime differs",
        )
        require(
            all(
                key in value and value[key] in (None, [])
                for key in ("EffectiveCaps", "BoundingCaps")
            ),
            "Explicit empty capabilities required",
        )
        require(
            any(
                item.startswith("no-new-privileges")
                for item in host.get("SecurityOpt", [])
            ),
            "No-new-privileges required",
        )
        observed_env = environment(config.get("Env") or [])
        expected_env = entry["environment"]
        if observed_env != expected_env:
            self.record.setdefault("environmentMismatch", {})[name] = {
                "addedKeys": sorted(observed_env.keys() - expected_env.keys()),
                "missingKeys": sorted(expected_env.keys() - observed_env.keys()),
                "changedKeys": sorted(
                    key
                    for key in observed_env.keys() & expected_env.keys()
                    if observed_env[key] != expected_env[key]
                ),
            }
            raise RehearsalError(
                "Container environment differs from synthetic allowlist"
            )
        require(config.get("Hostname") == name, "Container hostname differs")
        require(
            set(value["NetworkSettings"]["Networks"]) == set(entry["networks"]),
            "Private network membership differs",
        )
        mounts = [
            item
            for item in value.get("Mounts", [])
            if item["Type"] in ("bind", "volume")
        ]
        require(
            not mounts and not host.get("Binds"), "Host mounts and volumes forbidden"
        )
        tmpfs_profile(host.get("Tmpfs"), component)
        if component == "backend":
            require(
                config.get("Entrypoint") == ["/app/entrypoint.sh"]
                and config.get("Cmd") in (None, []),
                "Original backend entrypoint differs",
            )

    def start(self, component, extra, command):
        name = self.prefix + "-" + component
        require(
            self.call(["container", "exists", name], check=False)[0] == 1,
            "Fresh container required",
        )
        lifetime = math.floor(self.commands.deadline - time.monotonic())
        require(lifetime >= 30, "Insufficient rehearsal lifetime")
        networks = [self.prefix + "-data"] + (
            [self.prefix + "-app"] if component == "backend" else []
        )
        values = dict(self.image_env[component])
        values.update(extra)
        values.update({"HOME": "/tmp", "HOSTNAME": name})
        self.containers[name] = {
            "id": None,
            "component": component,
            "timeout": lifetime,
            "networks": networks,
            "environment": values,
        }
        self.record["ownedContainers"] = {
            key: {k: v for k, v in value.items() if k != "environment"}
            for key, value in self.containers.items()
        }
        self.save()
        args = [
            "create",
            "--name",
            name,
            "--hostname=" + name,
            "--label=infra.backend-rehearsal=" + self.nonce,
            "--pull=never",
            "--read-only",
            "--read-only-tmpfs=false",
            "--image-volume=ignore",
            "--unsetenv-all",
            "--http-proxy=false",
            "--user=" + ("65534:65534" if component == "backend" else "999:999"),
            "--cap-drop=ALL",
            "--security-opt=no-new-privileges",
            "--memory=" + str(MEMORY[component]) + "m",
            "--memory-swap=" + str(MEMORY[component]) + "m",
            "--cpu-period=100000",
            "--cpu-quota=" + str(CPUS[component]),
            "--pids-limit=" + str(PIDS[component]),
            "--timeout=" + str(lifetime),
            "--stop-timeout=5",
            "--log-driver=none",
        ]
        for network in networks:
            args += ["--network", network]
        if component != "backend":
            args += ["--network-alias=llunde-" + component]
        for destination, size in TMPFS[component].items():
            options = "rw,nosuid,nodev,size=" + str(size) + "m,mode=1777"
            args += [
                "--tmpfs="
                + destination
                + ":"
                + options
                + (",noexec" if destination != "/tmp" else "")
            ]
        if component == "valkey":
            args += ["--entrypoint=valkey-server"]
        for key, value in sorted(values.items()):
            args += ["--env=" + key + "=" + value]
        args += [IMAGES[component], *command]
        _, raw = self.call(args, timeout=20)
        identity = raw.decode().strip()
        require(re.fullmatch(r"[a-f0-9]{64}", identity), "Created container ID differs")
        self.containers[name]["id"] = identity
        self.save()
        self.profile(self.inspect(name), name)
        self.call(["start", identity], timeout=20)
        value = self.inspect(name)
        self.profile(value, name)
        proof = kernel_profile(value)
        current = self.inspect(name)
        require(
            (value["Id"], value["State"]["Pid"])
            == (current["Id"], current["State"]["Pid"])
            and current["State"]["Running"],
            "Owned process changed during kernel proof",
        )
        self.record.setdefault("kernelProfiles", {})[component] = proof
        self.save()
        return name

    def wait(self, name, args, expected):
        end = min(self.commands.deadline, time.monotonic() + 45)
        while time.monotonic() < end:
            code, raw = self.call(["exec", name, *args], check=False, timeout=5)
            if code == 0 and raw.strip() == expected:
                return
            time.sleep(0.25)
        raise ValueError("Datastore readiness deadline reached")

    def sql(self, query):
        name = self.prefix + "-postgres"
        return self.call(
            [
                "exec",
                "--env=PGPASSWORD=" + self.password,
                name,
                "psql",
                "--host=127.0.0.1",
                "--username=llunde",
                "--dbname=llunde",
                "--tuples-only",
                "--no-align",
                "--set=ON_ERROR_STOP=1",
                "--command",
                query,
            ]
        )[1]

    def postgres(self):
        name = self.start(
            "postgres",
            {
                "POSTGRES_USER": "llunde",
                "POSTGRES_DB": "llunde",
                "POSTGRES_PASSWORD": self.password,
                "PGDATA": "/var/lib/postgresql/data/pgdata",
            },
            ["postgres", "-c", "shared_buffers=256MB"],
        )
        self.wait(
            name,
            [
                "env",
                "PGPASSWORD=" + self.password,
                "psql",
                "--host=127.0.0.1",
                "--username=llunde",
                "--dbname=llunde",
                "--tuples-only",
                "--no-align",
                "--command=SELECT 1",
            ],
            b"1",
        )
        require(
            self.sql(
                "SELECT count(*) FROM pg_tables WHERE schemaname='public';"
            ).strip()
            == b"0",
            "Fresh empty PostgreSQL database required",
        )
        self.record["emptyDatabaseVerified"] = True

    def valkey(self):
        name = self.start(
            "valkey",
            {},
            [
                "--dir",
                "/data",
                "--save",
                "",
                "--appendonly",
                "yes",
                "--maxmemory",
                "192mb",
                "--maxmemory-policy",
                "noeviction",
            ],
        )
        self.wait(name, ["valkey-cli", "PING"], b"PONG")
        require(
            self.call(["exec", name, "valkey-cli", "DBSIZE"])[1].strip() == b"0",
            "Fresh empty Valkey database required",
        )
        self.record["emptyValkeyVerified"] = True

    def request(self, method, path, data=None, *, cookie=None, csrf=True):
        require(
            method in ("GET", "POST") and path in AUTH_PATHS,
            "Fixed local API operation required",
        )
        args = [
            "exec",
            "-i",
            self.prefix + "-backend",
            "/usr/bin/curl",
            "--disable",
            "--noproxy",
            "*",
            "--proto",
            "=http",
            "--http1.1",
            "--silent",
            "--include",
            "--max-filesize",
            "16384",
            "--max-time",
            "5",
            "--connect-timeout",
            "2",
            "--request",
            method,
        ]
        if method == "POST" and csrf:
            args += [
                "--header",
                "Origin: " + ORIGIN,
                "--header",
                "X-CSRF-Token: isolated-rehearsal",
            ]
        if cookie:
            require(
                re.fullmatch(r"llunde_session=[A-Za-z0-9_-]{20,512}", cookie),
                "Synthetic session cookie differs",
            )
            args += ["--header", "Cookie: " + cookie]
        payload = b""
        if data is not None:
            payload = canonical(data)
            require(len(payload) <= 2048, "Synthetic request exceeds bound")
            args += [
                "--header",
                "Content-Type: application/json",
                "--data-binary",
                "@-",
            ]
        args += ["http://127.0.0.1:8080" + path]
        code, raw = self.call(args, data=payload, timeout=7, maximum=32768, check=False)
        self.record["lastHttpClientStatus"] = code
        require(code == 0, "Synthetic HTTP client failed")
        result = response(raw)
        if path == "/ready":
            self.record["lastReadinessStatus"] = result[0]
        return result

    def backend(self):
        name = self.start(
            "backend",
            {
                "APP_ENV": "prod",
                "DB_HOST": "llunde-postgres",
                "DB_PORT": "5432",
                "DB_NAME": "llunde",
                "DB_USER": "llunde",
                "DB_PASSWORD": self.password,
                "VALKEY_HOST": "llunde-valkey",
                "VALKEY_PORT": "6379",
                "CORS_ALLOWED_ORIGINS": ORIGIN,
                "JAVA_OPTS": "-Xmx640m",
                "HOME": "/tmp",
            },
            [],
        )
        end = min(self.commands.deadline, time.monotonic() + 120)
        while time.monotonic() < end:
            require(
                self.inspect(name)["State"]["Running"], "Backend exited during startup"
            )
            try:
                if self.request("GET", "/ready")[0] == 200:
                    break
            except ValueError:
                pass
            time.sleep(0.5)
        else:
            raise ValueError("Backend readiness deadline reached")
        require(self.request("GET", "/health")[0] == 200, "Backend health failed")
        flyway = json.loads(
            self.sql(
                "SELECT json_build_object('versions',json_agg(version ORDER BY installed_rank),'successful',bool_and(success)) FROM flyway_schema_history;"
            )
        )
        require(
            flyway.get("successful") is True
            and isinstance(flyway.get("versions"), list)
            and len(flyway["versions"]) == 2
            and all(
                isinstance(value, str) and re.fullmatch(r"0*[12]", value)
                for value in flyway["versions"]
            )
            and [int(value) for value in flyway["versions"]] == [1, 2],
            "Actual Flyway migration history differs",
        )
        self.record["flyway"] = flyway
        self.record["syntheticAccount"] = auth_flow(
            self.request,
            self.nonce,
            self.password,
            cookie_diagnostics=self.record.setdefault("cookieProfile", {}),
        )
        require(
            self.request("GET", "/ready")[0] == 200,
            "Backend readiness failed after authenticated writes",
        )
        self.record["backendReadinessVerified"] = True

    def remove(self, name, *, cleanup=False):
        entry = self.containers[name]
        reference = entry["id"] or name
        code, _ = self.call(
            ["container", "exists", reference], check=False, cleanup=cleanup
        )
        if code == 1:
            require(entry["id"] is None, "Recorded container disappeared")
        else:
            value = self.inspect(name, cleanup=cleanup)
            entry["id"] = value["Id"]
            status, _ = self.call(
                ["rm", "--force", "--time=5", value["Id"]],
                cleanup=cleanup,
                timeout=15,
                check=False,
            )
            require(
                self.call(
                    ["container", "exists", value["Id"]], check=False, cleanup=cleanup
                )[0]
                == 1
                and self.call(
                    ["container", "exists", name], check=False, cleanup=cleanup
                )[0]
                == 1,
                "Owned container remains",
            )
            self.record.setdefault("removalClientStatus", {})[name] = status
        del self.containers[name]

    def cleanup(self):
        self.cleanup_commands = Commands(90)
        errors = []
        for name in list(reversed(self.containers)):
            try:
                self.remove(name, cleanup=True)
            except NATIVE_ERRORS:
                errors.append("container:" + name)
        for name in list(reversed(self.networks)):
            try:
                reference = self.networks[name] or name
                code, _ = self.call(
                    ["network", "exists", reference], cleanup=True, check=False
                )
                if code != 1:
                    value = json.loads(
                        self.call(["network", "inspect", reference], cleanup=True)[1]
                    )[0]
                    self.network_owner(value, name)
                    self.call(["network", "rm", value["id"]], cleanup=True, check=False)
                    require(
                        self.call(
                            ["network", "exists", value["id"]],
                            cleanup=True,
                            check=False,
                        )[0]
                        == 1
                        and self.call(
                            ["network", "exists", name], cleanup=True, check=False
                        )[0]
                        == 1,
                        "Owned network remains",
                    )
                del self.networks[name]
            except NATIVE_ERRORS:
                errors.append("network:" + name)
        if "before" in self.record:
            try:
                self.record["after"] = self.baseline(cleanup=True)
                require(
                    comparable_baseline(self.record["after"])
                    == comparable_baseline(self.record["before"]),
                    "Target baseline changed",
                )
                self.record["applicationStatesUnchanged"] = True
            except NATIVE_ERRORS:
                errors.append("baseline")
        self.record["cleanup"] = {
            "failures": errors,
            "containersRemoved": not self.containers,
            "networksRemoved": not self.networks,
        }
        self.record["cleanupVerified"] = (
            not errors and not self.containers and not self.networks
        )
        return self.record["cleanupVerified"]

    def run(self):
        try:
            self.preflight()
            for phase in ("postgres", "valkey", "backend"):
                self.record["phase"] = phase
                self.save()
                getattr(self, phase)()
            require(inputs(self.candidate) == self.plan, "Candidate changed")
            self.record["behaviorPassed"] = True
        except (*NATIVE_ERRORS, KeyboardInterrupt) as error:
            self.record["failure"] = {
                "phase": self.record.get("phase", "preflight"),
                "type": type(error).__name__,
                "reason": str(error) if isinstance(error, RehearsalError) else None,
            }
        finally:
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
            signal.signal(signal.SIGINT, signal.SIG_IGN)
            cleaned = False
            try:
                cleaned = self.cleanup()
            except BaseException as error:  # noqa: BLE001
                self.record["cleanupFailureType"] = type(error).__name__
                self.record["cleanupVerified"] = False
                raise RehearsalError("Cleanup failed; receipt retained") from None
            finally:
                self.record["passed"] = bool(
                    self.record.get("behaviorPassed")
                    and cleaned
                    and self.record.get("applicationStatesUnchanged")
                )
                if self.initialized:
                    self.save()
        return self.record


def main():
    parser = argparse.ArgumentParser()
    for name in ("candidate", "guard-receipt", "output"):
        parser.add_argument("--" + name, required=True, type=Path)
    parser.add_argument("--nonce", required=True)
    args = parser.parse_args()
    host_identity("fredrir-09")
    os.umask(0o077)
    require(re.fullmatch(r"[a-f0-9]{16}", args.nonce), "Fixed rehearsal nonce required")
    controller_scope(args.nonce)

    def interrupted(signum, frame):
        raise RuntimeError("Rehearsal interrupted")

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    result = Rehearsal(
        args.candidate,
        args.guard_receipt,
        args.output,
        args.nonce,
    ).run()
    print(
        json.dumps(
            {
                key: result.get(key)
                for key in (
                    "kind",
                    "passed",
                    "phase",
                    "cleanupVerified",
                    "applicationStatesUnchanged",
                )
            }
        )
    )
    raise SystemExit(0 if result["passed"] else 1)


def controller_scope(nonce):
    rows = Path("/proc/self/cgroup").read_text().splitlines()
    require(
        len(rows) == 1 and rows[0].startswith("0::/"),
        "Unified controller cgroup required",
    )
    relative = Path(rows[0][3:])
    require(
        relative.name == "infra-backend-rehearsal-" + nonce + ".service",
        "Owned controller unit required",
    )
    root = Path("/sys/fs/cgroup") / str(relative).lstrip("/")
    require(
        0 < int((root / "memory.max").read_text()) <= 128 * 1024**2
        and int((root / "memory.swap.max").read_text()) == 0
        and 0 < int((root / "pids.max").read_text()) <= 32,
        "Controller memory or process budget differs",
    )
    quota, period = (root / "cpu.max").read_text().split()
    require(
        quota != "max" and 0 < int(quota) <= int(period) // 4,
        "Controller CPU budget differs",
    )


if __name__ == "__main__":
    main()
