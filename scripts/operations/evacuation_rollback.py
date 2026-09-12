import argparse
import hashlib
import json
import math
import os
import re
import signal
import subprocess
import time
from pathlib import Path

import evacuation_cutover as cutover
import evacuation_restart as restart
from evacuation_execution import (
    MARKERS,
    TARGET_BASE,
    Commands,
    all_export_processes,
    archive_processes,
    backup_gate,
    canonical,
    durable_json,
    graceful_exit_event,
    guard_state,
    host_identity,
    live_fence,
    marker_value,
    postgres_clients,
    require,
    stopped,
    unit_state,
    wait_stopped,
)
from evacuation_images import read_json
from evacuation_target import DATA_PARENT, private_acceptance, promoted_data


def destination_observation(guard_receipt, commands=None, reverse_execution=None):
    host_identity("fredrir-09")
    guards = guard_state(guard_receipt)
    require(guards["host"] == "fredrir-09", "Destination guard identity differs")
    commands = commands or Commands(60)
    states = {}
    for user, units in cutover.APP_UNITS.items():
        states[user] = {}
        for unit in units:
            value = unit_state(commands, unit, user)
            require(
                stopped(value),
                "Destination application, connector and data must be stopped",
            )
            states[user][unit] = value
        _, containers = commands.user(
            user, ["podman", "ps", "--all", "--format", "{{.Names}}"]
        )
        require(not containers.strip(), "Destination containers remain")
    attempted = (TARGET_BASE / "target-writer-start-attempted").exists()
    require(
        not (TARGET_BASE / "target-writer-start-attempted").is_symlink(),
        "Writer-attempt marker symlink forbidden",
    )
    marker = (
        marker_value(TARGET_BASE / "target-writer-start-attempted")
        if attempted
        else None
    )
    if attempted:
        require(
            marker["writerStartAttempted"] is True, "Persistent writer attempt differs"
        )
        require(
            reverse_execution is not None
            and reverse_execution["schemaVersion"] == 1
            and reverse_execution["kind"] == "evacuation-execution-fence"
            and reverse_execution["host"] == "fredrir-09"
            and reverse_execution["direction"] == "reverse"
            and reverse_execution["phase"] == "pair-sealed"
            and reverse_execution["candidateSHA256"] == marker["candidateSHA256"],
            "Exact sealed reverse execution required",
        )
        for name in ("source-locked", "reconciliation-locked"):
            require(
                marker_value(MARKERS / name) == reverse_execution["marker"],
                "Current reverse fence belongs to another execution",
            )
        require(
            Path("/proc/sys/kernel/random/boot_id").read_text().strip()
            == reverse_execution["bootId"],
            "Reverse source rebooted",
        )
    else:
        for name in (
            "stage-approved",
            "restore-approved",
            "source-fenced",
            "edge-approved",
        ):
            require(
                not (TARGET_BASE / name).exists()
                and not (TARGET_BASE / name).is_symlink(),
                "Checkpoint without a writer-attempt marker requires inspection",
            )
    return {
        "schemaVersion": 1,
        "kind": "evacuation-destination-stopped",
        "host": "fredrir-09",
        "hostname": cutover.HOSTS["fredrir-09"],
        "observedAt": time.time(),
        "bootId": Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
        "guardFilesSHA256": guards["guardFilesSHA256"],
        "writerStartAttempted": attempted,
        "writerMarker": marker,
        "persistentFenceVerified": attempted,
        "reversePairManifestSHA256": reverse_execution["pairManifestSHA256"]
        if attempted
        else None,
        "reverseFenceMarkerSHA256": hashlib.sha256(
            canonical(reverse_execution["marker"])
        ).hexdigest()
        if attempted
        else None,
        "allApplicationAndDataUnitsInactive": True,
        "allServiceUserContainersAbsent": True,
        "unitStates": states,
    }


def validate_destination(value, candidate_sha, now=None):
    require(
        value["schemaVersion"] == 1
        and value["kind"] == "evacuation-destination-stopped"
        and value["host"] == "fredrir-09"
        and value["hostname"] == cutover.HOSTS["fredrir-09"],
        "Exact destination observation required",
    )
    require(
        type(value["writerStartAttempted"]) is bool
        and value["allApplicationAndDataUnitsInactive"] is True
        and value["allServiceUserContainersAbsent"] is True,
        "Stopped destination proof required",
    )
    elapsed = (time.time() if now is None else now) - value["observedAt"]
    require(
        math.isfinite(elapsed) and 0 <= elapsed <= 30,
        "Fresh destination observation required",
    )
    if value["writerStartAttempted"]:
        require(
            value["persistentFenceVerified"] is True
            and value["writerMarker"]["writerStartAttempted"] is True
            and value["writerMarker"]["candidateSHA256"] == candidate_sha,
            "Written destination requires its persistent fence",
        )
        require(
            all(
                re.fullmatch(r"[a-f0-9]{64}", value[key])
                for key in ("reversePairManifestSHA256", "reverseFenceMarkerSHA256")
            ),
            "Exact reverse pair and persistent execution fence required",
        )
    else:
        require(value["writerMarker"] is None, "Unexpected writer marker")
    return value["writerStartAttempted"]


def settle_destination(guard_receipt, execution, output, commands=None):
    host_identity("fredrir-09")
    require(
        execution["host"] == "fredrir-09"
        and execution["direction"] == "reverse"
        and execution["phase"] == "pair-sealed",
        "Sealed reverse export required before stopping destination data",
    )
    commands = commands or Commands(90)
    live_fence(commands, guard_receipt, execution)
    record = {
        "schemaVersion": 1,
        "kind": "evacuation-settle-destination",
        "pairManifestSHA256": execution["pairManifestSHA256"],
        "startedAt": time.time(),
        "completed": False,
        "forcedPersistenceStop": False,
    }
    durable_json(output, record)
    for user, unit in [
        ("edge", "caddy.service"),
        ("llunde-frontend", "llunde-frontend.service"),
    ]:
        commands.user(user, ["systemctl", "--user", "stop", "--no-block", unit])
        wait_stopped(commands, unit, user)
    require(
        postgres_clients(commands) == 0, "PostgreSQL clients remain before shutdown"
    )
    record["postgresRestartInhibitor"] = restart.install(
        commands, execution, "postgres"
    )
    durable_json(output, record, replace=True)
    restart.verify(commands, execution, "postgres")
    record["postgresShutdownRequestedAt"] = time.time()
    durable_json(output, record, replace=True)
    record["postgresShutdownClientStatus"], _ = commands.user(
        "llunde-backend",
        [
            "podman",
            "exec",
            "llunde-postgres",
            "pg_ctl",
            "--pgdata=/var/lib/postgresql/data/pgdata",
            "--mode=fast",
            "--wait",
            "--timeout=30",
            "stop",
        ],
        timeout=35,
        check=False,
    )
    wait_stopped(commands, "llunde-postgres.service", "llunde-backend", successful=True)
    restart.verify(commands, execution, "postgres", stopped_required=True)
    record["postgresExitEvent"] = graceful_exit_event(
        commands,
        record["postgresRestartInhibitor"]["container"]["id"],
        record["postgresShutdownRequestedAt"],
        container_name="llunde-postgres",
    )
    record["observation"] = destination_observation(guard_receipt, commands, execution)
    record["completed"] = True
    durable_json(output, record, replace=True)
    return record


def settle_source(guard_receipt, execution, output, commands=None):
    host_identity("fredrir-05")
    require(
        execution["host"] == "fredrir-05"
        and execution["direction"] == "forward"
        and execution["phase"] == "pair-sealed",
        "Original sealed source execution required",
    )
    commands = commands or Commands(90)
    live_fence(commands, guard_receipt, execution)
    record = {
        "schemaVersion": 1,
        "kind": "evacuation-settle-original-source",
        "host": "fredrir-05",
        "pairManifestSHA256": execution["pairManifestSHA256"],
        "candidateSHA256": execution["candidateSHA256"],
        "completed": False,
        "forcedPersistenceStop": False,
    }
    durable_json(output, record)
    for user, unit in [
        ("edge", "caddy.service"),
        ("llunde-frontend", "llunde-frontend.service"),
    ]:
        commands.user(user, ["systemctl", "--user", "stop", "--no-block", unit])
        wait_stopped(commands, unit, user)
    require(postgres_clients(commands) == 0, "Source PostgreSQL clients remain")
    record["postgresRestartInhibitor"] = restart.install(
        commands, execution, "postgres"
    )
    durable_json(output, record, replace=True)
    restart.verify(commands, execution, "postgres")
    record["postgresShutdownRequestedAt"] = time.time()
    durable_json(output, record, replace=True)
    record["postgresShutdownClientStatus"], _ = commands.user(
        "llunde-backend",
        [
            "podman",
            "exec",
            "llunde-postgres",
            "pg_ctl",
            "--pgdata=/var/lib/postgresql/data/pgdata",
            "--mode=fast",
            "--wait",
            "--timeout=30",
            "stop",
        ],
        timeout=35,
        check=False,
    )
    wait_stopped(commands, "llunde-postgres.service", "llunde-backend", successful=True)
    restart.verify(commands, execution, "postgres", stopped_required=True)
    record["postgresExitEvent"] = graceful_exit_event(
        commands,
        record["postgresRestartInhibitor"]["container"]["id"],
        record["postgresShutdownRequestedAt"],
        container_name="llunde-postgres",
    )
    for user, units in cutover.APP_UNITS.items():
        for unit in units:
            require(
                stopped(unit_state(commands, unit, user)),
                "Original source application or data remains active",
            )
        _, raw = commands.user(
            user, ["podman", "ps", "--all", "--format", "{{.Names}}"]
        )
        require(not raw.strip(), "Original source containers remain")
    for name in ("source-locked", "reconciliation-locked"):
        require(
            marker_value(MARKERS / name) == execution["marker"],
            "Original source fence changed",
        )
    guard_state(guard_receipt)
    record["completed"] = True
    durable_json(output, record, replace=True)
    return record


def remove_owned_marker(name, expected):
    require(
        name in ("source-locked", "reconciliation-locked"),
        "Fixed fence marker required",
    )
    from evacuation_guards import Filesystem

    fs = Filesystem()
    path = str(MARKERS / name)
    metadata, data = fs.read(path)
    require(
        metadata["mode"] == "0644" and json.loads(data) == expected,
        "Fence marker belongs to another execution",
    )
    fs.unlink(path, metadata)


def resume_source_writer(
    guard_receipt,
    execution,
    destination,
    output,
    *,
    reverse_pair=None,
    restore_receipt=None,
    backup_receipt=None,
    independent_restore=None,
    commands=None,
):
    host_identity("fredrir-05")
    require(
        execution["schemaVersion"] == 1
        and execution["kind"] == "evacuation-execution-fence"
        and execution["host"] == "fredrir-05"
        and execution["direction"] == "forward",
        "Original source execution required",
    )
    attempted = validate_destination(destination, execution["candidateSHA256"])
    guards = guard_state(guard_receipt)
    require(guards["host"] == "fredrir-05", "Original source guards required")
    for name in ("source-locked", "reconciliation-locked"):
        require(
            marker_value(MARKERS / name) == execution["marker"],
            "Original source fence changed",
        )
    commands = commands or Commands(180)
    require(
        stopped(unit_state(commands, "llunde-backend.service", "llunde-backend"))
        and stopped(unit_state(commands, "cloudflared.service", "edge")),
        "Source must remain stopped before rollback",
    )
    require(
        not archive_processes(execution["exportApplicationName"]),
        "Namespace archive export remains active",
    )
    if not stopped(unit_state(commands, "llunde-postgres.service", "llunde-backend")):
        require(
            not all_export_processes(commands, execution["exportApplicationName"])
            and postgres_clients(commands) == 0,
            "Native export or database client remains",
        )
    recovery = {
        "kind": "original-source-data",
        "targetNeverAttemptedWriter": not attempted,
    }
    if attempted:
        require(
            reverse_pair is not None
            and restore_receipt is not None
            and backup_receipt is not None
            and independent_restore is not None,
            "Fresh reverse copy, native promotion and independent backup restore are mandatory",
        )
        manifest = cutover.verify_pair(reverse_pair)
        require(
            manifest["direction"] == "reverse"
            and manifest["destination"] == "fredrir-05"
            and manifest["candidateSHA256"] == execution["candidateSHA256"],
            "Exact reverse recovery pair required",
        )
        require(
            manifest["fenceAfter"]["bootId"] == destination["bootId"]
            and manifest["fenceAfter"]["guardFilesSHA256"]
            == destination["guardFilesSHA256"],
            "Destination fence changed after reverse copy",
        )
        require(
            hashlib.sha256(canonical(manifest)).hexdigest()
            == destination["reversePairManifestSHA256"]
            and manifest["fenceAfter"]["executionMarkerSHA256"]
            == destination["reverseFenceMarkerSHA256"],
            "A different reverse pair or execution cannot resume the source",
        )
        promoted_data(manifest, restore_receipt)
        recovery = backup_gate(reverse_pair, backup_receipt, independent_restore) | {
            "kind": "fresh-reverse-copy"
        }
    else:
        require(
            reverse_pair is None and restore_receipt is None,
            "Original-data recovery must not mix reverse state",
        )
        for name, value in execution["originalData"].items():
            path = DATA_PARENT / name
            require(
                not path.is_symlink()
                and path.is_dir()
                and (path.stat().st_dev, path.stat().st_ino)
                == (value["device"], value["inode"]),
                "Original source data directory changed",
            )
    record = {
        "schemaVersion": 1,
        "kind": "evacuation-source-writer-resume",
        "host": "fredrir-05",
        "candidateSHA256": execution["candidateSHA256"],
        "recovery": recovery,
        "writerResumeAttempted": True,
        "connectorResumeAttempted": False,
        "privateAcceptancePassed": False,
        "startedAt": time.time(),
        "authenticatedUserSessionVerified": False,
    }
    durable_json(output, record)
    validate_destination(destination, execution["candidateSHA256"])
    if execution.get("restartInhibitorsRequired"):
        record["restartPoliciesRestored"] = restart.restore_all(commands, execution)
        durable_json(output, record, replace=True)
        validate_destination(destination, execution["candidateSHA256"])
    remove_owned_marker("source-locked", execution["marker"])
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
    record["privateAcceptance"] = private_acceptance(commands)
    record["privateAcceptancePassed"] = True
    durable_json(output, record, replace=True)
    return record


def resume_source_connector(
    execution, writer_receipt, destination, output, commands=None
):
    host_identity("fredrir-05")
    require(
        writer_receipt["kind"] == "evacuation-source-writer-resume"
        and writer_receipt["privateAcceptancePassed"] is True
        and writer_receipt["candidateSHA256"] == execution["candidateSHA256"],
        "Successful source writer recovery required",
    )
    validate_destination(destination, execution["candidateSHA256"])
    require(
        not (MARKERS / "source-locked").exists()
        and marker_value(MARKERS / "reconciliation-locked") == execution["marker"],
        "Source recovery fence state changed",
    )
    commands = commands or Commands(60)
    require(
        stopped(unit_state(commands, "cloudflared.service", "edge")),
        "Source connector already active",
    )
    private_acceptance(commands, seconds=15)
    validate_destination(destination, execution["candidateSHA256"])
    record = {
        "schemaVersion": 1,
        "kind": "evacuation-source-connector-resume",
        "host": "fredrir-05",
        "candidateSHA256": execution["candidateSHA256"],
        "connectorResumeAttempted": True,
        "connectorActive": False,
        "destinationObservedAt": destination["observedAt"],
        "providerConnectorInventoryVerified": False,
        "publicApplicationVerified": False,
    }
    durable_json(output, record)
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
    require(record["connectorActive"], "Source connector did not become active")
    return record


def main():
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)
    observe = sub.add_parser("observe-destination")
    observe.add_argument("guard_receipt", type=Path)
    observe.add_argument("--reverse-execution", type=Path)
    settle = sub.add_parser("settle-destination")
    for name in ("guard_receipt", "execution", "output"):
        settle.add_argument(name, type=Path)
    source_settle = sub.add_parser("settle-source")
    for name in ("guard_receipt", "execution", "output"):
        source_settle.add_argument(name, type=Path)
    writer = sub.add_parser("resume-source-writer")
    for name in ("guard_receipt", "execution", "destination", "output"):
        writer.add_argument(name, type=Path)
    for name in (
        "reverse-pair",
        "restore-receipt",
        "backup-receipt",
        "independent-restore",
    ):
        writer.add_argument("--" + name, type=Path)
    connector = sub.add_parser("resume-source-connector")
    for name in ("execution", "writer_receipt", "destination", "output"):
        connector.add_argument(name, type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    signal.signal(
        signal.SIGTERM,
        lambda *_: (_ for _ in ()).throw(ValueError("Recovery interrupted")),
    )
    try:
        if args.command == "observe-destination":
            result = destination_observation(
                read_json(args.guard_receipt, 0),
                reverse_execution=read_json(args.reverse_execution, 0)
                if args.reverse_execution
                else None,
            )
        elif args.command == "settle-destination":
            result = settle_destination(
                read_json(args.guard_receipt, 0),
                read_json(args.execution, 0),
                args.output,
            )
        elif args.command == "settle-source":
            result = settle_source(
                read_json(args.guard_receipt, 0),
                read_json(args.execution, 0),
                args.output,
            )
        elif args.command == "resume-source-writer":
            recovery = {
                name: read_json(getattr(args, name), 0) if getattr(args, name) else None
                for name in ("restore_receipt", "backup_receipt", "independent_restore")
            }
            result = resume_source_writer(
                read_json(args.guard_receipt, 0),
                read_json(args.execution, 0),
                read_json(args.destination, 0),
                args.output,
                reverse_pair=args.reverse_pair,
                **recovery,
            )
        else:
            result = resume_source_connector(
                read_json(args.execution, 0),
                read_json(args.writer_receipt, 0),
                read_json(args.destination, 0),
                args.output,
            )
        print(json.dumps(result))
        return 0
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
        print(
            json.dumps(
                {
                    "error": "Recovery stopped; retained data and fences require inspection"
                }
            )
        )
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
