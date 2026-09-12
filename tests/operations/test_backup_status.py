import importlib.util
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
import backup_status as status

SPEC = importlib.util.spec_from_file_location(
    "local_watchdog", ROOT / "modules/platform/watchdog.py"
)
watchdog = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(watchdog)


def payload():
    return {
        "schemaVersion": 1,
        "job": status.JOB,
        "host": "fredrir-09",
        "snapshotId": "a" * 64,
        "archiveSHA256": "b" * 64,
        "recoveryPointAt": 1000,
        "completedAt": 1010,
    }


class StatusTests(unittest.TestCase):
    @unittest.skipUnless(
        Path("/usr/sbin/sshd").exists() and shutil.which("ssh-keygen"),
        "Native OpenSSH tools required",
    )
    def test_native_sshd_matches_only_the_restricted_account(self):
        with tempfile.TemporaryDirectory() as temporary:
            key = Path(temporary) / "host_key"
            subprocess.run(
                ["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(key)],
                check=True,
                capture_output=True,
            )
            config = ROOT / "ansible/roles/evacuation_backup/templates/receiver.sshd.j2"

            def effective(user):
                result = subprocess.run(
                    [
                        "/usr/sbin/sshd",
                        "-T",
                        "-h",
                        str(key),
                        "-f",
                        str(config),
                        "-C",
                        f"user={user},addr=85.190.100.72,host=fredrir-09",
                    ],
                    capture_output=True,
                    check=True,
                )
                return dict(
                    line.split(" ", 1) for line in result.stdout.decode().splitlines()
                )

            restricted = effective("infra-backup-status")
            self.assertEqual(
                restricted["forcecommand"],
                "/usr/bin/timeout -s TERM -k 1 8 /usr/bin/python3 -I /usr/local/libexec/infra-backup-status",
            )
            self.assertEqual(
                restricted["authorizedkeysfile"],
                "/etc/ssh/authorized_keys/infra-backup-status",
            )
            self.assertEqual(restricted["authenticationmethods"], "publickey")
            self.assertEqual(restricted["disableforwarding"], "yes")
            self.assertEqual(restricted["trustedusercakeys"], "none")
            self.assertEqual(restricted["permittty"], "no")
            self.assertEqual(restricted["permituserrc"], "no")
            self.assertEqual(effective("existing-user")["forcecommand"], "none")

    def test_connection_accepts_only_exact_source_with_no_remote_command(self):
        environment = {"SSH_CONNECTION": "85.190.100.72 12345 172.232.145.251 22"}
        status.validate_connection(environment)
        for changes in [
            {"SSH_ORIGINAL_COMMAND": "sh"},
            {"SSH_ORIGINAL_COMMAND": "internal-sftp"},
            {"SSH_CONNECTION": "100.87.168.66 12345 172.232.145.251 22"},
            {"SSH_CONNECTION": "85.190.100.72 0 172.232.145.251 22"},
            {"SSH_CONNECTION": ""},
        ]:
            with self.assertRaises(ValueError):
                status.validate_connection(environment | changes)

    def test_key_cannot_add_another_command_or_forwarding_option(self):
        key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILj/ObqEIPVA2jEJ2V7azJPSjKTvXlcMCbkbon0EoS4N"
        rendered = status.authorized_key(key)
        self.assertTrue(
            rendered.startswith(
                'restrict,from="85.190.100.72",command="/usr/bin/timeout -s TERM -k 1 8 /usr/bin/python3 -I /usr/local/libexec/infra-backup-status" '
            )
        )
        for invalid in [
            key + "\n" + key,
            'command="sh" ' + key,
            key + " trailing-comment",
            key.replace("ssh-ed25519", "ssh-rsa"),
        ]:
            with self.assertRaises(ValueError):
                status.authorized_key(invalid)

    def test_receiver_records_local_time_and_rejects_replay_without_changing_file(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            with patch.object(
                status,
                "open_directory",
                side_effect=lambda *args: os.open(
                    directory, os.O_RDONLY | os.O_DIRECTORY
                ),
            ):
                status.receive(payload(), 1020, directory)
                path = directory / (status.JOB + ".json")
                before = path.read_bytes()
                self.assertEqual(json.loads(before)["receivedAt"], 1020)
                self.assertEqual(path.stat().st_mode & 0o777, 0o644)
                with self.assertRaisesRegex(ValueError, "Repeated"):
                    status.receive(payload(), 1021, directory)
                self.assertEqual(path.read_bytes(), before)
                next_value = payload() | {
                    "recoveryPointAt": 1021,
                    "completedAt": 1022,
                    "snapshotId": "c" * 64,
                }
                status.receive(next_value, 1023, directory)
                self.assertEqual(json.loads(path.read_bytes())["receivedAt"], 1023)

    def test_malformed_stale_future_and_path_payloads_are_rejected(self):
        for value in [
            payload() | {"job": "../escape"},
            payload() | {"completedAt": 1200},
            payload() | {"recoveryPointAt": True},
            payload() | {"path": "/tmp/escape"},
            payload() | {"snapshotId": "latest"},
        ]:
            with self.assertRaises(ValueError):
                status.validate_payload(value, 1020)
        with self.assertRaises(ValueError):
            status.validate_payload(payload(), 4000)

    def test_input_timeout_and_limit_are_enforced_on_real_pipes(self):
        read, write = os.pipe()
        try:
            started = time.monotonic()
            with self.assertRaisesRegex(ValueError, "timed out"):
                status.bounded_input(read, seconds=0.02)
            self.assertLess(time.monotonic() - started, 1)
            os.write(write, b"x" * 4097)
            with self.assertRaisesRegex(ValueError, "limit"):
                status.bounded_input(read, seconds=0.2)
        finally:
            os.close(read)
            os.close(write)

    def test_unexpected_symlink_or_permissions_preserve_outside_file(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            sentinel = directory / "sentinel"
            sentinel.write_text("preserve")
            (directory / (status.JOB + ".json")).symlink_to(sentinel)
            with (
                patch.object(
                    status,
                    "open_directory",
                    side_effect=lambda *args: os.open(
                        directory, os.O_RDONLY | os.O_DIRECTORY
                    ),
                ),
                self.assertRaises(OSError),
            ):
                status.receive(payload(), 1020, directory)
            self.assertEqual(sentinel.read_text(), "preserve")

    def test_watchdog_detects_missing_stale_and_future_local_receipts_without_network(
        self,
    ):
        check = {"name": "application backup", "job": status.JOB, "maxAgeSeconds": 7200}
        config = {
            "localHeartbeats": [check],
            "alertWebhook": "https://alerts.example/credential",
        }
        watchdog.validate_config(config)
        for value, now, failures in [
            (payload() | {"receivedAt": 1020}, 1021, []),
            (payload() | {"receivedAt": 1020}, 8300, ["application backup"]),
            (payload() | {"receivedAt": 10000}, 1021, ["application backup"]),
        ]:
            with (
                patch.object(watchdog, "read_local_backup", return_value=value),
                patch.object(watchdog, "request") as network,
            ):
                self.assertEqual(watchdog.check_health(config, now), failures)
                network.assert_not_called()
        with (
            patch.object(watchdog, "read_local_backup", side_effect=FileNotFoundError),
            patch.object(watchdog, "request") as network,
        ):
            self.assertEqual(
                watchdog.check_health(config, 1020), ["application backup"]
            )
            network.assert_not_called()

    def test_local_watchdog_cannot_be_pointed_at_arbitrary_files(self):
        check = {"name": "backup", "job": status.JOB, "maxAgeSeconds": 7200}
        for changes in [
            {"job": "../../secrets"},
            {"path": "/etc/shadow"},
            {"maxAgeSeconds": 10},
        ]:
            with self.assertRaises(watchdog.WatchdogError):
                watchdog.validate_config(
                    {
                        "localHeartbeats": [check | changes],
                        "alertWebhook": "https://alerts.example/credential",
                    }
                )


if __name__ == "__main__":
    unittest.main()
