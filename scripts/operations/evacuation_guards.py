import argparse
import fcntl
import hashlib
import json
import os
import re
import secrets
import socket
import stat
import subprocess
import time
from collections import Counter
from contextlib import contextmanager
from pathlib import Path

from evacuation_preflight import HOSTS, SERVICES, USERS, as_user, command

BASE = "/var/lib/platform-evacuation"
MARKERS = {
    "source-locked": BASE + "/source-locked",
    "reconciliation-locked": BASE + "/reconciliation-locked",
}
ROOTS = {
    "fredrir-05": {
        "system": "/etc/systemd/system.control",
        "user": "/etc/xdg/systemd/user",
    },
    "fredrir-09": {"system": "/etc/systemd/system", "user": "/etc/systemd/user"},
}
RECEIPT = BASE + "/receipts/guards.json"
SYSTEM_UNITS = (
    "gitops-pull.service",
    "gitops-pull.timer",
    "restic-backups-llunde-backend.service",
    "restic-backups-llunde-backend.timer",
)
UPDATE_UNITS = ("llunde-auto-update.service", "llunde-auto-update.timer")
UNIT_PROPERTIES = {
    "LoadState",
    "FragmentPath",
    "ActiveState",
    "SubState",
    "MainPID",
    "DropInPaths",
    "ConditionResult",
    "Result",
    "ExecMainStatus",
    "ExecMainStartTimestampMonotonic",
}


class GuardError(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise GuardError(message)


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


def plan(host, candidate_sha256):
    require(
        host in HOSTS and re.fullmatch(r"[a-f0-9]{64}", candidate_sha256),
        "Fixed host and candidate required",
    )
    files = {}
    specifications = [
        ("system", unit, "reconciliation-locked") for unit in SYSTEM_UNITS
    ]
    specifications += [("user", unit, "reconciliation-locked") for unit in UPDATE_UNITS]
    specifications += [
        ("user", service + ".service", "source-locked")
        for service in sorted({name for values in SERVICES.values() for name in values})
    ]
    for manager, unit, marker in specifications:
        path = ROOTS[host][manager] + "/" + unit + ".d/95-evacuation-fence.conf"
        content = "[Unit]\nConditionPathExists=!" + MARKERS[marker] + "\n"
        files[path] = {
            "manager": manager,
            "unit": unit,
            "marker": marker,
            "content": content,
            "sha256": hashlib.sha256(content.encode()).hexdigest(),
        }
    return {
        "schemaVersion": 1,
        "kind": "evacuation-guard-plan",
        "host": host,
        "hostname": HOSTS[host],
        "candidateSHA256": candidate_sha256,
        "roots": ROOTS[host],
        "markers": MARKERS,
        "files": files,
        "sourceFence": False,
        "changesStopPolicy": False,
    }


def validate_plan(value):
    require(
        value == plan(value["host"], value["candidateSHA256"]),
        "Guard plan differs from fixed contract",
    )
    return value


def manager_units(host):
    result = {"system": {unit: host == "fredrir-05" for unit in SYSTEM_UNITS}}
    for user, services in SERVICES.items():
        result[user] = {name + ".service": True for name in services} | {
            unit: host == "fredrir-05" for unit in UPDATE_UNITS
        }
    return result


def condition_rows(value):
    require(
        value["type"] == "a(sbbsi)"
        and isinstance(value["data"], list)
        and len(value["data"]) <= 64,
        "D-Bus condition array required",
    )
    rows = value["data"]
    require(
        all(
            isinstance(row, list)
            and len(row) == 5
            and isinstance(row[0], str)
            and type(row[1]) is bool
            and type(row[2]) is bool
            and isinstance(row[3], str)
            and type(row[4]) is int
            for row in rows
        ),
        "D-Bus condition fields differ",
    )
    return rows


class Manager:
    def __init__(self):
        self.deadline = time.monotonic() + 240

    def invoke(self, manager, arguments, optional=False):
        require(time.monotonic() < self.deadline, "Guard manager budget exceeded")
        return command(
            arguments if manager == "system" else as_user(manager, arguments),
            optional=optional,
        )

    def properties(self, manager, unit):
        raw = self.invoke(
            manager,
            [
                "systemctl",
                *([] if manager == "system" else ["--user"]),
                "show",
                unit,
                "--property=" + ",".join(sorted(UNIT_PROPERTIES)),
                "--no-pager",
            ],
        )
        values = dict(line.split("=", 1) for line in raw.splitlines())
        require(set(values) <= UNIT_PROPERTIES, "Unexpected unit properties")
        return values

    def paths(self, manager):
        raw = self.invoke(
            manager,
            [
                "systemctl",
                *([] if manager == "system" else ["--user"]),
                "show",
                "--property=UnitPath",
                "--value",
            ],
        )
        return raw.split()

    def conditions(self, manager, unit):
        require(
            re.fullmatch(r"[a-z][a-z0-9@.-]{1,160}[.](service|timer)", unit),
            "Bounded unit identity required",
        )
        path = "/org/freedesktop/systemd1/unit/" + "".join(
            character if character.isalnum() else "_" + format(ord(character), "02x")
            for character in unit
        )
        prefix = [
            "busctl",
            *([] if manager == "system" else ["--user"]),
            "--json=short",
        ]
        return condition_rows(
            json.loads(
                self.invoke(
                    manager,
                    prefix
                    + [
                        "get-property",
                        "org.freedesktop.systemd1",
                        path,
                        "org.freedesktop.systemd1.Unit",
                        "Conditions",
                    ],
                )
            )
        )

    def reload(self):
        for manager in ["system", *USERS]:
            self.invoke(
                manager,
                [
                    "systemctl",
                    *([] if manager == "system" else ["--user"]),
                    "daemon-reload",
                ],
            )

    def probe_start(self, manager, unit):
        require(
            re.fullmatch(
                r"infra-evacuation-guard-probe-[a-f0-9]{16}[.](service|target)", unit
            ),
            "Only isolated guard probes may start",
        )
        self.invoke(
            manager,
            ["systemctl", *([] if manager == "system" else ["--user"]), "start", unit],
        )

    def probe_stop(self, manager, unit):
        require(
            re.fullmatch(r"infra-evacuation-guard-probe-[a-f0-9]{16}[.]target", unit),
            "Only isolated probe reference targets may stop",
        )
        self.invoke(
            manager,
            ["systemctl", *([] if manager == "system" else ["--user"]), "stop", unit],
        )


class Filesystem:
    def __init__(self, root=Path("/"), owner=0):
        self.root, self.owner = Path(root), owner

    def parts(self, value):
        require(
            isinstance(value, str)
            and value.startswith("/")
            and str(Path(value)) == value
            and all(part not in {".", ".."} for part in value.split("/")[1:]),
            "Canonical absolute path required",
        )
        return Path(value).parts[1:]

    def path(self, value):
        self.parts(value)
        return self.root / value.lstrip("/")

    @contextmanager
    def directory_fd(self, value, *, create=False, mode=0o755, created=None):
        parts = self.parts(value) if value != "/" else ()
        descriptor = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            for index, part in enumerate(parts):
                info = os.fstat(descriptor)
                require(
                    info.st_uid == self.owner
                    and not stat.S_IMODE(info.st_mode) & 0o022,
                    "Unsafe guard parent",
                )
                made = False
                try:
                    child = os.open(
                        part,
                        os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                        dir_fd=descriptor,
                    )
                except FileNotFoundError:
                    if not create:
                        raise
                    desired = mode if index == len(parts) - 1 else 0o755
                    os.mkdir(part, desired, dir_fd=descriptor)
                    child = os.open(
                        part,
                        os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                        dir_fd=descriptor,
                    )
                    os.fchmod(child, desired)
                    made = True
                os.close(descriptor)
                descriptor = child
                info = os.fstat(descriptor)
                require(
                    info.st_uid == self.owner
                    and not stat.S_IMODE(info.st_mode) & 0o022,
                    "Unsafe guard directory",
                )
                if made:
                    created["/" + "/".join(parts[: index + 1])] = {
                        "uid": info.st_uid,
                        "mode": format(stat.S_IMODE(info.st_mode), "04o"),
                        "inode": info.st_ino,
                        "device": info.st_dev,
                    }
            info = os.fstat(descriptor)
            require(
                info.st_uid == self.owner and not stat.S_IMODE(info.st_mode) & 0o022,
                "Unsafe guard directory",
            )
            if create:
                require(
                    stat.S_IMODE(info.st_mode) == mode,
                    "Owned directory permissions differ",
                )
            yield descriptor
        finally:
            os.close(descriptor)

    @contextmanager
    def parent_fd(self, value):
        parts = self.parts(value)
        require(parts, "Guard file path required")
        with self.directory_fd(
            "/" + "/".join(parts[:-1]) if len(parts) > 1 else "/"
        ) as descriptor:
            yield descriptor, parts[-1]

    def read(self, value):
        try:
            with self.parent_fd(value) as (parent, name):
                descriptor = os.open(
                    name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent
                )
        except FileNotFoundError:
            return {"exists": False}, None
        try:
            info = os.fstat(descriptor)
            require(
                stat.S_ISREG(info.st_mode)
                and info.st_uid == self.owner
                and info.st_nlink == 1
                and 0 <= info.st_size <= 65536,
                "Owned regular guard file required",
            )
            data = os.read(descriptor, 65537)
            after = os.fstat(descriptor)
            require(
                len(data) == info.st_size
                and all(
                    getattr(info, key) == getattr(after, key)
                    for key in [
                        "st_size",
                        "st_mtime_ns",
                        "st_ctime_ns",
                        "st_uid",
                        "st_gid",
                        "st_mode",
                        "st_nlink",
                    ]
                ),
                "Guard file changed",
            )
            return {
                "exists": True,
                "uid": info.st_uid,
                "gid": info.st_gid,
                "mode": format(stat.S_IMODE(info.st_mode), "04o"),
                "inode": info.st_ino,
                "device": info.st_dev,
                "sha256": hashlib.sha256(data).hexdigest(),
            }, data
        finally:
            os.close(descriptor)

    def metadata(self, value, expected=None):
        metadata, _ = self.read(value)
        if expected is not None:
            require(metadata == expected, "Owned guard file changed")
        return metadata

    def directory(self, value, mode, created):
        with self.directory_fd(value, create=True, mode=mode, created=created):
            pass

    def write(self, value, data, mode=0o644):
        with self.parent_fd(value) as (parent, name):
            descriptor = os.open(
                name,
                os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                mode,
                dir_fd=parent,
            )
            with os.fdopen(descriptor, "wb") as stream:
                os.fchmod(stream.fileno(), mode)
                stream.write(data)
                stream.flush()
                os.fsync(stream.fileno())
            os.fsync(parent)
        return self.metadata(value)

    def read_json(self, value):
        info, data = self.read(value)
        require(
            info["exists"] and info["mode"] == "0600", "Private guard receipt required"
        )
        return json.loads(data)

    def save(self, value, document):
        data = json.dumps(document, indent=2).encode() + b"\n"
        require(len(data) <= 65536, "Guard receipt exceeds budget")
        with self.parent_fd(value) as (parent, name):
            temporary = name + ".new-" + secrets.token_hex(8)
            try:
                descriptor = os.open(
                    temporary,
                    os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                    0o600,
                    dir_fd=parent,
                )
                with os.fdopen(descriptor, "wb") as stream:
                    os.fchmod(stream.fileno(), 0o600)
                    stream.write(data)
                    stream.flush()
                    os.fsync(stream.fileno())
                existing = self.metadata(value)
                require(
                    not existing["exists"] or existing["mode"] == "0600",
                    "Existing receipt ownership differs",
                )
                os.replace(temporary, name, src_dir_fd=parent, dst_dir_fd=parent)
                os.fsync(parent)
            finally:
                try:
                    os.unlink(temporary, dir_fd=parent)
                except FileNotFoundError:
                    pass

    def unlink(self, value, expected):
        with self.parent_fd(value) as (parent, name):
            self.metadata(value, expected)
            info = os.stat(name, dir_fd=parent, follow_symlinks=False)
            require(
                info.st_ino == expected["inode"] and info.st_dev == expected["device"],
                "Owned file moved",
            )
            os.unlink(name, dir_fd=parent)
            os.fsync(parent)

    def remove_directories(self, created):
        retained = []
        for value, expected in sorted(
            created.items(), key=lambda item: len(Path(item[0]).parts), reverse=True
        ):
            try:
                with self.parent_fd(value) as (parent, name):
                    descriptor = os.open(
                        name,
                        os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                        dir_fd=parent,
                    )
                    try:
                        info = os.fstat(descriptor)
                        require(
                            info.st_uid == expected["uid"]
                            and info.st_ino == expected["inode"]
                            and info.st_dev == expected["device"]
                            and format(stat.S_IMODE(info.st_mode), "04o")
                            == expected["mode"],
                            "Created guard directory changed",
                        )
                        if os.listdir(descriptor):
                            retained.append(value)
                        else:
                            os.rmdir(name, dir_fd=parent)
                    finally:
                        os.close(descriptor)
            except FileNotFoundError:
                pass
        return retained


def live_identity(host):
    require(
        os.geteuid() == 0 and socket.gethostname() == HOSTS[host],
        "Fixed root host identity required",
    )


def marker_state(fs):
    try:
        with fs.directory_fd(BASE) as descriptor:
            require(
                stat.S_IMODE(os.fstat(descriptor).st_mode) == 0o755,
                "Marker parent must be readable by application users",
            )
    except FileNotFoundError:
        pass
    result = {name: fs.metadata(path) for name, path in MARKERS.items()}
    require(
        all(
            not value["exists"] or value["mode"] == "0644" for value in result.values()
        ),
        "Marker ownership or mode differs",
    )
    return result


def require_inert(fs):
    require(
        all(not value["exists"] for value in marker_state(fs).values()),
        "Real fence markers must remain absent",
    )


def states(manager, host):
    return {
        name: {
            unit: {
                key: value.get(key) for key in ["ActiveState", "SubState", "MainPID"]
            }
            for unit in units
            for value in [manager.properties(name, unit)]
        }
        for name, units in manager_units(host).items()
    }


def loaded_proof(manager, value, require_loaded):
    proof, complete = {}, True
    for name, units in manager_units(value["host"]).items():
        root = value["roots"]["system" if name == "system" else "user"]
        require(
            root in manager.paths(name),
            "Approved guard root is absent from running manager search path",
        )
        proof[name] = {}
        for unit, required in units.items():
            observed = manager.properties(name, unit)
            loaded = observed["LoadState"] == "loaded"
            if not loaded:
                require(
                    not (required and require_loaded),
                    "Required application guard merge remains unverified",
                )
                complete = complete and not required
                proof[name][unit] = {
                    "loaded": False,
                    "required": required,
                    "exactGuardMerged": False,
                }
                continue
            path = root + "/" + unit + ".d/95-evacuation-fence.conf"
            specification = value["files"][path]
            expected = [
                "ConditionPathExists",
                False,
                True,
                MARKERS[specification["marker"]],
            ]
            conditions = manager.conditions(name, unit)
            require(
                path in observed["DropInPaths"].split()
                and sum(row[:4] == expected for row in conditions) == 1,
                "Loaded guard file or negated condition differs",
            )
            proof[name][unit] = {
                "loaded": True,
                "required": required,
                "exactGuardMerged": True,
                "conditions": conditions,
                "dropInPath": path,
            }
    return proof, complete


def verify_installation(
    receipt, require_loaded=True, *, _filesystem=None, _manager=None
):
    fs, manager = _filesystem or Filesystem(), _manager or Manager()
    if isinstance(receipt, (str, Path)):
        receipt = fs.read_json(str(receipt))
    value = validate_receipt(receipt)
    if _filesystem is None:
        live_identity(value["host"])
    require(
        receipt["schemaVersion"] == 1
        and receipt["kind"] == "evacuation-guard-installation"
        and receipt["status"] == "installed"
        and receipt["host"] == value["host"]
        and receipt["planSHA256"] == hashlib.sha256(canonical(value)).hexdigest(),
        "Installed guard receipt identity differs",
    )
    require(
        set(receipt["files"]) == set(value["files"]), "Guard receipt file set differs"
    )
    for path, specification in value["files"].items():
        metadata = fs.metadata(path, receipt["files"][path])
        require(
            metadata["mode"] == "0644"
            and metadata["sha256"] == specification["sha256"],
            "Guard content or mode differs",
        )
    markers = marker_state(fs)
    proof, complete = loaded_proof(manager, value, require_loaded)
    _, probe_marker, probes = probe_specifications(value, receipt["probeNonce"])
    require(
        not receipt["probeFiles"]
        and receipt["probeMarker"] is None
        and receipt["pendingFile"] is None
        and not fs.metadata(probe_marker)["exists"]
        and all(not fs.metadata(path)["exists"] for path in probes),
        "Probe cleanup differs",
    )
    require(
        receipt["probeCleanupVerified"] is True
        and all(
            record["absentRan"] is True and record["presentSkipped"] is True
            for record in receipt["conditionProbes"].values()
        )
        and set(receipt["conditionProbes"]) == {"system", *USERS},
        "Actual manager probe evidence required",
    )
    return {
        "host": value["host"],
        "planSHA256": receipt["planSHA256"],
        "files": receipt["files"],
        "managerProof": proof,
        "exactUnitMergeVerified": complete,
        "guardFilesSHA256": hashlib.sha256(canonical(receipt["files"])).hexdigest(),
        "markers": markers,
    }


def guard_for(value, manager, unit):
    root = value["roots"]["system" if manager == "system" else "user"]
    return root + "/" + unit + ".d/95-evacuation-fence.conf"


def assert_preserved_conditions(manager, value, baseline):
    for name, units in baseline.items():
        for unit, conditions in units.items():
            specification = value["files"][guard_for(value, name, unit)]
            expected = conditions + [
                ["ConditionPathExists", False, True, MARKERS[specification["marker"]]]
            ]
            actual = [row[:4] for row in manager.conditions(name, unit)]
            require(
                Counter(map(tuple, actual)) == Counter(map(tuple, expected)),
                "Existing unit conditions changed during guard installation",
            )


def probe_specifications(value, nonce):
    require(
        isinstance(nonce, str) and re.fullmatch(r"[a-f0-9]{16}", nonce),
        "Probe identity differs",
    )
    unit = "infra-evacuation-guard-probe-" + nonce + ".service"
    marker = BASE + "/probe-" + nonce
    true = (
        "/run/current-system/sw/bin/true"
        if value["host"] == "fredrir-05"
        else "/usr/bin/true"
    )
    fragment = f"[Unit]\nDefaultDependencies=no\n[Service]\nType=oneshot\nExecStart={true}\nTimeoutStartSec=5s\nTimeoutStopSec=5s\nMemoryMax=32M\nTasksMax=8\nCPUQuota=10%\nNoNewPrivileges=yes\n"
    files = {}
    for root in value["roots"].values():
        files[root + "/" + unit] = fragment
        files[root + "/" + unit.removesuffix(".service") + ".target"] = (
            "[Unit]\nDefaultDependencies=no\nWants=" + unit + "\nAfter=" + unit + "\n"
        )
        files[root + "/" + unit + ".d/95-evacuation-probe.conf"] = (
            "[Unit]\nConditionPathExists=!" + marker + "\n"
        )
    return unit, marker, files


def validate_metadata(value):
    require(
        isinstance(value, dict)
        and set(value) == {"exists", "uid", "gid", "mode", "inode", "device", "sha256"}
        and value["exists"] is True
        and value["mode"] == "0644"
        and re.fullmatch(r"[a-f0-9]{64}", value["sha256"])
        and all(
            type(value[key]) is int and value[key] >= 0
            for key in ["uid", "gid", "inode", "device"]
        ),
        "Guard receipt metadata differs",
    )


def validate_receipt(receipt):
    value = validate_plan(receipt["plan"])
    require(
        receipt["schemaVersion"] == 1
        and receipt["kind"] == "evacuation-guard-installation"
        and receipt["host"] == value["host"]
        and receipt["planSHA256"] == hashlib.sha256(canonical(value)).hexdigest(),
        "Guard receipt identity differs",
    )
    _, marker, probes = probe_specifications(value, receipt["probeNonce"])
    require(
        set(receipt["files"]) <= set(value["files"])
        and set(receipt["probeFiles"]) <= set(probes),
        "Receipt contains an unowned file",
    )
    for path, metadata in receipt["files"].items():
        validate_metadata(metadata)
        require(
            metadata["sha256"] == value["files"][path]["sha256"],
            "Receipt guard content differs",
        )
    for path, metadata in receipt["probeFiles"].items():
        validate_metadata(metadata)
        require(
            metadata["sha256"] == hashlib.sha256(probes[path].encode()).hexdigest(),
            "Receipt probe content differs",
        )
    if receipt["probeMarker"] is not None:
        validate_metadata(receipt["probeMarker"])
        require(
            receipt["probeMarker"]["sha256"] == hashlib.sha256(b"probe\n").hexdigest(),
            "Probe marker content differs",
        )
    expected = {
        path: {"path": path, "sha256": spec["sha256"], "kind": "guard"}
        for path, spec in value["files"].items()
    }
    expected |= {
        path: {
            "path": path,
            "sha256": hashlib.sha256(content.encode()).hexdigest(),
            "kind": "probe",
        }
        for path, content in probes.items()
    }
    expected[marker] = {
        "path": marker,
        "sha256": hashlib.sha256(b"probe\n").hexdigest(),
        "kind": "probe-marker",
    }
    pending = receipt["pendingFile"]
    require(pending is None or pending in expected.values(), "Unowned pending write")
    directories = {
        str(parent)
        for path in [*value["files"], *probes, RECEIPT]
        for parent in Path(path).parents
        if str(parent) != "/"
    }
    require(
        set(receipt["directoriesCreated"]) <= directories,
        "Receipt contains an unowned directory",
    )
    for path, metadata in receipt["directoriesCreated"].items():
        require(
            isinstance(metadata, dict)
            and set(metadata) == {"uid", "mode", "inode", "device"}
            and metadata["mode"]
            == ("0700" if path == str(Path(RECEIPT).parent) else "0755")
            and all(
                type(metadata[key]) is int and metadata[key] >= 0
                for key in ["uid", "inode", "device"]
            ),
            "Created directory receipt differs",
        )
    return value


def probe_conditions(fs, manager, receipt, save):
    value = receipt["plan"]
    unit, marker, specifications = probe_specifications(value, receipt["probeNonce"])
    target = unit.removesuffix(".service") + ".target"
    for path, content in specifications.items():
        require(not fs.metadata(path)["exists"], "Isolated probe file already exists")
        fs.directory(str(Path(path).parent), 0o755, receipt["directoriesCreated"])
        receipt["pendingFile"] = {
            "path": path,
            "sha256": hashlib.sha256(content.encode()).hexdigest(),
            "kind": "probe",
        }
        save()
        receipt["probeFiles"][path] = fs.write(path, content.encode())
        receipt["pendingFile"] = None
        save()
    manager.reload()
    for name in ["system", *USERS]:
        require_inert(fs)
        require(
            not fs.metadata(marker)["exists"], "Isolated probe marker already exists"
        )
        root = value["roots"]["system" if name == "system" else "user"]
        observed = manager.properties(name, unit)
        expected = ["ConditionPathExists", False, True, marker]
        require(
            observed["LoadState"] == "loaded"
            and observed["FragmentPath"] == root + "/" + unit
            and observed["DropInPaths"].split()
            == [root + "/" + unit + ".d/95-evacuation-probe.conf"]
            and [row[:4] for row in manager.conditions(name, unit)] == [expected],
            "Probe drop-in did not merge in the actual manager",
        )
        companion = manager.properties(name, target)
        require(
            companion["LoadState"] == "loaded"
            and companion["FragmentPath"] == root + "/" + target
            and companion["DropInPaths"] == ""
            and companion["ActiveState"] == "inactive",
            "Probe reference target differs",
        )
        manager.probe_start(name, target)
        require(
            manager.properties(name, target)["ActiveState"] == "active",
            "Probe reference target did not remain active",
        )
        absent = manager.properties(name, unit)
        require(
            absent["ConditionResult"] == "yes"
            and absent["ActiveState"] == "inactive"
            and absent["ExecMainStatus"] == "0"
            and int(absent["ExecMainStartTimestampMonotonic"]) > 0,
            "Absent marker probe did not execute successfully",
        )
        receipt["pendingFile"] = {
            "path": marker,
            "sha256": hashlib.sha256(b"probe\n").hexdigest(),
            "kind": "probe-marker",
        }
        save()
        receipt["probeMarker"] = fs.write(marker, b"probe\n")
        receipt["pendingFile"] = None
        save()
        manager.probe_start(name, unit)
        present = manager.properties(name, unit)
        require(
            present["ConditionResult"] == "no"
            and present["ActiveState"] == "inactive"
            and present["ExecMainStartTimestampMonotonic"]
            == absent["ExecMainStartTimestampMonotonic"],
            "Present marker probe did not inhibit execution",
        )
        receipt["conditionProbes"][name] = {
            "absentRan": True,
            "presentSkipped": True,
            "dropInPath": root + "/" + unit + ".d/95-evacuation-probe.conf",
            "execTimestamp": absent["ExecMainStartTimestampMonotonic"],
            "referenceTarget": target,
        }
        fs.unlink(marker, receipt["probeMarker"])
        receipt["probeMarker"] = None
        save()
        manager.probe_stop(name, target)
        require(
            manager.properties(name, target)["ActiveState"] == "inactive",
            "Probe reference target remains active",
        )
    for path, expected in list(receipt["probeFiles"].items()):
        fs.unlink(path, expected)
        del receipt["probeFiles"][path]
        save()
    manager.reload()
    receipt["probeCleanupVerified"] = (
        not fs.metadata(marker)["exists"]
        and all(not fs.metadata(path)["exists"] for path in specifications)
        and all(
            manager.properties(name, probe)["ActiveState"] == "inactive"
            for name in ["system", *USERS]
            for probe in [unit, target]
        )
    )
    save()


def release_probe_references(fs, manager, receipt):
    value = validate_receipt(receipt)
    unit, _, specifications = probe_specifications(value, receipt["probeNonce"])
    target = unit.removesuffix(".service") + ".target"
    for name in ["system", *USERS]:
        observed = manager.properties(name, target)
        if observed["ActiveState"] == "inactive":
            continue
        root = value["roots"]["system" if name == "system" else "user"]
        path = root + "/" + target
        metadata = fs.metadata(path)
        require(
            metadata["exists"]
            and metadata["mode"] == "0644"
            and metadata["sha256"]
            == hashlib.sha256(specifications[path].encode()).hexdigest()
            and observed["FragmentPath"] == path
            and observed["DropInPaths"] == "",
            "Owned probe reference target changed",
        )
        manager.probe_stop(name, target)
        require(
            manager.properties(name, target)["ActiveState"] == "inactive",
            "Probe reference target remains active",
        )


def install(value, *, _filesystem=None, _manager=None):
    value = validate_plan(value)
    fs, manager = _filesystem or Filesystem(), _manager or Manager()
    if _filesystem is None:
        live_identity(value["host"])
    require_inert(fs)
    if fs.metadata(RECEIPT)["exists"]:
        receipt = fs.read_json(RECEIPT)
        require(receipt["plan"] == value, "Another guard installation receipt exists")
        return verify_installation(
            receipt,
            require_loaded=value["host"] == "fredrir-05",
            _filesystem=fs,
            _manager=manager,
        )
    require(
        all(not fs.metadata(path)["exists"] for path in value["files"]),
        "A guard file already exists",
    )
    baseline, before = {}, states(manager, value["host"])
    for name, units in manager_units(value["host"]).items():
        root = value["roots"]["system" if name == "system" else "user"]
        require(
            root in manager.paths(name),
            "Approved guard root is absent from running manager search path",
        )
        baseline[name] = {
            unit: [row[:4] for row in manager.conditions(name, unit)]
            for unit in units
            if manager.properties(name, unit)["LoadState"] == "loaded"
        }
    receipt = {
        "schemaVersion": 1,
        "kind": "evacuation-guard-installation",
        "host": value["host"],
        "plan": value,
        "planSHA256": hashlib.sha256(canonical(value)).hexdigest(),
        "status": "installing",
        "createdAt": int(time.time()),
        "files": {},
        "directoriesCreated": {},
        "beforeStates": before,
        "baselineConditions": baseline,
        "pendingFile": None,
        "probeNonce": secrets.token_hex(8),
        "probeFiles": {},
        "probeMarker": None,
        "conditionProbes": {},
        "probeCleanupVerified": False,
        "sourceFence": False,
    }
    fs.directory(BASE, 0o755, receipt["directoriesCreated"])
    fs.directory(str(Path(RECEIPT).parent), 0o700, receipt["directoriesCreated"])
    save = lambda: fs.save(RECEIPT, receipt)
    save()
    try:
        for path, specification in value["files"].items():
            fs.directory(str(Path(path).parent), 0o755, receipt["directoriesCreated"])
            receipt["pendingFile"] = {
                "path": path,
                "sha256": specification["sha256"],
                "kind": "guard",
            }
            save()
            receipt["files"][path] = fs.write(path, specification["content"].encode())
            receipt["pendingFile"] = None
            save()
        manager.reload()
        loaded_proof(manager, value, require_loaded=value["host"] == "fredrir-05")
        assert_preserved_conditions(manager, value, baseline)
        probe_conditions(fs, manager, receipt, save)
        require_inert(fs)
        require(
            states(manager, value["host"]) == before,
            "Application or timer state changed during inert installation",
        )
        receipt["status"] = "installed"
        receipt["afterStates"] = states(manager, value["host"])
        receipt["finishedAt"] = int(time.time())
        save()
        return verify_installation(
            receipt,
            require_loaded=value["host"] == "fredrir-05",
            _filesystem=fs,
            _manager=manager,
        )
    except BaseException as error:
        try:
            release_probe_references(fs, manager, receipt)
        except Exception as cleanup_error:
            receipt["probeCleanupFailureType"] = type(cleanup_error).__name__
        receipt["status"], receipt["failureType"] = (
            "recovery-required",
            type(error).__name__,
        )
        save()
        raise


def rollback(receipt, *, _filesystem=None, _manager=None):
    fs, manager = _filesystem or Filesystem(), _manager or Manager()
    value = validate_receipt(receipt)
    if _filesystem is None:
        live_identity(value["host"])
    require(
        receipt["status"]
        in {"installed", "installing", "recovery-required", "rolling-back"},
        "Guard rollback receipt differs",
    )
    require_inert(fs)
    before = states(manager, value["host"])
    release_probe_references(fs, manager, receipt)
    unit, _, _ = probe_specifications(value, receipt["probeNonce"])
    require(
        all(
            manager.properties(name, unit)["ActiveState"] in {"inactive", "failed"}
            for name in ["system", *USERS]
        ),
        "Owned probe is still active",
    )
    files = dict(receipt["files"]) | dict(receipt["probeFiles"])
    pending = receipt["pendingFile"]
    if pending:
        observed = fs.metadata(pending["path"])
        if observed["exists"]:
            require(
                observed["mode"] == "0644" and observed["sha256"] == pending["sha256"],
                "Pending guard write has unknown content",
            )
            files[pending["path"]] = observed
    marker = BASE + "/probe-" + receipt["probeNonce"]
    if receipt["probeMarker"]:
        files[marker] = receipt["probeMarker"]
    files = {
        path: expected
        for path, expected in files.items()
        if fs.metadata(path)["exists"]
    }
    for path, expected in files.items():
        fs.metadata(path, expected)
    receipt["status"] = "rolling-back"
    fs.save(RECEIPT, receipt)
    for path, expected in files.items():
        fs.unlink(path, expected)
    manager.reload()
    require(
        states(manager, value["host"]) == before,
        "Application or timer state changed during rollback",
    )
    receipt["status"] = "rolled-back"
    receipt["rolledBackAt"] = int(time.time())
    receipt["retainedDirectories"] = fs.remove_directories(
        receipt["directoriesCreated"]
    )
    fs.save(RECEIPT, receipt)
    return {
        "host": value["host"],
        "status": "rolled-back",
        "sourceFence": False,
        "applicationStatesUnchanged": True,
        "retainedDirectories": receipt["retainedDirectories"],
    }


@contextmanager
def operation_lock():
    descriptor = os.open(
        "/run/lock/infra-evacuation-guards.lock",
        os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW,
        0o600,
    )
    try:
        info = os.fstat(descriptor)
        require(
            stat.S_ISREG(info.st_mode)
            and info.st_uid == 0
            and stat.S_IMODE(info.st_mode) == 0o600
            and info.st_nlink == 1,
            "Guard operation lock differs",
        )
        fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield
    finally:
        os.close(descriptor)


def main():
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="action", required=True)
    generate = commands.add_parser("plan")
    generate.add_argument("host", choices=tuple(HOSTS))
    generate.add_argument("--candidate-sha256", required=True)
    apply = commands.add_parser("install")
    apply.add_argument("plan", type=Path)
    check = commands.add_parser("verify")
    check.add_argument("--allow-unloaded", action="store_true")
    commands.add_parser("rollback")
    args = parser.parse_args()
    os.umask(0o077)
    try:
        if args.action == "plan":
            result = plan(args.host, args.candidate_sha256)
        elif args.action == "verify":
            result = verify_installation(
                RECEIPT, require_loaded=not args.allow_unloaded
            )
        else:
            with operation_lock():
                fs = Filesystem()
                result = (
                    install(fs.read_json(str(args.plan.absolute())))
                    if args.action == "install"
                    else rollback(fs.read_json(RECEIPT))
                )
        print(json.dumps(result, indent=2))
        return 0
    except (
        OSError,
        ValueError,
        KeyError,
        TypeError,
        subprocess.SubprocessError,
    ) as error:
        print(
            json.dumps(
                {
                    "error": "Guard operation failed; inspect the private receipt",
                    "exceptionType": type(error).__name__,
                }
            )
        )
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
