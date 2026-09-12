import argparse
import hashlib
import json
import os
import re
import signal
import stat
import subprocess
import sys
import time
from contextlib import contextmanager
from pathlib import Path

INVENTORY_ROOT = Path("/root/infra-evacuation-reset-c")
INVENTORY_SHA = "879571ec75ee727c47ffc3b9f3f58bfdd0bca04338b9d1e5e95fcafe436abad6"
MANIFEST_SHA = "56c73fcc59338d35997f110fb8cebb31fa26c7b8382a5699c3ee695f9575bae3"
RECOVERY_ERRORS = (
    OSError,
    ValueError,
    LookupError,
    TypeError,
    RuntimeError,
    subprocess.SubprocessError,
    KeyboardInterrupt,
    SystemExit,
)


def private_module(path, maximum):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        if (
            not stat.S_ISREG(info.st_mode)
            or info.st_uid != 0
            or info.st_nlink != 1
            or stat.S_IMODE(info.st_mode) != 0o600
            or not 0 < info.st_size <= maximum
        ):
            raise ValueError("Unsafe frozen module")
        raw = os.read(fd, maximum + 1)
        if len(raw) != info.st_size:
            raise ValueError("Frozen module changed while reading")
        return raw
    finally:
        os.close(fd)


def load_inventory():
    if not INVENTORY_ROOT.exists() and not (
        __name__ != "__main__"
        and os.environ.get("INFRA_RESET_EXECUTOR_TEST_IMPORT") == "1"
    ):
        raise ValueError("Frozen inventory bundle is absent")
    if INVENTORY_ROOT.exists():
        for path in (Path("/"), Path("/root"), INVENTORY_ROOT):
            info = path.lstat()
            if (
                not stat.S_ISDIR(info.st_mode)
                or info.st_uid != 0
                or info.st_mode & 0o022
            ):
                raise ValueError("Unsafe frozen inventory ancestry")
        raw = private_module(INVENTORY_ROOT / "manifest.json", 65536)
        if hashlib.sha256(raw).hexdigest() != MANIFEST_SHA:
            raise ValueError("Frozen inventory manifest changed")
        manifest = json.loads(raw)
        expected = set(manifest["files"]) | {"manifest.json"}
        expected_dirs = {
            str(parent)
            for name in expected
            for parent in Path(name).parents
            if str(parent) != "."
        }
        found, directories = set(), set()
        for parent, children, files in os.walk(INVENTORY_ROOT, followlinks=False):
            if len(found) + len(directories) + len(children) + len(files) > 50:
                raise ValueError("Frozen dependency inventory exceeds bound")
            for name in children + files:
                path = Path(parent) / name
                relative = str(path.relative_to(INVENTORY_ROOT))
                info = path.lstat()
                if (
                    info.st_uid != 0
                    or stat.S_ISLNK(info.st_mode)
                    or info.st_mode & 0o022
                ):
                    raise ValueError("Unsafe frozen dependency path")
                if name in children:
                    directories.add(relative)
                else:
                    found.add(relative)
        if found != expected or directories != expected_dirs:
            raise ValueError("Frozen dependency inventory changed")
        for name, value in manifest["files"].items():
            raw = private_module(INVENTORY_ROOT / name, 1024**2)
            if (
                len(raw) != value["bytes"]
                or hashlib.sha256(raw).hexdigest() != value["sha256"]
            ):
                raise ValueError("Frozen dependency bytes changed")
        if (
            manifest["files"]["evacuation_reset_inventory.py"]["sha256"]
            != INVENTORY_SHA
        ):
            raise ValueError("Frozen inventory module binding differs")
        sys.path.insert(0, str(INVENTORY_ROOT))
    import evacuation_reset_inventory

    return evacuation_reset_inventory


inventory = load_inventory()


def validate_limits(action, control, kernel):
    seconds = 70 if action == "prepare" else 60
    inventory.require(
        control["RuntimeMaxUSec"] == seconds * 1000000
        and control["TimeoutStopUSec"] == 25000000
        and control["KillMode"] == "mixed"
        and control["SendSIGKILL"] is True
        and control["MainPID"] == os.getpid(),
        "Exact native lifetime, stop grace and main process required",
    )
    quota, period = kernel["cpu.max"].split()
    inventory.require(
        quota.isdigit()
        and period.isdigit()
        and 0 < int(quota) <= int(period)
        and kernel["memory.max"].isdigit()
        and 0 < int(kernel["memory.max"]) <= 256 * 1024**2
        and kernel["memory.swap.max"] == "0"
        and kernel["pids.max"].isdigit()
        and 0 < int(kernel["pids.max"]) <= 64,
        "Native CPU, memory, swap and task limits required",
    )
    return {
        "runtimeSeconds": seconds,
        "stopGraceSeconds": 25,
        "control": control,
        "kernel": kernel,
    }


def native_limits(action):
    raw = Path("/proc/self/cgroup").read_text()
    match = re.fullmatch(
        r"0::(/system[.]slice/(infra-evacuation-reset-"
        + action
        + r"-c-[a-f0-9]{12}[.]service))\n",
        raw,
    )
    inventory.require(match is not None, "Exact owned transient service required")
    path, unit = match.groups()
    kernel = {}
    for name in ("cpu.max", "memory.max", "memory.swap.max", "pids.max"):
        with (Path("/sys/fs/cgroup") / path.lstrip("/") / name).open("rb") as stream:
            value = stream.read(129)
        inventory.require(len(value) <= 128, "Bounded kernel control required")
        kernel[name] = value.decode().strip()
    unit_path = "/org/freedesktop/systemd1/unit/" + "".join(
        character if character.isalnum() else "_" + format(ord(character), "02x")
        for character in unit
    )
    commands, control = inventory.Commands(8), {}
    for name, signature in (
        ("RuntimeMaxUSec", "t"),
        ("TimeoutStopUSec", "t"),
        ("KillMode", "s"),
        ("SendSIGKILL", "b"),
        ("MainPID", "u"),
    ):
        _, raw = commands.run(
            [
                "busctl",
                "--json=short",
                "get-property",
                "org.freedesktop.systemd1",
                unit_path,
                "org.freedesktop.systemd1.Service",
                name,
            ],
            timeout=2,
            maximum=4096,
        )
        value = json.loads(raw)
        inventory.require(
            value["type"] == signature, "Typed native service property required"
        )
        control[name] = value["data"]
    result = validate_limits(action, control, kernel)
    result["controlGroup"] = path
    return result


def interrupted(signum, frame):
    raise InterruptedError("Reset received termination signal")


@contextmanager
def cleanup_signals():
    previous = {
        number: signal.signal(number, signal.SIG_IGN)
        for number in (signal.SIGTERM, signal.SIGINT)
    }
    try:
        yield
    finally:
        for number, handler in previous.items():
            signal.signal(number, handler)


class Executor(inventory.Reset):
    def save(self):
        if self.record is None:
            raise ValueError("Owned journal is not initialized")
        if hasattr(self, "journal_metadata"):
            self.fs.metadata(inventory.JOURNAL, self.journal_metadata)
        self.fs.save(inventory.JOURNAL, self.record)
        self.journal_metadata = self.fs.metadata(inventory.JOURNAL)

    def phase(self, name):
        inventory.require(
            time.monotonic() < self.commands.deadline,
            "Reset execution deadline reached",
        )
        self.record["phase"] = name
        self.save()

    def directory(self, path):
        paths = [path]
        if path.startswith(self.archive + "/"):
            relative = Path(path).relative_to(self.archive)
            paths = [self.archive] + [
                self.archive + "/" + str(Path(*relative.parts[:index]))
                for index in range(1, len(relative.parts) + 1)
            ]
        for value in paths:
            self.fs.directory(value, 0o700, self.record["directoriesCreated"])
        self.save()

    def directory_metadata(self, path):
        with self.fs.directory_fd(path) as fd:
            return inventory.directory_info(os.fstat(fd))

    def journal_event(self, value):
        self.record["events"].append({**value, "status": "pending"})
        self.save()
        return self.record["events"][-1]

    def finish_event(self, event, observed):
        event.update(status="complete", after=observed)
        self.save()

    def copy_before(self, path, expected):
        observed, raw = self.fs.read(path)
        inventory.require(
            observed == expected, "Restart history changed before preservation"
        )
        destination = self.archive + "/before" + path
        inventory.require(
            not self.fs.metadata(destination)["exists"],
            "Preserved receipt already exists",
        )
        event = self.journal_event(
            {
                "operation": "copy",
                "source": path,
                "destination": destination,
                "before": expected,
                "sha256": inventory.digest(raw),
            }
        )
        self.directory(str(Path(destination).parent))
        self.finish_event(event, self.fs.write(destination, raw, 0o600))

    def move_file(self, path, expected):
        destination = self.archive + "/files" + path
        inventory.require(
            self.fs.metadata(path) == expected
            and not self.fs.metadata(destination)["exists"],
            "Archive source drift or destination exists",
        )
        event = self.journal_event(
            {
                "operation": "rename-file",
                "source": path,
                "destination": destination,
                "before": expected,
            }
        )
        self.directory(str(Path(destination).parent))
        with (
            self.fs.parent_fd(path) as (parent, name),
            self.fs.parent_fd(destination) as (target_parent, target_name),
        ):
            self.fs.metadata(path, expected)
            inventory.require(
                os.fstat(parent).st_dev
                == os.fstat(target_parent).st_dev
                == expected["device"]
                and not self.fs.metadata(destination)["exists"],
                "Same-filesystem fresh archival required",
            )
            os.rename(name, target_name, src_dir_fd=parent, dst_dir_fd=target_parent)
            os.fsync(parent)
            os.fsync(target_parent)
        observed = self.fs.metadata(destination, expected)
        inventory.require(
            not self.fs.metadata(path)["exists"], "Archived file remains active"
        )
        self.finish_event(event, observed)

    def move_candidates(self, expected_tree):
        path = inventory.TARGET + "/candidates"
        destination = self.archive + "/files" + path
        inventory.require(
            self.tree_snapshot(
                path, self.plan["requiredFreshObservations"]["installedCandidates"]
            )
            == expected_tree,
            "Inert candidate tree changed",
        )
        before = self.directory_metadata(path)
        inventory.require(
            before["mode"] == "0700"
            and not self.fs.path(destination).exists()
            and not self.fs.path(destination).is_symlink(),
            "Private fresh candidate archive required",
        )
        event = self.journal_event(
            {
                "operation": "rename-tree",
                "source": path,
                "destination": destination,
                "before": before,
                "treeSHA256": inventory.digest(inventory.canonical(expected_tree)),
            }
        )
        self.directory(str(Path(destination).parent))
        with (
            self.fs.parent_fd(path) as (parent, name),
            self.fs.parent_fd(destination) as (target_parent, target_name),
        ):
            inventory.require(
                self.directory_metadata(path) == before
                and os.fstat(target_parent).st_dev == before["device"],
                "Candidate directory identity or filesystem changed",
            )
            inventory.require(
                not self.fs.path(destination).exists()
                and not self.fs.path(destination).is_symlink(),
                "Candidate archive destination appeared",
            )
            os.rename(name, target_name, src_dir_fd=parent, dst_dir_fd=target_parent)
            os.fsync(parent)
            os.fsync(target_parent)
        inventory.require(
            self.tree_snapshot(
                destination,
                self.plan["requiredFreshObservations"]["installedCandidates"],
            )
            == expected_tree,
            "Preserved candidate tree differs",
        )
        self.finish_event(event, self.directory_metadata(destination))

    def move_data(self, item):
        before = item["expected"]
        event = self.journal_event(
            {
                "operation": "rename-data",
                "source": item["source"],
                "destination": item["destination"],
                "before": before,
            }
        )
        with self.data_parent() as parent:
            name, destination = (
                Path(item["source"]).name,
                Path(item["destination"]).name,
            )
            observed = os.stat(name, dir_fd=parent, follow_symlinks=False)
            inventory.require(
                stat.S_ISDIR(observed.st_mode)
                and inventory.directory_info(observed) == before,
                "Data identity changed before preservation",
            )
            try:
                os.stat(destination, dir_fd=parent, follow_symlinks=False)
            except FileNotFoundError:
                pass
            else:
                raise ValueError("Data preservation path appeared")
            os.rename(name, destination, src_dir_fd=parent, dst_dir_fd=parent)
            os.fsync(parent)
            after = inventory.directory_info(
                os.stat(destination, dir_fd=parent, follow_symlinks=False)
            )
            inventory.require(after == before, "Preserved data identity changed")
        self.finish_event(event, after)

    def write_new(self, path, data):
        inventory.require(
            not self.fs.metadata(path)["exists"], "Fresh staging path required"
        )
        event = self.journal_event(
            {
                "operation": "write-new",
                "destination": path,
                "sha256": inventory.digest(data),
            }
        )
        parent = str(Path(path).parent)
        if parent == inventory.TARGET:
            with self.fs.directory_fd(parent):
                pass
        else:
            self.directory(parent)
        self.finish_event(event, self.fs.write(path, data, 0o600))

    def archive_empty_unit_directories(self):
        for path, expected in self.record["oldUnitDirectories"].items():
            destination = self.archive + "/empty-directories" + path
            inventory.require(
                self.directory_metadata(path) == expected
                and not any(self.fs.path(path).iterdir()),
                "Receipt-owned unit directory is not unchanged and empty",
            )
            inventory.require(
                not self.fs.path(destination).exists()
                and not self.fs.path(destination).is_symlink(),
                "Directory archive already exists",
            )
            event = self.journal_event(
                {
                    "operation": "rename-empty-directory",
                    "source": path,
                    "destination": destination,
                    "before": expected,
                }
            )
            self.directory(str(Path(destination).parent))
            with (
                self.fs.parent_fd(path) as (parent, name),
                self.fs.parent_fd(destination) as (other, other_name),
            ):
                inventory.require(
                    self.directory_metadata(path) == expected
                    and not any(self.fs.path(path).iterdir())
                    and os.fstat(other).st_dev == expected["device"],
                    "Owned directory changed before archival",
                )
                inventory.require(
                    not self.fs.path(destination).exists()
                    and not self.fs.path(destination).is_symlink(),
                    "Directory archive destination appeared",
                )
                os.rename(name, other_name, src_dir_fd=parent, dst_dir_fd=other)
                os.fsync(parent)
                os.fsync(other)
            self.finish_event(event, self.directory_metadata(destination))

    def stable_runtime(self, *, unloaded=False):
        current = self.runtime(unloaded=unloaded)
        baseline = self.record["initialRuntime"]
        inventory.require(
            current["managers"] == baseline["managers"]
            and current["timers"] == baseline["timers"],
            "Manager PID or reconciliation configuration changed",
        )
        service = self.fs.metadata(
            "/etc/systemd/system/restic-backups-llunde-backend.service"
        )
        inventory.require(
            service == self.record["recurringBackupRefresh"]["existingService"],
            "Dormant backup service changed",
        )
        return current

    def preserved(self, *, promoted=False):
        self.data_snapshot(preserved=True)
        for event in self.record["events"]:
            inventory.require(
                event["status"] == "complete",
                "Pending reset event needs explicit recovery",
            )
            operation = event["operation"]
            if operation in ("copy", "rename-file", "write-new"):
                self.fs.metadata(event["destination"], event["after"])
                if operation == "rename-file":
                    renewed = {
                        item["destination"]
                        for item in self.record["events"]
                        if item["operation"] == "write-new"
                    }
                    if promoted:
                        renewed |= set(self.plan["newInstalledUnitHashes"]) | {
                            inventory.UNITS
                        }
                    if event["source"] not in renewed:
                        inventory.require(
                            not self.fs.metadata(event["source"])["exists"],
                            "Archived source reappeared",
                        )
            elif operation == "rename-tree":
                inventory.require(
                    self.directory_metadata(event["destination"]) == event["after"],
                    "Archived directory changed",
                )
                tree = self.tree_snapshot(
                    event["destination"],
                    self.plan["requiredFreshObservations"]["installedCandidates"],
                )
                inventory.require(
                    inventory.digest(inventory.canonical(tree)) == event["treeSHA256"],
                    "Archived candidate bytes changed",
                )
                inventory.require(
                    not self.fs.path(event["source"]).is_symlink(),
                    "Staged candidate symlink appeared",
                )
            elif operation == "rename-empty-directory":
                inventory.require(
                    self.directory_metadata(event["destination"]) == event["after"]
                    and not any(self.fs.path(event["destination"]).iterdir()),
                    "Preserved empty directory changed",
                )
                if not promoted:
                    inventory.require(
                        not self.fs.path(event["source"]).exists()
                        and not self.fs.path(event["source"]).is_symlink(),
                        "Old unit directory reappeared",
                    )

    def failure(self):
        with cleanup_signals():
            return self._failure()

    def _failure(self):
        if self.record is None:
            return {"recoveryRequired": False, "journalInitialized": False}
        self.commands = inventory.Commands(15)
        result = {
            "recoveryRequired": True,
            "automaticUnwind": False,
            "errors": [],
            "fences": {},
            "applicationsStoppedVerified": False,
        }
        self.record["status"] = "failed-retained"
        self.record["failure"] = result
        try:
            self.save()
        except RECOVERY_ERRORS:
            result["errors"].append("failure-journal-write")
        for path, value in self.plan["requiredFreshObservations"]["fences"].items():
            try:
                info, raw = self.fs.read(path)
                if info["exists"]:
                    inventory.require(
                        info["mode"] == "0644" and json.loads(raw) == value,
                        "Unexpected recovery fence",
                    )
                    result["fences"][path] = "retained"
                    continue
                archived = self.archive + "/files" + path
                saved, raw = self.fs.read(archived)
                expected = self.record["initialMarkers"][path]
                inventory.require(
                    saved == expected and json.loads(raw) == value,
                    "Owned archived reverse fence required",
                )
                self.record.setdefault("refenceIntent", {})[path] = {
                    "source": archived,
                    "metadata": saved,
                }
                self.save()
                result["fences"][path] = self.fs.write(path, raw, 0o644)
                self.save()
            except RECOVERY_ERRORS:
                result["errors"].append("refence:" + path)
        try:
            self.stable_runtime()
            result["applicationsStoppedVerified"] = True
        except RECOVERY_ERRORS:
            result["errors"].append("runtime-readback")
        try:
            result["journalPersisted"] = True
            self.save()
        except RECOVERY_ERRORS:
            result["journalPersisted"] = False
        return result

    def prepare(self, inventory_path, source_path, provider_path):
        self.commands = inventory.Commands(70)
        expected = self.fs.read_json(inventory_path)
        inventory.fresh(expected["observedAt"], time.time())
        actual = self.inventory()
        inventory.require(
            {key: value for key, value in actual.items() if key != "observedAt"}
            == {key: value for key, value in expected.items() if key != "observedAt"},
            "Fresh inventory changed before reset",
        )
        authority = self.source_authority(source_path, provider_path)
        self.record = {
            "schemaVersion": 1,
            "kind": "evacuation-reset-execution",
            "planSHA256": inventory.PLAN_SHA,
            "host": "fredrir-09",
            "bootId": self.reverse["bootId"],
            "archive": self.archive,
            "status": "preparing",
            "phase": "reserve",
            "inventorySHA256": inventory.digest(inventory.canonical(expected)),
            "sourceBeforePrepare": authority,
            "nativePrepareLimits": self.native_limits,
            "inventoryBundleSHA256": MANIFEST_SHA,
            "initialRuntime": actual["runtime"],
            "initialMarkers": actual["markers"],
            "recurringBackupRefresh": actual["recurringBackupRefresh"],
            "events": [],
            "directoriesCreated": {},
            "applicationStartAttempted": False,
        }
        unit_receipt = self.fs.read_json(inventory.UNITS)
        required_dirs = {
            "/etc/containers/systemd/users/" + str(uid)
            for uid in inventory.USERS.values()
        }
        inventory.require(
            set(unit_receipt["directoriesCreated"]) == required_dirs,
            "Only three original receipt-owned unit directories may move",
        )
        owned = {}
        for path, expected_dir in unit_receipt["directoriesCreated"].items():
            current = self.directory_metadata(path)
            inventory.require(
                current["gid"] == 0
                and all(current[key] == value for key, value in expected_dir.items()),
                "Original unit directory identity changed",
            )
            owned[path] = current
        self.record["oldUnitDirectories"] = owned
        self.record["planRefinement"] = (
            "Archive only the three original receipt-owned empty UID directories in a distinct subtree before inert promotion"
        )
        self.journal_metadata = self.fs.write(
            inventory.JOURNAL, inventory.canonical(self.record), 0o600
        )
        try:
            self.directory(self.archive)
            self.phase("restore-restart-policy")
            self.check_markers()
            for item in self.plan["requiredFreshObservations"]["restartInhibitors"]:
                initial = actual["inhibitors"][item["datastore"]]
                self.copy_before(item["path"], initial["file"])
                self.copy_before(item["receipt"], initial["receipt"])
            self.record["restartPoliciesRestored"] = [
                inventory.restart.restore(
                    self.commands, self.reverse, name, _filesystem=self.fs
                )
                for name in ("postgres", "valkey")
            ]
            self.save()
            self.stable_runtime()
            self.phase("archive-approval-markers")
            self.check_markers()
            for path in self.plan["requiredFreshObservations"]["markers"]:
                self.move_file(path, actual["markers"][path])
            self.phase("preserve-data")
            self.check_markers(approvals=False)
            self.stable_runtime()
            self.data_snapshot()
            for item in self.plan["dataRenames"]:
                self.move_data(item)
            self.phase("archive-old-units")
            self.check_markers(approvals=False)
            self.stable_runtime()
            for path, value in self.plan["requiredFreshObservations"][
                "oldUnitMetadata"
            ].items():
                self.move_file(path, value["metadata"])
            for path in (inventory.UNITS, inventory.TARGET + "/staging.json"):
                self.move_file(path, actual["files"][path])
            self.move_candidates(actual["candidateTree"])
            self.archive_empty_unit_directories()
            self.phase("unload-old-units")
            for user in inventory.USERS:
                self.commands.user(
                    user, ["systemctl", "--user", "daemon-reload"], timeout=10
                )
            self.stable_runtime(unloaded=True)
            self.guard_proof(loaded=False)
            self.quadlet_inventory({})
            self.phase("stage-new-candidate")
            self.bound_candidates()
            self.directory(inventory.TARGET + "/candidates")
            new = self.fs.read_json(inventory.NEW + "/staging.json")
            for name, checksum in new["unitSHA256"].items():
                info, raw = self.fs.read(inventory.NEW + "/" + name)
                inventory.require(
                    info["sha256"] == checksum, "New candidate changed before staging"
                )
                self.write_new(
                    inventory.TARGET + "/candidates/" + name.removeprefix("units/"), raw
                )
            self.write_new(
                inventory.TARGET + "/staging.json",
                self.fs.read(inventory.NEW + "/staging.json")[1],
            )
            self.check_markers(approvals=False)
            self.stable_runtime(unloaded=True)
            self.preserved()
            self.record.update(
                status="prepared-fenced",
                phase="awaiting-fresh-source-proof",
                preparedAt=time.time(),
            )
            self.save()
            return {
                "status": self.record["status"],
                "journal": inventory.JOURNAL,
                "journalSHA256": self.journal_metadata["sha256"],
                "bothReverseFencesRetained": True,
                "applicationStartAttempted": False,
            }
        except BaseException:
            self.failure()
            raise

    def finalize(self, source_path, provider_path):
        inventory.host_identity("fredrir-09")
        self.commands = inventory.Commands(60)
        self.journal_metadata = self.fs.metadata(inventory.JOURNAL)
        self.record = self.fs.read_json(inventory.JOURNAL)
        inventory.require(
            self.record["schemaVersion"] == 1
            and self.record["kind"] == "evacuation-reset-execution"
            and self.record["planSHA256"] == inventory.PLAN_SHA
            and self.record["status"] == "prepared-fenced"
            and self.record["archive"] == self.archive
            and self.record["bootId"] == self.reverse["bootId"],
            "Exact completed fenced preparation required",
        )
        try:
            self.bound_candidates()
            inventory.require(
                Path("/proc/sys/kernel/random/boot_id").read_text().strip()
                == self.record["bootId"],
                "Target rebooted during reset",
            )
            self.check_markers(approvals=False)
            self.stable_runtime(unloaded=True)
            self.guard_proof(loaded=False)
            self.quadlet_inventory({})
            self.preserved()
            authority = self.source_authority(source_path, provider_path)
            self.record["sourceBeforeFinalize"] = authority
            self.record["nativeFinalizeLimits"] = self.native_limits
            self.phase("archive-reverse-fences")
            for path in self.plan["requiredFreshObservations"]["fences"]:
                self.move_file(path, self.record["initialMarkers"][path])
            self.check_markers(approvals=False, fences=False)
            self.phase("promote-new-inert-units")
            self.stable_runtime(unloaded=True)
            result = inventory.target.promote_units(
                inventory.NEW, self.guard, inventory.UNITS, self.commands
            )
            inventory.require(
                result["completed"] is True
                and result["candidateSHA256"] == self.plan["newCandidateSHA256"]
                and {path: value["sha256"] for path, value in result["files"].items()}
                == self.plan["newInstalledUnitHashes"],
                "Nine-file inert promotion differs",
            )
            self.record["newPromotionReceipt"] = self.fs.metadata(inventory.UNITS)
            self.phase("verify")
            self.check_markers(approvals=False, fences=False)
            self.stable_runtime()
            inventory.target.verify_unit_files(result)
            inventory.target.checkpoint_proof()
            self.guard_proof()
            self.data_snapshot(preserved=True)
            self.preserved(promoted=True)
            self.record.update(
                status="complete-inert",
                phase="complete",
                completedAt=time.time(),
                recurringBackupRefreshRequiredBeforeEnablement=True,
            )
            self.save()
            return {
                "status": self.record["status"],
                "journal": inventory.JOURNAL,
                "journalSHA256": self.journal_metadata["sha256"],
                "applicationStartAttempted": False,
                "recurringBackupRefreshRequiredBeforeEnablement": True,
            }
        except BaseException:
            self.failure()
            raise


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=("prepare", "finalize"))
    parser.add_argument("--source-proof")
    parser.add_argument("--provider-proof")
    parser.add_argument("--inventory")
    parser.add_argument("--check-bounds", action="store_true")
    args = parser.parse_args()
    inventory.require(
        INVENTORY_ROOT.is_dir(), "Frozen inventory bundle required for execution"
    )
    inventory.host_identity("fredrir-09")
    for number in (signal.SIGTERM, signal.SIGINT):
        signal.signal(number, interrupted)
    limits = native_limits(args.action)
    if args.check_bounds:
        print(json.dumps(limits))
        return
    inventory.require(
        args.source_proof
        and args.provider_proof
        and (args.action == "prepare") == (args.inventory is not None),
        "Fresh authority and action-specific inventory required",
    )
    with inventory.guards.operation_lock():
        reset = Executor()
        reset.native_limits = limits
        try:
            result = (
                reset.prepare(args.inventory, args.source_proof, args.provider_proof)
                if args.action == "prepare"
                else reset.finalize(args.source_proof, args.provider_proof)
            )
            print(json.dumps(result))
        except BaseException:
            print(
                json.dumps(
                    {
                        "status": "failed",
                        "journal": inventory.JOURNAL,
                        "recoveryRequired": reset.record is not None,
                        "phase": None
                        if reset.record is None
                        else reset.record["phase"],
                    }
                )
            )
            raise


if __name__ == "__main__":
    main()
