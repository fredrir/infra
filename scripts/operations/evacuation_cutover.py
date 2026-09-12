import argparse
import hashlib
import json
import math
import os
import re
import tarfile
from pathlib import Path

from evacuation_images import open_private, private_directory, read_json, write_json
from evacuation_staging import validate_plan

MAX_FILE = 64 * 1024**2
DATA_IMAGES = ("llunde-postgres", "llunde-valkey", "llunde-backend")
HOSTS = {"fredrir-05": "llunde-01", "fredrir-09": "cloud-server-10643982"}
APP_UNITS = {
    "edge": ("caddy.service", "cloudflared.service"),
    "llunde-backend": (
        "llunde-backend.service",
        "llunde-postgres.service",
        "llunde-valkey.service",
    ),
    "llunde-frontend": ("llunde-frontend.service",),
}
SYSTEM_RECONCILERS = (
    "gitops-pull.service",
    "gitops-pull.timer",
    "restic-backups-llunde-backend.service",
    "restic-backups-llunde-backend.timer",
)
BASE = "/var/lib/platform-evacuation"


def require(condition, message):
    if not condition:
        raise ValueError(message)


def sha_file(path, owner):
    with open_private(path, owner, MAX_FILE) as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def valkey_archive(path, owner):
    entries, references, total = {}, set(), 0
    with (
        open_private(path, owner, MAX_FILE) as stream,
        tarfile.open(fileobj=stream, mode="r:") as archive,
    ):
        for member in archive:
            require(
                not member.name.startswith("/") and ".." not in member.name.split("/"),
                "Valkey archive path escaped",
            )
            name = member.name.removeprefix("./").rstrip("/") or "."
            require(
                name not in entries
                and len(entries) < 128
                and 0 <= member.size <= MAX_FILE,
                "Duplicate or excessive Valkey archive entry",
            )
            require(
                0 <= member.uid < 65536
                and 0 <= member.gid < 65536
                and member.mode & ~0o777 == 0,
                "Valkey namespace ownership or mode invalid",
            )
            if member.isdir():
                require(
                    name in (".", "appendonlydir") and member.size == 0,
                    "Unexpected Valkey archive directory",
                )
                entries[name] = {
                    "type": "directory",
                    "uid": member.uid,
                    "gid": member.gid,
                    "mode": member.mode,
                }
                continue
            require(
                member.isfile() and not member.sparse,
                "Valkey links and special files forbidden",
            )
            require(
                name in ("dump.rdb", "appendonlydir/appendonly.aof.manifest")
                or re.fullmatch(
                    r"appendonlydir/appendonly\.aof\.[1-9][0-9]*\.(?:base\.(?:rdb|aof)|incr\.aof)",
                    name,
                ),
                "Unexpected Valkey archive file",
            )
            total += member.size
            require(total <= MAX_FILE, "Valkey archive expansion exceeds budget")
            with archive.extractfile(member) as payload:
                if name.endswith(".manifest"):
                    require(member.size <= 65536, "AOF manifest exceeds budget")
                    content = payload.read()
                    digest = hashlib.sha256(content).hexdigest()
                    for line in content.decode().splitlines():
                        fields = line.split()
                        require(
                            len(fields) == 6
                            and fields[0] == "file"
                            and fields[2] == "seq"
                            and fields[3].isdigit()
                            and int(fields[3]) > 0
                            and fields[4] == "type"
                            and fields[5] in ("b", "i", "h"),
                            "AOF manifest record invalid",
                        )
                        reference = "appendonlydir/" + fields[1]
                        require(
                            reference not in references
                            and "/" not in fields[1]
                            and fields[1] not in (".", ".."),
                            "AOF manifest reference invalid",
                        )
                        references.add(reference)
                else:
                    digest = hashlib.file_digest(payload, "sha256").hexdigest()
            entries[name] = {
                "type": "file",
                "uid": member.uid,
                "gid": member.gid,
                "mode": member.mode,
                "bytes": member.size,
                "sha256": digest,
            }
        stream.seek(archive.offset)
        while chunk := stream.read(65536):
            require(not any(chunk), "Malformed or hidden Valkey archive members")
    require(
        "appendonlydir/appendonly.aof.manifest" in entries and references,
        "Complete multipart AOF required",
    )
    aof_files = {
        name
        for name, value in entries.items()
        if value["type"] == "file"
        and name.startswith("appendonlydir/")
        and not name.endswith(".manifest")
    }
    require(references == aof_files, "AOF manifest and full archive differ")
    return entries


def validate_fence(value, source):
    require(
        value["schemaVersion"] == 1
        and value["kind"] == "evacuation-fence-observation"
        and value["host"] == source
        and value["hostname"] == HOSTS[source],
        "Fence source identity differs",
    )
    require(
        re.fullmatch(r"[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}", value["bootId"])
        and re.fullmatch(r"[a-f0-9]{64}", value["guardFilesSHA256"]),
        "Fence boot and guard identity required",
    )
    require(
        type(value["observedAt"]) in (int, float)
        and math.isfinite(value["observedAt"])
        and value["observedAt"] > 0,
        "Fence timestamp required",
    )
    require(
        value["applicationWriterInactive"] is True
        and value["connectorInactive"] is True
        and value["valkeyInactive"] is True
        and value["persistentGuardsVerified"] is True
        and value["guardUserManagerEvaluationVerified"] is True
        and value["reconciliationInactive"] is True,
        "Writer, connector, persistence and reconciliation must be fenced",
    )
    require(
        type(value["postgresOtherClients"]) is int
        and value["postgresOtherClients"] == 0
        and value["administrativeWritesExcludedByOperator"] is True,
        "Other database writers must be excluded",
    )
    require(
        type(value["valkeyGracefulExitCode"]) is int
        and value["valkeyGracefulExitCode"] == 0
        and value["valkeyAofWriteStatusBeforeStop"] == "ok"
        and value["valkeyRewriteInactiveBeforeStop"] is True,
        "Graceful complete AOF persistence required",
    )


def pair_identity(candidate, direction):
    plan = validate_plan(candidate)
    require(direction in ("forward", "reverse"), "Transfer direction required")
    source, destination = (
        ("fredrir-05", "fredrir-09")
        if direction == "forward"
        else ("fredrir-09", "fredrir-05")
    )
    return {
        "direction": direction,
        "source": source,
        "destination": destination,
        "candidateSHA256": hashlib.sha256(
            (Path(candidate) / "staging.json").read_bytes()
        ).hexdigest(),
        "imageIDs": {
            name: plan["services"][name]["runtimeImage"] for name in DATA_IMAGES
        },
    }


def verify_pair(directory, candidate_directory=None):
    directory = private_directory(directory, os.geteuid())
    require(
        {path.name for path in directory.iterdir()}
        == {"database.dump", "valkey.tar", "manifest.json"},
        "Exact paired bundle files required",
    )
    manifest = read_json(directory / "manifest.json", os.geteuid())
    require(
        manifest["schemaVersion"] == 1
        and manifest["kind"] == "evacuation-paired-state",
        "Paired bundle identity required",
    )
    require(
        manifest["direction"] in ("forward", "reverse"), "Transfer direction required"
    )
    expected_hosts = (
        ("fredrir-05", "fredrir-09")
        if manifest["direction"] == "forward"
        else ("fredrir-09", "fredrir-05")
    )
    require(
        (manifest["source"], manifest["destination"]) == expected_hosts,
        "Paired transfer hosts differ",
    )
    require(
        set(manifest["imageIDs"]) == set(DATA_IMAGES)
        and all(
            re.fullmatch(r"sha256:[a-f0-9]{64}", value)
            for value in manifest["imageIDs"].values()
        ),
        "Exact data and application image IDs required",
    )
    require(
        re.fullmatch(r"[a-f0-9]{64}", manifest["candidateSHA256"]),
        "Candidate digest required",
    )
    if candidate_directory is not None:
        require(
            all(
                manifest[key] == value
                for key, value in pair_identity(
                    candidate_directory, manifest["direction"]
                ).items()
            ),
            "Paired bundle differs from candidate",
        )
    before, after = manifest["fenceBefore"], manifest["fenceAfter"]
    for fence in (before, after):
        validate_fence(fence, manifest["source"])
    require(
        before["bootId"] == after["bootId"]
        and before["guardFilesSHA256"] == after["guardFilesSHA256"],
        "Source restarted or fence changed during copy",
    )
    require(
        0 <= after["observedAt"] - before["observedAt"] <= 300,
        "Final paired export exceeded its quiesced window",
    )
    require(
        manifest["consistency"] == "same-quiesced-window"
        and manifest["writerStopPerformedByVerifier"] is False
        and manifest["nativeRestoreVerified"] is False,
        "Verifier scope must remain explicit",
    )
    require(
        set(manifest["files"]) == {"database.dump", "valkey.tar"},
        "Paired file inventory differs",
    )
    require(
        after["exportedFiles"] == manifest["files"],
        "Closing fence must bind the captured export bytes",
    )
    for name, record in manifest["files"].items():
        require(
            type(record["bytes"]) is int
            and 0 < record["bytes"] <= MAX_FILE
            and record["bytes"] == (directory / name).stat().st_size
            and record["sha256"] == sha_file(directory / name, os.geteuid()),
            "Paired file checksum differs",
        )
    with open_private(directory / "database.dump", os.geteuid(), MAX_FILE) as stream:
        require(stream.read(5) == b"PGDMP", "PostgreSQL custom dump required")
    require(
        manifest["valkeyEntries"]
        == valkey_archive(directory / "valkey.tar", os.geteuid()),
        "Valkey bytes or namespace ownership differ",
    )
    return manifest


def seal_pair(directory, candidate, direction, before_path, after_path):
    directory = private_directory(directory, os.geteuid())
    require(
        {path.name for path in directory.iterdir()} == {"database.dump", "valkey.tar"},
        "Fresh paired bundle required",
    )
    before, after = (
        read_json(Path(before_path), os.geteuid()),
        read_json(Path(after_path), os.geteuid()),
    )
    manifest = {
        "schemaVersion": 1,
        "kind": "evacuation-paired-state",
        **pair_identity(candidate, direction),
        "fenceBefore": before,
        "fenceAfter": after,
        "consistency": "same-quiesced-window",
        "writerStopPerformedByVerifier": False,
        "nativeRestoreVerified": False,
        "files": {
            name: {
                "bytes": (directory / name).stat().st_size,
                "sha256": sha_file(directory / name, os.geteuid()),
            }
            for name in ("database.dump", "valkey.tar")
        },
        "valkeyEntries": valkey_archive(directory / "valkey.tar", os.geteuid()),
    }
    for fence in (before, after):
        validate_fence(fence, manifest["source"])
    write_json(directory / "manifest.json", manifest)
    try:
        return verify_pair(directory, candidate)
    except BaseException:
        (directory / "manifest.json").unlink()
        raise


def rollback_path(
    *, target_writer_start_attempted, target_writer_inactive, target_connector_inactive
):
    require(
        type(target_writer_start_attempted) is bool
        and target_writer_inactive is True
        and target_connector_inactive is True,
        "Target writer and connector must be confirmed stopped",
    )
    return (
        "fenced-reverse-copy-required"
        if target_writer_start_attempted
        else "original-source-data-eligible"
    )


def export_commands():
    return {
        "user": "llunde-backend",
        "postgres": [
            "podman",
            "exec",
            "--env",
            "PGAPPNAME=infra-evacuation-final-export",
            "llunde-postgres",
            "timeout",
            "--signal=TERM",
            "--kill-after=5s",
            "120s",
            "pg_dump",
            "--username=llunde",
            "--dbname=llunde",
            "--format=custom",
            "--lock-wait-timeout=10s",
        ],
        "valkey": [
            "podman",
            "unshare",
            "timeout",
            "--signal=TERM",
            "--kill-after=5s",
            "60s",
            "tar",
            "--format=posix",
            "--numeric-owner",
            "--one-file-system",
            "-C",
            "/home/llunde-backend/data/valkey",
            "-cf",
            "-",
            ".",
        ],
        "stdoutDestinations": {"postgres": "database.dump", "valkey": "valkey.tar"},
        "requiresVerifiedFence": True,
        "execute": False,
    }


def cutover_plan(candidate, outage_seconds=1200):
    require(
        type(outage_seconds) is int and 1200 <= outage_seconds <= 1800,
        "Reviewed outage budget must be 1200–1800 seconds",
    )
    identity = pair_identity(candidate, "forward")
    from evacuation_guards import plan as guard_plan

    guards = [
        {"path": path, "content": specification["content"]}
        for path, specification in guard_plan(
            "fredrir-05", identity["candidateSHA256"]
        )["files"].items()
    ]
    phases = [
        {
            "id": "preflight",
            "deadlineSeconds": 120,
            "gates": [
                "Fresh matching source images/endpoints and target empty stores",
                "Target secret regeneration, routing, recovery and off-host backup authority verified",
                "No active GitOps/deadman/auto-update/legacy backup operation",
                "Exact unit fragments, drop-in parent paths and previous timer states captured",
                "No other PostgreSQL/Valkey clients; operator excludes administrative writes",
                "Provider SSH retained; no concurrent Kata or host maintenance",
            ],
        },
        {
            "id": "freeze-reconciliation",
            "deadlineSeconds": 60,
            "gates": [
                "Hold existing /var/lib/gitops-pull/lock without truncation",
                "Install only new reviewed conditional drop-ins; preserve existing files",
                "Root0755 guard directory and root0644 markers must be traversable by all three service users",
                "Create reconciliation-locked; reload root and three user managers; prove their actual condition evaluation",
                "Stop timers, let existing jobs finish, recheck images and inactive reconciliation",
            ],
        },
        {
            "id": "fence-source",
            "deadlineSeconds": 60,
            "gates": [
                "Begin outage clock; create persistent source-locked",
                "Stop05 cloudflared first, then backend; verify exact processes exited and inhibited restart through actual user managers",
                "Exclude other PG client sessions and Valkey clients",
                "Wait for successful AOF persistence/no rewrite; gracefully stop Valkey without SIGKILL",
                "Capture fence-before observation and exact guard digest",
            ],
        },
        {
            "id": "final-pair",
            "deadlineSeconds": 300,
            "gates": [
                "PG custom dump under an in-container120s timeout/5s kill grace and lock wait10s; no live PGDATA copy",
                "Namespace archive entire stopped Valkey directory preserving UID/GID/modes",
                "Capture fence-after with exported file SHA/bytes; source boot/guards/writer state unchanged",
                "Seal pair; verify PG archive list and writable private AOF copy without fix/truncate; rehash after checker",
                "Retain original05 directories and immutable pair",
            ],
        },
        {
            "id": "restore-target",
            "deadlineSeconds": 180,
            "gates": [
                "Transfer verified pair to fresh root0700 target directory",
                "Use exact preloaded images and bounded one-at-a-time restore jobs",
                "PG fresh cluster and empty llunde database; TCP final-server readiness; exit-on-error single-transaction restore",
                "Valkey namespace-aware fresh-directory restore; bytes/modes/UID/GID rearchive equality before startup",
                "Restore proof references exact pair manifestSHA256; expected schema/native persistence/TTL pass",
                "Production backend and connector remain fenced",
            ],
        },
        {
            "id": "start-target-writer",
            "deadlineSeconds": 300,
            "gates": [
                "Reverify05 persistent writer/connector fence",
                "Persist target-writer-start-attempted BEFORE any backend/Flyway start",
                "Enable only target app checkpoint after verified restore; start backend/frontend/Caddy",
                "Validate private health/ready, effective local data endpoints and application/auth behavior",
            ],
        },
        {
            "id": "handoff-connector",
            "deadlineSeconds": 60,
            "gates": [
                "05 connector still absent and inhibited; verify live tunnel origin unchanged",
                "Start09 connector only; public root/www/API checks and exactly one origin pass",
                "Keep05 reconciliation and writers inhibited",
            ],
        },
        {
            "id": "accept",
            "deadlineSeconds": 1800,
            "outsideOutageBudget": True,
            "gates": [
                "Independent SMTP health monitoring and fresh target off-host backup pass",
                "Restore exact backup snapshot to a fresh independent workspace",
                "Observe application/auth/session behavior; retain05 data until rollback horizon accepted",
                "Do not convert05 or delete original data merely because target health is200",
            ],
        },
    ]
    return {
        "schemaVersion": 1,
        "kind": "evacuation-cutover-plan",
        **identity,
        "execute": False,
        "liveMutationImplemented": False,
        "outageBudgetSeconds": outage_seconds,
        "maximumForwardPhaseSeconds": sum(
            phase["deadlineSeconds"] for phase in phases[2:7]
        ),
        "originalSourceRollbackReserveSeconds": 300,
        "guaranteedMaximumOutage": False,
        "outagePolicy": "Before target start attempt, abort while rollback reserve remains; afterwards keep both writers fenced until verified reverse copy or deliberate forward recovery",
        "automaticRollback": False,
        "guardAccess": {
            "directoryMode": "0755",
            "markerMode": "0644",
            "owner": "root",
            "actualUserManagerEvaluationRequired": True,
        },
        "helperLimits": {
            "cpu": 1,
            "memoryBytes": 512 * 1024**2,
            "tasks": 128,
            "bundleFileBytes": MAX_FILE,
            "archiveExpandedBytes": MAX_FILE,
            "minimumFreeDiskBytes": 10 * 1024**3,
        },
        "guardFiles": guards,
        "exportCommands": export_commands(),
        "phases": phases,
        "rollback": {
            "beforeTargetWriterStartAttempt": "Stop09 app/connector, preserve target evidence, remove only owned source writer guard, start05 data/backend and verify health before its connector; restore prior timers last",
            "afterTargetWriterStartAttempt": "Stop09 connector/backend, keep05 fenced, create fresh reverse PG+stopped-AOF pair; restore into fresh05 directories with matching image/schema; preserve both previous copies; then activate exactly one source writer/connector",
            "reverseCopyDeadlineSeconds": 900,
            "expiryDoesNotAuthorizeDataLoss": True,
        },
    }


def main():
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    plan = commands.add_parser("plan")
    plan.add_argument("candidate", type=Path)
    plan.add_argument("--outage-seconds", type=int, default=1200)
    seal = commands.add_parser("seal-pair")
    seal.add_argument("directory", type=Path)
    seal.add_argument("candidate", type=Path)
    seal.add_argument("direction", choices=("forward", "reverse"))
    seal.add_argument("fence_before", type=Path)
    seal.add_argument("fence_after", type=Path)
    verify = commands.add_parser("verify-pair")
    verify.add_argument("directory", type=Path)
    verify.add_argument("--candidate", type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    try:
        if args.command == "plan":
            result = cutover_plan(args.candidate, args.outage_seconds)
        elif args.command == "seal-pair":
            result = seal_pair(
                args.directory,
                args.candidate,
                args.direction,
                args.fence_before,
                args.fence_after,
            )
        else:
            result = verify_pair(args.directory, args.candidate)
        print(json.dumps(result, indent=2))
        return 0
    except (OSError, ValueError, KeyError, TypeError, tarfile.TarError):
        print(
            json.dumps(
                {"error": "Cutover preparation failed; no host action performed"}
            )
        )
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
