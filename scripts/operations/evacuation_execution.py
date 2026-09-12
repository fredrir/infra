import argparse
import fcntl
import hashlib
import json
import math
import os
import pwd
import re
import select
import signal
import socket
import stat
import subprocess
import sys
import tarfile
import time
from datetime import UTC, datetime
from pathlib import Path

import evacuation_cutover as cutover
from evacuation_images import open_private, private_directory, read_json
from evacuation_preflight import PATH, USERS, as_user
from evacuation_staging import validate_plan

MARKERS = Path(cutover.BASE)
TARGET_BASE = Path("/var/lib/infra-evacuation/llunde")
MAX_TRANSFER = 2 * cutover.MAX_FILE + 1048576
UNIT_FIELDS = (
    "LoadState",
    "ActiveState",
    "SubState",
    "MainPID",
    "ExecMainCode",
    "ExecMainStatus",
    "Result",
    "ConditionResult",
)


def require(condition, message):
    if not condition:
        raise ValueError(message)


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


def durable_json(path, value, *, replace=False, mode=0o600):
    path = Path(path)
    temporary = path.with_name(path.name + ".pending")
    require(
        not temporary.exists() and not temporary.is_symlink(),
        "Pending state requires inspection",
    )
    if not replace:
        require(
            not path.exists() and not path.is_symlink(),
            "Existing state must not be overwritten",
        )
    else:
        with open_private(path, os.geteuid(), 4194304):
            pass
    descriptor = os.open(
        temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode
    )
    try:
        with os.fdopen(descriptor, "wb") as output:
            os.fchmod(output.fileno(), mode)
            output.write(canonical(value) + b"\n")
            output.flush()
            os.fsync(output.fileno())
        if replace:
            os.replace(temporary, path)
        else:
            os.link(temporary, path, follow_symlinks=False)
            temporary.unlink()
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        temporary.unlink(missing_ok=True)


class Commands:
    def __init__(self, seconds=300):
        self.deadline = time.monotonic() + seconds

    def run(
        self,
        arguments,
        *,
        timeout=15,
        source=None,
        output=None,
        maximum=262144,
        check=True,
        monitor=None,
    ):
        require(0 < maximum <= MAX_TRANSFER, "Command output budget required")
        remaining = min(timeout, self.deadline - time.monotonic())
        require(remaining > 0, "Execution deadline reached")
        process = subprocess.Popen(
            arguments,
            stdin=source if source is not None else subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            cwd="/",
            env={
                "PATH": PATH,
                "LC_ALL": "C",
                "HOME": "/root",
                "SYSTEMD_PAGER": "cat",
                "SYSTEMD_COLORS": "0",
            },
            close_fds=True,
        )
        deadline, size, chunks, sampled = time.monotonic() + remaining, 0, [], 0
        try:
            while True:
                require(time.monotonic() < deadline, "Command deadline reached")
                if monitor is not None and time.monotonic() - sampled >= 1:
                    monitor()
                    sampled = time.monotonic()
                if not select.select(
                    [process.stdout],
                    [],
                    [],
                    min(0.2, max(0, deadline - time.monotonic())),
                )[0]:
                    continue
                data = os.read(process.stdout.fileno(), 65536)
                if not data:
                    break
                size += len(data)
                require(size <= maximum, "Command output budget exceeded")
                if output is None:
                    chunks.append(data)
                else:
                    output.write(data)
            code = process.wait(timeout=max(0.1, deadline - time.monotonic()))
            require(not check or code == 0, "Native command failed; output withheld")
            return code, b"".join(chunks)
        finally:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=2)
            process.stdout.close()

    def user(self, user, arguments, **kwargs):
        return self.run(as_user(user, arguments), **kwargs)


def unit_state(commands, unit, user=None):
    require(
        re.fullmatch(r"[a-z0-9@.-]+\.(?:service|timer)", unit),
        "Fixed unit name required",
    )
    arguments = [
        "systemctl",
        *(["--user"] if user else []),
        "show",
        unit,
        "--property=" + ",".join(UNIT_FIELDS),
    ]
    _, output = commands.user(user, arguments) if user else commands.run(arguments)
    result = {}
    for line in output.decode().splitlines():
        key, value = line.split("=", 1)
        require(key in UNIT_FIELDS and key not in result, "Unit projection differs")
        result[key] = value
    required = (
        set(UNIT_FIELDS)
        if unit.endswith(".service")
        else set(UNIT_FIELDS) - {"MainPID", "ExecMainCode", "ExecMainStatus"}
    )
    require(required <= set(result), "Complete unit state required")
    return result


def stopped(value):
    return (
        value["ActiveState"] in ("inactive", "failed")
        and value["SubState"]
        not in ("auto-restart", "start", "stop", "stop-sigterm", "stop-sigkill")
        and value.get("MainPID", "0") == "0"
    )


def host_identity(host):
    require(
        host in cutover.HOSTS
        and os.geteuid() == 0
        and socket.gethostname() == cutover.HOSTS[host],
        "Verified root host required",
    )
    for name, uid in USERS.items():
        account = pwd.getpwnam(name)
        require(
            account.pw_uid == account.pw_gid == uid
            and account.pw_dir == "/home/" + name,
            "Service identity differs",
        )


def process_identity(pid):
    require(type(pid) is int and pid > 1, "Native process required")
    try:
        value = Path(f"/proc/{pid}/stat").read_bytes()
    except FileNotFoundError:
        return None
    require(len(value) < 8192, "Process metadata exceeds budget")
    return value.rsplit(b")", 1)[1].split()[19].decode()


def private_marker(path, value):
    require(
        path.parent == MARKERS or path.parent == TARGET_BASE,
        "Fixed marker parent required",
    )
    parent = path.parent.lstat()
    require(
        stat.S_ISDIR(parent.st_mode)
        and parent.st_uid == 0
        and stat.S_IMODE(parent.st_mode) == 0o755,
        "Traversable root marker directory required",
    )
    durable_json(path, value, mode=0o644)


def marker_value(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(descriptor)
        require(
            stat.S_ISREG(info.st_mode)
            and info.st_uid == 0
            and info.st_nlink == 1
            and stat.S_IMODE(info.st_mode) == 0o644
            and 0 < info.st_size <= 4096,
            "Owned marker required",
        )
        return json.loads(os.read(descriptor, 4097))
    finally:
        os.close(descriptor)


def guard_state(receipt):
    import evacuation_guards

    verified = evacuation_guards.verify_installation(receipt, require_loaded=True)
    require(
        verified["host"] in cutover.HOSTS
        and verified["exactUnitMergeVerified"] is True,
        "Loaded guard proof required",
    )
    return verified


def postgres_clients(commands):
    query = "SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid();"
    _, value = commands.user(
        "llunde-backend",
        [
            "podman",
            "exec",
            "llunde-postgres",
            "psql",
            "--username=llunde",
            "--dbname=llunde",
            "--tuples-only",
            "--no-align",
            "--command",
            query,
        ],
    )
    require(value.strip().isdigit(), "PostgreSQL client count required")
    return int(value.strip())


def postgres_schema(commands):
    query = "SELECT json_build_object('tables',(SELECT count(*) FROM pg_tables WHERE schemaname='public'),'constraints',(SELECT count(*) FROM pg_constraint WHERE connamespace='public'::regnamespace));"
    _, value = commands.user(
        "llunde-backend",
        [
            "podman",
            "exec",
            "llunde-postgres",
            "psql",
            "--username=llunde",
            "--dbname=llunde",
            "--tuples-only",
            "--no-align",
            "--command",
            query,
        ],
    )
    result = json.loads(value)
    require(
        set(result) == {"tables", "constraints"}
        and all(type(number) is int and number >= 0 for number in result.values()),
        "Native schema count required",
    )
    return result


def export_processes(commands, application_name):
    require(
        re.fullmatch(r"infra-evacuation-[a-f0-9]{12}", application_name),
        "Exact export identity required",
    )
    _, raw = commands.user(
        "llunde-backend", ["podman", "top", "llunde-postgres", "hpid"]
    )
    lines = raw.decode().splitlines()
    require(
        lines and lines[0].strip() == "HPID" and len(lines) <= 256,
        "Bounded PostgreSQL process inventory required",
    )
    result = []
    for line in lines[1:]:
        require(line.strip().isdigit(), "Native process identity required")
        pid = int(line.strip())
        identity = process_identity(pid)
        if identity is None:
            continue
        try:
            with open(f"/proc/{pid}/environ", "rb") as source:
                environment = source.read(1048577)
            require(len(environment) <= 1048576, "Process environment exceeds budget")
            if b"PGAPPNAME=" + application_name.encode() in environment.split(b"\0"):
                require(
                    process_identity(pid) == identity, "Export process identity changed"
                )
                result.append((pid, identity))
        except FileNotFoundError:
            continue
    return result


def archive_processes(application_name, proc=Path("/proc")):
    require(
        re.fullmatch(r"infra-evacuation-[a-f0-9]{12}", application_name),
        "Exact archive identity required",
    )
    result, count = [], 0
    for process in proc.iterdir():
        if not process.name.isdigit():
            continue
        count += 1
        require(count <= 100000, "Host process inventory exceeds budget")
        try:
            if process.stat().st_uid != 2001:
                continue
            pid = int(process.name)
            identity = process_identity(pid)
            if identity is None:
                continue
            with (process / "environ").open("rb") as source:
                environment = source.read(1048577)
            require(len(environment) <= 1048576, "Archive environment exceeds budget")
            if (
                b"INFRA_EVACUATION_EXPORT=" + application_name.encode()
                not in environment.split(b"\0")
            ):
                continue
            require(
                process_identity(pid) == identity, "Archive process identity changed"
            )
            result.append((pid, identity))
        except (FileNotFoundError, ProcessLookupError):
            continue
    return result


def all_export_processes(commands, application_name):
    return sorted(
        set(
            export_processes(commands, application_name)
            + archive_processes(application_name)
        )
    )


def stop_export(commands, application_name):
    processes = all_export_processes(commands, application_name)
    for pid, identity in processes:
        if process_identity(pid) == identity:
            os.kill(pid, signal.SIGTERM)
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        if (
            not all_export_processes(commands, application_name)
            and postgres_clients(commands) == 0
        ):
            return True
        time.sleep(0.2)
    for pid, identity in all_export_processes(commands, application_name):
        if process_identity(pid) == identity:
            os.kill(pid, signal.SIGKILL)
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        if (
            not all_export_processes(commands, application_name)
            and postgres_clients(commands) == 0
        ):
            return True
        time.sleep(0.2)
    return False


def info_fields(raw):
    result = {}
    for line in raw.decode().splitlines():
        if not line or line.startswith("#"):
            continue
        key, value = line.split(":", 1)
        require(key not in result, "Duplicate persistence field")
        result[key] = value
    return result


def persistence_state(commands):
    _, raw = commands.user(
        "llunde-backend",
        [
            "podman",
            "exec",
            "llunde-valkey",
            "valkey-cli",
            "--raw",
            "INFO",
            "persistence",
        ],
    )
    value = info_fields(raw)
    require(
        value.get("aof_enabled") == "1"
        and value.get("aof_last_write_status") == "ok"
        and value.get("aof_rewrite_in_progress") == "0"
        and value.get("aof_rewrite_scheduled") == "0",
        "Successful stable AOF required",
    )
    _, raw = commands.user(
        "llunde-backend",
        ["podman", "exec", "llunde-valkey", "valkey-cli", "--raw", "INFO", "clients"],
    )
    require(
        info_fields(raw).get("connected_clients") == "1",
        "Other Valkey clients must leave",
    )
    _, raw = commands.user(
        "llunde-backend",
        [
            "podman",
            "exec",
            "llunde-valkey",
            "valkey-cli",
            "--raw",
            "INFO",
            "replication",
        ],
    )
    value = info_fields(raw)
    require(
        value.get("role") == "master" and value.get("connected_slaves") == "0",
        "Standalone Valkey required",
    )
    return {"aofLastWriteStatus": "ok", "rewriteInactive": True}


def graceful_exit_event(commands, container_id, started_at):
    require(
        re.fullmatch(r"[a-f0-9]{64}", container_id),
        "Exact Valkey container identity required",
    )
    template = '{"id":{{json .ID}},"type":{{json .Type}},"status":{{json .Status}},"exitCode":{{json .ContainerExitCode}},"timeNano":{{json .TimeNano}}}'
    since = datetime.fromtimestamp(started_at, UTC).isoformat()
    _, raw = commands.user(
        "llunde-backend",
        [
            "podman",
            "events",
            "--stream=false",
            "--since",
            since,
            "--filter",
            "container=" + container_id,
            "--format",
            template,
        ],
    )
    events = [json.loads(line) for line in raw.splitlines()]
    require(0 < len(events) <= 128, "Bounded persistence event history required")
    for event in events:
        require(
            set(event) == {"id", "type", "status", "exitCode", "timeNano"}
            and event["id"] == container_id
            and event["type"] == "container"
            and type(event["timeNano"]) is int
            and event["timeNano"] >= int(started_at * 10**9),
            "Persistence event identity differs",
        )
        require(
            event["status"] not in ("kill", "stop", "restart", "start"),
            "Forced stop or restart invalidates graceful persistence proof",
        )
    exits = [event for event in events if event["status"] == "died"]
    require(
        len(exits) == 1
        and type(exits[0]["exitCode"]) is int
        and exits[0]["exitCode"] == 0,
        "Exactly one native successful container exit required",
    )
    return exits[0]


def wait_stopped(commands, unit, user, *, successful=False, seconds=30):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        value = unit_state(commands, unit, user)
        if stopped(value):
            if successful:
                require(
                    value["ExecMainStatus"] == "0"
                    and value["Result"] in ("success", "start-limit-hit"),
                    "Successful native persistence exit required",
                )
            return value
        time.sleep(0.2)
    raise ValueError("Unit did not stop within deadline")


def live_fence(commands, receipt, record):
    verified = guard_state(receipt)
    require(verified["host"] == record["host"], "Guard host changed")
    host_identity(record["host"])
    for marker in ("source-locked", "reconciliation-locked"):
        require(
            marker_value(MARKERS / marker) == record["marker"], "Fence marker changed"
        )
    for user, unit in [
        ("edge", "cloudflared.service"),
        ("llunde-backend", "llunde-backend.service"),
        ("llunde-backend", "llunde-valkey.service"),
    ]:
        require(
            stopped(unit_state(commands, unit, user)),
            "Source writer or connector remains active",
        )
    for unit in cutover.SYSTEM_RECONCILERS:
        require(stopped(unit_state(commands, unit)), "Reconciliation remains active")
    for user in USERS:
        for unit in ("llunde-auto-update.service", "llunde-auto-update.timer"):
            require(
                stopped(unit_state(commands, unit, user)),
                "User reconciliation remains active",
            )
    require(postgres_clients(commands) == 0, "Other PostgreSQL clients remain")
    require(
        process_identity(record["valkeyPID"]) != record["valkeyProcessIdentity"],
        "Original Valkey process remains",
    )
    boot = Path("/proc/sys/kernel/random/boot_id").read_text().strip()
    require(boot == record["bootId"], "Source rebooted during fence")
    require(
        not all_export_processes(commands, record["exportApplicationName"]),
        "Native export process remains",
    )
    require(
        graceful_exit_event(
            commands, record["valkeyContainerId"], record["valkeyShutdownRequestedAt"]
        )
        == record["valkeyExitEvent"],
        "Valkey shutdown history changed",
    )
    return {
        "schemaVersion": 1,
        "kind": "evacuation-fence-observation",
        "host": record["host"],
        "hostname": cutover.HOSTS[record["host"]],
        "bootId": boot,
        "guardFilesSHA256": verified["guardFilesSHA256"],
        "observedAt": time.time(),
        "executionMarkerSHA256": hashlib.sha256(
            canonical(record["marker"])
        ).hexdigest(),
        "outageStartedAt": record["outageStartedAt"],
        "outageBudgetSeconds": record["outageBudgetSeconds"],
        "applicationWriterInactive": True,
        "connectorInactive": True,
        "valkeyInactive": True,
        "persistentGuardsVerified": True,
        "guardUserManagerEvaluationVerified": True,
        "reconciliationInactive": True,
        "postgresOtherClients": 0,
        "nativeExportProcessesRemaining": 0,
        "postgresSchema": postgres_schema(commands),
        "administrativeWritesExcludedByOperator": record[
            "administrativeWritesExcludedByOperator"
        ],
        "valkeyGracefulExitCode": 0,
        "valkeyAofWriteStatusBeforeStop": record["persistence"]["aofLastWriteStatus"],
        "valkeyRewriteInactiveBeforeStop": record["persistence"]["rewriteInactive"],
    }


def fence_export(
    candidate,
    receipt,
    destination,
    direction,
    *,
    administrative_writes_excluded=False,
    outage_seconds=1200,
    commands=None,
):
    require(
        administrative_writes_excluded is True,
        "Operator must exclude administrative writes",
    )
    require(
        type(outage_seconds) is int and 1200 <= outage_seconds <= 1800,
        "Reviewed outage budget must be1200–1800 seconds",
    )
    identity = cutover.pair_identity(candidate, direction)
    host = identity["source"]
    host_identity(host)
    verified = guard_state(receipt)
    require(verified["host"] == host, "Guard host differs")
    plan = validate_plan(candidate)
    commands = commands or Commands(420)
    destination = Path(destination).absolute()
    private_directory(destination.parent, 0)
    require(
        not destination.exists() and not destination.is_symlink(),
        "Fresh execution workspace required",
    )
    for marker in ("source-locked", "reconciliation-locked"):
        require(
            not (MARKERS / marker).exists() and not (MARKERS / marker).is_symlink(),
            "Existing fence requires explicit recovery",
        )
    lock = None
    if host == "fredrir-05":
        lock = os.open(
            "/var/lib/gitops-pull/lock", os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK
        )
        require(
            stat.S_ISREG(os.fstat(lock).st_mode) and os.fstat(lock).st_uid == 0,
            "Existing GitOps lock required",
        )
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    destination.mkdir(mode=0o700)
    record = {
        "schemaVersion": 1,
        "kind": "evacuation-execution-fence",
        "host": host,
        **identity,
        "administrativeWritesExcludedByOperator": True,
        "bootId": Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
        "startedAt": time.time(),
        "outageBudgetSeconds": outage_seconds,
        "exportApplicationName": "infra-evacuation-" + os.urandom(6).hex(),
        "phase": "preflight",
        "marker": {
            "candidateSHA256": identity["candidateSHA256"],
            "run": destination.name,
            "direction": direction,
            "executionID": os.urandom(16).hex(),
        },
        "timersBefore": {},
        "originalData": {
            name: {
                "device": Path("/home/llunde-backend/data", name).stat().st_dev,
                "inode": Path("/home/llunde-backend/data", name).stat().st_ino,
            }
            for name in ("postgres", "valkey")
        },
    }
    state_path = destination / "execution.json"
    durable_json(state_path, record)
    try:
        for service, item in plan["services"].items():
            commands.user(
                item["user"], ["podman", "image", "exists", item["runtimeImage"]]
            )
            container = (
                (
                    "systemd-caddy"
                    if service == "caddy"
                    else "systemd-llunde-frontend"
                    if service == "llunde-frontend"
                    else "llunde-cloudflared"
                    if service == "cloudflared"
                    else service
                )
                if host == "fredrir-05"
                else (
                    "llunde-caddy"
                    if service == "caddy"
                    else "llunde-cloudflared"
                    if service == "cloudflared"
                    else service
                )
            )
            code, _ = commands.user(
                item["user"], ["podman", "container", "exists", container], check=False
            )
            require(code in (0, 1), "Container identity check failed")
            if code == 1:
                require(
                    service not in ("llunde-postgres", "llunde-valkey"),
                    "Running source data required",
                )
                require(
                    stopped(unit_state(commands, service + ".service", item["user"])),
                    "Absent container has an active service",
                )
                continue
            _, raw = commands.user(
                item["user"], ["podman", "inspect", "--format", "{{.Image}}", container]
            )
            require(
                "sha256:" + raw.decode().strip().removeprefix("sha256:")
                == item["runtimeImage"],
                "Running source image changed",
            )
        for unit in (
            "gitops-pull.service",
            "gitops-deadman.service",
            "restic-backups-llunde-backend.service",
        ):
            require(
                stopped(unit_state(commands, unit)),
                "Existing source operation must finish before fence",
            )
        for user in USERS:
            require(
                stopped(unit_state(commands, "llunde-auto-update.service", user)),
                "Existing user update must finish before fence",
            )
        _, logger = commands.user(
            "llunde-backend", ["podman", "info", "--format", "{{.Host.EventLogger}}"]
        )
        require(
            logger.strip() in (b"journald", b"file"),
            "Persistent container exit events required before fencing",
        )
        record["phase"] = "freeze-reconciliation"
        durable_json(state_path, record, replace=True)
        private_marker(MARKERS / "reconciliation-locked", record["marker"])
        for user, units in [
            (None, ("gitops-pull.timer", "restic-backups-llunde-backend.timer")),
            *[(user, ("llunde-auto-update.timer",)) for user in USERS],
        ]:
            for unit in units:
                state = unit_state(commands, unit, user)
                enabled_arguments = [
                    "systemctl",
                    *(["--user"] if user else []),
                    "show",
                    unit,
                    "--property=UnitFileState",
                    "--value",
                ]
                _, enabled = (
                    commands.user(user, enabled_arguments)
                    if user
                    else commands.run(enabled_arguments)
                )
                record["timersBefore"][(user or "root") + "/" + unit] = {
                    "activeState": state["ActiveState"],
                    "loadState": state["LoadState"],
                    "unitFileState": enabled.decode().strip(),
                }
                if state["LoadState"] == "not-found":
                    durable_json(state_path, record, replace=True)
                    continue
                durable_json(state_path, record, replace=True)
                arguments = ["systemctl", *(["--user"] if user else []), "stop", unit]
                (commands.user(user, arguments) if user else commands.run(arguments))
        record["phase"], record["outageStartedAt"] = "fence-writers", time.time()
        durable_json(state_path, record, replace=True)
        private_marker(MARKERS / "source-locked", record["marker"])
        guard_state(receipt)
        for user, unit in [
            ("edge", "cloudflared.service"),
            ("llunde-backend", "llunde-backend.service"),
        ]:
            commands.user(user, ["systemctl", "--user", "stop", "--no-block", unit])
            wait_stopped(commands, unit, user)
        require(postgres_clients(commands) == 0, "Other PostgreSQL clients remain")
        record["persistence"] = persistence_state(commands)
        _, raw = commands.user(
            "llunde-backend",
            ["podman", "inspect", "--format", "{{.State.Pid}}", "llunde-valkey"],
        )
        record["valkeyPID"] = int(raw.strip())
        _, raw = commands.user(
            "llunde-backend",
            ["podman", "inspect", "--format", "{{.ID}}", "llunde-valkey"],
        )
        record["valkeyContainerId"] = raw.decode().strip()
        record["valkeyProcessIdentity"] = process_identity(record["valkeyPID"])
        require(
            record["valkeyProcessIdentity"] is not None,
            "Valkey process identity missing",
        )
        record["phase"] = "graceful-valkey-shutdown"
        record["valkeyShutdownRequestedAt"] = time.time()
        durable_json(state_path, record, replace=True)
        commands.user(
            "llunde-backend",
            ["podman", "exec", "llunde-valkey", "valkey-cli", "SHUTDOWN", "NOSAVE"],
            timeout=20,
            check=False,
        )
        wait_stopped(
            commands, "llunde-valkey.service", "llunde-backend", successful=True
        )
        record["valkeyExitEvent"] = graceful_exit_event(
            commands, record["valkeyContainerId"], record["valkeyShutdownRequestedAt"]
        )
        record["phase"] = "export-pair"
        durable_json(state_path, record, replace=True)
        before = live_fence(commands, receipt, record)
        durable_json(destination / "fence-before.json", before)
        pair = destination / "pair"
        pair.mkdir(mode=0o700)
        for key, name, limit in [
            ("postgres", "database.dump", 130),
            ("valkey", "valkey.tar", 65),
        ]:
            descriptor = os.open(
                pair / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600
            )
            with os.fdopen(descriptor, "wb") as output:
                argv = cutover.export_commands()[key]
                if key == "postgres":
                    argv[3] = "PGAPPNAME=" + record["exportApplicationName"]
                else:
                    argv[2:2] = [
                        "env",
                        "INFRA_EVACUATION_EXPORT=" + record["exportApplicationName"],
                    ]
                commands.user(
                    "llunde-backend",
                    argv,
                    output=output,
                    timeout=limit,
                    maximum=cutover.MAX_FILE,
                )
                output.flush()
                os.fsync(output.fileno())
        after = live_fence(commands, receipt, record)
        require(
            before["postgresSchema"] == after["postgresSchema"],
            "Source schema changed during quiesced export",
        )
        after["exportedFiles"] = {
            name: {
                "bytes": (pair / name).stat().st_size,
                "sha256": cutover.sha_file(pair / name, 0),
            }
            for name in ("database.dump", "valkey.tar")
        }
        durable_json(destination / "fence-after.json", after)
        manifest = cutover.seal_pair(
            pair,
            candidate,
            direction,
            destination / "fence-before.json",
            destination / "fence-after.json",
        )
        record.update(
            phase="pair-sealed",
            pairManifestSHA256=hashlib.sha256(canonical(manifest)).hexdigest(),
            completedAt=time.time(),
            sourceFenceRetained=True,
        )
        durable_json(state_path, record, replace=True)
        return record
    except BaseException:
        try:
            record["nativeExportCleanupVerified"] = stop_export(
                Commands(20), record["exportApplicationName"]
            )
        except (OSError, ValueError, KeyError, subprocess.SubprocessError):
            record["nativeExportCleanupVerified"] = False
        record["failurePhase"] = record["phase"]
        record.update(
            phase="failed-retained",
            sourceFenceRetained=(MARKERS / "source-locked").exists(),
            automaticRollback=False,
        )
        durable_json(state_path, record, replace=True)
        raise
    finally:
        if lock is not None:
            os.close(lock)


def backup_gate(pair, backup_receipt, restore_receipt):
    manifest = cutover.verify_pair(pair)
    from evacuation_backup import validate_bundle

    bundle = validate_bundle(pair)
    require(
        backup_receipt["schemaVersion"] == 1
        and backup_receipt["kind"] == "evacuation-offhost-backup"
        and backup_receipt["bundle"] == bundle,
        "Exact final-pair backup required",
    )
    require(
        restore_receipt["schemaVersion"] == 1
        and restore_receipt["kind"] == "evacuation-independent-restore"
        and restore_receipt["bundle"] == bundle
        and restore_receipt["verified"] is True
        and restore_receipt["independentHost"] is True,
        "Independent final-pair restore required",
    )
    require(
        re.fullmatch(r"[a-f0-9]{64}", backup_receipt["snapshotId"])
        and re.fullmatch(r"[a-f0-9]{64}", backup_receipt["archiveSHA256"])
        and all(
            backup_receipt[key] == restore_receipt[key]
            for key in ("snapshotId", "archiveSHA256")
        ),
        "Backup and restore identities differ",
    )
    return {
        "pairManifestSHA256": hashlib.sha256(canonical(manifest)).hexdigest(),
        "snapshotId": backup_receipt["snapshotId"],
        "independentRestoreVerified": True,
        "applicationRestoreVerified": False,
    }


def verify_fresh_source(value, pair, *, now=None):
    manifest = cutover.verify_pair(pair)
    cutover.validate_fence(value, manifest["source"])
    require(
        value["bootId"] == manifest["fenceAfter"]["bootId"]
        and value["guardFilesSHA256"] == manifest["fenceAfter"]["guardFilesSHA256"]
        and re.fullmatch(r"[a-f0-9]{64}", value["executionMarkerSHA256"])
        and value["executionMarkerSHA256"]
        == manifest["fenceAfter"]["executionMarkerSHA256"],
        "Source boot or fence changed",
    )
    age = (time.time() if now is None else now) - value["observedAt"]
    require(math.isfinite(age) and 0 <= age <= 30, "Fresh live source fence required")
    return manifest


def outage_remaining(manifest, *, now=None):
    fence = manifest["fenceAfter"]
    started, budget = fence["outageStartedAt"], fence["outageBudgetSeconds"]
    require(
        type(started) in (int, float)
        and math.isfinite(started)
        and type(budget) is int
        and 1200 <= budget <= 1800,
        "Bounded outage identity required",
    )
    elapsed = (time.time() if now is None else now) - started
    require(math.isfinite(elapsed) and elapsed >= 0, "Outage clock differs")
    return budget - elapsed


def pack_pair(directory, output):
    before = cutover.verify_pair(directory)
    with tarfile.open(fileobj=output, mode="w|") as archive:
        for name in ("manifest.json", "database.dump", "valkey.tar"):
            with open_private(
                Path(directory) / name, os.geteuid(), cutover.MAX_FILE
            ) as source:
                member = tarfile.TarInfo(name)
                member.size, member.mode = os.fstat(source.fileno()).st_size, 0o600
                archive.addfile(member, source)
    require(cutover.verify_pair(directory) == before, "Pair changed during transfer")


def receive_pair(destination, source, direction):
    destination = Path(destination)
    private_directory(destination.parent, os.geteuid())
    require(
        not destination.exists() and not destination.is_symlink(),
        "Fresh pair destination required",
    )
    destination.mkdir(mode=0o700)
    seen, total = set(), 0
    transfer = destination / ".transfer.tar"
    try:
        descriptor = os.open(
            transfer, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600
        )
        with os.fdopen(descriptor, "wb") as output:
            while chunk := source.read(65536):
                total += len(chunk)
                require(total <= MAX_TRANSFER, "Pair transfer exceeds budget")
                output.write(chunk)
        total = 0
        with (
            open_private(transfer, os.geteuid(), MAX_TRANSFER) as stream,
            tarfile.open(fileobj=stream, mode="r:") as archive,
        ):
            for member in archive:
                require(
                    member.name in ("manifest.json", "database.dump", "valkey.tar")
                    and member.name not in seen
                    and member.isfile()
                    and not member.sparse
                    and 0 < member.size <= cutover.MAX_FILE,
                    "Unsafe paired transfer",
                )
                total += member.size
                require(total <= MAX_TRANSFER, "Pair transfer exceeds budget")
                descriptor = os.open(
                    destination / member.name,
                    os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                    0o600,
                )
                with (
                    os.fdopen(descriptor, "wb") as output,
                    archive.extractfile(member) as data,
                ):
                    remaining = member.size
                    while remaining:
                        chunk = data.read(min(65536, remaining))
                        require(chunk, "Incomplete pair transfer")
                        remaining -= len(chunk)
                        output.write(chunk)
                    output.flush()
                    os.fsync(output.fileno())
                seen.add(member.name)
            stream.seek(archive.offset)
            while chunk := stream.read(65536):
                require(not any(chunk), "Hidden transfer members forbidden")
        transfer.unlink()
        require(
            seen == {"manifest.json", "database.dump", "valkey.tar"},
            "Complete pair required",
        )
        manifest = cutover.verify_pair(destination)
        require(manifest["direction"] == direction, "Transfer direction differs")
        return manifest
    except BaseException:
        for name in seen | {
            "manifest.json",
            "database.dump",
            "valkey.tar",
            ".transfer.tar",
        }:
            (destination / name).unlink(missing_ok=True)
        destination.rmdir()
        raise


def main():
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)
    export = sub.add_parser("fence-export")
    export.add_argument("candidate", type=Path)
    export.add_argument("guard_receipt", type=Path)
    export.add_argument("destination", type=Path)
    export.add_argument("--direction", choices=("forward", "reverse"), required=True)
    export.add_argument("--administrative-writes-excluded", action="store_true")
    export.add_argument("--outage-seconds", type=int, default=1200)
    verify = sub.add_parser("observe-fence")
    verify.add_argument("guard_receipt", type=Path)
    verify.add_argument("execution", type=Path)
    pack = sub.add_parser("pack-pair")
    pack.add_argument("directory", type=Path)
    receive = sub.add_parser("receive-pair")
    receive.add_argument("destination", type=Path)
    receive.add_argument("--direction", choices=("forward", "reverse"), required=True)
    args = parser.parse_args()
    os.umask(0o077)
    signal.signal(
        signal.SIGTERM,
        lambda *_: (_ for _ in ()).throw(ValueError("Execution interrupted")),
    )
    signal.signal(
        signal.SIGALRM,
        lambda *_: (_ for _ in ()).throw(ValueError("Transfer deadline reached")),
    )
    if args.command in ("pack-pair", "receive-pair"):
        signal.alarm(180)
    try:
        if args.command == "fence-export":
            result = fence_export(
                args.candidate,
                read_json(args.guard_receipt, 0),
                args.destination,
                args.direction,
                administrative_writes_excluded=args.administrative_writes_excluded,
                outage_seconds=args.outage_seconds,
            )
        elif args.command == "observe-fence":
            result = live_fence(
                Commands(60),
                read_json(args.guard_receipt, 0),
                read_json(args.execution, 0),
            )
        elif args.command == "receive-pair":
            result = receive_pair(args.destination, sys.stdin.buffer, args.direction)
        else:
            pack_pair(args.directory, sys.stdout.buffer)
            return 0
        print(json.dumps(result))
        return 0
    except (
        OSError,
        ValueError,
        KeyError,
        TypeError,
        subprocess.SubprocessError,
        tarfile.TarError,
    ):
        print(
            json.dumps(
                {
                    "error": "Execution failed; state retained; automatic rollback disabled"
                }
            ),
            file=sys.stderr,
        )
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
