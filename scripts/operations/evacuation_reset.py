import argparse
import hashlib
import json
import os
import re
from pathlib import Path

import evacuation_cutover as cutover
from evacuation_execution import backup_gate, canonical, require
from evacuation_images import open_private, private_directory, write_json
from evacuation_restart import CONTENT
from evacuation_restart import paths as restart_paths
from evacuation_staging import USERS, upgrade_network_contents, validate_plan

BASE = "/var/lib/platform-evacuation"
TARGET = "/var/lib/infra-evacuation/llunde"
DATA = "/home/llunde-backend/data"
EVIDENCE = (
    "execution.json",
    "writer-failed.json",
    "reverse-execution.json",
    "reverse-native-restore-promoted.json",
    "reverse-backup.json",
    "reverse-admin-restore.json",
    "rollback-writer.json",
    "rollback-connector.json",
    "reconciliation-release.json",
)


def read_local(path):
    with open_private(path, os.geteuid(), 2 * 1024**2) as stream:
        raw = stream.read()
    return json.loads(raw), hashlib.sha256(raw).hexdigest()


def installed_paths(plan):
    return {
        (
            "/etc/infra-evacuation/llunde/Caddyfile"
            if name == "units/Caddyfile"
            else "/etc/containers/systemd/users/"
            + str(USERS[Path(name).parts[1]])
            + "/"
            + Path(name).name
        ): digest
        for name, digest in plan["unitSHA256"].items()
    }


def verify_recovery(old, new, pair, evidence, unit_receipt):
    old_plan = validate_plan(old, legacy_networks=True)
    new_plan = validate_plan(new)
    _, old_sha = read_local(Path(old) / "staging.json")
    _, new_sha = read_local(Path(new) / "staging.json")
    require(
        new_plan.get("sourceNetworkCandidateSHA256") == old_sha
        and {
            key: value
            for key, value in new_plan.items()
            if key not in ("unitSHA256", "sourceNetworkCandidateSHA256")
        }
        == {key: value for key, value in old_plan.items() if key != "unitSHA256"},
        "Only the reviewed network candidate transition is allowed",
    )
    expected = upgrade_network_contents(
        {name: (Path(old) / name).read_bytes() for name in old_plan["unitSHA256"]}
    )
    require(
        new_plan["unitSHA256"]
        == {name: hashlib.sha256(raw).hexdigest() for name, raw in expected.items()},
        "Candidate changes exceed the exact network replacement",
    )
    manifest = cutover.verify_pair(pair)
    pair_sha = hashlib.sha256(canonical(manifest)).hexdigest()
    (
        forward,
        writer,
        reverse,
        restored,
        backup,
        independent,
        resumed,
        connector,
        release,
    ) = (evidence[name] for name in EVIDENCE)
    require(
        all(
            value["candidateSHA256"] == old_sha
            for value in (
                forward,
                writer,
                reverse,
                restored,
                resumed,
                connector,
                release,
            )
        ),
        "Recovery candidate identities differ",
    )
    for value, host, destination, direction in (
        (forward, "fredrir-05", "fredrir-09", "forward"),
        (reverse, "fredrir-09", "fredrir-05", "reverse"),
    ):
        require(
            value["schemaVersion"] == 1
            and value["kind"] == "evacuation-execution-fence"
            and value["phase"] == "pair-sealed"
            and value["host"] == value["source"] == host
            and value["destination"] == destination
            and value["direction"] == value["marker"]["direction"] == direction
            and re.fullmatch(r"[a-f0-9]{32}", value["marker"]["executionID"])
            and re.fullmatch(direction + r"-[a-z0-9-]{1,48}", value["marker"]["run"])
            and value["marker"]["candidateSHA256"] == old_sha,
            "Exact completed forward and reverse executions required",
        )
    require(
        manifest["candidateSHA256"] == old_sha
        and manifest["source"] == "fredrir-09"
        and manifest["destination"] == "fredrir-05"
        and manifest["direction"] == "reverse"
        and reverse["pairManifestSHA256"] == pair_sha,
        "Exact sealed reverse pair required",
    )
    for name in ("fenceBefore", "fenceAfter"):
        require(
            manifest[name]["bootId"] == reverse["bootId"]
            and manifest[name]["executionMarkerSHA256"]
            == hashlib.sha256(canonical(reverse["marker"])).hexdigest(),
            "Sealed pair belongs to another reverse fence or boot",
        )
    require(
        writer["kind"] == "evacuation-target-writer-attempt"
        and writer["host"] == "fredrir-09"
        and writer["writerStartAttempted"] is True
        and writer["connectorStartAttempted"] is False
        and writer["pairManifestSHA256"] == forward["pairManifestSHA256"],
        "Target writer attempt must bind the original forward pair",
    )
    require(
        restored["kind"] == "evacuation-native-data-restore"
        and restored["host"] == "fredrir-05"
        and restored["direction"] == "reverse"
        and restored["nativeRestoreVerified"] is True
        and restored["promoted"] is True
        and restored["containersRemoved"] is True
        and restored["pairManifestSHA256"] == pair_sha,
        "Native reverse restore and promotion on 05 required",
    )
    retained = backup_gate(pair, backup, independent)
    require(
        resumed["kind"] == "evacuation-source-writer-resume"
        and resumed["host"] == "fredrir-05"
        and resumed["writerResumeAttempted"] is True
        and resumed["privateAcceptancePassed"] is True
        and resumed["recovery"] == {"kind": "fresh-reverse-copy", **retained},
        "Recovered source must use the exact independently restored reverse pair",
    )
    require(
        connector["kind"] == "evacuation-source-connector-resume"
        and connector["host"] == "fredrir-05"
        and connector["connectorActive"] is True
        and release["kind"] == "evacuation-source-reconciliation-release"
        and release["host"] == "fredrir-05"
        and release["completed"] is True
        and release["enabledStatesChanged"] is False
        and release["privateAcceptance"]["connectorActive"] is True
        and release["provider"]["host"] == "fredrir-05"
        and release["provider"]["originIP"] == "46.62.214.182"
        and release["provider"]["samples"] == 3,
        "Completed source connector and reconciliation recovery required",
    )
    require(
        unit_receipt["schemaVersion"] == 1
        and unit_receipt["kind"] == "evacuation-target-unit-promotion"
        and unit_receipt["candidateSHA256"] == old_sha
        and unit_receipt["completed"] is True
        and unit_receipt.get("rolledBack") is not True
        and set(unit_receipt["files"]) == set(installed_paths(old_plan)),
        "Original eight-file promotion receipt required",
    )
    for path, digest in installed_paths(old_plan).items():
        value = unit_receipt["files"][path]
        metadata = value["metadata"]
        require(
            value["sha256"] == metadata["sha256"] == digest
            and metadata["exists"] is True
            and metadata["uid"] == metadata["gid"] == 0
            and metadata["mode"] == "0644"
            and type(metadata["inode"]) is int
            and type(metadata["device"]) is int,
            "Original installed unit ownership or hash differs",
        )
    return old_plan, new_plan, old_sha, new_sha, reverse, pair_sha


def reset_plan(old, new, pair, evidence, unit_receipt):
    old_plan, new_plan, old_sha, new_sha, reverse, pair_sha = verify_recovery(
        old, new, pair, evidence, unit_receipt
    )
    token = reverse["marker"]["executionID"]
    archive = BASE + "/resets/" + token
    writer = {
        "candidateSHA256": old_sha,
        "pairManifestSHA256": evidence["execution.json"]["pairManifestSHA256"],
    }
    markers = {
        TARGET + "/" + name: writer
        for name in ("stage-approved", "restore-approved", "source-fenced")
    }
    markers[TARGET + "/target-writer-start-attempted"] = {
        **writer,
        "writerStartAttempted": True,
    }
    fences = {
        BASE + "/" + name: reverse["marker"]
        for name in ("source-locked", "reconciliation-locked")
    }
    mappings = []
    for path in sorted(set(markers) | set(fences) | set(unit_receipt["files"])):
        mappings.append({"source": path, "destination": archive + "/files" + path})
    for path in (
        TARGET + "/staging.json",
        BASE + "/receipts/unit-promotion.json",
        TARGET + "/candidates",
    ):
        mappings.append({"source": path, "destination": archive + "/files" + path})
    data = []
    for name in ("postgres", "valkey"):
        data.append(
            {
                "source": DATA + "/" + name,
                "destination": DATA + "/.recovered-" + token + "-" + name,
                "expected": {
                    **reverse["originalData"][name],
                    "uid": old_plan["users"]["llunde-backend"]["targetSubIdStart"]
                    + 998,
                    "gid": old_plan["users"]["llunde-backend"]["targetSubIdStart"]
                    + 998,
                    "mode": "0700",
                    "kind": "directory",
                },
            }
        )
    inhibitors = []
    for name in ("postgres", "valkey"):
        path, receipt = restart_paths(reverse, name)
        inhibitors.append(
            {
                "datastore": name,
                "path": path,
                "receipt": receipt,
                "contentSHA256": hashlib.sha256(CONTENT).hexdigest(),
                "preserveReceiptBeforeRestoreAt": archive + "/before" + receipt,
            }
        )
    phases = (
        (
            "reserve",
            15,
            "All fresh observations match; acquire exclusive operation lock",
            "Create fresh root0700 archive and root0600 durable intent; record each pending move before rename and fsync both parents",
        ),
        (
            "restore-restart-policy",
            20,
            "Both exact reverse fences and old loaded units remain",
            "Archive original inhibitor receipts; call evacuation_restart.restore_all(commands, reverse_execution); prove stopped PID/job state and restored Restart=always; retain updated receipts",
        ),
        (
            "archive-approval-markers",
            5,
            "Both reverse fences remain",
            "Rename only four matching forward approval/writer markers into the archive; edge-approved stays absent",
        ),
        (
            "preserve-data",
            5,
            "No containers/jobs; exact reverse originalData identity and mapped ownership",
            "Rename postgres and valkey directories to same-parent preservation paths; do not walk, rewrite or delete payload",
        ),
        (
            "archive-old-units",
            10,
            "Reverse fences present; four approval/writer markers absent",
            "Archive exact eight installed files, original unit receipt, installed staging metadata and inert candidate directory; preserve all other state",
        ),
        (
            "unload-old-units",
            15,
            "All application source files archived",
            "Daemon-reload only existing user managers; require no app fragments, containers or jobs; verify guards with require_loaded=False",
        ),
        (
            "stage-new-candidate",
            10,
            "Fresh canonical staging paths; reverse fences present",
            "Install validated nine-file inert candidate and staging.json under root-owned private paths; verify exact inventory and hashes",
        ),
        (
            "archive-reverse-fences",
            5,
            "Canonical data and approvals absent; apps unloaded; inhibitors restored; source authority freshly reconfirmed",
            "Rename exact reverse fences into archive; retain historical writer-attempt evidence",
        ),
        (
            "promote-new-inert-units",
            45,
            "All seven live markers absent; fresh unit-promotion receipt",
            "Call evacuation_target.promote_units(new_candidate, unchanged_guard_receipt, original_unit_receipt_path); verify nine files and loaded checkpoint/guard conditions",
        ),
        (
            "verify",
            20,
            "No application starts or enablement changes",
            "Verify seven markers absent, apps stopped, no containers/jobs/listeners/linger, canonical data absent, archive identities retained and timers dormant; then commit receipt",
        ),
    )
    return {
        "schemaVersion": 1,
        "kind": "evacuation-reset-restaging-plan",
        "host": "fredrir-09",
        "executionEnabled": False,
        "hostMutationsPerformed": False,
        "status": "requires-fresh-inventory-and-reviewed-executor",
        "oldCandidateSHA256": old_sha,
        "newCandidateSHA256": new_sha,
        "reversePairManifestSHA256": pair_sha,
        "reverseExecutionID": token,
        "archive": archive,
        "budget": {
            "totalSeconds": 180,
            "cleanupReserveSeconds": 30,
            "memoryMiB": 256,
            "cpuPercent": 100,
            "tasks": 64,
        },
        "requiredFreshObservations": {
            "maximumAgeSeconds": 30,
            "source": {
                "host": "fredrir-05",
                "privateAcceptance": True,
                "connectorActive": True,
                "singleProviderOrigin": "46.62.214.182",
                "sourceAndReconciliationMarkersAbsent": True,
            },
            "target": {
                "bootId": reverse["bootId"],
                "allApplicationAndDataUnitsStopped": True,
                "allServiceUserContainersAbsent": True,
                "allJobsAbsent": True,
                "allReconciliationTimersAndServicesStopped": True,
                "allUserLingerDisabled": True,
                "applicationPortsAbsent": [8080, 8081, 8085, 9101],
            },
            "fileRequirements": {
                "noSymlinks": True,
                "singleLinkRegularFiles": True,
                "trustedDirectoryFDAncestry": True,
                "metadataAndHashesBeforeEveryMutation": True,
                "sameFilesystemForEveryRename": True,
                "everyArchiveDestinationAbsent": True,
                "rootFileUidAndGid": 0,
                "markerMode": "0644",
                "stagingAndReceiptMode": "0600",
                "archiveDirectoryMode": "0700",
            },
            "dataParent": {
                "path": DATA,
                "uid": 2001,
                "gid": 2001,
                "mode": "0700",
                "realTrustedAncestors": True,
            },
            "markers": markers,
            "fences": fences,
            "edgeApprovalMustBeAbsent": TARGET + "/edge-approved",
            "oldUnitMetadata": unit_receipt["files"],
            "installedStagingSHA256": old_sha,
            "installedCandidates": {
                name.removeprefix("units/"): digest
                for name, digest in old_plan["unitSHA256"].items()
            },
            "data": data,
            "restartInhibitors": inhibitors,
            "loadedGuardAndCheckpointProof": "Exact installed guards plus candidate conditions per service",
        },
        "archiveRenames": mappings,
        "dataRenames": data,
        "newInstalledUnitHashes": installed_paths(new_plan),
        "phases": [
            {
                "id": name,
                "seconds": seconds,
                "requires": preconditions,
                "action": action,
            }
            for name, seconds, preconditions, action in phases
        ],
        "failurePolicy": {
            "automaticRetry": False,
            "automaticUnwind": False,
            "startStopOrKillApplications": False,
            "deleteDataOrEvidence": False,
            "retainAllPartialState": True,
            "reportRecoveryRequired": True,
            "neverRecreateApprovalMarkers": True,
            "freshBoundedCleanupBudgetSeconds": 30,
            "reFenceOnlyFromReceiptOwnedArchivedReverseValuesIfFencesWereMoved": True,
        },
        "unchanged": [
            "all source05 state",
            "secrets and encryption identities",
            "image stores and existing networks",
            "backup credentials and dormant timer enablement",
            "previous runs, restored workspaces, pairs and snapshots",
            "installed conditional guards",
            "frozen execution bundles",
        ],
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("old_candidate")
    parser.add_argument("new_candidate")
    parser.add_argument("reverse_pair")
    parser.add_argument("evidence")
    parser.add_argument("unit_receipt")
    parser.add_argument("output")
    args = parser.parse_args()
    evidence, hashes = {}, {}
    for name in EVIDENCE:
        evidence[name], hashes[name] = read_local(Path(args.evidence) / name)
    receipt, hashes["unitReceipt"] = read_local(args.unit_receipt)
    if receipt.get("operation") == "promote":
        require(receipt["resultCode"] == 0, "Successful original promotion required")
        receipt = json.loads(receipt["result"])
    result = reset_plan(
        args.old_candidate, args.new_candidate, args.reverse_pair, evidence, receipt
    )
    result["evidenceSHA256"] = hashes
    private_directory(Path(args.output).absolute().parent, os.geteuid())
    require(
        not Path(args.output).exists() and not Path(args.output).is_symlink(),
        "Fresh local output required",
    )
    write_json(Path(args.output), result)
    print(
        json.dumps(
            {
                "planSHA256": hashlib.sha256(
                    Path(args.output).read_bytes()
                ).hexdigest(),
                "executionEnabled": False,
                "status": result["status"],
            }
        )
    )


if __name__ == "__main__":
    main()
