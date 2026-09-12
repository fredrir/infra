import json
import os
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from contextlib import contextmanager
from datetime import UTC, datetime
from pathlib import Path
from unittest.mock import Mock, patch

import test_evacuation_reset as plan_fixture

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
with patch.dict(os.environ, {"INFRA_RESET_EXECUTOR_TEST_IMPORT": "1"}):
    import evacuation_reset_execute as execute


class ResetExecutorTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.root.chmod(0o700)
        self.reset = object.__new__(execute.Executor)
        self.reset.fs = execute.inventory.guards.Filesystem(self.root, os.geteuid())
        self.reset.archive = "/archive"
        self.reset.plan = {"requiredFreshObservations": {"fences": {}}}
        self.reset.record = {
            "events": [],
            "directoriesCreated": {},
            "initialMarkers": {},
            "status": "preparing",
        }
        self.reset.fs.directory(str(Path(execute.inventory.JOURNAL).parent), 0o700, {})
        self.reset.journal_metadata = self.reset.fs.write(
            execute.inventory.JOURNAL, b"{}", 0o600
        )
        self.reset.save()

    def file(self, path, raw=b"retained bytes", mode=0o644):
        self.reset.fs.directory(str(Path(path).parent), 0o755, {})
        return self.reset.fs.write(path, raw, mode)

    def test_archival_preserves_file_bytes_inode_and_original_mode(self):
        before = self.file("/etc/unit.container")
        self.reset.move_file("/etc/unit.container", before)
        archived = self.reset.fs.metadata("/archive/files/etc/unit.container")
        self.assertEqual(archived, before)
        self.assertEqual(
            self.reset.fs.read("/archive/files/etc/unit.container")[1],
            b"retained bytes",
        )
        self.assertFalse(self.reset.fs.path("/etc/unit.container").exists())
        self.assertEqual(
            self.reset.fs.read_json(execute.inventory.JOURNAL)["events"][0]["status"],
            "complete",
        )

    def test_drift_or_existing_archive_is_rejected_before_a_move(self):
        before = self.file("/etc/unit.container")
        self.reset.fs.path("/etc/unit.container").write_bytes(b"changed")
        with self.assertRaises(ValueError):
            self.reset.move_file("/etc/unit.container", before)
        self.assertEqual(self.reset.record["events"], [])
        current = self.reset.fs.metadata("/etc/unit.container")
        self.file("/archive/files/etc/unit.container", b"outside history")
        with self.assertRaises(ValueError):
            self.reset.move_file("/etc/unit.container", current)
        self.assertEqual(
            self.reset.fs.read("/archive/files/etc/unit.container")[1],
            b"outside history",
        )

    def test_interruption_after_rename_retains_payload_and_pending_journal(self):
        before = self.file("/etc/unit.container")
        save = self.reset.fs.save

        def interrupted(path, value):
            if self.reset.fs.path("/archive/files/etc/unit.container").exists():
                raise OSError("journal unavailable")
            return save(path, value)

        with (
            patch.object(self.reset.fs, "save", side_effect=interrupted),
            self.assertRaises(OSError),
        ):
            self.reset.move_file("/etc/unit.container", before)
        self.assertFalse(self.reset.fs.path("/etc/unit.container").exists())
        self.assertEqual(
            self.reset.fs.metadata("/archive/files/etc/unit.container"), before
        )
        journal = self.reset.fs.read_json(execute.inventory.JOURNAL)
        self.assertEqual(journal["events"][0]["status"], "pending")

    def test_data_move_preserves_nested_contents_without_traversal(self):
        parent = self.root / "data"
        parent.mkdir(mode=0o700)
        source = parent / "postgres"
        source.mkdir(mode=0o700)
        (source / "fixture").write_bytes(b"datastore bytes")
        outside = self.root / "outside"
        outside.write_bytes(b"outside untouched")
        (source / "link").symlink_to(outside)
        before = execute.inventory.directory_info(source.stat())

        @contextmanager
        def data_parent():
            fd = os.open(parent, os.O_RDONLY | os.O_DIRECTORY)
            try:
                yield fd
            finally:
                os.close(fd)

        self.reset.data_parent = data_parent
        item = {
            "source": "/data/postgres",
            "destination": "/data/.recovered-fixed-postgres",
            "expected": before,
        }
        self.reset.move_data(item)
        preserved = parent / ".recovered-fixed-postgres"
        self.assertEqual(execute.inventory.directory_info(preserved.stat()), before)
        self.assertEqual((preserved / "fixture").read_bytes(), b"datastore bytes")
        self.assertTrue((preserved / "link").is_symlink())
        self.assertEqual(outside.read_bytes(), b"outside untouched")
        with self.assertRaises(FileNotFoundError):
            self.reset.move_data(item)

    def test_empty_unit_directories_require_original_identity_and_distinct_archive(
        self,
    ):
        path = "/etc/containers/systemd/users/2001"
        self.reset.fs.directory(path, 0o755, {})
        expected = self.reset.directory_metadata(path)
        self.reset.record["oldUnitDirectories"] = {path: expected}
        self.file(path + "/unexpected", b"preserve")
        with self.assertRaises(ValueError):
            self.reset.archive_empty_unit_directories()
        self.reset.fs.path(path + "/unexpected").unlink()
        self.reset.archive_empty_unit_directories()
        destination = "/archive/empty-directories" + path
        self.assertEqual(self.reset.directory_metadata(destination), expected)
        self.assertFalse(self.reset.fs.path(path).exists())

    def test_new_staging_metadata_does_not_chmod_existing_public_parent(self):
        self.reset.fs.directory(execute.inventory.TARGET, 0o755, {})
        before = self.reset.directory_metadata(execute.inventory.TARGET)
        self.reset.write_new(execute.inventory.TARGET + "/staging.json", b"{}")
        self.assertEqual(
            self.reset.directory_metadata(execute.inventory.TARGET), before
        )
        self.assertEqual(
            self.reset.fs.metadata(execute.inventory.TARGET + "/staging.json")["mode"],
            "0600",
        )

    def test_failure_refences_only_owned_archived_values_and_continues_on_conflict(
        self,
    ):
        paths = [
            "/var/lib/platform-evacuation/source-locked",
            "/var/lib/platform-evacuation/reconciliation-locked",
        ]
        value = {"executionID": "e" * 32}
        self.reset.plan["requiredFreshObservations"]["fences"] = dict.fromkeys(
            paths, value
        )
        for path in paths:
            expected = self.file(path, json.dumps(value).encode())
            self.reset.record["initialMarkers"][path] = expected
            self.reset.move_file(path, expected)
        self.file(paths[0], b'{"executionID":"unexpected"}')
        self.reset.stable_runtime = Mock(return_value={})
        result = self.reset.failure()
        self.assertTrue(result["recoveryRequired"])
        self.assertTrue(result["journalPersisted"])
        self.assertEqual(
            json.loads(self.reset.fs.read(paths[0])[1]), {"executionID": "unexpected"}
        )
        self.assertEqual(json.loads(self.reset.fs.read(paths[1])[1]), value)
        self.assertIn("refence:" + paths[0], result["errors"])
        self.assertTrue(self.reset.fs.path("/archive/files" + paths[1]).exists())

    def test_native_bounds_require_lifetime_and_all_resource_controls(self):
        kernel = {
            "cpu.max": "100000 100000",
            "memory.max": str(256 * 1024**2),
            "memory.swap.max": "0",
            "pids.max": "64",
        }
        control = {
            "RuntimeMaxUSec": 70000000,
            "TimeoutStopUSec": 25000000,
            "KillMode": "mixed",
            "SendSIGKILL": True,
            "MainPID": os.getpid(),
        }
        self.assertEqual(
            execute.validate_limits("prepare", control, kernel)["runtimeSeconds"], 70
        )
        final = {**control, "RuntimeMaxUSec": 60000000}
        self.assertEqual(
            execute.validate_limits("finalize", final, kernel)["runtimeSeconds"], 60
        )
        for key, value in (
            ("RuntimeMaxUSec", 80000000),
            ("TimeoutStopUSec", 10000000),
            ("KillMode", "control-group"),
            ("SendSIGKILL", False),
            ("MainPID", os.getpid() + 1),
        ):
            with self.subTest(key=key), self.assertRaises(ValueError):
                execute.validate_limits("prepare", {**control, key: value}, kernel)
        for key, value in (
            ("cpu.max", "max 100000"),
            ("cpu.max", "200000 100000"),
            ("memory.max", str(256 * 1024**2 + 1)),
            ("memory.swap.max", "1"),
            ("pids.max", "65"),
        ):
            with self.subTest(key=key), self.assertRaises(ValueError):
                execute.validate_limits("prepare", control, {**kernel, key: value})

    def test_termination_is_deferred_only_during_recovery(self):
        before = {
            number: signal.getsignal(number)
            for number in (signal.SIGTERM, signal.SIGINT)
        }
        with execute.cleanup_signals():
            for number in before:
                self.assertEqual(signal.getsignal(number), signal.SIG_IGN)
        self.assertEqual(
            before, {number: signal.getsignal(number) for number in before}
        )
        with self.assertRaises(InterruptedError):
            execute.interrupted(signal.SIGTERM, None)

    def test_command_line_cannot_use_mutable_test_import_fallback(self):
        if execute.INVENTORY_ROOT.exists():
            self.skipTest("Frozen native bundle is present")
        result = subprocess.run(
            [
                sys.executable,
                "-B",
                str(ROOT / "scripts/operations/evacuation_reset_execute.py"),
                "prepare",
                "--check-bounds",
            ],
            env={**os.environ, "INFRA_RESET_EXECUTOR_TEST_IMPORT": "1"},
            capture_output=True,
            check=False,
            timeout=5,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b"Frozen inventory bundle is absent", result.stderr)


class PortableRootFilesystem(execute.inventory.guards.Filesystem):
    def read(self, value):
        metadata, raw = super().read(value)
        if metadata["exists"]:
            metadata.update(uid=0, gid=0)
        return metadata, raw


class ResetChoreographyTests(unittest.TestCase):
    def setUp(self):
        fixture = plan_fixture.ResetPlanTests()
        fixture.setUp()
        self.addCleanup(fixture.doCleanups)
        self.fixture = fixture
        self.root = fixture.root / "target"
        self.root.mkdir(mode=0o700)
        self.reset = object.__new__(execute.Executor)
        self.reset.fs = PortableRootFilesystem(self.root, os.geteuid())
        self.reset.plan = fixture.plan()
        self.reset.archive = self.reset.plan["archive"]
        self.reset.reverse = fixture.evidence["reverse-execution.json"]
        self.reset.guard = {"fixture": "native guard proof mocked"}
        self.reset.record = None
        self.reset.commands = self.commands(70)
        self.reset.native_limits = {"runtimeSeconds": 70, "stopGraceSeconds": 25}
        self.reset.guard_proof = Mock(return_value="b" * 64)
        original_directory = execute.inventory.directory_info

        def portable_directory(info):
            return {**original_directory(info), "uid": 0, "gid": 0}

        self.addCleanup(patch.stopall)
        patch.object(
            execute.inventory, "directory_info", side_effect=portable_directory
        ).start()
        patch.object(execute.inventory, "host_identity").start()
        patch.object(execute.inventory, "Commands", side_effect=self.commands).start()
        patch.object(execute.inventory.restart, "verify", return_value={}).start()
        patch.object(
            execute.inventory.restart,
            "loaded_inhibitor",
            return_value={"Restart": "no"},
        ).start()
        patch.object(
            execute.inventory.restart, "restore", side_effect=self.restore_policy
        ).start()
        patch.object(
            execute.inventory.target, "promote_units", side_effect=self.promote
        ).start()
        patch.object(
            execute.inventory.target, "verify_unit_files", side_effect=self.verify_units
        ).start()
        patch.object(
            execute.inventory.target, "checkpoint_proof", side_effect=self.checkpoint
        ).start()
        original_read = Path.read_text

        def read_boot(path, *args, **kwargs):
            if str(path) == "/proc/sys/kernel/random/boot_id":
                return fixture.boot
            return original_read(path, *args, **kwargs)

        patch.object(Path, "read_text", read_boot).start()
        self.reset.runtime = Mock(side_effect=self.runtime)
        self.reset.source_authority = Mock(wraps=self.reset.source_authority)
        self.reset.data_parent = self.data_parent
        self.observation = self.reset.plan["requiredFreshObservations"]
        self.reset.fs.directory(execute.inventory.TARGET, 0o755, {})
        for directory, physical in (
            (execute.inventory.OLD, fixture.old),
            (execute.inventory.NEW, fixture.new),
        ):
            for file in physical.rglob("*"):
                if file.is_file():
                    self.write(
                        directory + "/" + str(file.relative_to(physical)),
                        file.read_bytes(),
                        0o600,
                        0o700,
                    )
        old = json.loads((fixture.old / "staging.json").read_bytes())
        old_bytes = {
            checksum: (fixture.old / name).read_bytes()
            for name, checksum in old["unitSHA256"].items()
        }
        for path, value in self.observation["oldUnitMetadata"].items():
            value["metadata"] = self.write(path, old_bytes[value["sha256"]])
        directories = {
            "/etc/containers/systemd/users/" + str(uid): self.reset.directory_metadata(
                "/etc/containers/systemd/users/" + str(uid)
            )
            for uid in execute.inventory.USERS.values()
        }
        self.write(
            execute.inventory.UNITS,
            execute.inventory.canonical(
                {
                    "completed": True,
                    "files": self.observation["oldUnitMetadata"],
                    "directoriesCreated": directories,
                }
            ),
            0o600,
        )
        self.write(
            execute.inventory.TARGET + "/staging.json",
            (fixture.old / "staging.json").read_bytes(),
            0o600,
        )
        for name, checksum in self.observation["installedCandidates"].items():
            self.write(
                execute.inventory.TARGET + "/candidates/" + name,
                old_bytes[checksum],
                0o600,
                0o700,
            )
        for group in ("markers", "fences"):
            for path, value in self.observation[group].items():
                self.write(path, execute.inventory.canonical(value))
        for item in self.observation["restartInhibitors"]:
            self.write(item["path"], b"[Service]\nRestart=no\n")
            self.write(
                item["receipt"],
                execute.inventory.canonical(
                    {
                        "status": "installed",
                        "unit": item["datastore"],
                        "originalPolicy": {"Restart": "always"},
                    }
                ),
                0o600,
            )
        self.write(
            "/etc/systemd/system/restic-backups-llunde-backend.service",
            b"[Service]\nType=oneshot\n",
        )
        self.reset.fs.directory(execute.inventory.DATA, 0o700, {})
        for item in self.reset.plan["dataRenames"]:
            path = self.reset.fs.path(item["source"])
            path.mkdir(mode=0o700)
            (path / "original-data").write_bytes(b"datastore history")
            item["expected"] = execute.inventory.directory_info(path.stat())
        now = time.time()
        source = {
            "schemaVersion": 1,
            "kind": "evacuation-reset-source-authority",
            "host": "fredrir-05",
            "planSHA256": execute.inventory.PLAN_SHA,
            "observedAt": now,
            "sourceFenceAbsent": True,
            "reconciliationFenceAbsent": True,
            "unitStates": {
                name: {"ActiveState": "active", "MainPID": "100"}
                for name in execute.inventory.target.SERVICE_USERS
            },
            "privateAcceptance": {
                "connectorActive": True,
                "checks": {
                    name: {"status": 200}
                    for name in ("health", "ready", "frontend", "proxy")
                },
            },
        }
        provider = {
            "schemaVersion": 1,
            "kind": "evacuation-cloudflare-origin-observation",
            "tunnelId": "c0cdd9b5-fa97-42a1-bca7-95da236ea949",
            "expectedHost": "fredrir-05",
            "expectedOriginIP": "46.62.214.182",
            "expectedConnectorId": fixture.boot,
            "providerInventoryMatches": True,
            "samples": [
                {
                    "observedAt": datetime.fromtimestamp(now - age, UTC).isoformat(),
                    "matches": True,
                    "connectors": [
                        {
                            "id": fixture.boot,
                            "connections": [{"originIP": "46.62.214.182"}],
                        }
                    ],
                }
                for age in (4, 2, 0)
            ],
        }
        self.write(
            "/inputs/source.json", execute.inventory.canonical(source), 0o600, 0o700
        )
        self.write(
            "/inputs/provider.json", execute.inventory.canonical(provider), 0o600, 0o700
        )
        self.initial = self.reset.inventory()
        self.write(
            "/inputs/inventory.json",
            execute.inventory.canonical(self.initial),
            0o600,
            0o700,
        )
        self.original_payload = {
            path: self.reset.fs.read(path)[1]
            for path in (
                *self.observation["markers"],
                *self.observation["fences"],
                *self.observation["oldUnitMetadata"],
            )
        }

    def write(self, path, raw, mode=0o644, parent_mode=0o755):
        parent = str(Path(path).parent)
        physical = self.reset.fs.path(parent)
        if not physical.exists():
            self.reset.fs.directory(parent, parent_mode, {})
        return self.reset.fs.write(path, raw, mode)

    def commands(self, seconds):
        value = Mock()
        value.deadline = time.monotonic() + seconds
        value.run.side_effect = AssertionError("Unexpected native host command")
        value.user.return_value = (0, b"")
        return value

    def runtime(self, *, unloaded=False):
        return {
            "units": {
                name: {
                    "LoadState": "not-found" if unloaded else "loaded",
                    "ActiveState": "inactive",
                    "SubState": "dead",
                    "MainPID": "0",
                    "Job": "",
                    "FragmentPath": ""
                    if unloaded
                    else "/run/user/2001/systemd/generator/" + name + ".service",
                    "DropInPaths": "/etc/systemd/user/"
                    + name
                    + ".service.d/95-evacuation-fence.conf",
                }
                for name in execute.inventory.target.SERVICE_USERS
            },
            "managers": {
                name: {"MainPID": str(100 + uid)}
                for name, uid in execute.inventory.USERS.items()
            },
            "timers": {
                name: {
                    "LoadState": "loaded",
                    "UnitFileState": "static"
                    if name == "restic-backups-llunde-backend.service"
                    else "disabled",
                    "Job": "",
                }
                for name in execute.inventory.target.cutover.SYSTEM_RECONCILERS
            },
        }

    @contextmanager
    def data_parent(self):
        with self.reset.fs.directory_fd(execute.inventory.DATA) as fd:
            yield fd

    def restore_policy(self, commands, reverse, name, *, _filesystem):
        self.reset.check_markers()
        item = next(
            item
            for item in self.observation["restartInhibitors"]
            if item["datastore"] == name
        )
        _filesystem.path(item["path"]).unlink()
        _filesystem.save(item["receipt"], {"status": "restored", "Restart": "always"})
        return {"datastore": name, "status": "restored"}

    def promote(self, candidate, guard, receipt, commands):
        self.reset.check_markers(approvals=False, fences=False)
        self.reset.quadlet_inventory({})
        root = execute.inventory.TARGET + "/candidates"
        self.assertEqual(self.reset.directory_metadata(root)["mode"], "0700")
        for user in execute.inventory.USERS:
            self.assertEqual(
                self.reset.directory_metadata(root + "/" + user)["mode"], "0700"
            )
        value = self.reset.fs.read_json(execute.inventory.NEW + "/staging.json")
        available = {
            checksum: self.reset.fs.read(candidate + "/" + name)[1]
            for name, checksum in value["unitSHA256"].items()
        }
        result = {
            "completed": True,
            "candidateSHA256": self.reset.plan["newCandidateSHA256"],
            "files": {},
        }
        for path, checksum in self.reset.plan["newInstalledUnitHashes"].items():
            metadata = self.write(path, available[checksum])
            result["files"][path] = {"sha256": checksum, "metadata": metadata}
        self.write(receipt, execute.inventory.canonical(result), 0o600)
        return result

    def verify_units(self, result):
        for path, value in result["files"].items():
            self.reset.fs.metadata(path, value["metadata"])
        self.reset.quadlet_inventory(result["files"])

    def checkpoint(self):
        self.reset.check_markers(approvals=False, fences=False)
        self.reset.data_snapshot(preserved=True)

    def prepare(self):
        return self.reset.prepare(
            "/inputs/inventory.json", "/inputs/source.json", "/inputs/provider.json"
        )

    def test_complete_prepare_finalize_preserves_history_and_stages_private_nine_files(
        self,
    ):
        prepared = self.prepare()
        self.assertEqual(prepared["status"], "prepared-fenced")
        self.reset.check_markers(approvals=False)
        self.reset.quadlet_inventory({})
        self.reset.native_limits = {"runtimeSeconds": 60, "stopGraceSeconds": 25}
        completed = self.reset.finalize("/inputs/source.json", "/inputs/provider.json")
        self.assertEqual(completed["status"], "complete-inert")
        self.assertFalse(completed["applicationStartAttempted"])
        for path, raw in self.original_payload.items():
            self.assertEqual(
                self.reset.fs.read(self.reset.archive + "/files" + path)[1], raw
            )
        for item in self.reset.plan["dataRenames"]:
            self.assertEqual(
                (
                    self.reset.fs.path(item["destination"]) / "original-data"
                ).read_bytes(),
                b"datastore history",
            )
            self.assertFalse(self.reset.fs.path(item["source"]).exists())
        journal = self.reset.fs.read_json(execute.inventory.JOURNAL)
        self.assertEqual(len(journal["events"]), 36)
        self.assertLessEqual(
            self.reset.fs.path(execute.inventory.JOURNAL).stat().st_size, 65536
        )
        self.assertTrue(journal["recurringBackupRefreshRequiredBeforeEnablement"])
        for path in self.reset.fs.path(self.reset.archive).rglob("*"):
            if path.is_dir() and "/empty-directories/" not in str(path):
                self.assertEqual(path.stat().st_mode & 0o777, 0o700)
        self.assertEqual(journal["nativePrepareLimits"]["runtimeSeconds"], 70)
        self.assertEqual(journal["nativeFinalizeLimits"]["runtimeSeconds"], 60)
        self.assertEqual(self.reset.source_authority.call_count, 2)

    def test_signal_after_first_fence_rename_refences_without_promoting_or_losing_history(
        self,
    ):
        self.prepare()
        rename = os.rename
        first = next(iter(self.observation["fences"]))
        signalled = False

        def interrupt_after_rename(*args, **kwargs):
            nonlocal signalled
            result = rename(*args, **kwargs)
            if (
                not signalled
                and self.reset.fs.path(self.reset.archive + "/files" + first).exists()
            ):
                signalled = True
                execute.interrupted(signal.SIGTERM, None)
            return result

        with (
            patch.object(os, "rename", side_effect=interrupt_after_rename),
            self.assertRaises(InterruptedError),
        ):
            self.reset.finalize("/inputs/source.json", "/inputs/provider.json")
        self.reset.check_markers(approvals=False)
        self.reset.quadlet_inventory({})
        self.reset.data_snapshot(preserved=True)
        journal = self.reset.fs.read_json(execute.inventory.JOURNAL)
        self.assertEqual(journal["status"], "failed-retained")
        self.assertTrue(journal["failure"]["journalPersisted"])
        self.assertTrue(journal["failure"]["applicationsStoppedVerified"])
        self.assertFalse(self.reset.fs.metadata(execute.inventory.UNITS)["exists"])
        self.assertEqual(
            self.reset.fs.read(self.reset.archive + "/files" + first)[1],
            self.original_payload[first],
        )

    def test_stale_second_source_proof_leaves_both_fences_and_data_preserved(self):
        self.prepare()
        source = self.reset.fs.read_json("/inputs/source.json")
        source["observedAt"] = time.time() - 31
        self.reset.fs.save("/inputs/source.json", source)
        with self.assertRaisesRegex(ValueError, "thirty seconds"):
            self.reset.finalize("/inputs/source.json", "/inputs/provider.json")
        self.reset.check_markers(approvals=False)
        self.reset.data_snapshot(preserved=True)
        self.reset.quadlet_inventory({})
        for path in self.observation["fences"]:
            self.assertFalse(
                self.reset.fs.metadata(self.reset.archive + "/files" + path)["exists"]
            )


if __name__ == "__main__":
    unittest.main()
