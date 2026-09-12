import argparse
import hashlib
import json
import math
import os
import stat
import time
from contextlib import contextmanager
from pathlib import Path

import evacuation_guards as guards
import evacuation_restart as restart
import evacuation_target as target
from evacuation_execution import (
    Commands,
    canonical,
    host_identity,
    require,
    stopped,
    unit_state,
)
from evacuation_finalize import provider_authority
from evacuation_preflight import quadlet_paths
from evacuation_staging import USERS, validate_plan

ROOT = "/root/infra-evacuation-reset-c"
PLAN = ROOT + "/plan.json"
PLAN_SHA = "45d60a841110315e7485b4dec4c511ef27f3bbd44f6708ee856b5fb7adb9a7d1"
OLD = "/root/infra-evacuation-cutover-v3/candidate"
NEW = ROOT + "/candidate"
REVERSE = "/var/lib/platform-evacuation/runs/reverse-20260912c/execution.json"
GUARDS = "/var/lib/platform-evacuation/receipts/guards.json"
UNITS = "/var/lib/platform-evacuation/receipts/unit-promotion.json"
JOURNAL = (
    "/var/lib/platform-evacuation/receipts/reset-3ef3868b5609ccbe2dabc3ece116ebf7.json"
)
DATA = "/home/llunde-backend/data"
TARGET = "/var/lib/infra-evacuation/llunde"
FIELDS = (
    "LoadState",
    "ActiveState",
    "SubState",
    "MainPID",
    "Job",
    "FragmentPath",
    "DropInPaths",
)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def directory_info(info):
    return {
        "kind": "directory",
        "uid": info.st_uid,
        "gid": info.st_gid,
        "mode": format(stat.S_IMODE(info.st_mode), "04o"),
        "inode": info.st_ino,
        "device": info.st_dev,
    }


def fresh(value, now):
    require(
        type(value) in (int, float) and math.isfinite(value) and 0 <= now - value <= 30,
        "Fresh observation within thirty seconds required",
    )


def reconciler_policy(name, state, properties):
    require(stopped(state), "Root reconciliation must remain stopped")
    allowed = ("", "disabled")
    if (
        name == "restic-backups-llunde-backend.service"
        and state["LoadState"] == "loaded"
    ):
        allowed += ("static",)
    require(
        set(properties) == {"UnitFileState", "Job"}
        and properties["Job"] in ("", "0")
        and properties["UnitFileState"] in allowed,
        "Reconciler enablement or job changed",
    )
    return {"LoadState": state["LoadState"], **properties}


class Reset:
    def __init__(self, *, filesystem=None, commands=None):
        self.fs = filesystem or guards.Filesystem()
        self.commands = commands or Commands(60)
        info, raw = self.fs.read(PLAN)
        require(
            info["exists"] and info["mode"] == "0600" and digest(raw) == PLAN_SHA,
            "Pinned reset plan required",
        )
        self.plan = json.loads(raw)
        self.reverse = self.fs.read_json(REVERSE)
        require(
            self.reverse["marker"]["executionID"] == self.plan["reverseExecutionID"]
            and self.reverse["pairManifestSHA256"]
            == self.plan["reversePairManifestSHA256"],
            "Reverse execution differs",
        )
        self.archive = self.plan["archive"]
        self.guard = self.fs.read_json(GUARDS)
        self.record = None

    def bound_candidates(self):
        for directory, field, legacy in (
            (OLD, "oldCandidateSHA256", True),
            (NEW, "newCandidateSHA256", False),
        ):
            info, raw = self.fs.read(directory + "/staging.json")
            require(
                info["mode"] == "0600" and digest(raw) == self.plan[field],
                "Candidate binding changed",
            )
            plan = validate_plan(self.fs.path(directory), legacy_networks=legacy)
            for name, checksum in plan["unitSHA256"].items():
                info, raw = self.fs.read(directory + "/" + name)
                require(
                    info["mode"] == "0600" and digest(raw) == checksum,
                    "Candidate file ownership or bytes changed",
                )

    @contextmanager
    def data_parent(self):
        fd = os.open(self.fs.root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            for name, owner, mode in (
                (None, self.fs.owner, None),
                ("home", self.fs.owner, None),
                ("llunde-backend", 2001, None),
                ("data", 2001, 0o700),
            ):
                if name is not None:
                    child = os.open(
                        name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd
                    )
                    os.close(fd)
                    fd = child
                info = os.fstat(fd)
                require(
                    info.st_uid == owner
                    and info.st_gid == (0 if owner == self.fs.owner else owner)
                    and not info.st_mode & 0o022
                    and (mode is None or stat.S_IMODE(info.st_mode) == mode),
                    "Unsafe data ancestry",
                )
            yield fd
        finally:
            os.close(fd)

    def data_snapshot(self, preserved=False):
        result = {}
        with self.data_parent() as parent:
            for item in self.plan["dataRenames"]:
                source = item["destination"] if preserved else item["source"]
                other = item["source"] if preserved else item["destination"]
                info = os.stat(Path(source).name, dir_fd=parent, follow_symlinks=False)
                require(
                    stat.S_ISDIR(info.st_mode)
                    and directory_info(info) == item["expected"],
                    "Stopped data identity changed",
                )
                try:
                    os.stat(Path(other).name, dir_fd=parent, follow_symlinks=False)
                except FileNotFoundError:
                    pass
                else:
                    raise ValueError(
                        "Data preservation destination or canonical data already exists"
                    )
                result[source] = directory_info(info)
        return result

    def tree_snapshot(self, path, expected):
        result = {}
        directory_names = {
            str(Path(name).parent) for name in expected if str(Path(name).parent) != "."
        }
        with self.fs.directory_fd(path):
            pass
        for parent, directories, files in os.walk(
            self.fs.path(path), followlinks=False
        ):
            require(
                len(result) + len(directories) + len(files) <= 20,
                "Candidate tree exceeds exact inventory",
            )
            for name in directories + files:
                physical = Path(parent) / name
                relative = str(physical.relative_to(self.fs.path(path)))
                absolute = path + "/" + relative
                require(not physical.is_symlink(), "Candidate tree symlink forbidden")
                if name in directories:
                    require(
                        relative in directory_names, "Unexpected candidate directory"
                    )
                    with self.fs.directory_fd(absolute) as fd:
                        info = directory_info(os.fstat(fd))
                    require(
                        info["mode"] == "0700", "Private candidate directory required"
                    )
                    result[relative] = info
                else:
                    require(relative in expected, "Unexpected candidate file")
                    info = self.fs.metadata(absolute)
                    require(
                        info["mode"] == "0600" and info["sha256"] == expected[relative],
                        "Installed inert candidate drift",
                    )
                    result[relative] = info
        require(
            set(result) == set(expected) | directory_names,
            "Candidate inventory incomplete",
        )
        return result

    def runtime(self, *, unloaded=False):
        result = {"units": {}, "managers": {}, "timers": {}}
        for user, uid in USERS.items():
            manager = unit_state(self.commands, f"user@{uid}.service")
            require(
                manager["ActiveState"] == "active" and int(manager["MainPID"]) > 1,
                "Existing active user manager required",
            )
            result["managers"][user] = {"MainPID": manager["MainPID"]}
            _, linger = self.commands.run(
                ["loginctl", "show-user", user, "--property=Linger", "--value"],
                timeout=5,
            )
            require(linger.strip() == b"no", "Dormant user linger required")
            _, containers = self.commands.user(
                user, ["podman", "ps", "--all", "--format", "{{.Names}}"], timeout=5
            )
            require(
                not containers.strip(), "All service-user containers must remain absent"
            )
            _, jobs = self.commands.user(
                user,
                ["systemctl", "--user", "list-jobs", "--no-legend", "--no-pager"],
                timeout=5,
            )
            require(not jobs.strip(), "User manager has queued jobs")
            for service, account in target.SERVICE_USERS.items():
                if account != user:
                    continue
                _, raw = self.commands.user(
                    user,
                    [
                        "systemctl",
                        "--user",
                        "show",
                        "--all",
                        service + ".service",
                        "--property=" + ",".join(FIELDS),
                    ],
                    timeout=5,
                )
                rows = [line.split("=", 1) for line in raw.decode().splitlines()]
                value = dict(rows)
                require(
                    len(rows) == len(value)
                    and set(value) == set(FIELDS)
                    and stopped(value)
                    and value["Job"] in ("", "0"),
                    "Application is active or queued",
                )
                if unloaded:
                    require(
                        value["LoadState"] == "not-found"
                        and value["FragmentPath"] == "",
                        "Old application fragment remains loaded",
                    )
                result["units"][service] = value
            for suffix in ("service", "timer"):
                name = "llunde-auto-update." + suffix
                value = unit_state(self.commands, name, user)
                require(stopped(value), "User reconciler must remain stopped")
        for name in target.cutover.SYSTEM_RECONCILERS:
            value = unit_state(self.commands, name)
            _, raw = self.commands.run(
                ["systemctl", "show", "--all", name, "--property=UnitFileState,Job"],
                timeout=5,
            )
            properties = dict(line.split("=", 1) for line in raw.decode().splitlines())
            result["timers"][name] = reconciler_policy(name, value, properties)
        _, listeners = self.commands.run(
            [
                "ss",
                "-H",
                "-ltn",
                "sport = :8080 or sport = :8081 or sport = :8085 or sport = :9101",
            ],
            timeout=5,
        )
        require(not listeners.strip(), "Application listeners remain")
        return result

    def guard_proof(self, loaded=True):
        result = guards.verify_installation(self.guard, require_loaded=loaded)
        require(
            result["host"] == "fredrir-09"
            and result["guardFilesSHA256"]
            == self.plan["requiredFreshObservations"]["target"].get(
                "guardFilesSHA256",
                "b3ac6addbf977da233685874c962edfc48e31e6723eaf38dc2aeb6af5e86b902",
            ),
            "Guard binding changed",
        )
        return result["guardFilesSHA256"]

    def check_markers(self, approvals=True, fences=True):
        expected = self.plan["requiredFreshObservations"]
        result = {}
        for names, present in (("markers", approvals), ("fences", fences)):
            for path, value in expected[names].items():
                info, raw = self.fs.read(path)
                if present:
                    require(
                        info["exists"]
                        and info["uid"] == info["gid"] == 0
                        and info["mode"] == "0644"
                        and json.loads(raw) == value,
                        "Marker identity changed",
                    )
                else:
                    require(not info["exists"], "Archived marker reappeared")
                result[path] = info
        require(
            not self.fs.metadata(expected["edgeApprovalMustBeAbsent"])["exists"],
            "Unexpected connector approval",
        )
        return result

    def quadlet_inventory(self, expected):
        observed = {}
        for root in quadlet_paths():
            physical = self.fs.path(root)
            if not physical.exists():
                require(not physical.is_symlink(), "Quadlet search root symlink")
                continue
            with self.fs.directory_fd(root):
                pass
            for parent, directories, files in os.walk(physical, followlinks=False):
                for name in directories + files:
                    path = Path(parent) / name
                    absolute = "/" + str(path.relative_to(self.fs.root))
                    require(not path.is_symlink(), "Unexpected Quadlet symlink")
                    if name in directories:
                        require(
                            any(item.startswith(absolute + "/") for item in expected),
                            "Unexpected Quadlet directory",
                        )
                    else:
                        require(absolute in expected, "Unexpected Quadlet file")
                        observed[absolute] = self.fs.metadata(absolute)
                        require(len(observed) <= 9, "Quadlet inventory exceeds bound")
        require(
            set(observed)
            == {name for name in expected if name.endswith((".container", ".network"))},
            "Quadlet inventory differs",
        )

    def inventory(self):
        host_identity("fredrir-09")
        self.bound_candidates()
        require(
            Path("/proc/sys/kernel/random/boot_id").read_text().strip()
            == self.reverse["bootId"],
            "Target rebooted after reverse fence",
        )
        self.guard_proof()
        unit_receipt = self.fs.read_json(UNITS)
        expected = self.plan["requiredFreshObservations"]
        require(
            unit_receipt["files"] == expected["oldUnitMetadata"]
            and unit_receipt["completed"] is True
            and unit_receipt.get("rolledBack") is not True,
            "Old promotion receipt changed",
        )
        files = {}
        for path, value in expected["oldUnitMetadata"].items():
            files[path] = self.fs.metadata(path, value["metadata"])
        for path, checksum in (
            (TARGET + "/staging.json", expected["installedStagingSHA256"]),
        ):
            files[path] = self.fs.metadata(path)
            require(
                files[path]["mode"] == "0600" and files[path]["sha256"] == checksum,
                "Installed metadata differs",
            )
        files[UNITS] = self.fs.metadata(UNITS)
        self.quadlet_inventory(expected["oldUnitMetadata"])
        inhibitors = {}
        for item in expected["restartInhibitors"]:
            receipt = restart.verify(
                self.commands,
                self.reverse,
                item["datastore"],
                stopped_required=True,
                _filesystem=self.fs,
            )
            inhibitors[item["datastore"]] = {
                "file": self.fs.metadata(item["path"]),
                "receipt": self.fs.metadata(item["receipt"]),
                "policy": restart.loaded_inhibitor(self.commands, receipt),
            }
        require(
            not self.fs.path(self.archive).exists()
            and not self.fs.path(self.archive).is_symlink()
            and not self.fs.metadata(JOURNAL)["exists"],
            "Existing reset state needs explicit recovery",
        )
        result = {
            "schemaVersion": 1,
            "kind": "evacuation-reset-inventory",
            "host": "fredrir-09",
            "planSHA256": PLAN_SHA,
            "bootId": self.reverse["bootId"],
            "observedAt": time.time(),
            "files": files,
            "markers": self.check_markers(),
            "data": self.data_snapshot(),
            "inhibitors": inhibitors,
            "candidateTree": self.tree_snapshot(
                TARGET + "/candidates", expected["installedCandidates"]
            ),
            "runtime": self.runtime(),
            "recurringBackupRefresh": {
                "requiredBeforeTimerEnablement": True,
                "requiredCandidateSHA256": self.plan["newCandidateSHA256"],
                "existingService": self.fs.metadata(
                    "/etc/systemd/system/restic-backups-llunde-backend.service"
                ),
                "preserveDuringReset": True,
                "requiredAcceptance": "Rebuild and redeploy candidate-bound helpers; prove installed-service backup and independent restore before enabling recurrence",
            },
        }
        return result

    def source_authority(self, source_path, provider_path):
        source = self.fs.read_json(source_path)
        provider = self.fs.read_json(provider_path)
        now = time.time()
        require(
            source["schemaVersion"] == 1
            and source["kind"] == "evacuation-reset-source-authority"
            and source["host"] == "fredrir-05"
            and source["planSHA256"] == PLAN_SHA,
            "Exact source authority required",
        )
        fresh(source["observedAt"], now)
        require(
            source["sourceFenceAbsent"] is True
            and source["reconciliationFenceAbsent"] is True
            and source["privateAcceptance"]["connectorActive"] is True,
            "Source recovery is no longer authoritative",
        )
        require(
            set(source["unitStates"]) == set(target.SERVICE_USERS)
            and all(
                value["ActiveState"] == "active" and int(value["MainPID"]) > 1
                for value in source["unitStates"].values()
            ),
            "All original source applications must be active",
        )
        require(
            set(source["privateAcceptance"]["checks"])
            == {"health", "ready", "frontend", "proxy"}
            and all(
                value["status"] == 200
                for value in source["privateAcceptance"]["checks"].values()
            ),
            "Private source acceptance failed",
        )
        authority = provider_authority(provider, "fredrir-05", now)
        fresh(authority["lastObservedAt"], now)
        return {
            "sourceSHA256": digest(canonical(source)),
            "providerSHA256": digest(canonical(provider)),
            "observedAt": source["observedAt"],
            "provider": authority,
        }


def source_proof():
    host_identity("fredrir-05")
    commands = Commands(25)
    markers = {}
    for name in ("source-locked", "reconciliation-locked"):
        path = target.MARKERS / name
        require(not path.exists() and not path.is_symlink(), "Source is fenced")
        markers[name] = True
    states = {
        service: unit_state(commands, service + ".service", user)
        for service, user in target.SERVICE_USERS.items()
    }
    require(
        all(
            value["ActiveState"] == "active" and int(value["MainPID"]) > 1
            for value in states.values()
        ),
        "Source application is not active",
    )
    acceptance = target.private_acceptance(commands, seconds=10, connector_active=True)
    for name in markers:
        path = target.MARKERS / name
        require(not path.exists() and not path.is_symlink(), "Source fence changed")
    return {
        "schemaVersion": 1,
        "kind": "evacuation-reset-source-authority",
        "host": "fredrir-05",
        "planSHA256": PLAN_SHA,
        "observedAt": time.time(),
        "sourceFenceAbsent": True,
        "reconciliationFenceAbsent": True,
        "unitStates": states,
        "privateAcceptance": acceptance,
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=("inventory", "source-proof"))
    args = parser.parse_args()
    result = Reset().inventory() if args.action == "inventory" else source_proof()
    print(json.dumps(result))


if __name__ == "__main__":
    main()
