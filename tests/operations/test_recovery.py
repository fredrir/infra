import importlib.util
import io
import json
from pathlib import Path
import tempfile
import tarfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('recovery', ROOT / 'scripts/operations/recovery.py')
recovery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(recovery)


class RecoveryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.snapshot = self.root / 'snapshot'
        self.token = self.root / 'token'
        recovery.write_private(self.snapshot, b'test snapshot')
        recovery.write_private(self.token, b'test-only-token')

    def tearDown(self):
        self.temp.cleanup()

    def bundle(self):
        destination = self.root / 'bundle'
        recovery.etcd_bundle(self.snapshot, self.token, destination, 'v1.36.3+k3s1')
        return destination

    def test_tampered_snapshot_prevents_restore_plan(self):
        directory = self.bundle()
        (directory / 'snapshot').write_bytes(b'altered')
        with self.assertRaisesRegex(recovery.RecoveryError, 'integrity'):
            recovery.etcd_restore_plan(directory, self.root / 'new-data', 'v1.36.3+k3s1')

    def test_world_readable_token_is_rejected(self):
        self.token.chmod(0o644)
        with self.assertRaisesRegex(recovery.RecoveryError, '0600'):
            self.bundle()

    def test_token_symlink_is_rejected(self):
        link = self.root / 'token-link'
        link.symlink_to(self.token)
        with self.assertRaises(recovery.RecoveryError):
            recovery.etcd_bundle(self.snapshot, link, self.root / 'bundle', 'v1.36.3+k3s1')

    def test_wrong_version_or_existing_target_prevents_restore_plan(self):
        directory = self.bundle()
        with self.assertRaisesRegex(recovery.RecoveryError, 'version'):
            recovery.etcd_restore_plan(directory, self.root / 'new-data', 'v1.36.4+k3s1')
        with self.assertRaisesRegex(recovery.RecoveryError, 'nonexistent'):
            recovery.etcd_restore_plan(directory, self.root, 'v1.36.3+k3s1')

    def test_restore_plan_references_token_file_without_exposing_token(self):
        directory = self.bundle()
        result = recovery.etcd_restore_plan(directory, self.root / 'new-data', 'v1.36.3+k3s1')
        self.assertIn('--token-file', result['command'])
        self.assertNotIn('test-only-token', json.dumps(result))

    def test_tar_import_rejects_traversal_and_removes_partial_material(self):
        archive = self.root / 'recovery.tar'
        with tarfile.open(archive, 'w') as output:
            member = tarfile.TarInfo('../escape')
            member.size = 4
            output.addfile(member, io.BytesIO(b'data'))
        archive.chmod(0o600)
        with self.assertRaises(recovery.RecoveryError):
            recovery.import_etcd_tar(archive, self.root / 'imported')
        self.assertFalse((self.root / 'imported').exists())

    def test_tar_import_verifies_exact_bundle_and_sets_private_modes(self):
        directory = self.bundle()
        archive = self.root / 'recovery.tar'
        with tarfile.open(archive, 'w') as output:
            for name in ['snapshot', 'server-token', 'manifest.json']:
                output.add(directory / name, arcname='./' + name)
        archive.chmod(0o600)
        recovery.import_etcd_tar(archive, self.root / 'imported')
        self.assertEqual((self.root / 'imported/server-token').stat().st_mode & 0o777, 0o600)

    def test_failed_backup_does_not_advance_last_success(self):
        metric = self.root / 'restic.prom'
        recovery.backup_metrics(metric, 'postgres', 3600)
        self.assertIn('restic_expected_job', metric.read_text())
        self.assertNotIn('last_success', metric.read_text())
        recovery.backup_metrics(metric, 'postgres', 3600, success=True, now=100)
        recovery.backup_metrics(metric, 'postgres', 3600, now=200)
        self.assertIn('} 100\n', metric.read_text())
        self.assertNotIn('} 200\n', metric.read_text())

    def test_postgres_restore_refuses_populated_database(self):
        directory = self.root / 'pg'
        directory.mkdir(mode=0o700)
        dump = directory / 'database.dump'
        recovery.write_private(dump, b'dump')
        recovery.write_private(directory / 'manifest.json', json.dumps({'kind': 'postgres-logical', 'sha256': recovery.digest(dump)}).encode())
        with patch.object(recovery, 'checked', return_value=b'4\n') as command:
            with self.assertRaisesRegex(recovery.RecoveryError, 'empty'):
                recovery.postgres_restore(directory, {'PGDATABASE': 'recovery'}, ['public.accounts'])
            self.assertEqual(command.call_count, 1)

    def test_postgres_restore_checks_exit_status_and_actual_tables(self):
        directory = self.root / 'pg'
        directory.mkdir(mode=0o700)
        dump = directory / 'database.dump'
        recovery.write_private(dump, b'dump')
        recovery.write_private(directory / 'manifest.json', json.dumps({'kind': 'postgres-logical', 'sha256': recovery.digest(dump)}).encode())
        with patch.object(recovery, 'checked', side_effect=[b'0\n', b'', b'3\n']) as command:
            recovery.postgres_restore(directory, {'PGDATABASE': 'recovery'}, ['public.accounts'])
            restore = command.call_args_list[1].args[0]
            self.assertIn('--exit-on-error', restore)
            self.assertIn('--single-transaction', restore)
            self.assertIn('"public"."accounts"', command.call_args_list[2].args[0][-1])


if __name__ == '__main__':
    unittest.main()
