import copy
import importlib.util
import json
import os
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
SPEC = importlib.util.spec_from_file_location(
    "evacuation_preflight", ROOT / "scripts/operations/evacuation_preflight.py"
)
preflight = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(preflight)


class PreflightTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def test_effective_endpoint_projection_never_returns_credentials_or_unapproved_fields(
        self,
    ):
        raw = b"DB_HOST=llunde-postgres\0DB_PORT=5432\0VALKEY_HOST=llunde-valkey\0DB_PASSWORD=SECRET\0DOPPLER_TOKEN=TOKEN\0DATABASE_URL=postgres://user:password@host/db\0"
        value = preflight.endpoint_projection(raw)
        self.assertEqual(
            value,
            {
                "DB_HOST": "llunde-postgres",
                "DB_PORT": "5432",
                "VALKEY_HOST": "llunde-valkey",
            },
        )
        self.assertNotIn("SECRET", json.dumps(value))
        self.assertNotIn("TOKEN", json.dumps(value))
        for raw in [
            b"DB_HOST=postgres://user:password@host\0",
            b"DB_HOST=one\0DB_HOST=two\0",
            b"DB_HOST=host\nTOKEN=private\0",
        ]:
            with self.assertRaises(preflight.PreflightError):
                preflight.endpoint_projection(raw)

    def test_unit_projection_rejects_environment_and_executable_arguments(self):
        self.assertEqual(
            preflight.unit_projection(
                "Id=llunde-backend.service\nActiveState=active\nMainPID=123\n"
            )["MainPID"],
            "123",
        )
        for raw in [
            "Environment=DOPPLER_TOKEN=PRIVATE\n",
            "ExecStart=/bin/secret --token PRIVATE\n",
            "ActiveState=active\nActiveState=inactive\n",
        ]:
            with self.assertRaises(preflight.PreflightError):
                preflight.unit_projection(raw)

    def test_container_projection_accepts_only_image_and_process_identity(self):
        record = {"image": "sha256:" + "a" * 64, "running": True, "pid": 100}
        self.assertEqual(preflight.container_projection(json.dumps(record)), record)
        self.assertEqual(
            preflight.container_projection(json.dumps(record | {"image": "a" * 64})),
            record,
        )
        for value in [
            record | {"Config": {"Env": ["SECRET=value"]}},
            record | {"image": "latest"},
            record | {"pid": True},
        ]:
            with self.assertRaises(preflight.PreflightError):
                preflight.container_projection(json.dumps(value))

    def test_readonly_preflight_can_never_be_relabelled_as_a_completed_fence(self):
        expected = {"candidateSHA256": "a" * 64}
        value = {
            "schemaVersion": 1,
            "kind": "evacuation-readonly-preflight",
            "host": "fredrir-05",
            "hostname": "llunde-01",
            "candidateSHA256": "a" * 64,
            "startedAt": 100,
            "finishedAt": 101,
            "fenceObservation": False,
            "writerStopPerformed": False,
            "guardInstallationPerformed": False,
            "applicationStartPerformed": False,
        }
        self.assertEqual(
            preflight.validate_result(value, "fredrir-05", expected), value
        )
        for changes in [
            {"fenceObservation": True},
            {"kind": "evacuation-fence-observation"},
            {"writerStopPerformed": True},
            {"guardInstallationPerformed": True},
            {"applicationStartPerformed": True},
            {"hostname": "other"},
            {"candidateSHA256": "b" * 64},
            {"finishedAt": 251},
            {"finishedAt": float("nan")},
        ]:
            with (
                self.subTest(changes=changes),
                self.assertRaises(preflight.PreflightError),
            ):
                preflight.validate_result(value | changes, "fredrir-05", expected)

    def test_target_dormancy_requires_absent_markers_processes_linger_ports_and_data(
        self,
    ):
        value = {
            "markers": {"stage-approved": {"exists": False}},
            "data": {"postgres": {"empty": True}},
            "activeQuadletPaths": {"edge": {"empty": True}},
            "listeners": [],
            "serviceProcesses": [],
            "users": {"edge": {"managerContacted": False, "linger": {"exists": False}}},
        }
        self.assertTrue(preflight.target_dormant(value))
        for mutation in [
            lambda x: x["markers"]["stage-approved"].update(exists=True),
            lambda x: x["data"]["postgres"].update(empty=False),
            lambda x: x["activeQuadletPaths"]["edge"].update(empty=False),
            lambda x: x["listeners"].append({"port": 8085}),
            lambda x: x["serviceProcesses"].append({"pid": 1}),
            lambda x: x["users"]["edge"].update(managerContacted=True),
            lambda x: x["users"]["edge"]["linger"].update(exists=True),
        ]:
            changed = copy.deepcopy(value)
            mutation(changed)
            self.assertFalse(preflight.target_dormant(changed))

    def test_data_inventory_never_follows_a_symlink_as_empty_data(self):
        empty = self.root / "empty"
        empty.mkdir()
        self.assertTrue(preflight.directory_state(empty)["empty"])
        self.assertTrue(preflight.directory_state(self.root / "absent")["empty"])
        link = self.root / "link"
        link.symlink_to(empty)
        self.assertFalse(preflight.directory_state(link)["empty"])
        (empty / "data").write_text("private fixture")
        result = preflight.directory_state(empty)
        self.assertFalse(result["empty"])
        self.assertNotIn("private fixture", json.dumps(result))

    def test_data_parent_readiness_checks_owner_mode_and_each_ancestor(self):
        healthy = {
            name: {
                "exists": True,
                "kind": "directory",
                "uid": uid,
                "gid": uid,
                "mode": mode,
            }
            for name, uid, mode in (
                ("/", 0, "0755"),
                ("/home", 0, "0755"),
                ("/home/llunde-backend", 2001, "0750"),
                ("/home/llunde-backend/data", 2001, "0700"),
            )
        }
        with patch.object(
            preflight, "metadata", side_effect=lambda path, **_: healthy[path]
        ):
            self.assertTrue(preflight.data_parent_observation()["readyForPromotion"])
        for path, change in (
            ("/home/llunde-backend/data", {"exists": False}),
            ("/home/llunde-backend/data", {"uid": 0}),
            ("/home/llunde-backend/data", {"gid": 2002}),
            ("/home/llunde-backend/data", {"mode": "0755"}),
            ("/home/llunde-backend", {"kind": "symlink"}),
            ("/home", {"mode": "0775"}),
        ):
            with self.subTest(path=path, change=change):
                altered = copy.deepcopy(healthy)
                altered[path].update(change)
                with patch.object(
                    preflight,
                    "metadata",
                    side_effect=lambda path, _observations=altered, **_: _observations[
                        path
                    ],
                ) as observed:
                    self.assertFalse(
                        preflight.data_parent_observation()["readyForPromotion"]
                    )
                    self.assertEqual(observed.call_args.args[0], path)

    def test_actual_checkpoint_gate_refuses_missing_parent_before_manager_calls(self):
        import evacuation_target

        manager = Mock()
        with (
            patch.object(
                preflight,
                "data_parent_observation",
                return_value={"readyForPromotion": False},
            ),
            patch.dict(sys.modules, {"evacuation_preflight": preflight}),
            self.assertRaisesRegex(ValueError, "data parent"),
        ):
            evacuation_target.checkpoint_proof(manager)
        manager.conditions.assert_not_called()

    def test_drop_in_parent_reports_resolved_symlink_and_readonly_filesystem(self):
        from types import SimpleNamespace

        target = self.root / "managed-tree"
        target.mkdir()
        link = self.root / "systemd-user"
        link.symlink_to(target)
        with patch.object(
            preflight.os, "statvfs", return_value=SimpleNamespace(f_flag=os.ST_RDONLY)
        ):
            result = preflight.writable_parent(link / "example.service.d")
        self.assertEqual(result["resolvedAncestor"], str(target.resolve()))
        self.assertTrue(result["filesystemReadOnly"])
        self.assertFalse(result["rootOwnedWritableCandidate"])
        self.assertFalse(result["futureManagerMergeVerified"])
        self.assertFalse((target / "example.service.d").exists())

    def test_inactive_user_unit_paths_are_computed_without_starting_or_contacting_manager(
        self,
    ):
        with patch.object(
            preflight, "command", return_value=str(self.root) + "\n"
        ) as command:
            result = preflight.unit_search_paths(
                ["caddy.service"], "edge", active=False
            )
        arguments = command.call_args.args[0]
        self.assertEqual(arguments[-3:], ["systemd-analyze", "--user", "unit-paths"])
        self.assertNotIn("systemctl", arguments)
        self.assertEqual(result["source"], "computed-defaults-no-manager-start")
        self.assertFalse(result["futureManagerMergeVerified"])

    def test_dangling_nix_profile_search_path_is_recorded_without_becoming_writable(
        self,
    ):
        link = self.root / "default-profile"
        link.symlink_to(self.root / "missing-profile")
        result = preflight.writable_parent(link / "lib/systemd/system")
        self.assertTrue(result["unresolvedAncestor"])
        self.assertEqual(result["ancestorMetadata"]["kind"], "symlink")
        self.assertFalse(result["rootOwnedWritableCandidate"])
        self.assertFalse(result["futureManagerMergeVerified"])

    def test_source_java_projection_uses_process_identity_and_ignores_raw_environment(
        self,
    ):
        process = self.root / "123"
        (process / "task/123").mkdir(parents=True)
        (process / "task/123/children").write_text("")
        (process / "comm").write_text("java\n")
        (process / "stat").write_text(
            "123 (java) " + " ".join(["S"] + ["0"] * 18 + ["12345"])
        )
        (process / "environ").write_bytes(
            b"DB_HOST=llunde-postgres\0DOPPLER_TOKEN=NEVER_PERSIST\0"
        )
        result = preflight.process_endpoints(123, self.root)
        self.assertEqual(result, {"pid": 123, "values": {"DB_HOST": "llunde-postgres"}})
        self.assertNotIn("NEVER_PERSIST", json.dumps(result))
        wrapper = self.root / "100"
        (wrapper / "task/100").mkdir(parents=True)
        (wrapper / "task/101").mkdir()
        (wrapper / "task/100/children").write_text("")
        (wrapper / "task/101/children").write_text("123")
        (wrapper / "stat").write_text(
            "100 (doppler) " + " ".join(["S"] + ["0"] * 18 + ["9999"])
        )
        (wrapper / "comm").write_text("doppler\n")
        self.assertEqual(preflight.process_endpoints(100, self.root), result)

    def test_process_inventory_includes_subordinate_user_without_commandline_reads(
        self,
    ):
        for pid, uid, name in [
            (1, 2001, "conmon"),
            (2, 297606, "valkey-server"),
            (3, 1000, "unrelated"),
        ]:
            path = self.root / str(pid)
            path.mkdir()
            (path / "status").write_text(
                f"Name:\t{name}\nUid:\t{uid}\t{uid}\t{uid}\t{uid}\n"
            )
        users = {
            "llunde-backend": {"subordinateIDs": {"subuid": [["296608", "65536"]]}}
        }
        self.assertEqual(
            {value["pid"] for value in preflight.service_processes(users, self.root)},
            {1, 2},
        )

    def test_listener_inventory_distinguishes_loopback_and_public_binds(self):
        raw = "header\n0: 0100007F:1F95 00000000:0000 0A\n1: 00000000:1F90 00000000:0000 0A\n2: 0100007F:0016 00000000:0000 0A\n"
        self.assertEqual(
            preflight.listen_projection(raw),
            [
                {"address": "127.0.0.1", "port": 8085},
                {"address": "0.0.0.0", "port": 8080},
            ],
        )
        self.assertEqual(
            preflight.listen_projection(
                "header\n0: 00000000000000000000000001000000:1F95 0:0 0A\n", True
            ),
            [{"address": "::1", "port": 8085}],
        )

    def test_command_capture_enforces_output_and_time_limits_and_reaps_children(self):
        environment = {"PATH": os.environ.get("PATH", "/usr/bin:/bin")}
        status, output = preflight.capture(
            [sys.executable, "-c", 'print("bounded")'],
            timeout=2,
            environment=environment,
        )
        self.assertEqual((status, output), (0, b"bounded\n"))
        for program in [
            'import os; os.write(1,b"x"*600000)',
            "import time; time.sleep(3)",
        ]:
            started = time.monotonic()
            with self.assertRaises(preflight.PreflightError):
                preflight.capture(
                    [sys.executable, "-c", program],
                    timeout=0.2,
                    environment=environment,
                )
            self.assertLess(time.monotonic() - started, 2)

    def test_stalled_source_stdin_is_covered_by_the_same_deadline(self):
        started = time.monotonic()
        with self.assertRaisesRegex(preflight.PreflightError, "time budget"):
            preflight.capture(
                [sys.executable, "-c", "import time; time.sleep(3)"],
                source=b"x" * 1048576,
                timeout=0.2,
                environment={"PATH": os.environ.get("PATH", "/usr/bin:/bin")},
            )
        self.assertLess(time.monotonic() - started, 2)

    def test_source_larger_than_pipe_buffer_is_fully_delivered_without_deadlock(self):
        status, value = preflight.capture(
            [sys.executable, "-c", "import sys; print(len(sys.stdin.buffer.read()))"],
            source=b"x" * 1048576,
            timeout=2,
            environment={"PATH": os.environ.get("PATH", "/usr/bin:/bin")},
        )
        self.assertEqual((status, value), (0, b"1048576\n"))

    def test_global_and_runtime_rootless_quadlet_locations_cannot_hide_activation_units(
        self,
    ):
        paths = preflight.quadlet_paths()
        self.assertEqual(len(paths), 10)
        self.assertIn("/etc/containers/systemd/users", paths)
        for user, uid in preflight.USERS.items():
            self.assertTrue(
                {
                    f"/run/user/{uid}/containers/systemd",
                    f"/home/{user}/.config/containers/systemd",
                    f"/etc/containers/systemd/users/{uid}",
                }
                <= set(paths)
            )

    def test_unapproved_host_is_rejected_before_any_command(self):
        with patch.object(preflight, "command") as command:
            with self.assertRaises(preflight.PreflightError):
                preflight.collect_host("other-host", {})
            command.assert_not_called()

    def test_error_diagnostics_never_persist_underlying_exception_values(self):
        encoded = json.dumps(
            preflight.error_diagnostic(
                ValueError("DOPPLER_TOKEN=private-runtime-value")
            )
        )
        self.assertNotIn("DOPPLER_TOKEN", encoded)
        self.assertNotIn("private-runtime-value", encoded)
        self.assertEqual(
            preflight.error_diagnostic(ValueError("one"))["reasonCode"],
            preflight.error_diagnostic(ValueError("two"))["reasonCode"],
        )

    def test_virtual_container_file_hash_does_not_resolve_as_a_host_path(self):
        path = self.root / "Caddyfile"
        path.write_bytes(b"private routing configuration")
        with patch.object(Path, "resolve", side_effect=FileNotFoundError):
            result = preflight.metadata(path, content_hash=True, resolve_path=False)
        self.assertIsNone(result["resolvedPath"])
        self.assertEqual(len(result["sha256"]), 64)
        self.assertNotIn("private routing configuration", json.dumps(result))

    def test_local_health_probe_is_fixed_to_loopback_and_never_follows_redirects(self):
        from unittest.mock import Mock

        connection = Mock()
        response = connection.getresponse.return_value
        response.status, response.read.return_value = 302, b""
        with patch.object(
            preflight.http.client, "HTTPConnection", return_value=connection
        ) as constructor:
            result = preflight.health(8080, "/health")
        constructor.assert_called_once_with("127.0.0.1", 8080, timeout=3)
        connection.request.assert_called_once_with("GET", "/health", headers={})
        self.assertEqual(result["status"], 302)
        connection.close.assert_called_once()
        with self.assertRaises(preflight.PreflightError):
            preflight.health(443, "/secrets")


if __name__ == "__main__":
    unittest.main()
