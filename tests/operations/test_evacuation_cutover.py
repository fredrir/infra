import hashlib
import importlib.util
import io
import json
import os
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
SPEC = importlib.util.spec_from_file_location(
    "evacuation_cutover", ROOT / "scripts/operations/evacuation_cutover.py"
)
cutover = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(cutover)


def identity(direction="forward"):
    hosts = (
        ("fredrir-05", "fredrir-09")
        if direction == "forward"
        else ("fredrir-09", "fredrir-05")
    )
    return {
        "direction": direction,
        "source": hosts[0],
        "destination": hosts[1],
        "candidateSHA256": "a" * 64,
        "imageIDs": {name: "sha256:" + "b" * 64 for name in cutover.DATA_IMAGES},
    }


def fence(direction="forward", observed=100):
    host = identity(direction)["source"]
    return {
        "schemaVersion": 1,
        "kind": "evacuation-fence-observation",
        "host": host,
        "hostname": cutover.HOSTS[host],
        "bootId": "12345678-1234-1234-1234-123456789abc",
        "guardFilesSHA256": "c" * 64,
        "observedAt": observed,
        "applicationWriterInactive": True,
        "connectorInactive": True,
        "valkeyInactive": True,
        "persistentGuardsVerified": True,
        "guardUserManagerEvaluationVerified": True,
        "reconciliationInactive": True,
        "postgresOtherClients": 0,
        "administrativeWritesExcludedByOperator": True,
        "valkeyGracefulExitCode": 0,
        "valkeyAofWriteStatusBeforeStop": "ok",
        "valkeyRewriteInactiveBeforeStop": True,
    }


def archive_bytes(extra=None, manifest=None):
    content = {
        "appendonlydir/appendonly.aof.manifest": manifest
        or b"file appendonly.aof.1.base.rdb seq 1 type b\nfile appendonly.aof.1.incr.aof seq 1 type i\n",
        "appendonlydir/appendonly.aof.1.base.rdb": b"REDIS-fixture",
        "appendonlydir/appendonly.aof.1.incr.aof": b"*1\r\n$4\r\nPING\r\n",
    }
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w") as archive:
        for name in (".", "appendonlydir"):
            member = tarfile.TarInfo(name)
            member.type, member.mode, member.uid, member.gid = (
                tarfile.DIRTYPE,
                0o750,
                999,
                1000,
            )
            archive.addfile(member)
        for name, value in content.items():
            member = tarfile.TarInfo(name)
            member.size, member.mode, member.uid, member.gid = (
                len(value),
                0o640,
                999,
                999,
            )
            archive.addfile(member, io.BytesIO(value))
        if extra is not None:
            if extra.size < 0:
                archive.fileobj.write(extra.tobuf(format=tarfile.GNU_FORMAT))
                archive.offset += tarfile.BLOCKSIZE
            else:
                archive.addfile(extra, io.BytesIO(b"x" * extra.size))
    return output.getvalue()


class CutoverTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.root.chmod(0o700)
        self.bundle = self.root / "pair"
        self.bundle.mkdir(mode=0o700)
        for name, content in {
            "database.dump": b"PGDMP-fixture",
            "valkey.tar": archive_bytes(),
        }.items():
            self.write(self.bundle / name, content)

    def write(self, path, content):
        path.write_bytes(content)
        path.chmod(0o600)

    def files(self):
        return {
            name: {
                "bytes": (self.bundle / name).stat().st_size,
                "sha256": hashlib.sha256((self.bundle / name).read_bytes()).hexdigest(),
            }
            for name in ("database.dump", "valkey.tar")
        }

    def seal(self, direction="forward", before=None, after=None):
        before = before or fence(direction)
        after = after or fence(direction, 150)
        after.setdefault("exportedFiles", self.files())
        for name, document in [("before", before), ("after", after)]:
            self.write(self.root / name, json.dumps(document).encode())
        with patch.object(
            cutover,
            "pair_identity",
            side_effect=lambda candidate, transfer: identity(transfer),
        ):
            return cutover.seal_pair(
                self.bundle,
                self.root,
                direction,
                self.root / "before",
                self.root / "after",
            )

    def test_paired_manifest_binds_both_files_and_namespace_ownership_without_claiming_restore(
        self,
    ):
        result = self.seal()
        self.assertEqual(cutover.verify_pair(self.bundle), result)
        self.assertEqual(result["files"], self.files())
        self.assertFalse(result["nativeRestoreVerified"])
        self.assertFalse(result["writerStopPerformedByVerifier"])
        self.assertEqual(result["valkeyEntries"]["appendonlydir"]["gid"], 1000)
        self.assertEqual(
            result["valkeyEntries"]["appendonlydir/appendonly.aof.1.base.rdb"]["gid"],
            999,
        )
        self.assertEqual((self.bundle / "manifest.json").stat().st_mode & 0o777, 0o600)
        import evacuation_backup

        self.assertEqual(
            evacuation_backup.validate_bundle(self.bundle)["kind"],
            "evacuation-paired-state",
        )

    def test_reverse_pair_requires_target_fence_and_preserves_image_identity(self):
        result = self.seal("reverse")
        self.assertEqual(
            (result["source"], result["destination"]), ("fredrir-09", "fredrir-05")
        )
        self.assertEqual(result["imageIDs"], identity()["imageIDs"])
        self.assertEqual(cutover.verify_pair(self.bundle), result)

    def test_sealed_pair_round_trip_preserves_fences_and_namespace_metadata(self):
        import evacuation_backup

        manifest = self.seal()
        archived = {}

        def transport(arguments, *, source=None, output=None, maximum=262144):
            if arguments[0] == "backup":
                archived["bytes"] = source.read()
                archived["tag"] = arguments[arguments.index("--tag") + 1]
                return json.dumps(
                    {"message_type": "summary", "snapshot_id": "e" * 64}
                ).encode()
            if arguments[0] == "cat":
                return json.dumps(
                    {
                        "hostname": "fredrir-09",
                        "paths": ["/evacuation-state.tar"],
                        "tags": [archived["tag"]],
                    }
                ).encode()
            self.assertEqual(arguments, ["dump", "e" * 64, "/evacuation-state.tar"])
            self.assertLessEqual(len(archived["bytes"]), maximum)
            output.write(archived["bytes"])
            return b""

        from unittest.mock import Mock

        restic = Mock()
        restic.identity.return_value = "f" * 64
        restic.call.side_effect = transport
        with patch.object(evacuation_backup, "require_upload_host"):
            receipt = evacuation_backup.backup(self.bundle, restic, self.root)
        restored = self.root / "restored"
        proof = evacuation_backup.restore(receipt, restored, restic)
        self.assertTrue(proof["verified"])
        self.assertFalse(proof["applicationRestoreVerified"])
        self.assertEqual(cutover.verify_pair(restored), manifest)
        self.assertEqual(
            {path.name for path in restored.iterdir()},
            {"database.dump", "valkey.tar", "manifest.json"},
        )
        for path in self.bundle.iterdir():
            self.assertEqual(path.read_bytes(), (restored / path.name).read_bytes())

    def test_changed_database_bytes_or_namespace_metadata_refuse_verification(self):
        manifest = self.seal()
        self.write(self.bundle / "database.dump", b"PGDMP-changed")
        with self.assertRaisesRegex(ValueError, "checksum"):
            cutover.verify_pair(self.bundle)
        self.write(self.bundle / "database.dump", b"PGDMP-fixture")
        manifest["valkeyEntries"]["appendonlydir"]["gid"] = 999
        self.write(self.bundle / "manifest.json", json.dumps(manifest).encode())
        with self.assertRaisesRegex(ValueError, "ownership"):
            cutover.verify_pair(self.bundle)

    def test_closing_fence_must_bind_the_exported_file_hashes(self):
        after = fence(observed=150) | {"exportedFiles": self.files()}
        after["exportedFiles"]["database.dump"]["sha256"] = "d" * 64
        with self.assertRaisesRegex(ValueError, "Closing fence"):
            self.seal(after=after)
        self.assertFalse((self.bundle / "manifest.json").exists())

    def test_restarted_source_changed_guards_and_excessive_window_leave_no_manifest(
        self,
    ):
        for changes in [
            {"bootId": "abcdef12-1234-1234-1234-123456789abc"},
            {"guardFilesSHA256": "d" * 64},
            {"observedAt": 401},
            {"observedAt": 99},
        ]:
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                self.seal(after=fence(observed=150) | changes)
            self.assertFalse((self.bundle / "manifest.json").exists())

    def test_active_or_uncertain_writers_and_incomplete_aof_persistence_are_rejected(
        self,
    ):
        changes = [
            {key: False}
            for key in (
                "applicationWriterInactive",
                "connectorInactive",
                "valkeyInactive",
                "persistentGuardsVerified",
                "guardUserManagerEvaluationVerified",
                "reconciliationInactive",
                "administrativeWritesExcludedByOperator",
                "valkeyRewriteInactiveBeforeStop",
            )
        ]
        changes += [
            {"postgresOtherClients": 1},
            {"postgresOtherClients": False},
            {"valkeyGracefulExitCode": 137},
            {"valkeyGracefulExitCode": False},
            {"valkeyAofWriteStatusBeforeStop": "err"},
            {"observedAt": float("nan")},
            {"observedAt": float("inf")},
            {"bootId": "-" * 36},
        ]
        for change in changes:
            with self.subTest(change=change), self.assertRaises(ValueError):
                cutover.validate_fence(fence() | change, "fredrir-05")

    def test_archive_traversal_absolute_paths_links_and_specials_are_rejected(self):
        for name, kind in [
            ("../outside", tarfile.REGTYPE),
            ("/", tarfile.DIRTYPE),
            ("appendonlydir/link", tarfile.SYMTYPE),
            ("appendonlydir/appendonly.aof.2.incr.aof", tarfile.LNKTYPE),
            ("appendonlydir/device", tarfile.CHRTYPE),
        ]:
            entry = tarfile.TarInfo(name)
            entry.type, entry.linkname = kind, "/etc/passwd"
            self.write(self.bundle / "valkey.tar", archive_bytes(entry))
            with self.subTest(name=name, kind=kind), self.assertRaises(ValueError):
                cutover.valkey_archive(self.bundle / "valkey.tar", os.geteuid())

    def test_archive_requires_every_manifest_part_and_rejects_extra_or_duplicate_files(
        self,
    ):
        manifests = [
            b"file appendonly.aof.9.base.rdb seq 9 type b\n",
            b"file ../outside seq 1 type b\n",
            b"file appendonly.aof.1.base.rdb seq 1 type b\nfile appendonly.aof.1.base.rdb seq 1 type b\n",
        ]
        for manifest in manifests:
            self.write(self.bundle / "valkey.tar", archive_bytes(manifest=manifest))
            with self.subTest(manifest=manifest), self.assertRaises(ValueError):
                cutover.valkey_archive(self.bundle / "valkey.tar", os.geteuid())
        duplicate = tarfile.TarInfo("appendonlydir/appendonly.aof.1.base.rdb")
        self.write(self.bundle / "valkey.tar", archive_bytes(duplicate))
        with self.assertRaisesRegex(ValueError, "Duplicate"):
            cutover.valkey_archive(self.bundle / "valkey.tar", os.geteuid())

    def test_archive_host_ids_special_modes_and_negative_sizes_are_rejected(self):
        for fields in [{"uid": 297606}, {"mode": 0o4640}, {"size": -1}]:
            entry = tarfile.TarInfo("dump.rdb")
            for key, value in fields.items():
                setattr(entry, key, value)
            self.write(self.bundle / "valkey.tar", archive_bytes(entry))
            with self.subTest(fields=fields), self.assertRaises(ValueError):
                cutover.valkey_archive(self.bundle / "valkey.tar", os.geteuid())

    def test_invalid_postgres_format_cannot_be_sealed_as_recoverable_custom_dump(self):
        self.write(self.bundle / "database.dump", b"not-a-custom-dump")
        with self.assertRaisesRegex(ValueError, "PostgreSQL custom"):
            self.seal()
        self.assertFalse((self.bundle / "manifest.json").exists())

    def test_existing_manifest_and_linked_input_are_never_overwritten(self):
        self.seal()
        original = (self.bundle / "manifest.json").read_bytes()
        with self.assertRaisesRegex(ValueError, "Fresh"):
            self.seal()
        self.assertEqual((self.bundle / "manifest.json").read_bytes(), original)
        (self.bundle / "database.dump").unlink()
        (self.bundle / "database.dump").symlink_to(self.bundle / "manifest.json")
        with self.assertRaises((OSError, ValueError)):
            cutover.verify_pair(self.bundle)

    def test_any_target_writer_start_attempt_requires_reverse_copy_even_after_failed_health(
        self,
    ):
        self.assertEqual(
            cutover.rollback_path(
                target_writer_start_attempted=False,
                target_writer_inactive=True,
                target_connector_inactive=True,
            ),
            "original-source-data-eligible",
        )
        self.assertEqual(
            cutover.rollback_path(
                target_writer_start_attempted=True,
                target_writer_inactive=True,
                target_connector_inactive=True,
            ),
            "fenced-reverse-copy-required",
        )
        for attempted, writer, connector in [
            (None, True, True),
            (False, False, True),
            (True, True, False),
        ]:
            with self.assertRaises(ValueError):
                cutover.rollback_path(
                    target_writer_start_attempted=attempted,
                    target_writer_inactive=writer,
                    target_connector_inactive=connector,
                )

    def test_plan_is_nonexecuting_bounded_and_preserves_original_source_rollback_reserve(
        self,
    ):
        with patch.object(cutover, "pair_identity", return_value=identity()):
            plan = cutover.cutover_plan(self.root)
            for budget in (True, 900, 1801):
                with self.assertRaises(ValueError):
                    cutover.cutover_plan(self.root, budget)
        self.assertFalse(plan["execute"])
        self.assertFalse(plan["liveMutationImplemented"])
        self.assertFalse(plan["automaticRollback"])
        self.assertFalse(plan["guaranteedMaximumOutage"])
        self.assertEqual(
            plan["maximumForwardPhaseSeconds"]
            + plan["originalSourceRollbackReserveSeconds"],
            plan["outageBudgetSeconds"],
        )
        paths = {entry["path"] for entry in plan["guardFiles"]}
        self.assertEqual(len(paths), 12)
        self.assertTrue(
            all(
                path.endswith(".service.d/95-evacuation-fence.conf")
                or path.endswith(".timer.d/95-evacuation-fence.conf")
                for path in paths
            )
        )
        self.assertTrue(plan["rollback"]["expiryDoesNotAuthorizeDataLoss"])
        self.assertFalse(plan["exportCommands"]["execute"])
        self.assertIn("--kill-after=5s", plan["exportCommands"]["postgres"])
        self.assertIn("--lock-wait-timeout=10s", plan["exportCommands"]["postgres"])
        self.assertEqual(plan["exportCommands"]["valkey"][:2], ["podman", "unshare"])


if __name__ == "__main__":
    unittest.main()
