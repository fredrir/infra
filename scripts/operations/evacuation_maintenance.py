import argparse
import fcntl
import hashlib
import json
import os
import re
import socket
import stat
import subprocess
import time
from pathlib import Path

TIMERS = ("apt-daily.timer", "apt-daily-upgrade.timer")
SERVICES = ("apt-daily.service", "apt-daily-upgrade.service", "restart-dbus.service")
LOCKS = (
    "/var/lib/dpkg/lock",
    "/var/lib/dpkg/lock-frontend",
    "/var/lib/apt/lists/lock",
    "/var/cache/apt/archives/lock",
)
BASE = Path("/var/lib/platform-maintenance")
PROC = Path("/proc")
PROPERTIES = (
    "Id",
    "LoadState",
    "ActiveState",
    "SubState",
    "Job",
    "UnitFileState",
    "FragmentPath",
    "DropInPaths",
)


def require(value, message):
    if not value:
        raise ValueError(message)


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode() + b"\n"


class Native:
    def __init__(self):
        self.deadline = time.monotonic() + 90

    def run(self, args):
        remaining = self.deadline - time.monotonic()
        require(remaining > 0, "Command budget exhausted")
        value = subprocess.run(
            args,
            stdin=subprocess.DEVNULL,
            capture_output=True,
            check=False,
            timeout=min(15, remaining),
            env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL": "C"},
        )
        require(
            value.returncode == 0 and len(value.stdout) <= 262144,
            "Native command failed",
        )
        return value.stdout.decode()

    def identity(self):
        require(
            os.geteuid() == 0 and socket.gethostname() == "cloud-server-10643982",
            "Verified root09 required",
        )
        return {
            "hostname": socket.gethostname(),
            "machineId": Path("/etc/machine-id").read_text().strip(),
            "bootId": Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
        }

    def state(self, name):
        require(name in TIMERS + SERVICES, "Unknown unit")
        text = self.run(
            ["systemctl", "show", "--all", name]
            + [f"--property={p}" for p in PROPERTIES]
        )
        result = dict(line.split("=", 1) for line in text.splitlines() if "=" in line)
        require(
            set(result) == set(PROPERTIES) and result["Id"] == name,
            "Incomplete unit state",
        )
        return result

    def configuration(self, state):
        files = []
        for value in [state["FragmentPath"]] + state["DropInPaths"].split():
            path = Path(value)
            require(
                path.is_absolute()
                and str(path).startswith(
                    ("/usr/lib/systemd/", "/etc/systemd/", "/run/systemd/")
                ),
                "Unexpected unit path",
            )
            fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
            try:
                st = os.fstat(fd)
                require(
                    stat.S_ISREG(st.st_mode)
                    and st.st_uid == 0
                    and not st.st_mode & 0o022
                    and st.st_size <= 65536,
                    "Unsafe unit file",
                )
                data = os.read(fd, 65537)
                require(len(data) == st.st_size, "Unit file changed")
                files.append(
                    {
                        "path": value,
                        "device": st.st_dev,
                        "inode": st.st_ino,
                        "mode": stat.S_IMODE(st.st_mode),
                        "uid": st.st_uid,
                        "gid": st.st_gid,
                        "sha256": hashlib.sha256(data).hexdigest(),
                    }
                )
            finally:
                os.close(fd)
        return {"enabled": state["UnitFileState"], "files": files}

    def idle(self):
        states = {name: self.state(name) for name in SERVICES}
        require(
            all(
                s["ActiveState"] == "inactive" and s["Job"] == ""
                for s in states.values()
            ),
            "Package service active or queued",
        )
        output = self.run(
            ["systemctl", "list-jobs", "--all", "--output=json", "--no-pager"]
        )
        jobs = json.loads(output) if output.strip() else []
        require(
            isinstance(jobs, list)
            and all(
                isinstance(j, dict) and isinstance(j.get("unit"), str) for j in jobs
            ),
            "Invalid job inventory",
        )
        require(
            not any(j.get("unit") in TIMERS + SERVICES for j in jobs),
            "Maintenance job queued",
        )
        processes = []
        for path in PROC.iterdir():
            if not path.name.isdigit():
                continue
            try:
                with (path / "comm").open("rb") as stream:
                    comm = stream.read(64).decode().strip()
                with (path / "cmdline").open("rb") as stream:
                    args = stream.read(4096).split(b"\0")
                package_names = {
                    "apt",
                    "apt-get",
                    "dpkg",
                    "dpkg-deb",
                    "needrestart",
                    "unattended-upgr",
                }
                package_paths = {
                    b"/usr/bin/apt",
                    b"/usr/bin/apt-get",
                    b"/usr/bin/dpkg",
                    b"/usr/bin/dpkg-deb",
                    b"/usr/bin/needrestart",
                    b"/usr/sbin/needrestart",
                    b"/usr/bin/unattended-upgrade",
                }
                monitor = (
                    b"/usr/share/unattended-upgrades/unattended-upgrade-shutdown"
                    in args[:3]
                    and b"--wait-for-signal" in args[:4]
                )
                if (
                    comm in package_names or package_paths.intersection(args[:3])
                ) and not monitor:
                    processes.append({"pid": int(path.name), "comm": comm})
            except FileNotFoundError:
                continue
        require(not processes, "Package process active")
        held = []
        lock_ids = {}
        for value in LOCKS:
            try:
                st = Path(value).stat()
            except FileNotFoundError:
                continue
            lock_ids[(os.major(st.st_dev), os.minor(st.st_dev), st.st_ino)] = value
        for line in (PROC / "locks").read_text().splitlines():
            fields = line.replace(" -> ", " ").split()
            if len(fields) < 6:
                continue
            match = re.fullmatch(r"([0-9a-f]+):([0-9a-f]+):([0-9]+)", fields[5])
            if match:
                key = (int(match[1], 16), int(match[2], 16), int(match[3]))
                if key in lock_ids:
                    held.append(lock_ids[key])
        require(not held, "Package lock held")
        return {
            "services": states,
            "packageProcessesAbsent": True,
            "packageLocksUnheld": True,
            "observedAt": time.time(),
        }

    def action(self, action, name):
        require(
            action in ("start", "stop") and name in TIMERS,
            "Only timer start/stop permitted",
        )
        self.run(["systemctl", action, name])


class Receipt:
    def __init__(self, nonce, create=False):
        require(re.fullmatch(r"[a-f0-9]{16}", nonce), "Invalid window ID")
        self.directory = BASE / nonce
        if create:
            try:
                BASE.mkdir(mode=0o700)
            except FileExistsError:
                pass
        self.check_directory(BASE)
        self.lock_fd = os.open(
            BASE / ".lock",
            os.O_RDWR | os.O_NOFOLLOW | (os.O_CREAT if create else 0),
            0o600,
        )
        lock = os.fstat(self.lock_fd)
        require(
            stat.S_ISREG(lock.st_mode)
            and lock.st_uid == 0
            and lock.st_nlink == 1
            and stat.S_IMODE(lock.st_mode) == 0o600,
            "Private maintenance lock required",
        )
        fcntl.flock(self.lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if create:
            self.require_no_open_window()
        if create:
            self.directory.mkdir(mode=0o700)
        self.check_directory(self.directory)
        self.fd = os.open(self.directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        self.path = self.directory / "receipt.json"
        self.last = None
        if not create:
            self.last = self.read()

    @classmethod
    def require_no_open_window(cls):
        entries = list(BASE.iterdir())
        require(len(entries) <= 257, "Maintenance history bound exceeded")
        for entry in entries:
            if entry.name == ".lock":
                continue
            require(
                re.fullmatch(r"[a-f0-9]{16}", entry.name),
                "Unknown maintenance artifact",
            )
            cls.check_directory(entry)
            require(
                {p.name for p in entry.iterdir()} == {"receipt.json"},
                "Incomplete prior maintenance window",
            )
            fd = os.open(entry / "receipt.json", os.O_RDONLY | os.O_NOFOLLOW)
            try:
                st = os.fstat(fd)
                require(
                    stat.S_ISREG(st.st_mode)
                    and st.st_uid == 0
                    and st.st_nlink == 1
                    and stat.S_IMODE(st.st_mode) == 0o600
                    and st.st_size <= 131072,
                    "Private prior receipt required",
                )
                record = json.loads(os.read(fd, 131073))
                require(
                    record.get("schemaVersion") == 1
                    and record.get("kind") == "evacuation-maintenance-window"
                    and record.get("status") == "restored",
                    "Restore previous maintenance window first",
                )
            finally:
                os.close(fd)

    @staticmethod
    def check_directory(path):
        st = path.lstat()
        require(
            stat.S_ISDIR(st.st_mode)
            and st.st_uid == 0
            and stat.S_IMODE(st.st_mode) == 0o700,
            "Private root directory required",
        )

    def read(self):
        fd = os.open("receipt.json", os.O_RDONLY | os.O_NOFOLLOW, dir_fd=self.fd)
        try:
            st = os.fstat(fd)
            require(
                stat.S_ISREG(st.st_mode)
                and st.st_uid == 0
                and st.st_nlink == 1
                and stat.S_IMODE(st.st_mode) == 0o600
                and st.st_size <= 131072,
                "Private root receipt required",
            )
            return json.loads(os.read(fd, 131073))
        finally:
            os.close(fd)

    def write(self, value):
        if self.last is not None:
            require(self.read() == self.last, "Receipt drift")
        temporary = "receipt.next"
        fd = os.open(
            temporary,
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
            0o600,
            dir_fd=self.fd,
        )
        try:
            with os.fdopen(fd, "wb") as stream:
                stream.write(canonical(value))
                stream.flush()
                os.fsync(stream.fileno())
            os.rename(temporary, "receipt.json", src_dir_fd=self.fd, dst_dir_fd=self.fd)
            os.fsync(self.fd)
        finally:
            if os.path.exists(self.directory / temporary):
                os.unlink(temporary, dir_fd=self.fd)
        self.last = json.loads(canonical(value))


def validate(record, native):
    require(
        record.get("schemaVersion") == 1
        and record.get("kind") == "evacuation-maintenance-window",
        "Invalid maintenance receipt",
    )
    require(
        record["identity"] == native.identity()
        and set(record["timers"]) == set(TIMERS),
        "Host or timer identity changed",
    )
    for name, entry in record["timers"].items():
        current = native.state(name)
        require(
            native.configuration(current) == entry["configuration"],
            "Timer configuration changed",
        )
        require(
            current["Job"] == "" and current["ActiveState"] in ("active", "inactive"),
            "Timer transition pending",
        )
    return record


def pause(native, receipt, duration):
    require(60 <= duration <= 3600, "Window must be 60..3600 seconds")
    identity = native.identity()
    before = native.idle()
    timers = {}
    for name in TIMERS:
        current = native.state(name)
        require(
            current["LoadState"] == "loaded"
            and current["ActiveState"] in ("active", "inactive")
            and current["Job"] == "",
            "Stable timer required",
        )
        timers[name] = {
            "before": current,
            "configuration": native.configuration(current),
            "stopIntent": False,
            "stopped": False,
            "restoreIntent": False,
            "restored": False,
        }
    record = {
        "schemaVersion": 1,
        "kind": "evacuation-maintenance-window",
        "identity": identity,
        "createdAt": time.time(),
        "expiresAt": time.time() + duration,
        "expiresMonotonic": time.monotonic() + duration,
        "status": "pausing",
        "timers": timers,
        "before": before,
    }
    receipt.write(record)
    for name, entry in timers.items():
        if entry["before"]["ActiveState"] != "active":
            continue
        validate(record, native)
        native.idle()
        entry["stopIntent"] = True
        receipt.write(record)
        native.action("stop", name)
        require(native.state(name)["ActiveState"] == "inactive", "Timer did not stop")
        entry["stopped"] = True
        receipt.write(record)
    record["status"] = "paused"
    record["after"] = verify(native, record)
    receipt.write(record)
    return record


def verify(native, record):
    validate(record, native)
    require(
        record["status"] == "paused" and time.monotonic() < record["expiresMonotonic"],
        "Maintenance window inactive or expired",
    )
    require(
        all(native.state(name)["ActiveState"] == "inactive" for name in TIMERS),
        "Maintenance timer active",
    )
    return native.idle()


def restore(native, receipt, record):
    record = json.loads(canonical(record))
    validate(record, native)
    require(
        record["status"] in ("pausing", "paused", "restoring", "restored"),
        "Invalid restoration state",
    )
    for name, entry in record["timers"].items():
        current = native.state(name)
        before_active = entry["before"]["ActiveState"] == "active"
        if not entry["stopIntent"]:
            require(
                current["ActiveState"] == entry["before"]["ActiveState"],
                "Unowned timer state changed",
            )
        elif entry["restored"]:
            require(
                before_active and current["ActiveState"] == "active",
                "Restored timer changed",
            )
        elif entry["restoreIntent"]:
            require(before_active, "Invalid restoration intent")
        else:
            require(
                current["ActiveState"] == "inactive"
                or (not entry["stopped"] and current["ActiveState"] == "active"),
                "Paused timer state changed",
            )
    record["status"] = "restoring"
    receipt.write(record)
    for name, entry in record["timers"].items():
        if not entry["stopIntent"]:
            continue
        if native.state(name)["ActiveState"] == "inactive":
            entry["restoreIntent"] = True
            receipt.write(record)
            native.action("start", name)
        require(native.state(name)["ActiveState"] == "active", "Timer did not resume")
        entry["restored"] = True
        receipt.write(record)
    validate(record, native)
    require(
        all(
            native.state(name)["ActiveState"] == entry["before"]["ActiveState"]
            for name, entry in record["timers"].items()
        ),
        "Restored timer state differs",
    )
    record["status"] = "restored"
    record["restoredAt"] = time.time()
    receipt.write(record)
    return record


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=("pause", "verify", "restore"))
    parser.add_argument("window")
    parser.add_argument("--seconds", type=int, default=1800)
    args = parser.parse_args()
    os.umask(0o077)
    native = Native()
    native.identity()
    receipt = Receipt(args.window, create=args.action == "pause")
    if args.action == "pause":
        value = pause(native, receipt, args.seconds)
    elif args.action == "verify":
        value = verify(native, receipt.last)
    else:
        value = restore(native, receipt, receipt.last)
    print(
        json.dumps(
            {
                "action": args.action,
                "receipt": str(receipt.path),
                "status": value.get("status", "verified"),
            }
        )
    )


if __name__ == "__main__":
    main()
