import hashlib
import json
import os
import re

from evacuation_guards import BASE, ROOTS, Filesystem

CONTENT = b"[Service]\nRestart=no\n"
FIELDS = (
    "LoadState",
    "ActiveState",
    "SubState",
    "MainPID",
    "Restart",
    "RestartForceExitStatus",
    "RestartPreventExitStatus",
    "DropInPaths",
    "FragmentPath",
    "NRestarts",
    "ExecMainStartTimestampMonotonic",
    "InvocationID",
    "Job",
)
STABLE = (
    "LoadState",
    "ActiveState",
    "SubState",
    "MainPID",
    "FragmentPath",
    "NRestarts",
    "ExecMainStartTimestampMonotonic",
    "InvocationID",
    "Job",
)


def require(value, message):
    if not value:
        raise ValueError(message)


def paths(execution, datastore):
    require(
        execution["host"] in ROOTS and datastore in ("valkey", "postgres"),
        "Fixed datastore host required",
    )
    nonce = execution["marker"]["executionID"]
    require(re.fullmatch(r"[a-f0-9]{32}", nonce), "Exact execution identity required")
    unit = "llunde-" + datastore + ".service"
    return (
        ROOTS[execution["host"]]["user"] + "/" + unit + ".d/96-evacuation-restart.conf",
        BASE + "/receipts/restart-" + nonce + "-" + datastore + ".json",
    )


def check_fence(execution):
    from evacuation_execution import MARKERS, host_identity, marker_value

    host_identity(execution["host"])
    for name in ("source-locked", "reconciliation-locked"):
        require(
            marker_value(MARKERS / name) == execution["marker"],
            "Restart policy requires its exact persistent fence",
        )


def unit_policy(commands, datastore):
    _, raw = commands.user(
        "llunde-backend",
        [
            "systemctl",
            "--user",
            "show",
            "--all",
            "llunde-" + datastore + ".service",
            "--property=" + ",".join(FIELDS),
        ],
    )
    rows = [line.split("=", 1) for line in raw.decode().splitlines()]
    require(all(len(row) == 2 for row in rows), "Complete restart policy required")
    value = dict(rows)
    require(
        set(value) == set(FIELDS) and len(rows) == len(value),
        "Exact restart policy projection required",
    )
    require(
        value["LoadState"] == "loaded" and value["Job"] in ("", "0"),
        "Loaded datastore without queued jobs required",
    )
    require(
        value["NRestarts"].isdigit()
        and value["MainPID"].isdigit()
        and value["ExecMainStartTimestampMonotonic"].isdigit(),
        "Numeric datastore process metadata required",
    )
    require(
        value["RestartForceExitStatus"] == value["RestartPreventExitStatus"] == "",
        "Unexpected restart exit-status override",
    )
    return value


def running_identity(commands, datastore):
    from evacuation_execution import process_identity

    template = '{"id":{{json .ID}},"running":{{json .State.Running}},"pid":{{json .State.Pid}}}'
    _, raw = commands.user(
        "llunde-backend",
        ["podman", "inspect", "--format", template, "llunde-" + datastore],
    )
    value = json.loads(raw)
    require(
        set(value) == {"id", "running", "pid"}
        and re.fullmatch(r"[a-f0-9]{64}", value["id"])
        and value["running"] is True
        and type(value["pid"]) is int
        and value["pid"] > 1,
        "Exact running datastore container required",
    )
    value["startIdentity"] = process_identity(value["pid"])
    require(value["startIdentity"] is not None, "Datastore process disappeared")
    return value


def reload_policy(commands):
    commands.user("llunde-backend", ["systemctl", "--user", "daemon-reload"])


def file_identity(info):
    return {key: info[key] for key in ("uid", "gid", "mode", "inode", "device")}


def validate_receipt(value, execution, datastore):
    path, receipt = paths(execution, datastore)
    require(
        value["schemaVersion"] == 1
        and value["kind"] == "evacuation-restart-inhibitor"
        and value["host"] == execution["host"]
        and value["marker"] == execution["marker"]
        and value["datastore"] == datastore
        and value["path"] == path
        and value["receipt"] == receipt
        and value["contentSHA256"] == hashlib.sha256(CONTENT).hexdigest(),
        "Restart receipt authority differs",
    )
    require(
        value["before"]["Restart"] == "always"
        and path not in value["before"]["DropInPaths"].split(),
        "Original restart policy differs",
    )
    return value


def loaded_inhibitor(commands, value):
    observed = unit_policy(commands, value["datastore"])
    require(
        observed["Restart"] == "no"
        and sorted(observed["DropInPaths"].split())
        == sorted(value["before"]["DropInPaths"].split() + [value["path"]]),
        "Exact loaded restart inhibitor required",
    )
    require(
        observed["FragmentPath"] == value["before"]["FragmentPath"],
        "Datastore fragment changed",
    )
    return observed


def install(commands, execution, datastore, *, _filesystem=None):
    check_fence(execution)
    fs = _filesystem or Filesystem()
    path, receipt = paths(execution, datastore)
    require(
        not fs.metadata(path)["exists"] and not fs.metadata(receipt)["exists"],
        "Existing restart policy state requires recovery",
    )
    before = unit_policy(commands, datastore)
    require(
        before["Restart"] == "always"
        and before["ActiveState"] == "active"
        and before["SubState"] == "running"
        and int(before["MainPID"]) > 1,
        "Stable active Restart=always datastore required",
    )
    require(
        path not in before["DropInPaths"].split(), "Restart override already loaded"
    )
    container = running_identity(commands, datastore)
    value = {
        "schemaVersion": 1,
        "kind": "evacuation-restart-inhibitor",
        "host": execution["host"],
        "marker": execution["marker"],
        "datastore": datastore,
        "path": path,
        "receipt": receipt,
        "contentSHA256": hashlib.sha256(CONTENT).hexdigest(),
        "before": before,
        "container": container,
        "status": "intent",
        "ownedFile": None,
    }
    fs.save(receipt, value)
    with fs.parent_fd(path) as (parent, name):
        descriptor = os.open(
            name,
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
            0o644,
            dir_fd=parent,
        )
        try:
            os.fchmod(descriptor, 0o644)
            os.fsync(parent)
            value["ownedFile"] = fs.metadata(path)
            value["status"] = "allocated"
            fs.save(receipt, value)
            require(
                os.write(descriptor, CONTENT) == len(CONTENT),
                "Incomplete restart policy write",
            )
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    value["ownedFile"] = fs.metadata(path)
    require(
        value["ownedFile"]["sha256"] == hashlib.sha256(CONTENT).hexdigest(),
        "Restart policy write differs",
    )
    value["status"] = "written"
    fs.save(receipt, value)
    reload_policy(commands)
    after = loaded_inhibitor(commands, value)
    require(
        all(after[key] == before[key] for key in STABLE)
        and running_identity(commands, datastore) == container,
        "Datastore changed while inhibiting restart",
    )
    check_fence(execution)
    value["status"] = "installed"
    value["after"] = after
    fs.save(receipt, value)
    return {"receipt": receipt, "container": container, "restart": "no"}


def verify(commands, execution, datastore, *, stopped_required=False, _filesystem=None):
    check_fence(execution)
    fs = _filesystem or Filesystem()
    path, receipt = paths(execution, datastore)
    value = validate_receipt(fs.read_json(receipt), execution, datastore)
    require(value["status"] == "installed", "Completed restart inhibition required")
    fs.metadata(path, value["ownedFile"])
    require(
        value["ownedFile"]["sha256"] == hashlib.sha256(CONTENT).hexdigest()
        and value["ownedFile"]["mode"] == "0644",
        "Restart inhibitor bytes differ",
    )
    observed = loaded_inhibitor(commands, value)
    require(
        int(observed["NRestarts"]) <= int(value["before"]["NRestarts"])
        and observed["ExecMainStartTimestampMonotonic"]
        in ("0", value["before"]["ExecMainStartTimestampMonotonic"]),
        "Datastore restarted during fence",
    )
    if stopped_required:
        require(
            observed["ActiveState"] in ("inactive", "failed")
            and observed["SubState"] in ("dead", "failed")
            and observed["MainPID"] == "0",
            "Datastore must remain stopped",
        )
        code, _ = commands.user(
            "llunde-backend",
            ["podman", "container", "exists", "llunde-" + datastore],
            check=False,
        )
        require(code == 1, "Replacement or retained datastore container exists")
    else:
        require(
            all(observed[key] == value["before"][key] for key in STABLE)
            and running_identity(commands, datastore) == value["container"],
            "Datastore identity changed before native shutdown",
        )
    return value


def restore(commands, execution, datastore, *, _filesystem=None):
    check_fence(execution)
    fs = _filesystem or Filesystem()
    path, receipt = paths(execution, datastore)
    if not fs.metadata(receipt)["exists"]:
        require(
            not fs.metadata(path)["exists"],
            "Unowned restart inhibitor cannot be removed",
        )
        return {"datastore": datastore, "changed": False}
    value = validate_receipt(fs.read_json(receipt), execution, datastore)
    require(
        value["status"]
        in (
            "intent",
            "allocated",
            "written",
            "installed",
            "restoring",
            "removed",
            "restored",
        ),
        "Unknown restart recovery phase",
    )
    before = unit_policy(commands, datastore)
    expected_paths = sorted(value["before"]["DropInPaths"].split())
    require(
        before["FragmentPath"] == value["before"]["FragmentPath"]
        and (before["Restart"], sorted(before["DropInPaths"].split()))
        in (("always", expected_paths), ("no", sorted(expected_paths + [path]))),
        "Loaded policy changed before recovery",
    )
    identity = (
        running_identity(commands, datastore)
        if before["ActiveState"] == "active"
        else None
    )
    require(
        before["ActiveState"] in ("active", "inactive", "failed")
        and before["SubState"] in ("running", "dead", "failed"),
        "Transient datastore state prevents restart-policy restoration",
    )
    info, data = fs.read(path)
    if info["exists"]:
        require(
            value["ownedFile"] is not None
            and file_identity(info) == file_identity(value["ownedFile"])
            and info["mode"] == "0644",
            "Restart file ownership changed",
        )
        if (
            value["status"] == "allocated"
            or value["status"] == "restoring"
            and info == value["ownedFile"]
        ):
            require(
                CONTENT.startswith(data), "Interrupted restart file has unrelated bytes"
            )
        else:
            require(data == CONTENT, "Restart file content changed")
        value["status"] = "restoring"
        value["ownedFile"] = info
        fs.save(receipt, value)
        fs.unlink(path, info)
        value["status"] = "removed"
        fs.save(receipt, value)
    else:
        require(
            value["status"] in ("intent", "restoring", "removed", "restored"),
            "Restart file disappeared without receipt-owned removal",
        )
    reload_policy(commands)
    after = unit_policy(commands, datastore)
    require(
        after["Restart"] == value["before"]["Restart"]
        and sorted(after["DropInPaths"].split())
        == sorted(value["before"]["DropInPaths"].split())
        and all(after[key] == before[key] for key in STABLE),
        "Original loaded restart policy or datastore identity differs",
    )
    require(
        identity is None or running_identity(commands, datastore) == identity,
        "Running datastore changed while restoring policy",
    )
    check_fence(execution)
    value["status"] = "restored"
    value["restoredPolicy"] = after
    fs.save(receipt, value)
    return {
        "datastore": datastore,
        "changed": info["exists"],
        "receipt": receipt,
        "restart": after["Restart"],
    }


def restore_all(commands, execution):
    return [restore(commands, execution, name) for name in ("postgres", "valkey")]
