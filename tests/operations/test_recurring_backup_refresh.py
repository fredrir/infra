import copy
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "scripts/operations"))
import recurring_backup_refresh as refresh
from evacuation_guards import Filesystem


class Native:
    def __init__(self):
        self.commands = []
        self.state = {"applications": "stopped", "timer": "disabled-inactive"}

    def baseline(self):
        return copy.deepcopy(self.state)

    def reload(self):
        self.commands.append("reload")

    def loaded(self, content):
        self.commands.append(content)


class RefreshTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.bundle = self.root / "recurring-backup-deploy-20260912T000000Z"
        self.bundle.mkdir(mode=0o700)
        self.remote = "/var/lib/infra-evacuation/llunde/" + self.bundle.name
        self.previous = b"[Service]\nType=oneshot\nExecStart=/usr/bin/python3 -B /var/lib/infra-evacuation/llunde/recurring-backup-deploy-20260912T052837Z/helpers/evacuation_recurring_backup.py --candidate /var/lib/infra-evacuation/llunde/unit-promotion-bundle-20260912T044726Z/candidate --credentials /fixed\nMemoryMax=512M\n"
        self.current = self.previous.replace(
            b"/var/lib/infra-evacuation/llunde/recurring-backup-deploy-20260912T052837Z",
            self.remote.encode(),
        ).replace(
            b"/var/lib/infra-evacuation/llunde/unit-promotion-bundle-20260912T044726Z/candidate",
            (self.remote + "/candidate").encode(),
        )
        self.files = {
            "previous.service": self.previous,
            "restic-backups-llunde-backend.service": self.current,
            "helpers/recurring_backup_refresh.py": b"pass\n",
            "helpers/evacuation_recurring_backup.py": b"pass\n",
            "candidate/staging.json": b"{}\n",
            "candidate/units/Caddyfile": b"localhost\n",
        }
        for name, data in self.files.items():
            path = self.bundle / name
            path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            path.write_bytes(data)
            path.chmod(0o600)
        for constant, value in (
            ("OLD_SERVICE_SHA", refresh.digest(self.previous)),
            ("CANDIDATE_SHA", refresh.digest(self.files["candidate/staging.json"])),
        ):
            context = patch.object(refresh, constant, value)
            context.start()
            self.addCleanup(context.stop)
        self.manifest = {
            "schemaVersion": 1,
            "kind": "recurring-backup-refresh",
            "remoteDirectory": self.remote,
            "candidateSHA256": refresh.CANDIDATE_SHA,
            "files": {
                name: {"sha256": refresh.digest(data), "bytes": len(data)}
                for name, data in self.files.items()
            },
        }
        self.manifest_sha = self.save_manifest()
        self.fs_root = self.root / "host"
        self.fs_root.mkdir(mode=0o700)
        self.fs = Filesystem(self.fs_root, owner=os.geteuid())
        for name in (*refresh.PRESERVED, refresh.SERVICE):
            self.fs.directory(str(Path(name).parent), 0o700, {})
            data = self.previous if name == refresh.SERVICE else b"private-fixture"
            self.fs.write(
                name, data, 0o644 if name in (refresh.SERVICE, refresh.TIMER) else 0o600
            )
        self.fs.save(
            refresh.INSTALLATION,
            {
                "kind": "recurring-backup-installation",
                "host": "fredrir-09",
                "status": "installed",
                "files": {
                    refresh.SERVICE: {
                        "desiredSHA256": refresh.OLD_SERVICE_SHA,
                        "after": self.fs.metadata(refresh.SERVICE),
                    }
                },
            },
        )
        self.native = Native()
        self.operation = refresh.Refresh(
            self.bundle, self.manifest, filesystem=self.fs, native=self.native
        )

    def save_manifest(self):
        raw = json.dumps(self.manifest).encode()
        (self.bundle / "manifest.json").write_bytes(raw)
        (self.bundle / "manifest.json").chmod(0o600)
        return refresh.digest(raw)

    def test_refresh_and_rollback_touch_only_owned_service(self):
        preserved = self.operation.preserved()
        record = self.operation.apply()
        self.assertEqual(record["status"], "installed")
        self.assertEqual(self.fs.read(refresh.SERVICE)[1], self.current)
        self.assertEqual(self.operation.preserved(), preserved)
        self.assertEqual(self.operation.verify()["status"], "installed")
        self.assertEqual(self.operation.rollback()["status"], "rolled-back")
        self.assertEqual(self.fs.read(refresh.SERVICE)[1], self.previous)
        self.assertEqual(self.operation.preserved(), preserved)
        self.assertEqual(self.operation.rollback()["status"], "rolled-back")
        self.assertTrue(
            all(
                command == "reload" or isinstance(command, bytes)
                for command in self.native.commands
            )
        )

    def test_existing_receipt_cannot_be_adopted_by_second_apply(self):
        self.operation.apply()
        with self.assertRaisesRegex(ValueError, "Existing refresh"):
            self.operation.apply()

    def test_native_dormant_observer_requests_empty_job_properties(self):
        import evacuation_execution
        import evacuation_guards

        class Commands:
            def user(self, *_args, **_kwargs):
                return 0, b""

            def run(self, args, **_kwargs):
                if "--all" not in args:
                    raise AssertionError("Empty Job would be omitted")
                unit = next(
                    value for value in args if value.startswith("restic-backups-")
                )
                enabled = "disabled" if unit.endswith(".timer") else "static"
                return 0, (
                    "ActiveState=inactive\nSubState=dead\nJob=\n"
                    f"UnitFileState={enabled}\nFragmentPath=/etc/systemd/system/{unit}\n"
                ).encode()

        native = refresh.Native.__new__(refresh.Native)
        native.commands = Commands()
        with (
            patch.object(evacuation_execution, "host_identity"),
            patch.object(evacuation_execution, "unit_state", return_value={}),
            patch.object(evacuation_execution, "stopped", return_value=True),
            patch.object(
                evacuation_guards,
                "verify_installation",
                return_value={"guardFilesSHA256": "a" * 64},
            ),
            patch.object(
                Path, "read_text", return_value="98b6af13dea54f4081903e96458be24f\n"
            ),
        ):
            result = native.baseline()
        self.assertEqual(result[Path(refresh.TIMER).name]["Job"], "")

    def test_changed_key_blocks_refresh_without_service_write(self):
        self.fs.unlink(refresh.PRESERVED[2], self.fs.metadata(refresh.PRESERVED[2]))
        with self.assertRaisesRegex(ValueError, "authority absent"):
            self.operation.apply()
        self.assertEqual(self.fs.read(refresh.SERVICE)[1], self.previous)

    def test_reload_failure_retains_receipt_and_allows_owned_rollback(self):
        with (
            patch.object(
                self.native, "reload", side_effect=RuntimeError("failed reload")
            ),
            self.assertRaises(RuntimeError),
        ):
            self.operation.apply()
        self.assertEqual(self.fs.read_json(refresh.RECEIPT)["status"], "preparing")
        self.operation.rollback()
        self.assertEqual(self.fs.read(refresh.SERVICE)[1], self.previous)

    def test_interruption_after_rename_is_reconciled_from_staged_inode(self):
        original = self.fs.save
        failed = False

        def save(path, value):
            nonlocal failed
            if path == refresh.RECEIPT and value.get("service") and not failed:
                failed = True
                raise RuntimeError("interrupted after rename")
            original(path, value)

        with (
            patch.object(self.fs, "save", side_effect=save),
            self.assertRaises(RuntimeError),
        ):
            self.operation.apply()
        self.operation.rollback()
        self.assertEqual(self.fs.read(refresh.SERVICE)[1], self.previous)

    def test_same_bytes_on_replaced_inode_are_not_owned_for_rollback(self):
        self.operation.apply()
        metadata = self.fs.metadata(refresh.SERVICE)
        replacement = refresh.SERVICE + ".other"
        self.fs.write(replacement, self.current, 0o644)
        with self.fs.parent_fd(refresh.SERVICE) as (parent, name):
            os.replace(
                Path(replacement).name, name, src_dir_fd=parent, dst_dir_fd=parent
            )
        self.assertNotEqual(
            self.fs.metadata(refresh.SERVICE)["inode"], metadata["inode"]
        )
        with self.assertRaisesRegex(ValueError, "ownership changed"):
            self.operation.rollback()

    def test_same_bytes_on_replaced_original_inode_block_apply(self):
        replacement = refresh.SERVICE + ".other"
        self.fs.write(replacement, self.previous, 0o644)
        with self.fs.parent_fd(refresh.SERVICE) as (parent, name):
            os.replace(
                Path(replacement).name, name, src_dir_fd=parent, dst_dir_fd=parent
            )
        with self.assertRaisesRegex(ValueError, "Original service ownership"):
            self.operation.apply()
        self.assertFalse(self.fs.metadata(refresh.RECEIPT)["exists"])

    def test_same_bytes_replaced_temporary_inode_is_not_adopted_or_removed(self):
        original = self.fs.metadata

        def metadata(path, expected=None):
            if path == refresh.SERVICE and expected is not None:
                raise RuntimeError("interrupted before rename")
            return original(path, expected)

        with (
            patch.object(self.fs, "metadata", side_effect=metadata),
            self.assertRaises(RuntimeError),
        ):
            self.operation.apply()
        record = self.fs.read_json(refresh.RECEIPT)
        temporary = record["replacement"]["temporary"]
        replacement = temporary + ".other"
        self.fs.write(replacement, self.current, 0o644)
        with self.fs.parent_fd(temporary) as (parent, name):
            os.replace(
                Path(replacement).name, name, src_dir_fd=parent, dst_dir_fd=parent
            )
        changed = self.fs.metadata(temporary)
        for operation in (
            lambda: self.operation.replace(record, self.current, record["before"]),
            self.operation.rollback,
        ):
            with self.assertRaisesRegex(ValueError, "Temporary service ownership"):
                operation()
            self.assertEqual(self.fs.metadata(temporary), changed)
        self.assertEqual(self.fs.read(refresh.SERVICE)[1], self.previous)

    def test_interruption_before_staged_receipt_retains_unowned_temporary(self):
        original = self.fs.save

        def save(path, value):
            if path == refresh.RECEIPT and (value.get("replacement") or {}).get(
                "staged"
            ):
                raise RuntimeError("interrupted before ownership receipt")
            original(path, value)

        with (
            patch.object(self.fs, "save", side_effect=save),
            self.assertRaises(RuntimeError),
        ):
            self.operation.apply()
        record = self.fs.read_json(refresh.RECEIPT)
        temporary = record["replacement"]["temporary"]
        self.assertTrue(self.fs.metadata(temporary)["exists"])
        with self.assertRaisesRegex(ValueError, "Temporary service ownership"):
            self.operation.rollback()
        self.assertTrue(self.fs.metadata(temporary)["exists"])
        self.assertEqual(self.fs.read(refresh.SERVICE)[1], self.previous)

    def test_marker_drift_and_enablement_drift_block_rollback(self):
        self.operation.apply()
        self.native.state["timer"] = "enabled"
        with self.assertRaisesRegex(ValueError, "baseline changed"):
            self.operation.rollback()
        self.native.state["timer"] = "disabled-inactive"
        path = "/var/lib/platform-evacuation/source-locked"
        self.fs.directory(str(Path(path).parent), 0o700, {})
        self.fs.write(path, b"owned-by-other-operation", 0o644)
        with self.assertRaisesRegex(ValueError, "authority changed"):
            self.operation.rollback()

    def test_bundle_checks_candidate_bytes_and_service_scope(self):
        self.assertEqual(
            refresh.verify_bundle(self.bundle, self.manifest_sha, owner=os.geteuid()),
            self.manifest,
        )
        path = self.bundle / "restic-backups-llunde-backend.service"
        path.write_bytes(self.current.replace(b"MemoryMax=512M", b"MemoryMax=8G"))
        data = path.read_bytes()
        self.manifest["files"][path.name] = {
            "sha256": refresh.digest(data),
            "bytes": len(data),
        }
        with self.assertRaisesRegex(ValueError, "Only ExecStart"):
            refresh.verify_bundle(self.bundle, self.save_manifest(), owner=os.geteuid())

    def test_bundle_rejects_links_extra_directories_and_traversal(self):
        extra = self.bundle / "extra"
        extra.mkdir(mode=0o700)
        with self.assertRaises(ValueError):
            refresh.verify_bundle(self.bundle, self.manifest_sha, owner=os.geteuid())
        extra.rmdir()
        extra.symlink_to(self.bundle / "helpers", target_is_directory=True)
        with self.assertRaises(ValueError):
            refresh.verify_bundle(self.bundle, self.manifest_sha, owner=os.geteuid())
        extra.unlink()
        self.manifest["files"]["../escape"] = {"sha256": "a" * 64, "bytes": 1}
        with self.assertRaisesRegex(ValueError, "path differs"):
            refresh.verify_bundle(self.bundle, self.save_manifest(), owner=os.geteuid())


if __name__ == "__main__":
    unittest.main()
