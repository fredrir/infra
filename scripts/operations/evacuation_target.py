import argparse
import hashlib
import http.client
import json
import os
import re
import shutil
import signal
import stat
import subprocess
import time
from pathlib import Path

import evacuation_cutover as cutover
from evacuation_execution import (
    MARKERS,
    TARGET_BASE,
    Commands,
    backup_gate,
    canonical,
    durable_json,
    guard_state,
    host_identity,
    marker_value,
    outage_remaining,
    private_marker,
    require,
    stopped,
    unit_state,
    verify_fresh_source,
)
from evacuation_images import open_private, private_directory, read_json
from evacuation_staging import SERVICE_USERS, USERS, validate_plan

DATA_PARENT = Path("/home/llunde-backend/data")
MAX_RESTORE_BYTES = 1024**3


def kernel_profile(document):
    state = document.get("State", {})
    pid = state.get("Pid")
    if state.get("Running") is not True or type(pid) is not int or pid <= 1:
        raise ValueError("Running owned process required for kernel proof")

    def identity():
        with Path(f"/proc/{pid}/stat").open("rb") as source:
            data = source.read(8193)
        if len(data) > 8192:
            raise ValueError("Process metadata exceeds bound")
        fields = data.rsplit(b")", 1)[-1].split()
        if len(fields) < 20 or not fields[19].isdigit():
            raise ValueError("Process start identity unavailable")
        return fields[19].decode()

    before = identity()
    with Path(f"/proc/{pid}/status").open("rb") as source:
        raw = source.read(16385)
    if len(raw) > 16384:
        raise ValueError("Kernel status exceeds bound")
    keys = {"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb", "NoNewPrivs"}
    rows = [
        line.split(":", 1)
        for line in raw.decode().splitlines()
        if line.split(":", 1)[0] in keys
    ]
    values = {key: value.strip() for key, value in rows}
    if (
        len(rows) != 6
        or set(values) != keys
        or values["NoNewPrivs"] != "1"
        or any(not re.fullmatch(r"0{16}", values[key]) for key in keys - {"NoNewPrivs"})
    ):
        raise ValueError("Kernel capabilities or no-new-privileges differ")
    if identity() != before:
        raise ValueError("Owned process changed during kernel proof")
    return {
        "hostPID": pid,
        "processStartIdentity": before,
        "capabilities": {key: 0 for key in sorted(keys - {"NoNewPrivs"})},
        "noNewPrivileges": True,
    }


CHECKER_GUARD = """seen=0
while IFS=: read -r key value; do
  case "$key" in
    CapInh|CapPrm|CapEff|CapBnd|CapAmb)
      set -- $value
      [ "$#" -eq 1 ] && [ "$1" = 0000000000000000 ] || exit 73
      seen=$((seen + 1)) ;;
    NoNewPrivs)
      set -- $value
      [ "$#" -eq 1 ] && [ "$1" = 1 ] || exit 73
      seen=$((seen + 1)) ;;
  esac
done < /proc/$$/status
[ "$seen" -eq 6 ] || exit 73
printf 'infra-kernel-zero-caps-nnp\\n'
exec valkey-check-aof /data/appendonlydir/appendonly.aof.manifest
"""


def database_secret(host):
    require(host in cutover.HOSTS, "Fixed database credential host required")
    if host == "fredrir-09":
        path = Path("/run/infra-evacuation/llunde/llunde-backend/db.env")
        expected = {"uid": 0, "gid": 2001, "mode": 0o640}
    else:
        generation = Path("/run/secrets").resolve(strict=True)
        link = Path("/run/secrets").lstat()
        require(
            stat.S_ISLNK(link.st_mode)
            and link.st_uid == 0
            and re.fullmatch(r"/run/secrets[.]d/[0-9]+", str(generation)),
            "Exact root-owned SOPS generation required",
        )
        path = generation / "llunde-backend-db-env"
        expected = {"uid": 2001, "gid": None, "mode": 0o400}
    descriptor = os.open("/", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for part in path.parts[1:-1]:
            child = os.open(
                part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor
            )
            os.close(descriptor)
            descriptor = child
            info = os.fstat(descriptor)
            require(
                info.st_uid == 0 and not info.st_mode & 0o022,
                "Unsafe database credential parent",
            )
        parent = os.fstat(descriptor)
        if host == "fredrir-09":
            require(
                parent.st_gid == 2001 and stat.S_IMODE(parent.st_mode) == 0o750,
                "Exact database credential group directory required",
            )
        secret = os.open(
            path.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=descriptor
        )
        try:
            info = os.fstat(secret)
            require(
                stat.S_ISREG(info.st_mode)
                and info.st_uid == expected["uid"]
                and (expected["gid"] is None or info.st_gid == expected["gid"])
                and stat.S_IMODE(info.st_mode) == expected["mode"]
                and info.st_nlink == 1
                and 0 < info.st_size <= 65536,
                "Database credential ownership or mode differs",
            )
        finally:
            os.close(secret)
    finally:
        os.close(descriptor)
    return path


def tree_bytes(path):
    size, count = 0, 0
    for parent, directories, files in os.walk(path, followlinks=False):
        for name in directories + files:
            item = Path(parent) / name
            info = item.lstat()
            require(
                stat.S_ISREG(info.st_mode) or stat.S_ISDIR(info.st_mode),
                "Restore tree contains a link or special file",
            )
            size += info.st_size if stat.S_ISREG(info.st_mode) else 0
            count += 1
            require(
                size <= MAX_RESTORE_BYTES and count <= 100000,
                "Restore tree exceeds sampled budget",
            )
    return size


def mounted_profile(document, image, workspace, memory, *, checker_proof=False):
    settings = document["HostConfig"]
    require(
        document["Image"].removeprefix("sha256:") == image.removeprefix("sha256:"),
        "Restore image differs",
    )
    require(
        settings["NetworkMode"] == "none"
        and not settings.get("PortBindings")
        and settings["Memory"] == memory * 1024**2
        and settings["MemorySwap"] == memory * 1024**2
        and settings["PidsLimit"] == 128,
        "Restore network or resource limits differ",
    )
    require(
        settings.get("Privileged") is False, "Privileged restore container forbidden"
    )
    require(
        settings.get("NanoCpus") == 1000000000
        or settings.get("CpuQuota") == settings.get("CpuPeriod") == 100000,
        "Restore CPU quota differs",
    )
    require(
        all(
            key in document and (document[key] is None or document[key] == [])
            for key in ("EffectiveCaps", "BoundingCaps")
        ),
        "Restore capabilities must be explicitly empty",
    )
    require(
        any(
            value.startswith("no-new-privileges")
            for value in settings.get("SecurityOpt", [])
        ),
        "Restore no-new-privileges required",
    )
    require(document["Config"]["User"] == "999:999", "Restore namespace user differs")
    require(
        type(document["Config"].get("Timeout")) is int
        and document["Config"]["Timeout"] == 120,
        "Disposable restore container native lifetime differs",
    )
    binds = [value for value in document.get("Mounts", []) if value["Type"] == "bind"]
    require(
        len(binds) == 1
        and Path(binds[0]["Source"]).resolve().is_relative_to(workspace.resolve()),
        "Restore bind escaped private workspace",
    )
    require(
        not any(
            value.split("=", 1)[0]
            in {
                "DOPPLER_TOKEN",
                "TUNNEL_TOKEN",
                "AWS_ACCESS_KEY_ID",
                "AWS_SECRET_ACCESS_KEY",
            }
            for value in document["Config"].get("Env", [])
        ),
        "Application or off-host credentials forbidden in restore",
    )
    if checker_proof:
        require(
            document["State"]["Running"] is False
            and document["State"]["ExitCode"] == 0
            and document["Config"].get("Entrypoint") == ["/bin/sh"]
            and document["Config"].get("Cmd") == ["-eu", "-c", CHECKER_GUARD],
            "Exact successful in-process checker guard required",
        )
        return {
            "sameProcessCheckerGuard": True,
            "noNewPrivileges": True,
            "capabilities": {
                key: 0 for key in ("CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb")
            },
        }
    return kernel_profile(document)


class Restore:
    def __init__(self, pair, candidate, receipt, output, commands=None):
        self.pair, self.candidate = Path(pair), Path(candidate)
        self.manifest = cutover.verify_pair(pair, candidate)
        self.plan = validate_plan(candidate)
        self.host = self.manifest["destination"]
        self.receipt, self.output = receipt, Path(output)
        self.commands = commands or Commands(150)
        self.names = []
        self.workspace = None
        self.record = {
            "schemaVersion": 1,
            "kind": "evacuation-native-data-restore",
            "host": self.host,
            "direction": self.manifest["direction"],
            "pairManifestSHA256": hashlib.sha256(canonical(self.manifest)).hexdigest(),
            "candidateSHA256": self.manifest["candidateSHA256"],
            "applicationStarted": False,
            "connectorStarted": False,
            "phase": "preflight",
            "nativeRestoreVerified": False,
            "promoted": False,
        }

    def call(self, arguments, **kwargs):
        self.monitor()
        return self.commands.user(
            "llunde-backend", arguments, monitor=self.monitor, **kwargs
        )

    def monitor(self):
        if self.workspace:
            tree_bytes(self.workspace)
            require(
                shutil.disk_usage(self.workspace).free >= 10 * 1024**3,
                "Restore free-disk reserve exhausted",
            )

    def preflight(self):
        host_identity(self.host)
        guards = guard_state(self.receipt)
        require(guards["host"] == self.host, "Destination guard identity differs")
        private_directory(self.output.parent, 0)
        require(
            not self.output.exists() and not self.output.is_symlink(),
            "Fresh restore receipt required",
        )
        for user, units in cutover.APP_UNITS.items():
            for unit in units:
                require(
                    stopped(unit_state(self.commands, unit, user)),
                    "Destination application and data must be stopped",
                )
        if self.host == "fredrir-09":
            for name in (
                "stage-approved",
                "restore-approved",
                "source-fenced",
                "edge-approved",
                "target-writer-start-attempted",
            ):
                require(
                    not (TARGET_BASE / name).exists()
                    and not (TARGET_BASE / name).is_symlink(),
                    "Fresh target checkpoint state required",
                )
        else:
            for name in ("source-locked", "reconciliation-locked"):
                marker_value(MARKERS / name)
        token = os.urandom(6).hex()
        self.workspace = Path("/home/llunde-backend/.infra-final-restore-" + token)
        require(
            not self.workspace.exists() and not self.workspace.is_symlink(),
            "Fresh private restore workspace required",
        )
        self.workspace.mkdir(mode=0o700)
        os.chown(self.workspace, 2001, 2001)
        self.record["workspace"] = str(self.workspace)
        self.record["token"] = token
        durable_json(self.output, self.record)
        side = "sourceSubIdStart" if self.host == "fredrir-05" else "targetSubIdStart"
        for kind in ("uid_map", "gid_map"):
            _, raw = self.call(["podman", "unshare", "cat", "/proc/self/" + kind])
            mapping = [
                [int(value) for value in line.split()]
                for line in raw.decode().splitlines()
            ]
            require(
                mapping
                == [
                    [0, 2001, 1],
                    [1, self.plan["users"]["llunde-backend"][side], 65536],
                ],
                "Native namespace mapping differs",
            )
        for image in cutover.DATA_IMAGES[:2]:
            self.call(["podman", "image", "exists", self.manifest["imageIDs"][image]])

    def data_directory(self, name):
        path = self.workspace / name
        path.mkdir(mode=0o700)
        os.chown(path, 2001, 2001)
        self.call(["podman", "unshare", "chown", "999:999", str(path)])
        return path

    def run_container(
        self, component, path, memory, extra, arguments, *, foreground=False
    ):
        name = "infra-final-restore-" + self.record["token"] + "-" + component
        image = self.manifest["imageIDs"][
            "llunde-postgres" if component == "postgres" else "llunde-valkey"
        ]
        argv = [
            "podman",
            "run",
            "--pull=never",
            "--timeout=120",
            "--name",
            name,
            "--network=none",
            "--user=999:999",
            "--cap-drop=ALL",
            "--security-opt=no-new-privileges",
            "--cpus=1",
            "--memory=" + str(memory) + "m",
            "--memory-swap=" + str(memory) + "m",
            "--pids-limit=128",
            "--volume",
            str(path)
            + (":/var/lib/postgresql/data" if component == "postgres" else ":/data"),
            *extra,
            image,
            *arguments,
        ]
        if not foreground:
            argv.insert(2, "--detach")
        self.names.append(name)
        _, output = self.call(argv, timeout=45)
        _, raw = self.call(["podman", "inspect", name])
        inspected = json.loads(raw)[0]
        require(
            inspected.get("Name") == name
            and re.fullmatch(r"[a-f0-9]{64}", inspected.get("Id", "")),
            "Owned restore container identity differs",
        )
        if foreground:
            require(
                component == "aof-check"
                and output.splitlines().count(b"infra-kernel-zero-caps-nnp") == 1,
                "Foreground checker kernel proof missing",
            )
        proof = mounted_profile(
            inspected, image, self.workspace, memory, checker_proof=foreground
        )
        self.record.setdefault("kernelProfiles", {})[component] = proof
        durable_json(self.output, self.record, replace=True)
        return name

    def wait(self, name, command, expected, seconds=30):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            code, value = self.call(["podman", "exec", name, *command], check=False)
            if code == 0 and value.strip() == expected:
                return
            time.sleep(0.2)
        raise ValueError("Native restored service readiness deadline reached")

    def remove(self, name):
        self.call(["podman", "rm", name])
        self.names.remove(name)

    def wait_exit(self, name):
        self.wait_state(name)
        self.remove(name)

    def wait_state(self, name):
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            _, raw = self.call(
                ["podman", "inspect", "--format", "{{json .State}}", name]
            )
            value = json.loads(raw)
            if not value["Running"]:
                require(
                    value["ExitCode"] == 0 and not value.get("OOMKilled"),
                    "Successful native restore shutdown required",
                )
                return
            time.sleep(0.2)
        raise ValueError("Restore process remains; workspace retained")

    def postgres(self):
        self.record["phase"] = "postgres"
        durable_json(self.output, self.record, replace=True)
        secret = database_secret(self.host)
        name = self.run_container(
            "postgres",
            self.data_directory("postgres"),
            768,
            [
                "--env-file",
                str(secret),
                "--env=POSTGRES_DB=llunde",
                "--env=POSTGRES_USER=llunde",
                "--env=PGDATA=/var/lib/postgresql/data/pgdata",
            ],
            ["postgres", "-c", "shared_buffers=128MB"],
        )
        auth = ["sh", "-c", 'export PGPASSWORD="$POSTGRES_PASSWORD"; exec "$@"', "sh"]
        psql = [
            "psql",
            "--host=127.0.0.1",
            "--username=llunde",
            "--dbname=llunde",
            "--tuples-only",
            "--no-align",
        ]
        self.wait(name, auth + psql + ["--command", "SELECT 1"], b"1")
        with open_private(self.pair / "database.dump", 0, cutover.MAX_FILE) as source:
            self.call(
                ["podman", "exec", "-i", name, "pg_restore", "--list"],
                source=source,
                maximum=1048576,
            )
        with open_private(self.pair / "database.dump", 0, cutover.MAX_FILE) as source:
            self.call(
                [
                    "podman",
                    "exec",
                    "-i",
                    name,
                    *auth,
                    "pg_restore",
                    "--exit-on-error",
                    "--single-transaction",
                    "--no-owner",
                    "--no-acl",
                    "--host=127.0.0.1",
                    "--username=llunde",
                    "--dbname=llunde",
                ],
                source=source,
                timeout=90,
            )
        query = "SELECT json_build_object('tables',(SELECT count(*) FROM pg_tables WHERE schemaname='public'),'constraints',(SELECT count(*) FROM pg_constraint WHERE connamespace='public'::regnamespace));"
        _, raw = self.call(["podman", "exec", name, *auth, *psql, "--command", query])
        self.record["postgresSchema"] = json.loads(raw)
        require(
            self.record["postgresSchema"]
            == self.manifest["fenceAfter"]["postgresSchema"],
            "Restored schema differs from final source observation",
        )
        self.call(
            [
                "podman",
                "exec",
                name,
                "pg_ctl",
                "--pgdata=/var/lib/postgresql/data/pgdata",
                "--mode=fast",
                "--wait",
                "--timeout=30",
                "stop",
            ],
            timeout=35,
        )
        self.wait_exit(name)

    def archive_valkey(self, path, name):
        archive = self.workspace / name
        with os.fdopen(
            os.open(
                archive, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600
            ),
            "wb",
        ) as output:
            self.call(
                [
                    "podman",
                    "unshare",
                    "tar",
                    "--numeric-owner",
                    "--format=posix",
                    "-C",
                    str(path),
                    "-cf",
                    "-",
                    ".",
                ],
                output=output,
                maximum=cutover.MAX_FILE,
            )
        return cutover.valkey_archive(archive, 0)

    def valkey(self):
        self.record["phase"] = "valkey"
        durable_json(self.output, self.record, replace=True)
        path = self.data_directory("valkey")
        with open_private(self.pair / "valkey.tar", 0, cutover.MAX_FILE) as source:
            self.call(
                [
                    "podman",
                    "unshare",
                    "tar",
                    "--numeric-owner",
                    "--same-owner",
                    "--same-permissions",
                    "-C",
                    str(path),
                    "-xf",
                    "-",
                ],
                source=source,
                timeout=30,
            )
        require(
            self.archive_valkey(path, "restored.tar") == self.manifest["valkeyEntries"],
            "Restored Valkey bytes or namespace ownership differ",
        )
        name = self.run_container(
            "aof-check",
            path,
            192,
            ["--entrypoint=/bin/sh"],
            ["-eu", "-c", CHECKER_GUARD],
            foreground=True,
        )
        self.wait_exit(name)
        require(
            self.archive_valkey(path, "checked.tar") == self.manifest["valkeyEntries"],
            "AOF checker modified persisted files",
        )
        name = self.run_container(
            "valkey",
            path,
            384,
            ["--entrypoint=valkey-server"],
            [
                "--dir",
                "/data",
                "--save",
                "",
                "--appendonly",
                "yes",
                "--appendfsync",
                "everysec",
                "--aof-load-truncated",
                "no",
                "--maxmemory",
                "192mb",
                "--maxmemory-policy",
                "noeviction",
            ],
        )
        self.wait(name, ["valkey-cli", "PING"], b"PONG")
        _, raw = self.call(
            ["podman", "exec", name, "valkey-cli", "--raw", "INFO", "persistence"]
        )
        from evacuation_execution import info_fields

        info = info_fields(raw)
        require(
            info.get("aof_last_write_status") == "ok"
            and info.get("aof_enabled") == "1",
            "Restored AOF persistence failed",
        )
        self.call(
            ["podman", "exec", name, "valkey-cli", "SHUTDOWN", "NOSAVE"], check=False
        )
        self.wait_exit(name)
        self.record["valkey"] = {
            "nativeAofVerified": True,
            "prestartNamespaceFilesVerified": True,
            "nativeCheckerPreservedBytesAndOwnership": True,
            "expiryDuringRestorePossible": True,
        }

    def run(self):
        try:
            self.preflight()
            self.postgres()
            self.valkey()
            require(
                cutover.verify_pair(self.pair, self.candidate) == self.manifest,
                "Final pair changed during restore",
            )
            require(not self.names, "Restore containers remain")
            self.record.update(
                phase="native-restore-complete",
                nativeRestoreVerified=True,
                workspaceBytes=tree_bytes(self.workspace),
                containersRemoved=True,
            )
            durable_json(self.output, self.record, replace=True)
            return self.record
        except BaseException:
            cleanup = Commands(30)
            retained = []
            for name in self.names:
                try:
                    require(
                        name.startswith(
                            "infra-final-restore-" + self.record["token"] + "-"
                        ),
                        "Cleanup container identity differs",
                    )
                    code, _ = cleanup.user(
                        "llunde-backend",
                        ["podman", "rm", "--force", "--ignore", name],
                        timeout=10,
                        check=False,
                    )
                    if code:
                        retained.append(name)
                except (OSError, ValueError, subprocess.SubprocessError):
                    retained.append(name)
            self.record.update(
                failurePhase=self.record["phase"],
                phase="failed-retained",
                retainedContainers=retained,
                containersRemoved=not retained,
                workspaceRetained=True,
                automaticRollback=False,
            )
            if self.output.exists():
                durable_json(self.output, self.record, replace=True)
            raise


def promote_data(pair, candidate, receipt_path, commands=None):
    manifest = cutover.verify_pair(pair, candidate)
    receipt = read_json(receipt_path, 0)
    host_identity(manifest["destination"])
    require(
        receipt["kind"] == "evacuation-native-data-restore"
        and receipt["nativeRestoreVerified"] is True
        and receipt["containersRemoved"] is True
        and receipt["promoted"] is False
        and receipt["pairManifestSHA256"]
        == hashlib.sha256(canonical(manifest)).hexdigest(),
        "Successful exact native restore required",
    )
    require(re.fullmatch(r"[a-f0-9]{12}", receipt["token"]), "Restore token required")
    workspace = Path("/home/llunde-backend/.infra-final-restore-" + receipt["token"])
    require(
        str(workspace) == receipt["workspace"] and not workspace.is_symlink(),
        "Owned restore workspace required",
    )
    commands = commands or Commands(30)
    for user, units in cutover.APP_UNITS.items():
        for unit in units:
            require(
                stopped(unit_state(commands, unit, user)),
                "Destination must remain stopped during promotion",
            )
    parent = DATA_PARENT.lstat()
    require(
        stat.S_ISDIR(parent.st_mode)
        and parent.st_uid == 2001
        and parent.st_mode & 0o022 == 0,
        "Owned data parent required",
    )
    receipt["promotionIntent"] = {}
    for name in ("postgres", "valkey"):
        fresh, destination = workspace / name, DATA_PARENT / name
        previous = DATA_PARENT / (".previous-" + receipt["token"] + "-" + name)
        require(
            fresh.is_dir()
            and not fresh.is_symlink()
            and not previous.exists()
            and not previous.is_symlink(),
            "Fresh restore and preservation paths required",
        )
        require(
            not destination.is_symlink()
            and (not destination.exists() or destination.is_dir()),
            "Existing data path must be a directory",
        )
        receipt["promotionIntent"][name] = {
            "fresh": str(fresh),
            "destination": str(destination),
            "preserved": str(previous) if destination.exists() else None,
            "freshInode": fresh.stat().st_ino,
            "device": fresh.stat().st_dev,
        }
    durable_json(receipt_path, receipt, replace=True)
    for item in receipt["promotionIntent"].values():
        if item["preserved"]:
            os.rename(item["destination"], item["preserved"])
        os.rename(item["fresh"], item["destination"])
    descriptor = os.open(DATA_PARENT, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    receipt.update(promoted=True, phase="data-promoted-originals-preserved")
    durable_json(receipt_path, receipt, replace=True)
    return receipt


def inert_markers_absent():
    markers = [MARKERS / name for name in ("source-locked", "reconciliation-locked")]
    markers += [
        TARGET_BASE / name
        for name in (
            "stage-approved",
            "restore-approved",
            "source-fenced",
            "edge-approved",
            "target-writer-start-attempted",
        )
    ]
    require(
        all(not path.exists() and not path.is_symlink() for path in markers),
        "Inert operation requires absent fence and approval markers",
    )


def promote_units(candidate, guard_receipt, output, commands=None):
    host_identity("fredrir-09")
    plan = validate_plan(candidate)
    commands = commands or Commands(60)
    import evacuation_guards
    from evacuation_preflight import directory_state, quadlet_paths

    fs = evacuation_guards.Filesystem()
    evacuation_guards.verify_installation(guard_receipt, require_loaded=False)
    private_directory(Path(output).parent, 0)
    inert_markers_absent()
    for path in quadlet_paths():
        require(
            directory_state(path)["empty"],
            "Existing active Quadlet search path requires inspection",
        )
    for user, units in cutover.APP_UNITS.items():
        for unit in units:
            require(
                stopped(unit_state(commands, unit, user)),
                "Existing application unit must remain inactive",
            )
        _, containers = commands.user(
            user, ["podman", "ps", "--all", "--format", "{{.Names}}"]
        )
        require(not containers.strip(), "Empty service-user container stores required")
    paths = []
    for user, uid in USERS.items():
        directory = Path("/etc/containers/systemd/users") / str(uid)
        require(
            not directory.is_symlink()
            and (not directory.exists() or not any(directory.iterdir())),
            "Empty target Quadlet directory required",
        )
        paths += [
            (path, directory / path.name)
            for path in (Path(candidate) / "units" / user).iterdir()
        ]
    paths.append(
        (
            Path(candidate) / "units/Caddyfile",
            Path("/etc/infra-evacuation/llunde/Caddyfile"),
        )
    )
    require(
        not any(
            destination.exists() or destination.is_symlink() for _, destination in paths
        ),
        "Existing candidate files must not be overwritten",
    )
    record = {
        "schemaVersion": 1,
        "kind": "evacuation-target-unit-promotion",
        "candidateSHA256": hashlib.sha256(
            (Path(candidate) / "staging.json").read_bytes()
        ).hexdigest(),
        "files": {
            str(destination): {
                "sha256": hashlib.sha256(source.read_bytes()).hexdigest(),
                "metadata": None,
            }
            for source, destination in paths
        },
        "directoriesCreated": {},
        "completed": False,
        "applicationsStarted": False,
    }
    durable_json(output, record)
    for source, destination in paths:
        inert_markers_absent()
        data = source.read_bytes()
        require(
            hashlib.sha256(data).hexdigest()
            == record["files"][str(destination)]["sha256"],
            "Candidate changed during inert promotion",
        )
        fs.directory(str(destination.parent), 0o755, record["directoriesCreated"])
        record["pendingFile"] = str(destination)
        durable_json(output, record, replace=True)
        record["files"][str(destination)]["metadata"] = fs.write(str(destination), data)
        record["pendingFile"] = None
        durable_json(output, record, replace=True)
    inert_markers_absent()
    for user in USERS:
        commands.user(user, ["systemctl", "--user", "daemon-reload"])
    guard_state(guard_receipt)
    record["checkpointProof"] = checkpoint_proof()
    verify_unit_files(record)
    for user, units in cutover.APP_UNITS.items():
        for unit in units:
            require(
                stopped(unit_state(commands, unit, user)),
                "Inert promotion unexpectedly started a unit",
            )
    inert_markers_absent()
    record["completed"] = True
    durable_json(output, record, replace=True)
    return record


def checkpoint_proof(manager=None):
    from evacuation_guards import Manager

    manager = manager or Manager()
    proof = {}
    for service, user in SERVICE_USERS.items():
        markers = ["stage-approved"]
        if service == "llunde-backend":
            markers += ["restore-approved", "source-fenced"]
        if service == "cloudflared":
            markers.append("edge-approved")
        conditions = manager.conditions(user, service + ".service")
        for marker in markers:
            expected = ["ConditionPathExists", False, False, str(TARGET_BASE / marker)]
            require(
                sum(row[:4] == expected for row in conditions) == 1,
                "Generated target checkpoint condition differs",
            )
        proof[service] = markers
    return proof


def allowed_unit_paths():
    return {
        f"/etc/containers/systemd/users/{USERS[user]}/{service}.container"
        for service, user in SERVICE_USERS.items()
    } | {
        "/etc/containers/systemd/users/2001/llunde-backend-data.network",
        "/etc/infra-evacuation/llunde/Caddyfile",
    }


def verify_unit_files(receipt, filesystem=None):
    from evacuation_guards import Filesystem
    from evacuation_preflight import quadlet_paths

    fs = filesystem or Filesystem()
    require(
        receipt["schemaVersion"] == 1
        and receipt["kind"] == "evacuation-target-unit-promotion"
        and set(receipt["files"]) == allowed_unit_paths(),
        "Exact target promotion receipt required",
    )
    allowed = set(receipt["files"])
    for path, value in receipt["files"].items():
        require(
            value["metadata"] is not None
            and re.fullmatch(r"[a-f0-9]{64}", value["sha256"]),
            "Completed unit file receipt required",
        )
        info = fs.metadata(path, value["metadata"])
        require(
            info["mode"] == "0644" and info["sha256"] == value["sha256"],
            "Promoted unit content changed",
        )
    for root in quadlet_paths():
        physical = fs.path(root)
        if not physical.exists():
            require(not physical.is_symlink(), "Unexpected Quadlet path symlink")
            continue
        with fs.directory_fd(root):
            pass
        count = 0
        for parent, directories, files in os.walk(physical, followlinks=False):
            for name in directories + files:
                path = Path(parent) / name
                relative = "/" + str(path.relative_to(fs.root))
                require(not path.is_symlink(), "Unexpected Quadlet symlink")
                count += 1
                require(count <= 32, "Quadlet inventory exceeds budget")
                if path.is_dir():
                    require(
                        any(value.startswith(relative + "/") for value in allowed),
                        "Unexpected Quadlet directory",
                    )
                else:
                    require(
                        relative in allowed and path.is_file(),
                        "Unexpected Quadlet file",
                    )


def rollback_units(receipt_path, commands=None):
    host_identity("fredrir-09")
    from evacuation_guards import Filesystem

    fs = Filesystem()
    receipt = read_json(receipt_path, 0)
    require(
        receipt["schemaVersion"] == 1
        and receipt["kind"] == "evacuation-target-unit-promotion"
        and set(receipt["files"]) == allowed_unit_paths()
        and receipt.get("rolledBack") is not True,
        "Exact target promotion receipt required",
    )
    allowed_directories = {
        str(parent)
        for path in allowed_unit_paths()
        for parent in Path(path).parents
        if str(parent) != "/"
    }
    require(
        set(receipt["directoriesCreated"]) <= allowed_directories,
        "Unowned promotion directory",
    )
    inert_markers_absent()
    commands = commands or Commands(60)
    for user, units in cutover.APP_UNITS.items():
        for unit in units:
            require(
                stopped(unit_state(commands, unit, user)),
                "Unit rollback requires inactive applications",
            )
    remove = {}
    for path, value in receipt["files"].items():
        metadata = fs.metadata(path)
        if not metadata["exists"]:
            continue
        require(
            value["metadata"] is not None or receipt.get("pendingFile") == path,
            "Unrecorded file must remain untouched",
        )
        require(
            metadata["mode"] == "0644" and metadata["sha256"] == value["sha256"],
            "Changed candidate file must remain untouched",
        )
        if value["metadata"] is not None:
            require(metadata == value["metadata"], "Candidate file identity changed")
        remove[path] = metadata
    receipt["rollbackStarted"] = True
    durable_json(receipt_path, receipt, replace=True)
    for path, value in remove.items():
        inert_markers_absent()
        fs.unlink(path, value)
    inert_markers_absent()
    for user in USERS:
        commands.user(user, ["systemctl", "--user", "daemon-reload"])
    receipt["retainedDirectories"] = fs.remove_directories(
        receipt["directoriesCreated"]
    )
    inert_markers_absent()
    receipt["rolledBack"] = True
    durable_json(receipt_path, receipt, replace=True)
    return receipt


def promoted_data(manifest, receipt):
    require(
        receipt["schemaVersion"] == 1
        and receipt["kind"] == "evacuation-native-data-restore"
        and receipt["nativeRestoreVerified"] is True
        and receipt["promoted"] is True
        and receipt["pairManifestSHA256"]
        == hashlib.sha256(canonical(manifest)).hexdigest(),
        "Promoted native restore of this pair required",
    )
    for name in ("postgres", "valkey"):
        path = DATA_PARENT / name
        record = receipt["promotionIntent"][name]
        info = path.lstat()
        require(
            stat.S_ISDIR(info.st_mode)
            and (info.st_dev, info.st_ino) == (record["device"], record["freshInode"])
            and record["destination"] == str(path),
            "Promoted data directory identity changed",
        )


def local_http(port, path, headers=None):
    require(
        (port, path) in ((8080, "/health"), (8080, "/ready"), (8081, "/"), (8085, "/")),
        "Fixed local acceptance endpoint required",
    )
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=3)
    try:
        connection.request("GET", path, headers=headers or {})
        response = connection.getresponse()
        body = response.read(1048577)
        require(len(body) <= 1048576, "Local response exceeds budget")
        return {
            "status": response.status,
            "bytes": len(body),
            "sha256": hashlib.sha256(body).hexdigest(),
        }
    finally:
        connection.close()


def private_acceptance(commands, seconds=60, *, connector_active=False):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            checks = {
                "health": local_http(8080, "/health"),
                "ready": local_http(8080, "/ready"),
                "frontend": local_http(8081, "/", {"Host": "llunde.no"}),
                "proxy": local_http(
                    8085, "/", {"Host": "llunde.no", "CF-Connecting-IP": "192.0.2.10"}
                ),
            }
            if (
                all(value["status"] == 200 for value in checks.values())
                and checks["frontend"]["bytes"] > 0
                and checks["frontend"]["sha256"] == checks["proxy"]["sha256"]
            ):
                connector = unit_state(commands, "cloudflared.service", "edge")
                require(
                    connector["ActiveState"] == "active"
                    and int(connector["MainPID"]) > 1
                    if connector_active
                    else stopped(connector),
                    "Connector state differs from the acceptance phase",
                )
                return {
                    "checks": checks,
                    "connectorActive": connector_active,
                    "authenticatedUserSessionVerified": False,
                    "requiredUserAction": "Open https://llunde.no, sign in with an existing account, and confirm account-scoped application content after connector handoff.",
                }
        except (OSError, http.client.HTTPException):
            pass
        time.sleep(0.5)
    raise ValueError("Private application acceptance failed")


def start_writer(
    pair,
    candidate,
    guard_receipt,
    restore_receipt,
    backup_receipt,
    independent_restore,
    source_fence,
    output,
    commands=None,
):
    host_identity("fredrir-09")
    manifest = verify_fresh_source(source_fence, pair)
    require(
        manifest["direction"] == "forward", "Forward application activation required"
    )
    require(
        cutover.verify_pair(pair, candidate) == manifest, "Activation candidate differs"
    )
    promoted_data(manifest, restore_receipt)
    retained = backup_gate(pair, backup_receipt, independent_restore)
    require(
        outage_remaining(manifest) >= 660,
        "Preserve startup, connector and original-source rollback budgets",
    )
    guards = guard_state(guard_receipt)
    require(guards["host"] == "fredrir-09", "Target guards required")
    for name in ("source-locked", "reconciliation-locked"):
        require(
            not (MARKERS / name).exists() and not (MARKERS / name).is_symlink(),
            "Target is fenced for recovery",
        )
    for name in (
        "stage-approved",
        "restore-approved",
        "source-fenced",
        "edge-approved",
        "target-writer-start-attempted",
    ):
        require(
            not (TARGET_BASE / name).exists() and not (TARGET_BASE / name).is_symlink(),
            "Existing activation requires explicit recovery",
        )
    commands = commands or Commands(300)
    for user, units in cutover.APP_UNITS.items():
        for unit in units:
            require(
                stopped(unit_state(commands, unit, user)),
                "Unexpected active target unit",
            )
    record = {
        "schemaVersion": 1,
        "kind": "evacuation-target-writer-attempt",
        "host": "fredrir-09",
        "candidateSHA256": manifest["candidateSHA256"],
        "pairManifestSHA256": retained["pairManifestSHA256"],
        "backup": retained,
        "createdAt": time.time(),
        "sourceFenceObservedAt": source_fence["observedAt"],
        "writerStartAttempted": True,
        "connectorStartAttempted": False,
        "privateAcceptancePassed": False,
        "authenticatedUserSessionVerified": False,
        "rollbackRequiresFreshReversePair": True,
    }
    durable_json(output, record)
    private_marker(
        TARGET_BASE / "target-writer-start-attempted",
        {
            "candidateSHA256": manifest["candidateSHA256"],
            "pairManifestSHA256": retained["pairManifestSHA256"],
            "writerStartAttempted": True,
        },
    )
    try:
        verify_fresh_source(source_fence, pair)
        for name in ("stage-approved", "restore-approved", "source-fenced"):
            private_marker(
                TARGET_BASE / name,
                {
                    "candidateSHA256": manifest["candidateSHA256"],
                    "pairManifestSHA256": retained["pairManifestSHA256"],
                },
            )
        for user, units in [
            (
                "llunde-backend",
                [
                    "llunde-postgres.service",
                    "llunde-valkey.service",
                    "llunde-backend.service",
                ],
            ),
            ("llunde-frontend", ["llunde-frontend.service"]),
            ("edge", ["caddy.service"]),
        ]:
            commands.user(user, ["systemctl", "--user", "start", "--no-block", *units])
        record["acceptance"] = private_acceptance(commands)
        record["privateAcceptancePassed"] = True
        durable_json(output, record, replace=True)
        return record
    except BaseException:
        record["failure"] = (
            "Target writer may have changed data; preserve both sources and fence before rollback"
        )
        durable_json(output, record, replace=True)
        raise


def handoff_connector(
    pair, guard_receipt, source_fence, writer_receipt_path, output, commands=None
):
    host_identity("fredrir-09")
    manifest = verify_fresh_source(source_fence, pair)
    require(manifest["direction"] == "forward", "Forward connector handoff required")
    writer = read_json(writer_receipt_path, 0)
    require(
        writer["kind"] == "evacuation-target-writer-attempt"
        and writer["writerStartAttempted"] is True
        and writer["privateAcceptancePassed"] is True
        and writer["pairManifestSHA256"]
        == hashlib.sha256(canonical(manifest)).hexdigest(),
        "Successful exact target writer receipt required",
    )
    marker = marker_value(TARGET_BASE / "target-writer-start-attempted")
    require(
        marker["pairManifestSHA256"] == writer["pairManifestSHA256"]
        and marker["writerStartAttempted"] is True,
        "Persistent writer attempt changed",
    )
    guard_state(guard_receipt)
    commands = commands or Commands(90)
    require(
        stopped(unit_state(commands, "cloudflared.service", "edge")),
        "Existing target connector requires inspection",
    )
    acceptance = private_acceptance(commands, seconds=15)
    verify_fresh_source(source_fence, pair)
    record = {
        "schemaVersion": 1,
        "kind": "evacuation-connector-handoff",
        "host": "fredrir-09",
        "candidateSHA256": manifest["candidateSHA256"],
        "pairManifestSHA256": writer["pairManifestSHA256"],
        "sourceConnectorVerifiedStoppedAt": source_fence["observedAt"],
        "connectorStartAttempted": True,
        "connectorActive": False,
        "providerConnectorInventoryVerified": False,
        "publicApplicationVerified": False,
        "authenticatedUserSessionVerified": False,
        "privateAcceptance": acceptance,
    }
    durable_json(output, record)
    private_marker(
        TARGET_BASE / "edge-approved",
        {
            "candidateSHA256": manifest["candidateSHA256"],
            "pairManifestSHA256": writer["pairManifestSHA256"],
        },
    )
    commands.user(
        "edge", ["systemctl", "--user", "start", "--no-block", "cloudflared.service"]
    )
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        state = unit_state(commands, "cloudflared.service", "edge")
        if state["ActiveState"] == "active" and int(state["MainPID"]) > 1:
            record["connectorActive"] = True
            break
        time.sleep(0.2)
    durable_json(output, record, replace=True)
    require(record["connectorActive"], "Target connector did not become active")
    return record


def main():
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)
    restore = sub.add_parser("restore-data")
    restore.add_argument("pair", type=Path)
    restore.add_argument("candidate", type=Path)
    restore.add_argument("guard_receipt", type=Path)
    restore.add_argument("output", type=Path)
    promote = sub.add_parser("promote-data")
    promote.add_argument("pair", type=Path)
    promote.add_argument("candidate", type=Path)
    promote.add_argument("restore_receipt", type=Path)
    units = sub.add_parser("promote-units")
    units.add_argument("candidate", type=Path)
    units.add_argument("guard_receipt", type=Path)
    units.add_argument("output", type=Path)
    unit_rollback = sub.add_parser("rollback-units")
    unit_rollback.add_argument("receipt", type=Path)
    start = sub.add_parser("start-writer")
    for name in (
        "pair",
        "candidate",
        "guard_receipt",
        "restore_receipt",
        "backup_receipt",
        "independent_restore",
        "source_fence",
        "output",
    ):
        start.add_argument(name, type=Path)
    handoff = sub.add_parser("handoff-connector")
    for name in ("pair", "guard_receipt", "source_fence", "writer_receipt", "output"):
        handoff.add_argument(name, type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    signal.signal(
        signal.SIGTERM,
        lambda *_: (_ for _ in ()).throw(ValueError("Target operation interrupted")),
    )
    try:
        if args.command == "restore-data":
            result = Restore(
                args.pair, args.candidate, read_json(args.guard_receipt, 0), args.output
            ).run()
        elif args.command == "promote-data":
            result = promote_data(args.pair, args.candidate, args.restore_receipt)
        elif args.command == "promote-units":
            result = promote_units(
                args.candidate, read_json(args.guard_receipt, 0), args.output
            )
        elif args.command == "rollback-units":
            result = rollback_units(args.receipt)
        elif args.command == "start-writer":
            result = start_writer(
                args.pair,
                args.candidate,
                read_json(args.guard_receipt, 0),
                read_json(args.restore_receipt, 0),
                read_json(args.backup_receipt, 0),
                read_json(args.independent_restore, 0),
                read_json(args.source_fence, 0),
                args.output,
            )
        else:
            result = handoff_connector(
                args.pair,
                read_json(args.guard_receipt, 0),
                read_json(args.source_fence, 0),
                args.writer_receipt,
                args.output,
            )
        print(json.dumps(result))
        return 0
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
        print(
            json.dumps(
                {
                    "error": "Target operation failed; original data and execution state retained"
                }
            )
        )
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
