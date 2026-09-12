import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
import recurring_backup_install as install
from evacuation_guards import Filesystem


class InstallTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.fs = Filesystem(self.root, os.geteuid())
        self.created = {}
        self.fs.directory(install.BASE, 0o700, self.created)
        self.fs.directory("/etc/systemd/system", 0o755, self.created)
        self.receipt = {
            "schemaVersion": 1,
            "kind": "recurring-backup-installation",
            "host": "fredrir-09",
            "status": "installing",
            "files": {},
            "directories": {},
            "keyPlanned": False,
        }
        self.fs.save(install.RECEIPT, self.receipt)

    def test_write_records_original_before_change_and_rollback_restores_exact_bytes_mode(
        self,
    ):
        path = "/etc/systemd/system/restic-backups-llunde-backend.service"
        old = self.fs.write(path, b"original", 0o644)
        install.install_file(self.fs, self.receipt, path, b"replacement", replace=True)
        self.assertEqual(self.fs.read(path)[1], b"replacement")
        original = self.receipt["files"][path]["original"]
        self.assertEqual(self.fs.read(original)[1], b"original")
        with (
            patch.object(install, "host_identity"),
            patch.object(install, "dormant_target"),
        ):
            result = install.rollback("fredrir-09", fs=self.fs, commands=Mock())
        restored = self.fs.metadata(path)
        self.assertEqual(restored["sha256"], old["sha256"])
        self.assertEqual(
            (restored["uid"], restored["gid"], restored["mode"]),
            (old["uid"], old["gid"], old["mode"]),
        )
        self.assertEqual(result["status"], "rolled-back")

    def test_unrelated_existing_file_is_not_overwritten(self):
        path = "/etc/systemd/system/restic-backups-llunde-backend.service"
        original = self.fs.write(path, b"foreign", 0o644)
        with self.assertRaisesRegex(ValueError, "unrelated"):
            install.install_file(self.fs, self.receipt, path, b"ours")
        self.assertEqual(self.fs.metadata(path), original)
        self.assertEqual(self.receipt["files"], {})

    def test_changed_owned_file_blocks_all_rollback_changes(self):
        a, b = (
            "/etc/systemd/system/restic-backups-llunde-backend.service",
            "/etc/systemd/system/restic-backups-llunde-backend.timer",
        )
        install.install_file(self.fs, self.receipt, a, b"ours-a")
        install.install_file(self.fs, self.receipt, b, b"ours-b")
        self.fs.path(b).write_bytes(b"changed")
        with (
            patch.object(install, "host_identity"),
            patch.object(install, "dormant_target"),
        ):
            with self.assertRaisesRegex(ValueError, "changed"):
                install.rollback("fredrir-09", fs=self.fs, commands=Mock())
        self.assertEqual(self.fs.read(a)[1], b"ours-a")
        self.assertEqual(self.fs.read(b)[1], b"changed")

    def test_interrupted_write_retains_private_journal_without_a_success_claim(self):
        path = "/etc/systemd/system/restic-backups-llunde-backend.service"
        with patch.object(self.fs, "write", side_effect=OSError("disk full")):
            with self.assertRaises(OSError):
                install.install_file(self.fs, self.receipt, path, b"ours")
        receipt = self.fs.read_json(install.RECEIPT)
        self.assertEqual(receipt["status"], "installing")
        self.assertIsNone(receipt["files"][path]["after"])
        self.assertFalse(self.fs.metadata(path)["exists"])
        with (
            patch.object(install, "host_identity"),
            patch.object(install, "dormant_target"),
        ):
            self.assertEqual(
                install.rollback("fredrir-09", fs=self.fs, commands=Mock())["status"],
                "rolled-back",
            )

    def test_generated_key_comment_binds_partial_creation_to_the_receipt(self):
        self.fs.directory(str(Path(install.KEY).parent), 0o700, self.created)
        self.receipt["nonce"] = "fixture"
        key = self.fs.path(install.KEY)
        previous = os.umask(0o077)
        try:
            subprocess.run(
                [
                    "ssh-keygen",
                    "-q",
                    "-t",
                    "ed25519",
                    "-N",
                    "",
                    "-C",
                    "infra-backup-status:fixture",
                    "-f",
                    str(key),
                ],
                capture_output=True,
                check=True,
            )
        finally:
            os.umask(previous)

        class Commands:
            def run(self, args, **kwargs):
                result = subprocess.run(
                    args[:-1] + [str(key)], capture_output=True, check=True
                )
                return 0, result.stdout

        public = install.reconcile_key(self.fs, self.receipt, Commands())
        self.assertEqual(len(public.split()), 2)
        self.assertEqual(
            set(self.receipt["files"]), {install.KEY, install.KEY + ".pub"}
        )
        altered = dict(self.receipt, nonce="unrelated", files={})
        with self.assertRaisesRegex(ValueError, "ownership"):
            install.reconcile_key(self.fs, altered, Commands())

    def test_symlink_parent_cannot_redirect_installation(self):
        outside = self.root / "outside"
        outside.mkdir()
        self.fs.path("/etc/redirect").symlink_to(outside)
        with self.assertRaises(OSError):
            install.install_file(
                self.fs, self.receipt, "/etc/redirect/sentinel", b"unsafe"
            )
        self.assertFalse((outside / "sentinel").exists())

    def test_unknown_receiver_identity_or_supplementary_group_is_rejected(self):
        nonce = "a" * 16
        expected = {
            "uid": 999,
            "gid": 999,
            "groupGid": 999,
            "groups": [999],
            "groupMembers": [],
            "home": install.HOME,
            "shell": "/bin/sh",
            "gecos": "infra-backup-receiver-" + nonce,
        }
        install.validate_account(expected, nonce)
        for changes in (
            {"uid": 0},
            {"groups": [27, 999]},
            {"gecos": "unrelated"},
            {"home": "/home/existing"},
            {"groupMembers": ["other"]},
        ):
            with self.assertRaises(ValueError):
                install.validate_account(expected | changes, nonce)

    @unittest.skipUnless(shutil.which("useradd"), "Native shadow useradd required")
    def test_native_receiver_comment_validation_without_account_creation(self):
        comment = install.account_comment("a" * 16)
        good = subprocess.run(
            ["useradd", "--comment", comment, "--help"], capture_output=True, timeout=5
        )
        bad = subprocess.run(
            ["useradd", "--comment", "infra-backup-receiver:" + "a" * 16, "--help"],
            capture_output=True,
            timeout=5,
        )
        self.assertEqual(good.returncode, 0)
        self.assertIn(b"Usage:", good.stdout)
        self.assertEqual(bad.returncode, 3)
        self.assertIn(b"invalid comment", bad.stderr)

    @unittest.skipUnless(
        Path("/usr/sbin/sshd").exists() and shutil.which("ssh-keygen"),
        "Native OpenSSH required",
    )
    def test_actual_full_sshd_composition_preserves_existing_admins_and_restricts_receiver(
        self,
    ):
        key = self.root / "host-key"
        subprocess.run(
            ["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(key)],
            capture_output=True,
            check=True,
        )
        original, candidate = self.root / "sshd-original", self.root / "sshd-candidate"
        original.write_text(
            "HostKey "
            + str(key)
            + "\nPasswordAuthentication no\nPermitUserEnvironment no\nAcceptEnv LANG LC_*\nMatch User existing-user\n    PermitTTY no\n"
        )
        stanza = (
            ROOT / "ansible/roles/evacuation_backup/templates/receiver.sshd.j2"
        ).read_text()
        candidate.write_text(original.read_text() + stanza)

        class Commands:
            def run(self, args, **kwargs):
                result = subprocess.run(args, capture_output=True, check=True)
                return result.returncode, result.stdout

        result = install.ssh_contract(
            Commands(), original, candidate, ["root", "existing-user"]
        )
        self.assertTrue(result["nativeSyntaxVerified"])
        self.assertEqual(len(result["adminContexts"]), 4)
        candidate.write_text(
            "HostKey "
            + str(key)
            + "\nPasswordAuthentication yes\nPermitUserEnvironment no\nAcceptEnv LANG LC_*\n"
            + stanza
        )
        with self.assertRaisesRegex(ValueError, "administrator"):
            install.ssh_contract(
                Commands(), original, candidate, ["root", "existing-user"]
            )

    def test_templates_leave_one_reconciliation_condition_to_existing_guard(self):
        for name in ("backup.service.j2", "backup.timer.j2"):
            data = (
                ROOT / "ansible/roles/evacuation_backup/templates" / name
            ).read_text()
            self.assertNotIn(
                "ConditionPathExists=!/var/lib/platform-evacuation/reconciliation-locked",
                data,
            )
        data = (
            ROOT / "ansible/roles/evacuation_backup/templates/backup.service.j2"
        ).read_text()
        self.assertIn("Requisite=user@2001.service", data)
        self.assertIn("TimeoutStartSec=960s", data)

    def test_actual_writer_attempt_marker_blocks_dormant_install(self):
        import evacuation_target

        (self.root / "target-writer-start-attempted").touch()
        with (
            patch.object(evacuation_target, "MARKERS", self.root),
            patch.object(evacuation_target, "TARGET_BASE", self.root),
        ):
            with self.assertRaisesRegex(ValueError, "absent"):
                install.dormant_target(Mock())

    def receiver_receipt(self):
        self.receipt.update(
            host="fredrir-06",
            accountPlanned=False,
            account=None,
            nonce="fixture",
            sshProof={"adminContexts": {}},
            adminUsers=["root"],
        )
        self.fs.directory("/etc/ssh", 0o755, {})
        return self.receipt

    def test_rollback_before_ssh_replacement_accepts_unchanged_original(self):
        receipt = self.receiver_receipt()
        original = self.fs.write(install.SSH_CONFIG, b"original ssh", 0o644)
        with patch.object(install, "replace_file", side_effect=OSError("interrupted")):
            with self.assertRaises(OSError):
                install.install_file(
                    self.fs, receipt, install.SSH_CONFIG, b"new ssh", replace=True
                )
        with (
            patch.object(install, "host_identity"),
            patch.object(install, "original_ssh_proof"),
            patch.object(
                install,
                "ssh_contract",
                side_effect=AssertionError("New SSH settings were never installed"),
            ),
        ):
            result = install.rollback("fredrir-06", fs=self.fs, commands=Mock())
        self.assertEqual(result["status"], "rolled-back")
        self.assertEqual(self.fs.metadata(install.SSH_CONFIG), original)

    def test_rollback_resume_after_restore_before_progress_write(self):
        path = "/etc/systemd/system/" + install.UNITS[0]
        self.fs.write(path, b"old", 0o644)
        install.install_file(self.fs, self.receipt, path, b"new", replace=True)
        save = self.fs.save

        def interrupted_save(name, value):
            if value["files"][path].get("rolledBack") is not None:
                raise OSError("interrupted after file restore")
            return save(name, value)

        with (
            patch.object(install, "host_identity"),
            patch.object(install, "dormant_target"),
        ):
            with (
                patch.object(self.fs, "save", side_effect=interrupted_save),
                self.assertRaises(OSError),
            ):
                install.rollback("fredrir-09", fs=self.fs, commands=Mock())
            self.assertEqual(self.fs.read(path)[1], b"old")
            result = install.rollback("fredrir-09", fs=self.fs, commands=Mock())
        self.assertEqual(result["status"], "rolled-back")

    def test_rollback_retries_ssh_reload_after_files_already_restored(self):
        receipt = self.receiver_receipt()
        self.fs.write(install.SSH_CONFIG, b"original ssh", 0o644)
        install.install_file(
            self.fs, receipt, install.SSH_CONFIG, b"new ssh", replace=True
        )
        with (
            patch.object(install, "host_identity"),
            patch.object(install, "original_ssh_proof"),
            patch.object(install, "ssh_contract", return_value=receipt["sshProof"]),
        ):
            with (
                patch.object(
                    install, "reload_ssh", side_effect=OSError("reload failed")
                ),
                self.assertRaises(OSError),
            ):
                install.rollback("fredrir-06", fs=self.fs, commands=Mock())
            self.assertEqual(self.fs.read(install.SSH_CONFIG)[1], b"original ssh")
            with patch.object(install, "reload_ssh") as reload:
                result = install.rollback("fredrir-06", fs=self.fs, commands=Mock())
                reload.assert_called_once()
        self.assertEqual(result["status"], "rolled-back")

    def test_directory_transition_recovers_gid_and_refuses_different_inode(self):
        created = {}
        self.fs.directory(str(install.DIRECTORY), 0o755, created)
        info = self.fs.path(str(install.DIRECTORY)).stat()
        self.receipt["directories"] = created
        self.receipt["receiverDirectory"] = dict(
            created[str(install.DIRECTORY)],
            beforeGID=info.st_gid,
            afterUID=info.st_uid,
            afterGID=info.st_gid,
        )
        install.reconcile_directory(self.fs, self.receipt)
        self.assertEqual(
            self.receipt["directories"][str(install.DIRECTORY)]["gid"], info.st_gid
        )
        self.receipt["receiverDirectory"]["inode"] += 1
        with self.assertRaisesRegex(ValueError, "ownership"):
            install.reconcile_directory(self.fs, self.receipt)


if __name__ == "__main__":
    unittest.main()
