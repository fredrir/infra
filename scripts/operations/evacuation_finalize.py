import argparse
import hashlib
import http.client
import json
import os
import re
import signal
import subprocess
import time
from datetime import datetime
from pathlib import Path

from evacuation_execution import (
    MARKERS,
    TARGET_BASE,
    Commands,
    canonical,
    durable_json,
    guard_state,
    host_identity,
    marker_value,
    private_marker,
    require,
    stopped,
    unit_state,
    verify_fresh_source,
)
from evacuation_guards import Filesystem
from evacuation_images import read_json
from evacuation_rollback import remove_owned_marker, validate_destination
from evacuation_target import checkpoint_proof, private_acceptance, verify_unit_files

TUNNEL = "c0cdd9b5-fa97-42a1-bca7-95da236ea949"
ORIGINS = {"fredrir-05": "46.62.214.182", "fredrir-09": "85.190.100.72"}
TIMERS = [
    ("root", "gitops-pull.timer"),
    ("root", "restic-backups-llunde-backend.timer"),
    ("edge", "llunde-auto-update.timer"),
    ("llunde-backend", "llunde-auto-update.timer"),
    ("llunde-frontend", "llunde-auto-update.timer"),
]


def provider_authority(receipt, host, now=None):
    now = time.time() if now is None else now
    require(
        receipt["schemaVersion"] == 1
        and receipt["kind"] == "evacuation-cloudflare-origin-observation"
        and receipt["tunnelId"] == TUNNEL
        and receipt["expectedHost"] == host
        and receipt["expectedOriginIP"] == ORIGINS[host]
        and receipt["providerInventoryMatches"] is True,
        "Exact provider origin receipt required",
    )
    require(
        re.fullmatch(
            r"[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}",
            receipt["expectedConnectorId"],
        )
        and len(receipt["samples"]) == 3,
        "Three exact connector samples required",
    )
    times = []
    for sample in receipt["samples"]:
        observed = datetime.fromisoformat(sample["observedAt"])
        require(
            observed.tzinfo is not None and 0 <= now - observed.timestamp() <= 90,
            "Fresh provider observation required",
        )
        times.append(observed.timestamp())
        require(
            sample["matches"] is True and len(sample["connectors"]) == 1,
            "Other connectors must remain visible",
        )
        connector = sample["connectors"][0]
        require(
            connector["id"] == receipt["expectedConnectorId"]
            and 0 < len(connector["connections"]) <= 16
            and all(
                value["originIP"] == ORIGINS[host] for value in connector["connections"]
            ),
            "Provider connector origin differs",
        )
    require(
        times == sorted(times) and 2 <= times[-1] - times[0] <= 60,
        "Bounded repeated provider observations required",
    )
    return {
        "host": host,
        "connectorId": receipt["expectedConnectorId"],
        "originIP": ORIGINS[host],
        "lastObservedAt": times[-1],
        "samples": 3,
    }


def public_acceptance():
    result = {}
    for host, path, status in [
        ("llunde.no", "/", 200),
        ("www.llunde.no", "/", 301),
        ("api.llunde.no", "/health", 403),
        ("api.llunde.no", "/ready", 403),
    ]:
        connection = http.client.HTTPSConnection(host, timeout=5)
        try:
            connection.request(
                "GET", path, headers={"User-Agent": "fredrir-infra-evacuation/1.0"}
            )
            response = connection.getresponse()
            body = response.read(1048577)
            require(
                response.status == status and len(body) <= 1048576,
                "Public route acceptance failed",
            )
            if host == "www.llunde.no":
                require(
                    response.getheader("Location") == "https://llunde.no/",
                    "Canonical public redirect differs",
                )
            result[host + path] = {
                "status": response.status,
                "bytes": len(body),
                "sha256": hashlib.sha256(body).hexdigest(),
            }
        finally:
            connection.close()
    return {
        "routes": result,
        "authenticatedUserSessionVerified": False,
        "requiredUserAction": "Open https://llunde.no, sign in with an existing account, and confirm account content loads.",
    }


def properties(commands, unit, names):
    _, raw = commands.run(["systemctl", "show", unit, "--property=" + ",".join(names)])
    value = dict(line.split("=", 1) for line in raw.decode().splitlines())
    require(set(value) == set(names), "Complete systemd dependency projection required")
    return value


def boot_dependencies(commands, filesystem=None):
    filesystem = filesystem or Filesystem()
    renderer = properties(
        commands,
        "infra-evacuation-secrets.service",
        ["LoadState", "ActiveState", "SubState", "Before"],
    )
    require(
        renderer["LoadState"] == "loaded"
        and renderer["ActiveState"] == "active"
        and renderer["SubState"] == "exited",
        "Successful persistent secret renderer required",
    )
    proof = {}
    expected = b"[Unit]\nRequires=infra-evacuation-secrets.service\nAfter=infra-evacuation-secrets.service\n"
    for uid in (2000, 2001, 2002):
        name = "user@" + str(uid) + ".service"
        path = "/etc/systemd/system/" + name + ".d/infra-evacuation.conf"
        metadata, content = filesystem.read(path)
        require(
            content == expected and metadata["mode"] == "0644",
            "Exact user-manager secret dependency required",
        )
        value = properties(
            commands,
            name,
            ["LoadState", "ActiveState", "Requires", "After", "DropInPaths"],
        )
        require(
            value["LoadState"] == "loaded"
            and value["ActiveState"] == "active"
            and all(
                "infra-evacuation-secrets.service" in value[key].split()
                for key in ("Requires", "After")
            )
            and path in value["DropInPaths"].split()
            and name in renderer["Before"].split(),
            "Loaded user-manager secret ordering differs",
        )
        proof[str(uid)] = metadata
    return proof


def persist_target(
    pair,
    guard_receipt,
    unit_receipt,
    writer_receipt,
    connector_receipt,
    provider_receipt,
    source_fence,
    output,
    commands=None,
):
    host_identity("fredrir-09")
    manifest = verify_fresh_source(source_fence, pair)
    pair_sha = hashlib.sha256(canonical(manifest)).hexdigest()
    require(
        writer_receipt["kind"] == "evacuation-target-writer-attempt"
        and writer_receipt["privateAcceptancePassed"] is True
        and writer_receipt["pairManifestSHA256"] == pair_sha
        and connector_receipt["kind"] == "evacuation-connector-handoff"
        and connector_receipt["connectorActive"] is True
        and connector_receipt["pairManifestSHA256"] == pair_sha,
        "Accepted target writer and connector required",
    )
    require(
        marker_value(TARGET_BASE / "target-writer-start-attempted")[
            "pairManifestSHA256"
        ]
        == pair_sha,
        "Persistent target writer identity changed",
    )
    for name in ("source-locked", "reconciliation-locked"):
        require(
            not (MARKERS / name).exists() and not (MARKERS / name).is_symlink(),
            "Fenced target cannot become persistent",
        )
    guard_state(guard_receipt)
    verify_unit_files(unit_receipt)
    require(
        unit_receipt["candidateSHA256"] == manifest["candidateSHA256"],
        "Promoted target candidate differs",
    )
    checkpoint_proof()
    for name in (
        "stage-approved",
        "restore-approved",
        "source-fenced",
        "edge-approved",
    ):
        require(
            marker_value(TARGET_BASE / name)["pairManifestSHA256"] == pair_sha,
            "Target checkpoint changed",
        )
    authority = provider_authority(provider_receipt, "fredrir-09")
    commands = commands or Commands(90)
    acceptance = private_acceptance(commands, seconds=15, connector_active=True)
    public = public_acceptance()
    dependencies = boot_dependencies(commands)
    before = {}
    for uid in (2000, 2001, 2002):
        _, raw = commands.run(
            ["loginctl", "show-user", str(uid), "--property=Linger", "--value"]
        )
        require(
            raw.strip() in (b"yes", b"no"), "Explicit existing linger state required"
        )
        before[str(uid)] = raw.strip().decode()
    record = {
        "schemaVersion": 1,
        "kind": "evacuation-target-boot-persistence",
        "host": "fredrir-09",
        "pairManifestSHA256": pair_sha,
        "candidateSHA256": manifest["candidateSHA256"],
        "lingerBefore": before,
        "lingerEnabled": [],
        "secretDependencies": dependencies,
        "provider": authority,
        "privateAcceptance": acceptance,
        "publicAcceptance": public,
        "completed": False,
        "rebootVerified": False,
        "authenticatedUserSessionVerified": False,
        "sourceRetirementAuthorized": False,
    }
    durable_json(output, record)
    verify_fresh_source(source_fence, pair)
    try:
        for uid in (2000, 2001, 2002):
            if before[str(uid)] == "no":
                commands.run(["loginctl", "enable-linger", str(uid)])
            _, raw = commands.run(
                ["loginctl", "show-user", str(uid), "--property=Linger", "--value"]
            )
            require(raw.strip() == b"yes", "Persistent user-manager activation failed")
            record["lingerEnabled"].append(uid)
            durable_json(output, record, replace=True)
        record["completed"] = True
        durable_json(output, record, replace=True)
        return record
    except BaseException:
        record["failure"] = (
            "Partial linger activation retained; inspect receipt before recovery"
        )
        durable_json(output, record, replace=True)
        raise


def timer_state(commands, user, unit):
    arguments = [
        "systemctl",
        *([] if user == "root" else ["--user"]),
        "show",
        unit,
        "--property=UnitFileState",
        "--value",
    ]
    _, raw = (
        commands.run(arguments) if user == "root" else commands.user(user, arguments)
    )
    return unit_state(
        commands, unit, None if user == "root" else user
    ), raw.decode().strip()


def release_source_reconciliation(
    execution,
    writer_receipt,
    connector_receipt,
    destination,
    provider_receipt,
    output,
    commands=None,
):
    host_identity("fredrir-05")
    require(
        execution["host"] == "fredrir-05"
        and execution["direction"] == "forward"
        and writer_receipt["kind"] == "evacuation-source-writer-resume"
        and writer_receipt["privateAcceptancePassed"] is True
        and connector_receipt["kind"] == "evacuation-source-connector-resume"
        and connector_receipt["connectorActive"] is True
        and all(
            value["candidateSHA256"] == execution["candidateSHA256"]
            for value in (writer_receipt, connector_receipt)
        ),
        "Recovered original authority required",
    )
    validate_destination(destination, execution["candidateSHA256"])
    require(
        not (MARKERS / "source-locked").exists()
        and not (MARKERS / "source-locked").is_symlink()
        and marker_value(MARKERS / "reconciliation-locked") == execution["marker"],
        "Original reconciliation fence changed",
    )
    authority = provider_authority(provider_receipt, "fredrir-05")
    commands = commands or Commands(90)
    private = private_acceptance(commands, seconds=15, connector_active=True)
    public = public_acceptance()
    require(
        set(execution["timersBefore"]) == {user + "/" + unit for user, unit in TIMERS},
        "Complete captured timer baseline required",
    )
    start = []
    for user, unit in TIMERS:
        before = execution["timersBefore"][user + "/" + unit]
        require(
            set(before) == {"activeState", "loadState", "unitFileState"}
            and before["activeState"] in ("active", "inactive"),
            "Explicit original timer active and enabled states required",
        )
        current, enabled = timer_state(commands, user, unit)
        require(
            stopped(current)
            and current["LoadState"] == before["loadState"]
            and enabled == before["unitFileState"],
            "Timer ownership or enabled state changed during fence",
        )
        if before["activeState"] == "active":
            require(
                before["loadState"] == "loaded",
                "Originally active timer must be loaded",
            )
            start.append((user, unit))
    record = {
        "schemaVersion": 1,
        "kind": "evacuation-source-reconciliation-release",
        "host": "fredrir-05",
        "candidateSHA256": execution["candidateSHA256"],
        "provider": authority,
        "privateAcceptance": private,
        "publicAcceptance": public,
        "timersToStart": start,
        "timerStartsAttempted": [],
        "enabledStatesChanged": False,
        "completed": False,
    }
    durable_json(output, record)
    validate_destination(destination, execution["candidateSHA256"])
    remove_owned_marker("reconciliation-locked", execution["marker"])
    try:
        for user, unit in start:
            record["timerStartsAttempted"].append([user, unit])
            durable_json(output, record, replace=True)
            arguments = [
                "systemctl",
                *([] if user == "root" else ["--user"]),
                "start",
                unit,
            ]
            commands.run(arguments) if user == "root" else commands.user(
                user, arguments
            )
            current, enabled = timer_state(commands, user, unit)
            require(
                current["ActiveState"] == "active"
                and enabled
                == execution["timersBefore"][user + "/" + unit]["unitFileState"],
                "Original timer failed to resume",
            )
        record["completed"] = True
        durable_json(output, record, replace=True)
        return record
    except BaseException:
        cleanup = Commands(30)
        record["cleanup"] = {
            "refenced": False,
            "errors": [],
            "timers": {},
            "services": {},
            "alreadyTriggeredServicesCancelled": False,
        }
        try:
            private_marker(MARKERS / "reconciliation-locked", execution["marker"])
            record["cleanup"]["refenced"] = True
        except (OSError, ValueError):
            record["cleanup"]["errors"].append(
                "Persistent reconciliation refence failed"
            )
        for user, unit in record["timerStartsAttempted"]:
            try:
                arguments = [
                    "systemctl",
                    *([] if user == "root" else ["--user"]),
                    "stop",
                    unit,
                ]
                cleanup.run(arguments) if user == "root" else cleanup.user(
                    user, arguments
                )
                state, _ = timer_state(cleanup, user, unit)
                record["cleanup"]["timers"][user + "/" + unit] = state
            except (OSError, ValueError, subprocess.SubprocessError):
                record["cleanup"]["errors"].append(
                    "Timer cleanup failed: " + user + "/" + unit
                )
            try:
                service = unit.removesuffix(".timer") + ".service"
                record["cleanup"]["services"][user + "/" + service] = unit_state(
                    cleanup, service, None if user == "root" else user
                )
            except (OSError, ValueError, subprocess.SubprocessError):
                record["cleanup"]["errors"].append(
                    "Triggered service observation failed: " + user + "/" + service
                )
        record["failure"] = (
            "Captured reconciliation restart failed; inspect persistent fence and any triggered services"
        )
        durable_json(output, record, replace=True)
        raise


def main():
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)
    persist = sub.add_parser("persist-target")
    for name in (
        "pair",
        "guard_receipt",
        "unit_receipt",
        "writer_receipt",
        "connector_receipt",
        "provider_receipt",
        "source_fence",
        "output",
    ):
        persist.add_argument(name, type=Path)
    release = sub.add_parser("release-source-reconciliation")
    for name in (
        "execution",
        "writer_receipt",
        "connector_receipt",
        "destination",
        "provider_receipt",
        "output",
    ):
        release.add_argument(name, type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    signal.signal(
        signal.SIGTERM,
        lambda *_: (_ for _ in ()).throw(ValueError("Finalization interrupted")),
    )
    try:
        if args.command == "persist-target":
            result = persist_target(
                args.pair,
                *[
                    read_json(getattr(args, name), 0)
                    for name in (
                        "guard_receipt",
                        "unit_receipt",
                        "writer_receipt",
                        "connector_receipt",
                        "provider_receipt",
                        "source_fence",
                    )
                ],
                args.output,
            )
        else:
            result = release_source_reconciliation(
                *[
                    read_json(getattr(args, name), 0)
                    for name in (
                        "execution",
                        "writer_receipt",
                        "connector_receipt",
                        "destination",
                        "provider_receipt",
                    )
                ],
                args.output,
            )
        print(json.dumps(result))
        return 0
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
        print(
            json.dumps(
                {
                    "error": "Finalization stopped; retained ownership and recovery state require inspection"
                }
            )
        )
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
