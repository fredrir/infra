import copy
import hashlib
import io
import json
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
import evacuation_backup as backup
import evacuation_online_restore as restore


class OnlineRestoreTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.input = self.root / "input"
        self.input.mkdir(mode=0o700)
        self.candidate = self.root / "candidate"
        self.candidate.mkdir(mode=0o700)
        (self.candidate / "staging.json").write_bytes(b"candidate fixture")
        images = {
            "llunde-postgres": "sha256:" + "a" * 64,
            "llunde-valkey": "sha256:" + "b" * 64,
        }
        self.plan = {
            "services": {
                name: {"runtimeImage": image} for name, image in images.items()
            }
        }
        manifest = {
            "schemaVersion": 1,
            "kind": "evacuation-online-state",
            "host": "fredrir-09",
            "writersFenced": False,
            "consistency": "independent-datastore-points",
            "candidateSHA256": hashlib.sha256(
                (self.candidate / "staging.json").read_bytes()
            ).hexdigest(),
            "imageIDs": images,
            "files": {},
            "recoveryPoints": {
                name: {"startedAt": 100, "completedAt": 110}
                for name in ("database.dump", "dump.rdb")
            },
        }
        for name, data in [
            ("database.dump", b"PGDMPfixture"),
            ("dump.rdb", b"REDISfixture"),
        ]:
            backup.write_private(self.input / name, data)
            manifest["files"][name] = {
                "sha256": hashlib.sha256(data).hexdigest(),
                "bytes": len(data),
            }
        backup.write_private(
            self.input / "manifest.json", json.dumps(manifest).encode()
        )
        self.proof = {
            "schemaVersion": 1,
            "kind": "evacuation-independent-restore",
            "verified": True,
            "independentHost": True,
            "bundle": backup.validate_bundle(self.input),
            "snapshotId": "c" * 64,
            "archiveSHA256": "d" * 64,
        }
        self.proof_path = self.root / "proof.json"
        backup.write_private(self.proof_path, json.dumps(self.proof).encode())

    def test_requires_verified_independent_bytes_and_exact_candidate(self):
        with patch.object(restore, "validate_plan", return_value=self.plan):
            self.assertEqual(
                restore.recovery_inputs(self.input, self.proof_path, self.candidate)[1],
                self.proof,
            )
            for changes in (
                {"verified": False},
                {"independentHost": False},
                {"snapshotId": "../bad"},
                {"bundle": {"kind": "evacuation-paired-state"}},
            ):
                self.proof_path.write_text(json.dumps(self.proof | changes))
                with self.assertRaises(ValueError):
                    restore.recovery_inputs(self.input, self.proof_path, self.candidate)
            self.proof_path.write_text(json.dumps(self.proof))
            (self.candidate / "staging.json").write_bytes(b"changed candidate")
            with self.assertRaisesRegex(ValueError, "candidate"):
                restore.recovery_inputs(self.input, self.proof_path, self.candidate)

    def test_candidate_image_mismatch_is_rejected_before_runtime(self):
        changed = copy.deepcopy(self.plan)
        changed["services"]["llunde-valkey"]["runtimeImage"] = "sha256:" + "e" * 64
        with (
            patch.object(restore, "validate_plan", return_value=changed),
            self.assertRaisesRegex(ValueError, "images"),
        ):
            restore.recovery_inputs(self.input, self.proof_path, self.candidate)

    def test_failed_container_removal_retains_owned_workspace_and_records_failure(self):
        pilot = object.__new__(restore.NativeRestore)
        pilot.names = ["owned-fixture"]
        pilot.result = {}
        with patch.object(pilot, "remove", side_effect=ValueError("busy")):
            self.assertFalse(pilot.cleanup())
        self.assertEqual(pilot.result["cleanupFailures"], ["owned-fixture"])
        self.assertTrue(pilot.result["privateWorkspaceRetained"])
        self.assertFalse(pilot.result["containersRemoved"])

    def test_unexpected_cleanup_failure_preserves_private_result_and_nonzero_exit(self):
        pilot = Mock()
        pilot.workspace = self.root
        pilot.prepared = True
        pilot.result = {"nativeRestoreVerified": False}
        pilot.cleanup.side_effect = RuntimeError("unlogged private details")
        with (
            patch.object(restore, "NativeRestore", return_value=pilot),
            patch.object(
                sys,
                "argv",
                [
                    "restore",
                    "--input",
                    "input",
                    "--independent-proof",
                    "proof",
                    "--candidate",
                    "candidate",
                    "--workspace",
                    "workspace",
                ],
            ),
            patch.object(sys, "stdout", new_callable=io.StringIO) as output,
            self.assertRaises(SystemExit) as exit_status,
        ):
            restore.main()
        self.assertEqual(exit_status.exception.code, 1)
        result = json.loads((self.root / "result.json").read_text())
        self.assertFalse(result["cleanupVerified"])
        self.assertTrue(result["privateWorkspaceRetained"])
        self.assertEqual(result["cleanupErrorType"], "RuntimeError")
        self.assertNotIn("unlogged", output.getvalue() + json.dumps(result))

    def test_kernel_profile_recheck_rejects_container_replacement(self):
        pilot = object.__new__(restore.NativeRestore)
        pilot.workspace = self.root / "infra-online-restore.fixture"
        pilot.manifest = {"imageIDs": {"llunde-valkey": "sha256:" + "a" * 64}}
        pilot.names = []
        pilot.identities = {}
        before = {
            "Id": "a" * 64,
            "State": {"Pid": 10, "Running": True},
            "HostConfig": {"ReadonlyRootfs": True},
        }
        after = {"Id": "b" * 64, "State": {"Pid": 10, "Running": True}}
        with (
            patch.object(pilot, "call", return_value=(0, b"a" * 64)),
            patch.object(pilot, "inspect", side_effect=[before, after]),
            patch.object(restore, "mounted_profile", return_value={}),
            self.assertRaisesRegex(ValueError, "changed"),
        ):
            pilot.start("llunde-valkey", 192, [], [])

    def test_cleanup_refuses_redirected_data_and_retains_evidence(self):
        pilot = object.__new__(restore.NativeRestore)
        pilot.prepared, pilot.names, pilot.result = True, [], {}
        pilot.workspace = self.root / "workspace"
        pilot.workspace.mkdir()
        outside = self.root / "unrelated"
        outside.mkdir()
        sentinel = outside / "sentinel"
        sentinel.write_bytes(b"keep")
        (pilot.workspace / "postgres").symlink_to(outside)
        with patch.object(pilot, "call") as call:
            self.assertFalse(pilot.cleanup())
            call.assert_not_called()
        self.assertEqual(sentinel.read_bytes(), b"keep")
        self.assertFalse(pilot.result["restoredDataRemoved"])
        self.assertEqual(
            pilot.result["cleanupFailures"], ["private-data-or-image-cleanup"]
        )

    def test_all_cleanup_commands_share_one_budget(self):
        pilot = object.__new__(restore.NativeRestore)
        pilot.cleanup_commands = None
        pilot.environment, pilot.podman = [], []
        with patch.object(restore, "Commands") as commands:
            pilot.call(["first"], cleanup=True)
            pilot.call(["second"], cleanup=True)
        commands.assert_called_once_with(90)

    def test_postgres_readonly_profile_has_bounded_writable_socket_directory(self):
        pilot = object.__new__(restore.NativeRestore)
        with (
            patch.object(pilot, "data_directory", return_value=self.root / "postgres"),
            patch.object(
                pilot, "start", side_effect=RuntimeError("before container execution")
            ) as start,
            self.assertRaises(RuntimeError),
        ):
            pilot.postgres()
        service, memory, arguments, _command = start.call_args.args
        self.assertEqual((service, memory), ("llunde-postgres", 768))
        self.assertIn(
            "--tmpfs=/run/postgresql:rw,nosuid,nodev,noexec,size=4m,mode=1777",
            arguments,
        )
        self.assertFalse(any("uid=" in value or "gid=" in value for value in arguments))
        self.assertIn("--env=POSTGRES_PASSWORD=" + restore.PASSWORD, arguments)

    def test_postgres_shutdown_client_status_does_not_replace_server_exit_proof(self):
        for exit_code, oom in (
            (0, False),
            (1, False),
            (137, False),
            (0, True),
            (0, None),
        ):
            pilot = object.__new__(restore.NativeRestore)
            pilot.directory, pilot.result = self.input, {}
            schema = {
                "tables": ["flyway_schema_history", "users"],
                "constraints": 3,
                "serverVersion": "17.10",
            }
            replies = [
                (0, b""),
                (0, json.dumps(schema).encode()),
                (137, b""),
                (0, str(exit_code).encode()),
            ]
            with (
                patch.object(
                    pilot, "data_directory", return_value=self.root / "postgres"
                ),
                patch.object(pilot, "start", return_value=("owned", {})),
                patch.object(pilot, "wait"),
                patch.object(pilot, "call", side_effect=replies),
                patch.object(
                    pilot,
                    "inspect",
                    return_value={
                        "State": {"Running": False, "ExitCode": exit_code}
                        | ({"OOMKilled": oom} if oom is not None else {})
                    },
                ),
                patch.object(pilot, "remove") as remove,
            ):
                if exit_code == 0 and oom is False:
                    pilot.postgres()
                    self.assertEqual(
                        pilot.result["postgres"]["shutdownClientStatus"], 137
                    )
                    self.assertEqual(pilot.result["postgres"]["serverExitCode"], 0)
                    self.assertIs(pilot.result["postgres"]["serverOOMKilled"], False)
                    remove.assert_called_once_with("owned")
                else:
                    with self.assertRaisesRegex(
                        ValueError, "did not exit successfully"
                    ):
                        pilot.postgres()
                    remove.assert_not_called()

    def test_nonzero_removal_client_requires_both_exact_id_and_name_absence(self):
        pilot = object.__new__(restore.NativeRestore)
        pilot.names, pilot.result = ["owned"], {}
        pilot.identities = {"owned": {"id": "a" * 64, "image": "sha256:" + "b" * 64}}
        document = {"Name": "owned", "Id": "a" * 64, "Image": "sha256:" + "b" * 64}
        with patch.object(
            pilot,
            "call",
            side_effect=[
                (0, b""),
                (0, json.dumps([document]).encode()),
                (125, b""),
                (1, b""),
                (1, b""),
            ],
        ) as call:
            pilot.remove("owned")
        self.assertFalse(pilot.names)
        self.assertEqual(pilot.result["removalClientStatus"]["owned"], 125)
        self.assertEqual(
            {item.args[0][-1] for item in call.call_args_list[-2:]}, {"owned", "a" * 64}
        )

    def test_cleanup_refuses_container_identity_drift_before_removal(self):
        pilot = object.__new__(restore.NativeRestore)
        pilot.names, pilot.result = ["owned"], {}
        pilot.identities = {"owned": {"id": "a" * 64, "image": "sha256:" + "b" * 64}}
        document = {"Name": "owned", "Id": "c" * 64, "Image": "sha256:" + "b" * 64}
        with (
            patch.object(
                pilot,
                "call",
                side_effect=[(0, b""), (0, json.dumps([document]).encode())],
            ) as call,
            self.assertRaisesRegex(ValueError, "identity differs"),
        ):
            pilot.remove("owned")
        self.assertFalse(any(item.args[0][0] == "rm" for item in call.call_args_list))

    @unittest.skipUnless(
        shutil.which("valkey-server") and shutil.which("valkey-cli"),
        "Native Valkey required",
    )
    def test_native_session_invalidation_preserves_non_session_data_and_expires_nothing_else(
        self,
    ):
        socket = self.root / "v.sock"
        server = subprocess.Popen(
            [
                "valkey-server",
                "--port",
                "0",
                "--unixsocket",
                str(socket),
                "--unixsocketperm",
                "700",
                "--save",
                "",
                "--appendonly",
                "no",
                "--maxmemory",
                "16mb",
                "--maxmemory-policy",
                "noeviction",
            ],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        try:

            def cli(*arguments):
                return subprocess.run(
                    ["valkey-cli", "-s", str(socket), "--raw", *arguments],
                    capture_output=True,
                    check=True,
                    timeout=5,
                ).stdout.strip()

            for _ in range(50):
                if socket.exists():
                    break
                time.sleep(0.02)
            for key in (
                "session:revoked",
                "user:1:sessions",
                "user:a\nb:sessions",
                "login-throttle:ip",
                "user:1:sessions:metadata",
                "xsession:keep",
            ):
                self.assertEqual(cli("SET", key, "fixture", "EX", "300"), b"OK")
            self.assertEqual(cli("EVAL", restore.COUNT_SCRIPT, "0"), b"3")
            self.assertEqual(cli("EVAL", restore.SESSION_SCRIPT, "0"), b"3")
            self.assertEqual(cli("EVAL", restore.COUNT_SCRIPT, "0"), b"0")
            self.assertEqual(cli("EVAL", restore.SESSION_SCRIPT, "0"), b"0")
            for key in (
                "login-throttle:ip",
                "user:1:sessions:metadata",
                "xsession:keep",
            ):
                self.assertEqual(cli("GET", key), b"fixture")
                self.assertGreater(int(cli("TTL", key)), 0)
        finally:
            server.terminate()
            try:
                server.wait(timeout=5)
            except subprocess.TimeoutExpired:
                server.kill()
                server.wait(timeout=5)


if __name__ == "__main__":
    unittest.main()
