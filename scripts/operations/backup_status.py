import fcntl
import json
import os
import pwd
import re
import resource
import select
import socket
import stat
import sys
import time
from pathlib import Path

ACCOUNT = "infra-backup-status"
DIRECTORY = Path("/var/lib/platform-backup-status")
JOB = "llunde-backend-fredrir-09"
SOURCE_ADDRESS = "85.190.100.72"
DESTINATION_ADDRESS = "172.232.145.251"
FIELDS = {
    "schemaVersion",
    "job",
    "host",
    "snapshotId",
    "archiveSHA256",
    "recoveryPointAt",
    "completedAt",
}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def validate_payload(value, now):
    require(
        isinstance(value, dict) and set(value) == FIELDS,
        "Exact receipt fields required",
    )
    require(
        value["schemaVersion"] == 1
        and value["job"] == JOB
        and value["host"] == "fredrir-09",
        "Receipt identity differs",
    )
    require(
        all(
            isinstance(value[key], str) and re.fullmatch("[a-f0-9]{64}", value[key])
            for key in ("snapshotId", "archiveSHA256")
        ),
        "Snapshot hashes required",
    )
    require(
        all(type(value[key]) is int for key in ("recoveryPointAt", "completedAt")),
        "Integer receipt times required",
    )
    require(
        0 < value["recoveryPointAt"] <= value["completedAt"] <= now + 60
        and now - 1800 <= value["recoveryPointAt"],
        "Stale or future receipt rejected",
    )
    return value


def validate_connection(environment):
    require(not environment.get("SSH_ORIGINAL_COMMAND"), "Remote commands forbidden")
    fields = environment.get("SSH_CONNECTION", "").split()
    require(
        len(fields) == 4
        and fields[0] == SOURCE_ADDRESS
        and fields[2] == DESTINATION_ADDRESS
        and fields[3] == "22"
        and fields[1].isdigit()
        and 0 < int(fields[1]) < 65536,
        "Exact authenticated source connection required",
    )


def authorized_key(public_key):
    require(
        isinstance(public_key, str)
        and re.fullmatch(
            r"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI[A-Za-z0-9+/]{43}", public_key
        ),
        "Bare Ed25519 public key required",
    )
    return (
        'restrict,from="'
        + SOURCE_ADDRESS
        + '",command="/usr/bin/timeout -s TERM -k 1 8 /usr/bin/python3 -I /usr/local/libexec/infra-backup-status" '
        + public_key
        + "\n"
    )


def bounded_input(descriptor, seconds=5):
    deadline, result = time.monotonic() + seconds, bytearray()
    while True:
        remaining = deadline - time.monotonic()
        require(remaining > 0, "Receipt input timed out")
        require(
            select.select([descriptor], [], [], remaining)[0], "Receipt input timed out"
        )
        data = os.read(descriptor, min(1024, 4097 - len(result)))
        if not data:
            break
        result.extend(data)
        require(len(result) <= 4096, "Receipt input exceeds limit")
    require(result, "Receipt input required")
    return bytes(result)


def open_directory(directory, owner):
    path = Path(directory).absolute()
    descriptor = os.open("/", os.O_RDONLY | os.O_DIRECTORY)
    try:
        for index, part in enumerate(path.parts[1:]):
            child = os.open(
                part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor
            )
            os.close(descriptor)
            descriptor = child
            info = os.fstat(descriptor)
            expected = owner if index == len(path.parts) - 2 else 0
            require(
                info.st_uid == expected and stat.S_IMODE(info.st_mode) == 0o755,
                "Owned traversable status directory required",
            )
        return descriptor
    except BaseException:
        os.close(descriptor)
        raise


def receive(value, now, directory=DIRECTORY, owner=None):
    owner = os.geteuid() if owner is None else owner
    validate_payload(value, now)
    directory_fd = open_directory(directory, owner)
    lock = None
    temporary = JOB + ".pending"
    try:
        lock = os.open(
            JOB + ".lock",
            os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW,
            0o600,
            dir_fd=directory_fd,
        )
        info = os.fstat(lock)
        require(
            stat.S_ISREG(info.st_mode)
            and info.st_uid == owner
            and info.st_nlink == 1
            and stat.S_IMODE(info.st_mode) == 0o600,
            "Private owned receipt lock required",
        )
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        try:
            previous_fd = os.open(
                JOB + ".json",
                os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK,
                dir_fd=directory_fd,
            )
        except FileNotFoundError:
            previous = None
        else:
            try:
                info = os.fstat(previous_fd)
                require(
                    stat.S_ISREG(info.st_mode)
                    and info.st_uid == owner
                    and info.st_nlink == 1
                    and stat.S_IMODE(info.st_mode) == 0o644
                    and 0 < info.st_size <= 4096,
                    "Owned previous receipt required",
                )
                previous = json.loads(os.read(previous_fd, 4097))
                require(
                    set(previous) == FIELDS | {"receivedAt"}
                    and type(previous["receivedAt"]) is int,
                    "Previous receipt malformed",
                )
            finally:
                os.close(previous_fd)
        if previous:
            require(
                value["recoveryPointAt"] > previous["recoveryPointAt"]
                and value["completedAt"] > previous["completedAt"]
                and value["snapshotId"] != previous["snapshotId"]
                and now >= previous["receivedAt"],
                "Repeated or reordered receipt rejected",
            )
        data = (
            json.dumps(
                value | {"receivedAt": now}, sort_keys=True, separators=(",", ":")
            ).encode()
            + b"\n"
        )
        descriptor = os.open(
            temporary,
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
            0o644,
            dir_fd=directory_fd,
        )
        try:
            with os.fdopen(descriptor, "wb") as output:
                os.fchmod(output.fileno(), 0o644)
                output.write(data)
                output.flush()
                os.fsync(output.fileno())
            os.rename(
                temporary,
                JOB + ".json",
                src_dir_fd=directory_fd,
                dst_dir_fd=directory_fd,
            )
            os.fsync(directory_fd)
        except BaseException:
            os.unlink(temporary, dir_fd=directory_fd)
            raise
    finally:
        if lock is not None:
            os.close(lock)
        os.close(directory_fd)


def main():
    os.umask(0o077)
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    resource.setrlimit(resource.RLIMIT_CPU, (2, 2))
    resource.setrlimit(resource.RLIMIT_AS, (64 * 1024**2, 64 * 1024**2))
    try:
        require(socket.gethostname() == "localhost", "Verified receiver host required")
        account = pwd.getpwnam(ACCOUNT)
        require(
            os.geteuid() == account.pw_uid != 0, "Restricted receiver account required"
        )
        validate_connection(os.environ)
        receive(json.loads(bounded_input(sys.stdin.fileno())), int(time.time()))
        print("backup-status: accepted")
    except (OSError, ValueError, KeyError, TypeError):
        print("backup-status: rejected", file=sys.stderr)
        raise SystemExit(1)


if __name__ == "__main__":
    main()
