import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import time
import unittest
from unittest.mock import Mock, patch


ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
SPEC = importlib.util.spec_from_file_location("evacuation_rehearsal", ROOT / "scripts/operations/evacuation_rehearsal.py")
rehearsal = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(rehearsal)


def private_file(path, content):
    path.write_bytes(content)
    path.chmod(0o600)


def fixture_inputs(directory):
    files = {"database.dump": b"PGDMPfixture", "dump.rdb": b"REDIS0012fixture", "Caddyfile": b"http://:8085 { respond fixture }\n"}
    metadata = {}
    for name, content in files.items():
        private_file(directory / name, content)
        metadata[name] = {"sha256": hashlib.sha256(content).hexdigest(), "bytes": len(content)}
    manifest = {"schemaVersion": 1, "kind": "isolated-target-rehearsal", "target": "fredrir-09", "hostname": "cloud-server-10643982", "images": {name: "sha256:" + str(index) * 64 for index, name in enumerate(rehearsal.SERVICES)}, "sources": {"postgres": metadata["database.dump"] | {"tables": 2, "constraints": 3}, "valkey": metadata["dump.rdb"]}, "files": metadata, "candidateSHA256": "f" * 64}
    private_file(directory / "manifest.json", json.dumps(manifest).encode())
    return manifest


def aof_archive(path, extra=None):
    entries = [(".", tarfile.DIRTYPE, b"", 999), ("appendonlydir", tarfile.DIRTYPE, b"", 999), ("appendonlydir/appendonly.aof.manifest", tarfile.REGTYPE, b"file appendonly.aof.1.incr.aof seq 1 type i\n", 1000), ("appendonlydir/appendonly.aof.1.incr.aof", tarfile.REGTYPE, b"synthetic persistence", 999)]
    with tarfile.open(path, "w") as archive:
        for name, kind, content, gid in entries + ([extra] if extra else []):
            item = tarfile.TarInfo(name)
            item.type, item.uid, item.gid = kind, 999, gid
            item.size, item.mode = len(content), 0o700 if kind == tarfile.DIRTYPE else 0o600
            if kind in [tarfile.SYMTYPE, tarfile.LNKTYPE]:
                item.linkname = "/etc/passwd"
            archive.addfile(item, io.BytesIO(content) if content else None)


class RehearsalTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.root.chmod(0o700)

    def pilot(self):
        manifest = fixture_inputs(self.root)
        with patch.object(rehearsal, "validate_inputs", return_value=manifest):
            return rehearsal.Pilot(self.root)

    def test_native_inputs_bind_checksums_headers_and_exact_inventory(self):
        expected = fixture_inputs(self.root)
        self.assertEqual(rehearsal.validate_inputs(self.root, os.geteuid()), expected)
        private_file(self.root / "database.dump", b"modified data")
        with self.assertRaisesRegex(ValueError, "checksum"):
            rehearsal.validate_inputs(self.root, os.geteuid())
        fixture_inputs(self.root)
        private_file(self.root / "unexpected", b"extra")
        with self.assertRaisesRegex(ValueError, "Unexpected rehearsal inputs"):
            rehearsal.validate_inputs(self.root, os.geteuid())

    def test_public_or_linked_backup_inputs_are_rejected(self):
        fixture_inputs(self.root)
        (self.root / "database.dump").chmod(0o644)
        with self.assertRaisesRegex(ValueError, "Private regular file"):
            rehearsal.validate_inputs(self.root, os.geteuid())
        fixture_inputs(self.root)
        backup = self.root / "database.dump"
        outside = self.root.parent / (self.root.name + "-fixture")
        backup.rename(outside)
        self.addCleanup(outside.unlink)
        backup.symlink_to(outside)
        with self.assertRaises((OSError, ValueError)):
            rehearsal.validate_inputs(self.root, os.geteuid())

    def test_preparation_copies_only_native_backups_and_caddy_without_claiming_cutover(self):
        candidate, source = self.root / "candidate", self.root / "source"
        for path in [candidate, source, candidate / "units", source / "postgres", source / "valkey"]:
            path.mkdir(mode=0o700)
        plan = {"source": {"id": "fredrir-05", "hostname": "llunde-01", "providerId": 132168416}, "target": {"id": "fredrir-09", "hostname": "cloud-server-10643982", "architecture": "amd64"}, "authorization": {"targetInstall": True, "sourceStop": False, "cutover": False}, "services": {}, "users": {user: {"uid": uid, "gid": uid} for user, uid in rehearsal.USERS.items()}, "data": {}, "unitSHA256": {}}
        for service in rehearsal.SERVICES:
            plan["services"][service] = {"user": rehearsal.SERVICE_USERS[service], "image": "docker.io/library/fixture@sha256:" + "a" * 64, "runtimeImage": "sha256:" + "b" * 64}
        for kind, suffix, contents in [("postgres", "dump", b"PGDMPfixture"), ("valkey", "rdb", b"REDIS0012fixture")]:
            reference = kind + "/20260912T004916Z.json"
            plan["data"][kind] = {"restoreProof": reference}
            receipt = {"source": f"fredrir-05/llunde-{kind}", "restoreVerified": True, "exitCode": 0, "format": "custom" if kind == "postgres" else "rdb", "bytes": len(contents), "sha256": hashlib.sha256(contents).hexdigest(), "createdAt": "2026-09-12T00:49:16Z", "restore": {"tables": 2, "constraints": 3}}
            private_file(source / reference, json.dumps(receipt).encode())
            private_file((source / reference).with_suffix("." + suffix), contents)
        caddy = b"http://:8085 { respond fixture }\n"
        private_file(candidate / "units/Caddyfile", caddy)
        plan["unitSHA256"]["units/Caddyfile"] = hashlib.sha256(caddy).hexdigest()
        private_file(candidate / "staging.json", json.dumps(plan).encode())
        destination = self.root / "inputs"
        result = rehearsal.prepare_inputs(candidate, source, destination)
        self.assertEqual(result, {"inputFiles": 3, "productionCredentials": False, "sourceWritersStopped": False})
        manifest = rehearsal.validate_inputs(destination, os.geteuid())
        self.assertEqual(manifest["sources"]["postgres"]["tables"], 2)
        with self.assertRaisesRegex(ValueError, "Fresh rehearsal bundle"):
            rehearsal.prepare_inputs(candidate, source, destination)

    def test_inspected_container_rejects_public_network_credentials_and_external_mounts(self):
        pilot = self.pilot()
        pilot.workspaces = {"edge": self.root}
        document = {"Image": pilot.manifest["images"]["caddy"], "HostConfig": {"NetworkMode": "none", "Memory": 256 * 1024**2, "PidsLimit": 128, "NanoCpus": 1000000000}, "Config": {"Env": []}, "Mounts": []}
        for change in [lambda item: item["HostConfig"].update(NetworkMode="host"), lambda item: item["Config"].update(Env=["DOPPLER_TOKEN=fixture-secret"]), lambda item: item.update(Mounts=[{"Type": "bind", "Source": "/run/secrets"}])]:
            modified = json.loads(json.dumps(document))
            change(modified)
            pilot.podman = Mock(side_effect=[subprocess.CompletedProcess([], 0, b"id", b""), subprocess.CompletedProcess([], 0, json.dumps([modified]).encode(), b"")])
            with self.assertRaises(ValueError):
                pilot.start("caddy", "caddy", 256, [])

    def test_archive_preserves_namespace_ownership_and_bytes(self):
        archive = self.root / "aof.tar"
        aof_archive(archive)
        manifest = rehearsal.verify_aof_archive(archive)
        self.assertEqual(manifest["appendonlydir/appendonly.aof.manifest"]["gid"], 1000)
        self.assertEqual(manifest["appendonlydir/appendonly.aof.1.incr.aof"]["sha256"], hashlib.sha256(b"synthetic persistence").hexdigest())

    def test_archive_rejects_traversal_links_extras_and_host_ownership(self):
        archive = self.root / "aof.tar"
        for extra in [("../escape", tarfile.REGTYPE, b"bad", 999), ("appendonlydir/link", tarfile.SYMTYPE, b"", 999), ("appendonlydir/hard", tarfile.LNKTYPE, b"", 999), ("unexpected", tarfile.DIRTYPE, b"", 999), ("appendonlydir/appendonly.aof.2.incr.aof", tarfile.REGTYPE, b"bad", 297606)]:
            with self.subTest(extra=extra):
                aof_archive(archive, extra)
                with self.assertRaises(ValueError):
                    rehearsal.verify_aof_archive(archive)
        self.assertFalse((self.root.parent / "escape").exists())

    def test_archive_budget_rejects_before_extraction(self):
        archive = self.root / "aof.tar"
        aof_archive(archive)
        with patch.object(rehearsal, "MAX_INPUT", 10), self.assertRaisesRegex(ValueError, "budget"):
            rehearsal.verify_aof_archive(archive)

    def test_native_aof_checker_uses_only_synthetic_copy_and_proves_no_mutation(self):
        pilot = self.pilot()
        pilot.workspaces = {"llunde-backend": self.root}
        source = self.root / "synthetic.tar"
        aof_archive(source)
        expected = rehearsal.verify_aof_archive(source)
        calls = []
        def podman(user, arguments, **kwargs):
            calls.append(arguments)
            if kwargs.get("output"):
                kwargs["output"].write(source.read_bytes())
            return subprocess.CompletedProcess([], 0, b"valid synthetic AOF", b"")
        pilot.podman = podman
        pilot.check_synthetic_aof(self.root / "valkey-restored", expected)
        self.assertIn(str(self.root / "valkey-restored") + ":/data", calls[0])
        self.assertNotIn("--fix", calls[0])
        self.assertFalse(any("truncate" in argument for argument in calls[0]))
        self.assertEqual(pilot.containers, [])
        (self.root / "checked-aof.tar").unlink()
        expected["appendonlydir/appendonly.aof.manifest"]["gid"] = 999
        with self.assertRaisesRegex(ValueError, "changed bytes, modes or namespace ownership"):
            pilot.check_synthetic_aof(self.root / "valkey-restored", expected)
        with self.assertRaisesRegex(ValueError, "Only restored synthetic AOF"):
            pilot.check_synthetic_aof(self.root / "production", expected)

    def test_containers_request_isolated_bounded_runtime(self):
        arguments = rehearsal.container_arguments("infra-rehearsal-0123456789ab-postgres", "sha256:" + "a" * 64, 768, ["--user=999:999", "--cap-drop=ALL"])
        for option in ["--network=none", "--pull=never", "--restart=no", "--timeout=300", "--memory=768m", "--memory-swap=768m", "--cpus=1", "--pids-limit=128", "--security-opt=no-new-privileges"]:
            self.assertIn(option, arguments)
        self.assertFalse(any(item in arguments for item in ["--privileged", "--publish", "--pid=host", "--network=host"]))
        child = rehearsal.podman_argv("llunde-backend", arguments)
        self.assertIn("env", child)
        self.assertIn("-i", child)
        with self.assertRaises(ValueError):
            rehearsal.podman_argv("administrator", arguments)

    def test_rehearsal_commands_filter_parent_credentials_and_enforce_deadline(self):
        pilot = self.pilot()
        result = subprocess.CompletedProcess([], 0, b"ok", b"")
        with patch.dict(os.environ, {"DOPPLER_TOKEN": "fixture-secret"}), patch.object(rehearsal.subprocess, "run", return_value=result) as run:
            pilot.call(["fixture", "command"])
            self.assertNotIn("DOPPLER_TOKEN", run.call_args.kwargs["env"])
            self.assertLessEqual(run.call_args.kwargs["timeout"], 30)
            pilot.deadline = time.monotonic() - 1
            with self.assertRaisesRegex(ValueError, "deadline"):
                pilot.call(["fixture"])
            self.assertEqual(run.call_count, 1)

    def test_private_failure_evidence_captures_bounded_diagnostic_without_stdin(self):
        pilot = self.pilot()
        pilot.report = self.root / "report"
        pilot.report.mkdir(mode=0o700)
        pilot.result["stage"] = "valkey-synthetic-aof-native-check"
        response = subprocess.CompletedProcess([], 1, b"native stdout diagnostic\n" + b"x" * 70000, b"")
        with patch.object(rehearsal.subprocess, "run", return_value=response), self.assertRaisesRegex(ValueError, "command failed"):
            pilot.call(["fixture-check", "approved-path"], data=b"private-input-not-for-evidence")
        metadata = json.loads((pilot.report / "command-1.json").read_text())
        self.assertEqual(metadata["stage"], "valkey-synthetic-aof-native-check")
        self.assertEqual(metadata["exitCode"], 1)
        self.assertEqual((pilot.report / "command-1.stdout").stat().st_size, 65536)
        self.assertFalse(any(b"private-input-not-for-evidence" in path.read_bytes() for path in pilot.report.iterdir()))

    def test_inspect_environment_is_never_retained_in_failure_evidence(self):
        pilot = self.pilot()
        pilot.report = self.root / "report"
        pilot.report.mkdir(mode=0o700)
        response = subprocess.CompletedProcess([], 0, b'[{"Config":{"Env":["DOPPLER_TOKEN=fixture-secret"]}}]', b"")
        with patch.object(rehearsal.subprocess, "run", return_value=response):
            returned = pilot.podman("edge", ["inspect", "fixture"])
            self.assertIn(b"fixture-secret", returned.stdout)
            pilot.record_failure()
        self.assertFalse(any(b"fixture-secret" in path.read_bytes() for path in pilot.report.iterdir()))

    def test_failed_container_cleanup_retains_its_private_workspace(self):
        pilot = self.pilot()
        workspace = Path(f"/home/llunde-backend/.infra-rehearsal-{pilot.token}")
        pilot.workspaces = {"llunde-backend": workspace}
        pilot.containers = [("llunde-backend", f"infra-rehearsal-{pilot.token}-postgres")]
        pilot.podman = Mock(return_value=subprocess.CompletedProcess([], 1, b"", b""))
        with patch.object(rehearsal.shutil, "rmtree") as remove:
            self.assertFalse(pilot.cleanup())
            remove.assert_not_called()
        self.assertEqual(pilot.result["retainedWorkspaces"], [str(workspace)])
        self.assertFalse(pilot.result["containersRemoved"])

    def test_failed_restore_cleans_only_its_owned_resources_and_never_runs_web(self):
        pilot = self.pilot()
        pilot.preflight = Mock()
        pilot.postgres = Mock(side_effect=rehearsal.RehearsalError("fixture failure"))
        pilot.valkey, pilot.web = Mock(), Mock()
        pilot.cleanup = Mock(return_value=True)
        with patch.object(rehearsal, "Pilot", return_value=pilot), patch.object(rehearsal.signal, "signal"):
            result = rehearsal.run_rehearsal(self.root)
        self.assertFalse(result["passed"])
        self.assertEqual(result["phase"], "postgres")
        self.assertEqual(result["reason"], "fixture failure")
        pilot.cleanup.assert_called_once()
        pilot.valkey.assert_not_called()
        pilot.web.assert_not_called()
        for field in ["productionCredentials", "sourceWritersStopped", "cloudflaredStarted", "backendStarted", "fullCrossUserRoutingVerified"]:
            self.assertFalse(result[field])

    def test_http_probe_has_no_redirect_or_external_endpoint(self):
        pilot = self.pilot()
        pilot.podman = Mock(return_value=subprocess.CompletedProcess([], 0, b"1234", b""))
        pilot.call = Mock(return_value=subprocess.CompletedProcess([], 0, b'{"status":301,"location":"https://llunde.no/fixture"}', b""))
        result = pilot.http("edge", "fixture", 8085, "/fixture", {"Host": "www.llunde.no"}, 301)
        arguments = pilot.call.call_args.args[0]
        self.assertEqual(arguments[:4], ["nsenter", "--net=/proc/1234/ns/net", "/usr/bin/python3", "-I"])
        self.assertEqual(result["status"], 301)
        self.assertIn("HTTPConnection('127.0.0.1'", arguments[5])
        self.assertNotIn("--mount", arguments)


if __name__ == "__main__":
    unittest.main()
