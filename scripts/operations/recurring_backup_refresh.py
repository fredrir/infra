import argparse
import fcntl
import hashlib
import importlib.util
import json
import os
import re
import stat
from contextlib import contextmanager
from itertools import chain, islice
from pathlib import Path

SERVICE = "/etc/systemd/system/restic-backups-llunde-backend.service"
TIMER = "/etc/systemd/system/restic-backups-llunde-backend.timer"
INSTALLATION = "/var/lib/platform-backup-install/receipt.json"
BASE = "/var/lib/platform-backup-refresh"
RECEIPT = BASE + "/receipt.json"
OLD_SERVICE_SHA = "6c487cdabd371b5b1cd7089282ffa6b0e213cc4864addddb871808f656634527"
CANDIDATE_SHA = "612113b8853b20682ad97424b43bbffe742be03bb228f491e173e1eb28426577"
PRESERVED = (
    INSTALLATION,
    TIMER,
    "/var/lib/infra-evacuation/llunde/identity/backup-status",
    "/var/lib/infra-evacuation/llunde/identity/backup-status.pub",
    "/var/lib/infra-evacuation/llunde/identity/age.key",
    "/etc/infra-evacuation/llunde/backup-status-known-hosts",
    "/etc/infra-evacuation/llunde/backup/manifest.json",
    "/etc/infra-evacuation/llunde/backup/credentials.age",
)


def require(value, message):
    if not value:
        raise ValueError(message)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def checked_read(path, owner, mode, maximum=262144):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as stream:
        before = os.fstat(stream.fileno())
        require(
            stat.S_ISREG(before.st_mode)
            and before.st_uid == owner
            and before.st_nlink == 1
            and stat.S_IMODE(before.st_mode) == mode
            and 0 < before.st_size <= maximum,
            "Private bundle file differs",
        )
        data = stream.read(maximum + 1)
        after = os.fstat(stream.fileno())
        require(
            len(data) == before.st_size
            and all(
                getattr(before, key) == getattr(after, key)
                for key in ("st_ino", "st_dev", "st_size", "st_mtime_ns", "st_ctime_ns")
            ),
            "Bundle file changed",
        )
        return data


def verify_bundle(directory, expected, *, owner=0):
    root = Path(directory)
    require(
        root.is_absolute()
        and re.fullmatch(r"recurring-backup-deploy-[0-9]{8}T[0-9]{6}Z", root.name)
        and re.fullmatch(r"[a-f0-9]{64}", expected),
        "Fixed refresh bundle required",
    )
    for path in [*reversed(root.parents), root]:
        info = path.lstat()
        require(
            stat.S_ISDIR(info.st_mode)
            and info.st_uid in (0, owner)
            and not stat.S_IMODE(info.st_mode) & 0o022,
            "Unsafe bundle ancestor",
        )
    raw = checked_read(root / "manifest.json", owner, 0o600)
    require(digest(raw) == expected, "Bundle manifest checksum differs")
    manifest = json.loads(raw)
    require(
        manifest["schemaVersion"] == 1
        and manifest["kind"] == "recurring-backup-refresh"
        and manifest["remoteDirectory"]
        == "/var/lib/infra-evacuation/llunde/" + root.name
        and manifest["candidateSHA256"] == CANDIDATE_SHA
        and isinstance(manifest["files"], dict)
        and 1 <= len(manifest["files"]) <= 40,
        "Refresh manifest differs",
    )
    required = {
        "previous.service",
        "restic-backups-llunde-backend.service",
        "helpers/recurring_backup_refresh.py",
        "helpers/evacuation_recurring_backup.py",
        "candidate/staging.json",
    }
    require(required <= set(manifest["files"]), "Required refresh files absent")
    directories = {"."}
    for name in manifest["files"]:
        require(
            re.fullmatch(
                r"(?:[a-z.-]+\.service|helpers/[a-z_]+\.py|candidate/staging\.json|"
                r"candidate/units/(?:edge|llunde-backend|llunde-frontend)/[A-Za-z0-9_.-]+|candidate/units/Caddyfile)",
                name,
            ),
            "Refresh file path differs",
        )
        directories.update(str(parent) for parent in Path(name).parents)
    actual_files, actual_dirs = set(), set()
    for path in islice(chain([root], root.rglob("*")), 52):
        require(
            len(actual_files) + len(actual_dirs) <= 50, "Bundle inventory exceeds bound"
        )
        name = str(path.relative_to(root))
        info = path.lstat()
        require(not stat.S_ISLNK(info.st_mode), "Bundle link forbidden")
        if stat.S_ISDIR(info.st_mode):
            require(
                info.st_uid == owner and stat.S_IMODE(info.st_mode) == 0o700,
                "Bundle directory differs",
            )
            actual_dirs.add(name)
        else:
            require(
                name in set(manifest["files"]) | {"manifest.json"},
                "Unexpected bundle file",
            )
            actual_files.add(name)
    require(
        actual_dirs == directories
        and actual_files == set(manifest["files"]) | {"manifest.json"},
        "Exact bundle inventory required",
    )
    for name, wanted in manifest["files"].items():
        data = checked_read(root / name, owner, 0o600)
        require(
            set(wanted) == {"sha256", "bytes"}
            and digest(data) == wanted["sha256"]
            and len(data) == wanted["bytes"],
            "Bundle byte contract differs",
        )
    require(
        manifest["files"]["candidate/staging.json"]["sha256"] == CANDIDATE_SHA,
        "Candidate binding differs",
    )
    previous = checked_read(root / "previous.service", owner, 0o600)
    require(digest(previous) == OLD_SERVICE_SHA, "Original service differs")
    current = checked_read(root / "restic-backups-llunde-backend.service", owner, 0o600)
    old_lines = previous.decode("ascii").splitlines(keepends=True)
    new_lines = current.decode("ascii").splitlines(keepends=True)
    require(len(old_lines) == len(new_lines), "Service layout changed")
    changed = [
        (old, new) for old, new in zip(old_lines, new_lines, strict=True) if old != new
    ]
    require(
        len(changed) == 1 and changed[0][0].startswith("ExecStart="),
        "Only ExecStart may change",
    )
    old, new = changed[0]
    wanted = re.sub(
        r"/var/lib/infra-evacuation/llunde/recurring-backup-deploy-[0-9TZ]+/helpers",
        manifest["remoteDirectory"] + "/helpers",
        old,
    )
    wanted = wanted.replace(
        "/var/lib/infra-evacuation/llunde/unit-promotion-bundle-20260912T044726Z/candidate",
        manifest["remoteDirectory"] + "/candidate",
    )
    require(new == wanted, "Backup command scope changed")
    return manifest


class Native:
    def __init__(self):
        from evacuation_execution import Commands, host_identity

        host_identity("fredrir-09")
        require(
            Path("/etc/machine-id").read_text().strip()
            == "98b6af13dea54f4081903e96458be24f",
            "Target machine differs",
        )
        self.commands = Commands(90)

    def baseline(self):
        import evacuation_cutover as cutover
        from evacuation_execution import host_identity, stopped, unit_state
        from evacuation_guards import verify_installation

        host_identity("fredrir-09")
        require(
            Path("/etc/machine-id").read_text().strip()
            == "98b6af13dea54f4081903e96458be24f",
            "Target machine differs",
        )
        guard = verify_installation(
            "/var/lib/platform-evacuation/receipts/guards.json", require_loaded=True
        )
        result = {"guardFilesSHA256": guard["guardFilesSHA256"], "applications": {}}
        for user, units in cutover.APP_UNITS.items():
            for unit in units:
                value = unit_state(self.commands, unit, user)
                require(stopped(value), "Applications must remain stopped")
                result["applications"][user + "/" + unit] = value
            _, output = self.commands.user(
                user, ["podman", "ps", "--all", "--format", "{{.ID}}"], maximum=1024
            )
            require(not output.strip(), "Container stores must be empty")
        for unit in (Path(SERVICE).name, Path(TIMER).name):
            _, raw = self.commands.run(
                [
                    "systemctl",
                    "show",
                    "--all",
                    unit,
                    "--property=ActiveState,SubState,UnitFileState,Job,FragmentPath",
                ],
                maximum=4096,
            )
            fields = dict(line.split("=", 1) for line in raw.decode().splitlines())
            require(
                fields["ActiveState"] == "inactive"
                and fields["Job"] == ""
                and fields["FragmentPath"] == "/etc/systemd/system/" + unit,
                "Dormant backup units required",
            )
            require(
                fields["UnitFileState"]
                == ("disabled" if unit.endswith(".timer") else "static"),
                "Backup enablement differs",
            )
            result[unit] = fields
        return result

    def reload(self):
        self.commands.run(["systemctl", "daemon-reload"], maximum=4096)

    def loaded(self, content):
        command = next(
            line.removeprefix("ExecStart=")
            for line in content.decode().splitlines()
            if line.startswith("ExecStart=")
        )
        _, raw = self.commands.run(
            [
                "systemctl",
                "show",
                Path(SERVICE).name,
                "--property=ExecStart",
                "--value",
            ],
            maximum=8192,
        )
        require(
            "argv[]=" + command + " ;" in raw.decode(), "Loaded backup command differs"
        )


class Refresh:
    def __init__(self, bundle, manifest, *, filesystem=None, native=None):
        from evacuation_guards import Filesystem

        self.fs, self.native = filesystem or Filesystem(), native or Native()
        self.bundle, self.manifest = Path(bundle), manifest
        self.before = (self.bundle / "previous.service").read_bytes()
        self.after = (
            self.bundle / "restic-backups-llunde-backend.service"
        ).read_bytes()

    def preserved(self):
        files = {path: self.fs.metadata(path) for path in PRESERVED}
        require(
            all(info["exists"] for info in files.values()),
            "Preserved backup authority absent",
        )
        for base, names in (
            (
                "/var/lib/platform-evacuation",
                ("source-locked", "reconciliation-locked"),
            ),
            (
                "/var/lib/infra-evacuation/llunde",
                (
                    "stage-approved",
                    "restore-approved",
                    "source-fenced",
                    "edge-approved",
                    "target-writer-start-attempted",
                ),
            ),
        ):
            files.update(
                {
                    base + "/" + name: self.fs.metadata(base + "/" + name)
                    for name in names
                }
            )
        installation = self.fs.read_json(INSTALLATION)
        require(
            installation["kind"] == "recurring-backup-installation"
            and installation["host"] == "fredrir-09"
            and installation["status"] == "installed"
            and installation["files"][SERVICE]["desiredSHA256"] == OLD_SERVICE_SHA,
            "Original installation authority differs",
        )
        return files

    @contextmanager
    def lock(self):
        self.fs.directory(BASE, 0o700, {})
        with self.fs.parent_fd(BASE + "/lock") as (parent, name):
            fd = os.open(
                name, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600, dir_fd=parent
            )
            try:
                info = os.fstat(fd)
                require(
                    stat.S_ISREG(info.st_mode)
                    and info.st_uid == self.fs.owner
                    and info.st_nlink == 1
                    and stat.S_IMODE(info.st_mode) == 0o600,
                    "Refresh lock differs",
                )
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                yield
            finally:
                os.close(fd)

    def replace(self, receipt, content, expected):
        temporary = SERVICE + ".refresh-" + receipt["nonce"]
        staged = self.fs.metadata(temporary)
        previous = receipt.get("replacement") or {}
        if staged["exists"]:
            require(
                previous.get("temporary") == temporary
                and previous.get("sha256") == digest(content)
                and previous.get("expected") == expected
                and previous.get("staged") == staged,
                "Temporary service ownership changed",
            )
        receipt["replacement"] = {
            "temporary": temporary,
            "sha256": digest(content),
            "expected": expected,
        }
        if staged["exists"]:
            receipt["replacement"]["staged"] = staged
        self.fs.save(RECEIPT, receipt)
        if not staged["exists"]:
            self.fs.write(temporary, content, 0o644)
        info = self.fs.metadata(temporary)
        require(
            info["sha256"] == digest(content) and info["mode"] == "0644",
            "Owned temporary service differs",
        )
        receipt["replacement"]["staged"] = info
        self.fs.save(RECEIPT, receipt)
        self.fs.metadata(SERVICE, expected)
        with self.fs.parent_fd(SERVICE) as (parent, name):
            os.replace(Path(temporary).name, name, src_dir_fd=parent, dst_dir_fd=parent)
            os.fsync(parent)
        receipt["service"] = self.fs.metadata(SERVICE)
        receipt["replacement"] = None
        self.fs.save(RECEIPT, receipt)

    def apply(self):
        with self.lock():
            require(
                not self.fs.metadata(RECEIPT)["exists"],
                "Existing refresh requires verification or rollback",
            )
            baseline = self.native.baseline()
            current = self.fs.metadata(SERVICE)
            require(
                current["sha256"] == OLD_SERVICE_SHA and current["mode"] == "0644",
                "Original service drifted",
            )
            require(
                self.fs.read_json(INSTALLATION)["files"][SERVICE]["after"] == current,
                "Original service ownership changed",
            )
            receipt = {
                "schemaVersion": 1,
                "kind": "recurring-backup-refresh",
                "host": "fredrir-09",
                "status": "preparing",
                "nonce": os.urandom(8).hex(),
                "bundle": self.manifest["remoteDirectory"],
                "manifestSHA256": digest((self.bundle / "manifest.json").read_bytes()),
                "candidateSHA256": CANDIDATE_SHA,
                "before": current,
                "preserved": self.preserved(),
                "baseline": baseline,
                "timerEnabled": False,
            }
            self.fs.save(RECEIPT, receipt)
            self.replace(receipt, self.after, current)
            self.native.reload()
            self.native.loaded(self.after)
            require(
                self.native.baseline() == baseline
                and self.preserved() == receipt["preserved"],
                "Refresh changed preserved state",
            )
            receipt["status"] = "installed"
            self.fs.save(RECEIPT, receipt)
            return receipt

    def receipt(self):
        receipt = self.fs.read_json(RECEIPT)
        require(
            receipt["schemaVersion"] == 1
            and receipt["kind"] == "recurring-backup-refresh"
            and receipt["host"] == "fredrir-09"
            and receipt["bundle"] == self.manifest["remoteDirectory"]
            and receipt["manifestSHA256"]
            == digest((self.bundle / "manifest.json").read_bytes())
            and re.fullmatch("[a-f0-9]{16}", receipt["nonce"]),
            "Owned refresh receipt differs",
        )
        require(self.preserved() == receipt["preserved"], "Preserved authority changed")
        return receipt

    def verify(self):
        with self.lock():
            receipt = self.receipt()
            require(receipt["status"] == "installed", "Refresh incomplete")
            self.fs.metadata(SERVICE, receipt["service"])
            self.native.loaded(self.after)
            require(
                self.native.baseline() == receipt["baseline"],
                "Dormant baseline changed",
            )
            return receipt

    def rollback(self):
        with self.lock():
            receipt = self.receipt()
            require(
                receipt["status"]
                in ("preparing", "installed", "rolling-back", "rolled-back"),
                "Refresh status differs",
            )
            require(
                self.native.baseline() == receipt["baseline"],
                "Dormant baseline changed",
            )
            current = self.fs.metadata(SERVICE)
            require(
                current["mode"] == "0644"
                and current["sha256"] in (OLD_SERVICE_SHA, digest(self.after)),
                "Service drift prevents rollback",
            )
            expected = [
                receipt["before"],
                receipt.get("service"),
                (receipt.get("replacement") or {}).get("staged"),
            ]
            require(current in expected, "Service ownership changed")
            receipt["status"] = "rolling-back"
            self.fs.save(RECEIPT, receipt)
            temporary = SERVICE + ".refresh-" + receipt["nonce"]
            staged = self.fs.metadata(temporary)
            if staged["exists"]:
                require(
                    (receipt.get("replacement") or {}).get("staged") == staged,
                    "Temporary service ownership changed",
                )
                self.fs.unlink(temporary, staged)
            if current["sha256"] != OLD_SERVICE_SHA:
                self.replace(receipt, self.before, current)
            self.native.reload()
            self.native.loaded(self.before)
            require(
                self.native.baseline() == receipt["baseline"]
                and self.preserved() == receipt["preserved"],
                "Rollback changed preserved state",
            )
            receipt["status"] = "rolled-back"
            self.fs.save(RECEIPT, receipt)
            return receipt


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "action", choices=("verify-bundle", "apply", "verify", "rollback")
    )
    parser.add_argument("--bundle", type=Path, required=True)
    parser.add_argument("--manifest-sha256", required=True)
    args = parser.parse_args()
    require(os.geteuid() == 0, "Root target helper required")
    os.umask(0o077)
    manifest = verify_bundle(args.bundle, args.manifest_sha256)
    require(
        str(args.bundle) == manifest["remoteDirectory"],
        "Delivered bundle location differs",
    )
    if args.action == "verify-bundle":
        result = {"verified": True, "candidateSHA256": CANDIDATE_SHA}
    else:
        spec = importlib.util.spec_from_file_location(
            "evacuation_staging", args.bundle / "helpers/evacuation_staging.py"
        )
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        module.validate_plan(args.bundle / "candidate")
        result = getattr(Refresh(args.bundle, manifest), args.action)()
        result = {
            "status": result["status"],
            "candidateSHA256": CANDIDATE_SHA,
            "timerEnabled": False,
        }
    print(json.dumps(result))


if __name__ == "__main__":
    main()
