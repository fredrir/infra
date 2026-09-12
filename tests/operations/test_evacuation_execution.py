import copy
import hashlib
import io
import json
import os
import sys
import tarfile
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
import evacuation_cutover as cutover
import evacuation_execution as execution
import evacuation_target as target
from test_evacuation_cutover import archive_bytes, fence, identity


def inactive(timer=False):
    value = {
        "LoadState": "loaded",
        "ActiveState": "inactive",
        "SubState": "dead",
        "MainPID": "0",
        "ExecMainCode": "1",
        "ExecMainStatus": "0",
        "Result": "success",
        "ConditionResult": "yes",
    }
    if timer:
        for name in ("MainPID", "ExecMainCode", "ExecMainStatus"):
            value.pop(name)
    return value


class Replies:
    def __init__(self, replies):
        self.replies, self.calls = list(replies), []

    def user(self, user, arguments, **kwargs):
        self.calls.append((user, arguments, kwargs))
        return self.run(arguments, **kwargs)

    def run(self, arguments, **kwargs):
        value = self.replies.pop(0)
        return value if isinstance(value, tuple) else (0, value)


class ExecutionTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.root.chmod(0o700)

    def pair(self):
        pair = self.root / "pair"
        pair.mkdir(mode=0o700)
        for name, content in {
            "database.dump": b"PGDMP-private-fixture",
            "valkey.tar": archive_bytes(),
        }.items():
            (pair / name).write_bytes(content)
            (pair / name).chmod(0o600)
        files = {
            name: {
                "bytes": (pair / name).stat().st_size,
                "sha256": hashlib.sha256((pair / name).read_bytes()).hexdigest(),
            }
            for name in ("database.dump", "valkey.tar")
        }
        before, after = fence(observed=100), fence(observed=150)
        before["executionMarkerSHA256"] = after["executionMarkerSHA256"] = "e" * 64
        after.update(
            exportedFiles=files,
            postgresSchema={"tables": 2, "constraints": 3},
            outageStartedAt=90,
            outageBudgetSeconds=1200,
        )
        for name, value in [("before.json", before), ("after.json", after)]:
            execution.durable_json(self.root / name, value)
        with patch.object(cutover, "pair_identity", return_value=identity()):
            manifest = cutover.seal_pair(
                pair,
                self.root,
                "forward",
                self.root / "before.json",
                self.root / "after.json",
            )
        return pair, manifest

    def test_durable_state_refuses_overwrite_links_and_unrecorded_pending_files(self):
        path = self.root / "state.json"
        execution.durable_json(path, {"phase": "before"})
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        with self.assertRaises(ValueError):
            execution.durable_json(path, {"phase": "overwrite"})
        execution.durable_json(path, {"phase": "after"}, replace=True)
        self.assertEqual(json.loads(path.read_text()), {"phase": "after"})
        (self.root / "state.json.pending").write_text("inspection required")
        with self.assertRaises(ValueError):
            execution.durable_json(path, {"phase": "hidden"}, replace=True)
        self.assertEqual(json.loads(path.read_text()), {"phase": "after"})
        (self.root / "linked.json").symlink_to(path)
        with self.assertRaises(ValueError):
            execution.durable_json(self.root / "linked.json", {})

    def test_native_command_bounds_output_and_runtime_without_retaining_stderr(self):
        commands = execution.Commands(5)
        code, output = commands.run(
            [sys.executable, "-c", 'import sys;print("ok");sys.stderr.write("PRIVATE")']
        )
        self.assertEqual((code, output), (0, b"ok\n"))
        for program, maximum in [
            ('import os;os.write(1,b"x"*10000)', 100),
            ("import time;time.sleep(2)", 10000),
        ]:
            started = time.monotonic()
            with self.assertRaises(ValueError):
                execution.Commands(3).run(
                    [sys.executable, "-c", program], timeout=0.1, maximum=maximum
                )
            self.assertLess(time.monotonic() - started, 2)

    def test_restore_monitor_can_abort_a_long_native_command(self):
        def monitor():
            raise ValueError("sampled disk cap")

        with self.assertRaisesRegex(ValueError, "disk cap"):
            execution.Commands(3).run(
                [sys.executable, "-c", "import time;time.sleep(2)"], monitor=monitor
            )

    def test_timer_state_does_not_require_service_only_pid_properties(self):
        value = inactive(timer=True)
        result = execution.unit_state(
            Replies(
                ["\n".join(key + "=" + value for key, value in value.items()).encode()]
            ),
            "gitops-pull.timer",
        )
        self.assertTrue(execution.stopped(result))
        with self.assertRaises(ValueError):
            execution.unit_state(
                Replies([b"ActiveState=inactive\n"]), "llunde-valkey.service"
            )
        self.assertFalse(execution.stopped(inactive() | {"MainPID": "42"}))
        self.assertFalse(execution.stopped(inactive() | {"SubState": "auto-restart"}))

    def test_graceful_valkey_exit_requires_native_zero_exit_without_kill_or_restart(
        self,
    ):
        event = {
            "id": "a" * 64,
            "type": "container",
            "status": "died",
            "exitCode": 0,
            "timeNano": 101000000000,
        }
        result = execution.graceful_exit_event(
            Replies([json.dumps(event).encode()]), "a" * 64, 100
        )
        self.assertEqual(result, event)
        for events in [
            [],
            [event | {"exitCode": 137}],
            [event | {"exitCode": None}],
            [event | {"exitCode": True}],
            [event, event],
            [event | {"id": "b" * 64}],
            [event | {"timeNano": 1}],
            [event | {"status": "kill"}, event],
            [event | {"status": "restart"}, event],
        ]:
            with self.subTest(events=events), self.assertRaises(ValueError):
                execution.graceful_exit_event(
                    Replies([b"\n".join(json.dumps(row).encode() for row in events)]),
                    "a" * 64,
                    100,
                )

    def test_persistence_preflight_rejects_rewrites_other_clients_or_replicas(self):
        good = b"aof_enabled:1\r\naof_last_write_status:ok\r\naof_rewrite_in_progress:0\r\naof_rewrite_scheduled:0\r\n"
        self.assertEqual(
            execution.persistence_state(
                Replies(
                    [
                        good,
                        b"connected_clients:1\r\n",
                        b"role:master\r\nconnected_slaves:0\r\n",
                    ]
                )
            ),
            {"aofLastWriteStatus": "ok", "rewriteInactive": True},
        )
        for replies in [
            [good.replace(b"status:ok", b"status:err")],
            [good.replace(b"in_progress:0", b"in_progress:1")],
            [good, b"connected_clients:2\r\n"],
            [good, b"connected_clients:1\r\n", b"role:slave\r\nconnected_slaves:0\r\n"],
        ]:
            with self.assertRaises(ValueError):
                execution.persistence_state(Replies(replies))

    def test_pg_export_cleanup_signals_only_matching_owned_process_identities(self):
        commands = Replies([])
        with (
            patch.object(
                execution, "all_export_processes", side_effect=[[(12, "same")], []]
            ),
            patch.object(execution, "process_identity", return_value="same"),
            patch.object(execution, "postgres_clients", return_value=0),
            patch.object(execution.os, "kill") as kill,
        ):
            self.assertTrue(
                execution.stop_export(commands, "infra-evacuation-" + "a" * 12)
            )
            kill.assert_called_once_with(12, execution.signal.SIGTERM)
        with (
            patch.object(
                execution, "all_export_processes", side_effect=[[(12, "old")], []]
            ),
            patch.object(execution, "process_identity", return_value="new"),
            patch.object(execution, "postgres_clients", return_value=0),
            patch.object(execution.os, "kill") as kill,
        ):
            self.assertTrue(
                execution.stop_export(commands, "infra-evacuation-" + "a" * 12)
            )
            kill.assert_not_called()

    def test_pair_transfer_round_trip_reverifies_manifest_and_namespace_ownership(self):
        pair, manifest = self.pair()
        stream = io.BytesIO()
        execution.pack_pair(pair, stream)
        destination = self.root / "received"
        self.assertEqual(
            execution.receive_pair(
                destination, io.BytesIO(stream.getvalue()), "forward"
            ),
            manifest,
        )
        self.assertEqual(cutover.verify_pair(destination), manifest)
        self.assertEqual(
            {path.name for path in destination.iterdir()},
            {"manifest.json", "database.dump", "valkey.tar"},
        )
        self.assertTrue(
            all(path.stat().st_mode & 0o777 == 0o600 for path in destination.iterdir())
        )

    def test_transfer_rejects_wrong_direction_links_traversal_and_hidden_members(self):
        pair, _ = self.pair()
        stream = io.BytesIO()
        execution.pack_pair(pair, stream)
        with self.assertRaises(ValueError):
            execution.receive_pair(
                self.root / "reverse", io.BytesIO(stream.getvalue()), "reverse"
            )
        self.assertFalse((self.root / "reverse").exists())
        for index, name in enumerate(("../escape", "/escape", "unknown")):
            content = io.BytesIO()
            with tarfile.open(fileobj=content, mode="w") as archive:
                member = tarfile.TarInfo(name)
                member.type, member.linkname = tarfile.SYMTYPE, "/etc/passwd"
                archive.addfile(member)
            with self.assertRaises(ValueError):
                execution.receive_pair(
                    self.root / f"rejected-{index}",
                    io.BytesIO(content.getvalue()),
                    "forward",
                )
            self.assertFalse((self.root / f"rejected-{index}").exists())
        with self.assertRaises(ValueError):
            execution.receive_pair(
                self.root / "hidden",
                io.BytesIO(stream.getvalue() + b"UNREVIEWED"),
                "forward",
            )
        self.assertFalse((self.root / "hidden").exists())

    def test_final_pair_backup_gate_requires_matching_snapshot_and_independent_host(
        self,
    ):
        pair, manifest = self.pair()
        from evacuation_backup import validate_bundle

        bundle = validate_bundle(pair)
        backup = {
            "schemaVersion": 1,
            "kind": "evacuation-offhost-backup",
            "bundle": bundle,
            "snapshotId": "a" * 64,
            "archiveSHA256": "b" * 64,
        }
        restore = {
            "schemaVersion": 1,
            "kind": "evacuation-independent-restore",
            "bundle": bundle,
            "snapshotId": "a" * 64,
            "archiveSHA256": "b" * 64,
            "verified": True,
            "independentHost": True,
        }
        self.assertFalse(
            execution.backup_gate(pair, backup, restore)["applicationRestoreVerified"]
        )
        for changes in (
            {"snapshotId": "c" * 64},
            {"verified": False},
            {"independentHost": False},
            {"bundle": {"kind": "isolated-target-rehearsal"}},
        ):
            with self.assertRaises(ValueError):
                execution.backup_gate(pair, backup, restore | changes)

    def test_fresh_source_observation_rejects_expiry_boot_changes_and_future_time(self):
        pair, manifest = self.pair()
        value = manifest["fenceAfter"]
        self.assertEqual(execution.verify_fresh_source(value, pair, now=170), manifest)
        for changes, now in [
            ({}, 181),
            ({}, 149),
            ({"bootId": "abcdef12-1234-1234-1234-123456789abc"}, 170),
            ({"guardFilesSHA256": "d" * 64}, 170),
            ({"executionMarkerSHA256": "f" * 64}, 170),
        ]:
            with self.assertRaises(ValueError):
                execution.verify_fresh_source(value | changes, pair, now=now)
        self.assertEqual(execution.outage_remaining(manifest, now=300), 990)

    def test_disposable_restore_lifetime_survives_abrupt_controller_loss(self):
        pair, manifest = self.pair()
        with (
            patch.object(cutover, "verify_pair", return_value=manifest),
            patch.object(target, "validate_plan", return_value={}),
        ):
            restore = target.Restore(pair, self.root, {}, self.root / "restore.json")
        restore.record["token"] = "a" * 12
        with (
            patch.object(
                restore,
                "call",
                side_effect=[
                    (0, b"container"),
                    (
                        0,
                        json.dumps(
                            [
                                {
                                    "Name": "infra-final-restore-"
                                    + "a" * 12
                                    + "-postgres",
                                    "Id": "b" * 64,
                                }
                            ]
                        ).encode(),
                    ),
                ],
            ) as call,
            patch.object(target, "mounted_profile"),
            patch.object(target, "durable_json"),
        ):
            restore.run_container("postgres", self.root, 768, [], [])
        arguments = call.call_args_list[0].args[0]
        self.assertIn("--timeout=120", arguments)
        self.assertNotIn("--rm", arguments)
        value = {
            "Image": "sha256:" + "a" * 64,
            "Config": {"User": "999:999", "Timeout": 120, "Env": []},
            "EffectiveCaps": [],
            "BoundingCaps": [],
            "Mounts": [{"Type": "bind", "Source": str(self.root)}],
            "HostConfig": {
                "Privileged": False,
                "NetworkMode": "none",
                "Memory": 768 * 1024**2,
                "MemorySwap": 768 * 1024**2,
                "PidsLimit": 128,
                "NanoCpus": 1000000000,
                "SecurityOpt": ["no-new-privileges"],
            },
        }
        with patch.object(target, "kernel_profile", return_value={"verified": True}):
            target.mounted_profile(value, "sha256:" + "a" * 64, self.root, 768)
        for timeout in (None, 0, 121, True):
            value["Config"]["Timeout"] = timeout
            with self.assertRaisesRegex(ValueError, "native lifetime"):
                target.mounted_profile(value, "sha256:" + "a" * 64, self.root, 768)

    def test_restore_failure_only_removes_its_new_containers_and_retains_data(self):
        pair, manifest = self.pair()
        output = self.root / "restore.json"
        with (
            patch.object(cutover, "verify_pair", return_value=manifest),
            patch.object(target, "validate_plan", return_value={}),
        ):
            restore = target.Restore(pair, self.root, {}, output)

        def preflight():
            restore.record["token"] = "a" * 12
            restore.record["workspace"] = str(self.root / "retained")
            restore.names = ["infra-final-restore-" + "a" * 12 + "-postgres"]
            execution.durable_json(output, restore.record)

        def failing():
            raise ValueError("restore failed")

        with (
            patch.object(restore, "preflight", side_effect=preflight),
            patch.object(restore, "postgres", side_effect=failing),
            patch.object(target.Commands, "user", return_value=(0, b"")) as command,
        ):
            with self.assertRaisesRegex(ValueError, "restore failed"):
                restore.run()
        self.assertEqual(
            command.call_args.args[1],
            [
                "podman",
                "rm",
                "--force",
                "--ignore",
                "infra-final-restore-" + "a" * 12 + "-postgres",
            ],
        )
        result = json.loads(output.read_text())
        self.assertTrue(result["workspaceRetained"])
        self.assertTrue(result["containersRemoved"])
        self.assertFalse(result["nativeRestoreVerified"])

    def test_data_promotion_preserves_existing_directories_and_binds_the_new_inodes(
        self,
    ):
        pair, manifest = self.pair()
        data, workspace = self.root / "data", self.root / "workspace"
        data.mkdir(mode=0o700)
        workspace.mkdir(mode=0o700)
        for name in ("postgres", "valkey"):
            (data / name).mkdir()
            (data / name / "old").write_text(name)
            (workspace / name).mkdir()
            (workspace / name / "new").write_text(name)
        receipt = {
            "schemaVersion": 1,
            "kind": "evacuation-native-data-restore",
            "nativeRestoreVerified": True,
            "containersRemoved": True,
            "promoted": False,
            "pairManifestSHA256": hashlib.sha256(
                execution.canonical(manifest)
            ).hexdigest(),
            "token": "a" * 12,
            "workspace": str(workspace),
        }
        receipt_path = self.root / "restore.json"
        execution.durable_json(receipt_path, receipt)
        real_path = target.Path

        def paths(value):
            return (
                workspace
                if value == "/home/llunde-backend/.infra-final-restore-" + "a" * 12
                else real_path(value)
            )

        original = Path.lstat

        def metadata(path, *args, **kwargs):
            info = original(path, *args, **kwargs)
            if path == data:
                from types import SimpleNamespace

                return SimpleNamespace(st_mode=info.st_mode, st_uid=2001)
            return info

        with (
            patch.object(target, "DATA_PARENT", data),
            patch.object(target, "Path", side_effect=paths),
            patch.object(target, "host_identity"),
            patch.object(target, "unit_state", return_value=inactive()),
            patch.object(cutover, "verify_pair", return_value=manifest),
            patch.object(target, "read_json", return_value=copy.deepcopy(receipt)),
            patch.object(Path, "lstat", metadata),
        ):
            result = target.promote_data(pair, self.root, receipt_path, Replies([]))
        self.assertTrue(result["promoted"])
        for name in ("postgres", "valkey"):
            self.assertEqual((data / name / "new").read_text(), name)
            self.assertEqual(
                (data / (".previous-" + "a" * 12 + "-" + name) / "old").read_text(),
                name,
            )
        with patch.object(target, "DATA_PARENT", data):
            target.promoted_data(manifest, result)
            (data / "postgres").rename(data / "changed")
            (data / "postgres").mkdir()
            with self.assertRaises(ValueError):
                target.promoted_data(manifest, result)

    def test_restore_tree_rejects_links_and_sampled_expansion(self):
        tree = self.root / "tree"
        tree.mkdir()
        (tree / "data").write_bytes(b"1234")
        self.assertEqual(target.tree_bytes(tree), 4)
        with (
            patch.object(target, "MAX_RESTORE_BYTES", 3),
            self.assertRaises(ValueError),
        ):
            target.tree_bytes(tree)
        (tree / "link").symlink_to(self.root)
        with self.assertRaises(ValueError):
            target.tree_bytes(tree)

    def test_application_readiness_does_not_claim_authenticated_user_acceptance(self):
        result = {"status": 200, "bytes": 10, "sha256": "a" * 64}
        with (
            patch.object(target, "local_http", return_value=result),
            patch.object(target, "unit_state", return_value=inactive()),
        ):
            acceptance = target.private_acceptance(Replies([]), seconds=1)
        self.assertFalse(acceptance["authenticatedUserSessionVerified"])
        self.assertIn("existing account", acceptance["requiredUserAction"])
        with self.assertRaises(ValueError):
            target.local_http(443, "/credentials")

    def test_private_acceptance_requires_the_expected_connector_phase(self):
        response = {"status": 200, "bytes": 10, "sha256": "a" * 64}
        active = inactive() | {
            "ActiveState": "active",
            "SubState": "running",
            "MainPID": "42",
        }
        for state, expected, passed in [
            (inactive(), False, True),
            (active, True, True),
            (inactive(), True, False),
            (active, False, False),
            (active | {"MainPID": "0"}, True, False),
        ]:
            with (
                patch.object(target, "local_http", return_value=response),
                patch.object(target, "unit_state", return_value=state),
            ):
                if passed:
                    self.assertEqual(
                        target.private_acceptance(
                            Replies([]), seconds=1, connector_active=expected
                        )["connectorActive"],
                        expected,
                    )
                else:
                    with self.assertRaisesRegex(ValueError, "Connector state"):
                        target.private_acceptance(
                            Replies([]), seconds=1, connector_active=expected
                        )

    def test_inert_unit_promotion_copies_reviewed_files_without_starting_apps(self):
        import evacuation_guards
        import evacuation_preflight

        candidate, filesystem = self.root / "candidate", self.root / "filesystem"
        candidate.mkdir()
        filesystem.mkdir(mode=0o700)
        (candidate / "staging.json").write_text("{}")
        for user, units in cutover.APP_UNITS.items():
            directory = candidate / "units" / user
            directory.mkdir(parents=True)
            for unit in units:
                (directory / unit.replace(".service", ".container")).write_text(
                    "[Unit]\nConditionPathExists=/var/lib/infra-evacuation/llunde/stage-approved\n"
                )
        (candidate / "units/llunde-backend/llunde-backend-data.network").write_text(
            "[Network]\nInternal=true\n"
        )
        (candidate / "units/Caddyfile").write_text(
            "http://:8085 {\n bind127.0.0.1\n}\n"
        )
        fs = evacuation_guards.Filesystem(filesystem, os.geteuid())
        commands = Mock()
        commands.user.return_value = (0, b"")
        output = self.root / "units.json"
        private = target.private_directory
        with (
            patch.object(target, "host_identity"),
            patch.object(target, "validate_plan", return_value={}),
            patch.object(
                target,
                "private_directory",
                side_effect=lambda path, owner: private(path, os.geteuid()),
            ),
            patch.object(target, "TARGET_BASE", self.root / "markers"),
            patch.object(target, "unit_state", return_value=inactive()),
            patch.object(target, "guard_state", return_value={}),
            patch.object(target, "checkpoint_proof", return_value={"checked": True}),
            patch.object(evacuation_guards, "verify_installation", return_value={}),
            patch.object(evacuation_guards, "Filesystem", return_value=fs),
            patch.object(
                evacuation_preflight,
                "quadlet_paths",
                return_value=[str(self.root / "absent-quadlets")],
            ),
        ):
            result = target.promote_units(candidate, {}, output, commands)
        self.assertTrue(result["completed"])
        self.assertFalse(result["applicationsStarted"])
        self.assertEqual(len(result["files"]), 8)
        for path, info in result["files"].items():
            actual = filesystem / path.lstrip("/")
            self.assertEqual(
                hashlib.sha256(actual.read_bytes()).hexdigest(), info["sha256"]
            )
            self.assertEqual(actual.stat().st_mode & 0o777, 0o644)
        args = [call.args[1] for call in commands.user.call_args_list]
        self.assertEqual(sum("daemon-reload" in row for row in args), 3)
        self.assertFalse(
            any(any(word in row for word in ("start", "enable", "run")) for row in args)
        )
        fence_markers = self.root / "fence-markers"
        fence_markers.mkdir()
        for marker in ("source-locked", "reconciliation-locked"):
            (fence_markers / marker).write_text("{}")
            with (
                patch.object(target, "host_identity"),
                patch.object(target, "read_json", return_value=copy.deepcopy(result)),
                patch.object(target, "MARKERS", fence_markers),
                patch.object(target, "TARGET_BASE", self.root / "markers"),
                patch.object(target, "unit_state", return_value=inactive()),
                patch.object(evacuation_guards, "Filesystem", return_value=fs),
            ):
                with self.assertRaisesRegex(ValueError, "absent fence"):
                    target.rollback_units(output, commands)
            self.assertTrue(
                all(
                    (filesystem / path.lstrip("/")).exists() for path in result["files"]
                )
            )
            (fence_markers / marker).unlink()
        with (
            patch.object(target, "host_identity"),
            patch.object(target, "read_json", return_value=copy.deepcopy(result)),
            patch.object(target, "TARGET_BASE", self.root / "markers"),
            patch.object(target, "unit_state", return_value=inactive()),
            patch.object(evacuation_guards, "Filesystem", return_value=fs),
        ):
            rolled_back = target.rollback_units(output, commands)
        self.assertTrue(rolled_back["rolledBack"])
        self.assertTrue(
            all(
                not (filesystem / path.lstrip("/")).exists() for path in result["files"]
            )
        )

    def test_inert_promotion_refuses_real_fences_before_receipt_or_file_writes(self):
        import evacuation_guards

        markers = self.root / "guards"
        markers.mkdir()
        for name in ("source-locked", "reconciliation-locked"):
            (markers / name).symlink_to(self.root / "absent")
            with (
                patch.object(target, "host_identity"),
                patch.object(target, "validate_plan", return_value={}),
                patch.object(target, "private_directory"),
                patch.object(target, "MARKERS", markers),
                patch.object(target, "TARGET_BASE", self.root / "checkpoints"),
                patch.object(evacuation_guards, "verify_installation"),
                patch.object(evacuation_guards, "Filesystem") as filesystem,
            ):
                with self.assertRaisesRegex(ValueError, "absent fence"):
                    target.promote_units(
                        self.root, {}, self.root / "must-not-exist.json", Mock()
                    )
                filesystem.return_value.write.assert_not_called()
            self.assertFalse((self.root / "must-not-exist.json").exists())
            (markers / name).unlink()

    def test_generated_checkpoint_proof_rejects_missing_negated_or_trigger_only_conditions(
        self,
    ):
        def rows(user, unit):
            markers = ["stage-approved"]
            if unit == "llunde-backend.service":
                markers += ["restore-approved", "source-fenced"]
            if unit == "cloudflared.service":
                markers += ["edge-approved"]
            return [
                [
                    "ConditionPathExists",
                    False,
                    False,
                    str(target.TARGET_BASE / marker),
                    0,
                ]
                for marker in markers
            ]

        manager = Mock()
        manager.conditions.side_effect = rows
        proof = target.checkpoint_proof(manager)
        self.assertEqual(
            proof["llunde-backend"],
            ["stage-approved", "restore-approved", "source-fenced"],
        )
        self.assertEqual(proof["cloudflared"], ["stage-approved", "edge-approved"])
        for change in [
            lambda values: [],
            lambda values: [values[0][:1] + [True] + values[0][2:]],
            lambda values: [values[0][:2] + [True] + values[0][3:]],
        ]:
            manager.conditions.side_effect = lambda user, unit: change(rows(user, unit))
            with self.assertRaises(ValueError):
                target.checkpoint_proof(manager)

    def test_writer_attempt_marker_precedes_every_checkpoint_and_start_even_on_failure(
        self,
    ):
        pair, manifest = self.pair()
        markers = self.root / "target-markers"
        markers.mkdir()
        events = []

        def marker(path, value):
            events.append(("marker", path.name))
            execution.durable_json(path, value, mode=0o644)

        commands = Mock()
        commands.user.side_effect = lambda user, args: (
            events.append(("command", user, args)) or (0, b"")
        )
        output = self.root / "writer.json"
        retained = {
            "pairManifestSHA256": hashlib.sha256(
                execution.canonical(manifest)
            ).hexdigest(),
            "snapshotId": "a" * 64,
        }
        with (
            patch.object(target, "host_identity"),
            patch.object(target, "verify_fresh_source", return_value=manifest),
            patch.object(cutover, "verify_pair", return_value=manifest),
            patch.object(target, "promoted_data"),
            patch.object(target, "backup_gate", return_value=retained),
            patch.object(target, "outage_remaining", return_value=1200),
            patch.object(target, "guard_state", return_value={"host": "fredrir-09"}),
            patch.object(target, "MARKERS", self.root / "guards"),
            patch.object(target, "TARGET_BASE", markers),
            patch.object(target, "unit_state", return_value=inactive()),
            patch.object(target, "private_marker", side_effect=marker),
            patch.object(
                target,
                "private_acceptance",
                side_effect=ValueError("application readiness failed"),
            ),
            self.assertRaisesRegex(ValueError, "readiness failed"),
        ):
            target.start_writer(
                pair,
                self.root,
                {},
                {},
                {},
                {},
                manifest["fenceAfter"],
                output,
                commands,
            )
        self.assertEqual(events[0], ("marker", "target-writer-start-attempted"))
        self.assertFalse(
            any(
                event[0] == "command" and "cloudflared.service" in event[2]
                for event in events
            )
        )
        result = json.loads(output.read_text())
        self.assertTrue(result["writerStartAttempted"])
        self.assertTrue(result["rollbackRequiresFreshReversePair"])
        self.assertFalse(result["privateAcceptancePassed"])
        self.assertTrue((markers / "target-writer-start-attempted").exists())

    def test_insufficient_outage_reserve_refuses_all_target_mutations(self):
        pair, manifest = self.pair()
        commands = Mock()
        with (
            patch.object(target, "host_identity"),
            patch.object(target, "verify_fresh_source", return_value=manifest),
            patch.object(cutover, "verify_pair", return_value=manifest),
            patch.object(target, "promoted_data"),
            patch.object(target, "backup_gate", return_value={}),
            patch.object(target, "outage_remaining", return_value=659),
            patch.object(target, "private_marker") as marker,
        ):
            with self.assertRaisesRegex(ValueError, "rollback budgets"):
                target.start_writer(
                    pair,
                    self.root,
                    {},
                    {},
                    {},
                    {},
                    manifest["fenceAfter"],
                    self.root / "writer.json",
                    commands,
                )
        commands.user.assert_not_called()
        marker.assert_not_called()
        self.assertFalse((self.root / "writer.json").exists())


if __name__ == "__main__":
    unittest.main()
