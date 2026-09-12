import argparse
import fcntl
import hashlib
import json
import os
import re
import resource
import shutil
import signal
import stat
import sys
import tempfile
import time
import uuid
from pathlib import Path

import evacuation_backup as backup
from evacuation_execution import (
    Commands,
    canonical,
    host_identity,
    info_fields,
    unit_state,
)
from evacuation_staging import validate_plan

JOB = "llunde-backend-fredrir-09"
KIND = "evacuation-online-state"
REPOSITORY_ID = "01b2b8ed881680cefdc8c0801e8b0a976c9f16909a65e30f732b1b36b7dfc860"
MARKERS = (
    "/var/lib/platform-evacuation/source-locked",
    "/var/lib/platform-evacuation/reconciliation-locked",
)
MAX_DUMP = 128 * 1024**2
MAX_RDB = 128 * 1024**2
MAX_VALKEY_MEMORY = 80 * 1024**2


def require(condition, message):
    if not condition:
        raise ValueError(message)


def validate_online(directory, manifest):
    require(
        set(manifest)
        == {
            "schemaVersion",
            "kind",
            "host",
            "consistency",
            "writersFenced",
            "candidateSHA256",
            "imageIDs",
            "recoveryPoints",
            "files",
        },
        "Online manifest fields differ",
    )
    require(
        manifest["schemaVersion"] == 1
        and manifest["kind"] == KIND
        and manifest["host"] == "fredrir-09"
        and manifest["consistency"] == "independent-datastore-points"
        and manifest["writersFenced"] is False,
        "Online recovery semantics differ",
    )
    require(
        re.fullmatch("[a-f0-9]{64}", manifest["candidateSHA256"]),
        "Candidate checksum required",
    )
    require(
        set(manifest["imageIDs"]) == {"llunde-postgres", "llunde-valkey"}
        and all(
            re.fullmatch("sha256:[a-f0-9]{64}", value)
            for value in manifest["imageIDs"].values()
        ),
        "Exact datastore images required",
    )
    require(
        set(manifest["files"])
        == set(manifest["recoveryPoints"])
        == {"database.dump", "dump.rdb"},
        "Exact online files required",
    )
    for name, maximum, magic in [
        ("database.dump", MAX_DUMP, b"PGDMP"),
        ("dump.rdb", MAX_RDB, b"REDIS"),
    ]:
        point, metadata = manifest["recoveryPoints"][name], manifest["files"][name]
        require(
            set(point) == {"startedAt", "completedAt"}
            and all(type(value) is int and value > 0 for value in point.values())
            and 0 <= point["completedAt"] - point["startedAt"] <= 180,
            "Bounded recovery point required",
        )
        require(
            set(metadata) == {"sha256", "bytes"}
            and re.fullmatch("[a-f0-9]{64}", metadata["sha256"])
            and type(metadata["bytes"]) is int
            and 5 < metadata["bytes"] <= maximum,
            "Bounded online file required",
        )
        with backup.open_private(Path(directory) / name, maximum) as source:
            require(
                os.fstat(source.fileno()).st_size == metadata["bytes"]
                and source.read(5) == magic,
                "Native export format differs",
            )
        require(
            backup.digest(Path(directory) / name, maximum) == metadata["sha256"],
            "Online file checksum differs",
        )
    points = manifest["recoveryPoints"].values()
    require(
        max(point["completedAt"] for point in points)
        - min(point["startedAt"] for point in points)
        <= 300,
        "Capture window exceeded",
    )
    return manifest


def markers_absent():
    require(
        not any(os.path.lexists(path) for path in MARKERS),
        "Cutover lock prevents recurring backup",
    )


def containers(commands, images):
    result = {}
    for name, expected in images.items():
        state = unit_state(commands, name + ".service", "llunde-backend")
        require(
            state["ActiveState"] == "active"
            and state["SubState"] == "running"
            and int(state["MainPID"]) > 1,
            "Active datastore unit required",
        )
        _, raw = commands.user(
            "llunde-backend",
            [
                "podman",
                "inspect",
                "--format",
                "{{json .ID}} {{json .Image}} {{json .State.Running}}",
                name,
            ],
            maximum=1024,
        )
        identifier, image, running = (
            json.loads(value) for value in raw.decode().split()
        )
        require(
            isinstance(identifier, str)
            and re.fullmatch("[a-f0-9]{64}", identifier)
            and isinstance(image, str)
            and re.fullmatch(r"(?:sha256:)?[a-f0-9]{64}", image)
            and image.removeprefix("sha256:") == expected.removeprefix("sha256:")
            and running is True,
            "Running datastore identity differs",
        )
        result[name] = identifier
    return result


def valkey_headroom(commands):
    _, raw = commands.user(
        "llunde-backend",
        ["podman", "exec", "llunde-valkey", "valkey-cli", "--raw", "INFO", "memory"],
        maximum=16384,
    )
    memory = info_fields(raw)
    require(
        memory.get("used_memory", "").isdigit()
        and 0 < int(memory["used_memory"]) <= MAX_VALKEY_MEMORY,
        "Valkey export requires reviewed fork headroom",
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
            "persistence",
        ],
        maximum=16384,
    )
    persistence = info_fields(raw)
    require(
        persistence.get("aof_enabled") == "1"
        and persistence.get("aof_last_write_status") == "ok"
        and persistence.get("rdb_bgsave_in_progress") == "0"
        and persistence.get("aof_rewrite_in_progress") == "0",
        "Stable successful Valkey persistence required",
    )


def capture(candidate, directory, commands):
    markers_absent()
    plan = validate_plan(candidate)
    images = {
        name: plan["services"][name]["runtimeImage"]
        for name in ("llunde-postgres", "llunde-valkey")
    }
    identity = containers(commands, images)
    require(
        shutil.disk_usage(directory).free >= 6 * 1024**3,
        "Backup requires six GiB free workspace",
    )
    manifest = {
        "schemaVersion": 1,
        "kind": KIND,
        "host": "fredrir-09",
        "consistency": "independent-datastore-points",
        "writersFenced": False,
        "candidateSHA256": hashlib.sha256(
            (Path(candidate) / "staging.json").read_bytes()
        ).hexdigest(),
        "imageIDs": images,
        "recoveryPoints": {},
        "files": {},
    }
    invocations = [
        (
            "database.dump",
            "llunde-postgres",
            120,
            MAX_DUMP,
            [
                "env",
                "PGAPPNAME=infra-online-" + uuid.uuid4().hex[:12],
                "pg_dump",
                "--username=llunde",
                "--dbname=llunde",
                "--format=custom",
                "--no-password",
                "--lock-wait-timeout=10s",
            ],
        ),
        ("dump.rdb", "llunde-valkey", 60, MAX_RDB, ["valkey-cli", "--rdb", "-"]),
    ]
    for name, container, seconds, maximum, arguments in invocations:
        markers_absent()
        if name == "dump.rdb":
            valkey_headroom(commands)
        started = int(time.time())
        descriptor = os.open(
            Path(directory) / name,
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
            0o600,
        )
        with os.fdopen(descriptor, "wb") as output:
            commands.user(
                "llunde-backend",
                [
                    "podman",
                    "exec",
                    identity[container],
                    "timeout",
                    "-s",
                    "TERM",
                    "-k",
                    "5",
                    str(seconds),
                    *arguments,
                ],
                timeout=seconds + 10,
                maximum=maximum,
                output=output,
                monitor=markers_absent,
            )
            output.flush()
            os.fsync(output.fileno())
        manifest["recoveryPoints"][name] = {
            "startedAt": started,
            "completedAt": int(time.time()),
        }
        manifest["files"][name] = {
            "sha256": backup.digest(Path(directory) / name, maximum),
            "bytes": (Path(directory) / name).stat().st_size,
        }
    markers_absent()
    require(
        containers(commands, images) == identity, "Datastore restarted during capture"
    )
    validate_online(directory, manifest)
    backup.write_private(Path(directory) / "manifest.json", canonical(manifest) + b"\n")
    return manifest


def deliver_status(commands, receipt, manifest, identity, known_hosts, address):
    require(
        address == "172.232.145.251", "Reviewed external watchdog endpoint required"
    )
    with backup.open_private(identity):
        pass
    with backup.open_private(known_hosts):
        pass
    payload = {
        "schemaVersion": 1,
        "job": JOB,
        "host": "fredrir-09",
        "snapshotId": receipt["snapshotId"],
        "archiveSHA256": receipt["archiveSHA256"],
        "recoveryPointAt": min(
            point["startedAt"] for point in manifest["recoveryPoints"].values()
        ),
        "completedAt": receipt["createdAt"],
    }
    with tempfile.TemporaryFile() as source:
        source.write(canonical(payload) + b"\n")
        source.seek(0)
        _, output = commands.run(
            [
                "ssh",
                "-F",
                "/dev/null",
                "-T",
                "-o",
                "BatchMode=yes",
                "-o",
                "StrictHostKeyChecking=yes",
                "-o",
                "IdentitiesOnly=yes",
                "-o",
                "IdentityAgent=none",
                "-o",
                "ForwardAgent=no",
                "-o",
                "ClearAllForwardings=yes",
                "-o",
                "ConnectTimeout=5",
                "-o",
                "ConnectionAttempts=1",
                "-o",
                "ServerAliveInterval=5",
                "-o",
                "ServerAliveCountMax=2",
                "-o",
                "UserKnownHostsFile=" + str(known_hosts),
                "-i",
                str(identity),
                "infra-backup-status@" + address,
            ],
            source=source,
            timeout=20,
            maximum=1024,
        )
    require(
        output == b"backup-status: accepted\n",
        "Independent status delivery not confirmed",
    )
    return payload


def run(
    candidate,
    credentials,
    age_identity,
    work,
    ssh_identity,
    known_hosts,
    address,
    commands=None,
):
    host_identity("fredrir-09")
    commands = commands or Commands(900)
    work = backup.private_directory(work)
    lock_fd = os.open(
        work / "recurring.lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600
    )
    try:
        info = os.fstat(lock_fd)
        require(
            stat.S_ISREG(info.st_mode)
            and info.st_uid == os.geteuid()
            and info.st_nlink == 1
            and stat.S_IMODE(info.st_mode) == 0o600,
            "Owned lock required",
        )
        fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        with tempfile.TemporaryDirectory(prefix="online-", dir=work) as temporary:
            manifest = capture(candidate, Path(temporary), commands)
            restic = backup.Restic(
                backup.decrypt_credentials(credentials, age_identity)
            )
            restic.deadline = commands.deadline - 25
            require(
                restic.identity() == REPOSITORY_ID,
                "Reviewed repository identity differs",
            )
            markers_absent()
            receipt = backup.backup(temporary, restic, work)
            markers_absent()
            receipt_path = work / ("snapshot-" + receipt["snapshotId"] + ".json")
            backup.write_private(receipt_path, canonical(receipt) + b"\n")
            deliver_status(
                commands, receipt, manifest, ssh_identity, known_hosts, address
            )
            return {
                "snapshotId": receipt["snapshotId"],
                "receipt": str(receipt_path),
                "independentStatusDelivered": True,
                "applicationRestoreVerified": False,
            }
    finally:
        os.close(lock_fd)


def main():
    parser = argparse.ArgumentParser()
    for name in (
        "candidate",
        "credentials",
        "age-identity",
        "work",
        "ssh-identity",
        "known-hosts",
    ):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--watchdog-address", required=True)
    args = parser.parse_args()
    os.umask(0o077)
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    signal.signal(
        signal.SIGTERM,
        lambda *_: (_ for _ in ()).throw(ValueError("Backup terminated")),
    )
    try:
        print(
            json.dumps(
                run(
                    args.candidate,
                    args.credentials,
                    args.age_identity,
                    args.work,
                    args.ssh_identity,
                    args.known_hosts,
                    args.watchdog_address,
                )
            )
        )
    except (OSError, ValueError, KeyError, TypeError):
        print("Recurring backup failed; sensitive output withheld", file=sys.stderr)
        raise SystemExit(1)


if __name__ == "__main__":
    main()
