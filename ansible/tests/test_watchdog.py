import importlib.util
import io
import json
import os
import smtplib
import ssl
import struct
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from urllib.error import HTTPError

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location(
    "external_watchdog", ROOT / "modules/platform/watchdog.py"
)
watchdog = importlib.util.module_from_spec(spec)
spec.loader.exec_module(watchdog)


class FakeSMTP:
    def __init__(self, *, starttls_error=None, login_error=None, rejected=None):
        self.events = []
        self.starttls_error, self.login_error = starttls_error, login_error
        self.rejected = rejected or {}
        self.messages = []

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.events.append("close")

    def ehlo(self):
        self.events.append("ehlo")
        return 250, b"fixture"

    def starttls(self, *, context):
        self.events.append("tls")
        assert context.check_hostname and context.verify_mode == ssl.CERT_REQUIRED
        assert context.minimum_version >= ssl.TLSVersion.TLSv1_2
        if self.starttls_error:
            raise self.starttls_error
        return 220, b"fixture"

    def login(self, username, password):
        self.events.append("login")
        if self.login_error:
            raise self.login_error

    def send_message(self, message, *, from_addr, to_addrs):
        self.events.append("send")
        self.messages.append((message, from_addr, to_addrs))
        return self.rejected


class WatchdogContract(unittest.TestCase):
    def config(self):
        return {
            "targets": [
                {"name": "application", "url": "https://application.example/health"}
            ],
            "alertWebhook": "https://alerts.example/fixture-credential",
        }

    def test_missing_alert_credential_prevents_probes(self):
        config = self.config()
        config.pop("alertWebhook")
        with (
            patch.object(watchdog, "request") as request,
            self.assertRaisesRegex(watchdog.WatchdogError, "credential"),
        ):
            watchdog.run(config, Path("/unused/state"))
        request.assert_not_called()

    def test_explicit_expected_http_error_status_is_healthy(self):
        error = HTTPError(
            "https://application.example/private",
            403,
            "Forbidden",
            {},
            io.BytesIO(b"forbidden"),
        )
        with patch("urllib.request.build_opener") as opener:
            opener.return_value.open.side_effect = error
            self.assertEqual(
                watchdog.request("https://application.example/private"),
                (403, b"forbidden"),
            )

    def test_probes_identify_the_monitor_and_preserve_authorization(self):
        error = HTTPError(
            "https://application.example/private",
            403,
            "Forbidden",
            {},
            io.BytesIO(b"forbidden"),
        )
        with patch("urllib.request.build_opener") as opener:
            opener.return_value.open.side_effect = error
            watchdog.request(
                "https://application.example/private",
                headers={"Authorization": "Bearer fixture"},
            )
        headers = dict(opener.return_value.open.call_args.args[0].header_items())
        self.assertEqual(headers["User-agent"], "fredrir-platform-watchdog/1.0")
        self.assertEqual(headers["Authorization"], "Bearer fixture")

    def test_failed_probe_alert_delivery_must_succeed_before_acknowledgement(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / "state.json"
            with (
                patch.object(
                    watchdog,
                    "request",
                    side_effect=[
                        OSError("probe failure"),
                        OSError("secret URL withheld"),
                    ],
                ),
                self.assertRaisesRegex(
                    watchdog.WatchdogError, "^Alert delivery failed$"
                ),
            ):
                watchdog.run(self.config(), state)
            self.assertFalse(state.exists())
            with patch.object(
                watchdog, "request", side_effect=[OSError("probe failure"), (204, b"")]
            ):
                self.assertEqual(watchdog.run(self.config(), state), 1)
            self.assertEqual(json.loads(state.read_text()), ["application"])

    def test_missing_stale_and_future_heartbeats_fail(self):
        config = {
            "heartbeats": [
                {
                    "name": "control",
                    "url": "https://deadman.example/status",
                    "timestampField": "last_ping",
                    "maxAgeSeconds": 300,
                }
            ],
            "alertWebhook": "https://alerts.example/fixture",
        }
        for value, failed in [
            (950, []),
            (600, ["control"]),
            (1200, ["control"]),
            (None, ["control"]),
        ]:
            with (
                self.subTest(value=value),
                patch.object(
                    watchdog,
                    "request",
                    return_value=(200, json.dumps({"last_ping": value}).encode()),
                ),
            ):
                self.assertEqual(watchdog.check_health(config, 1000), failed)

    def test_deadman_is_confirmed_only_when_all_checks_are_healthy(self):
        config = self.config() | {"deadmanURL": "https://deadman.example/ping/fixture"}
        with (
            tempfile.TemporaryDirectory() as directory,
            patch.object(
                watchdog, "request", side_effect=[(200, b"ok"), (200, b"ok")]
            ) as request,
        ):
            self.assertEqual(watchdog.run(config, Path(directory) / "state.json"), 0)
            self.assertEqual(request.call_args.args[0], config["deadmanURL"])


class EmailWatchdogContract(unittest.TestCase):
    def test_delivery_test_has_honest_subject_and_never_probes_or_changes_incident_state(
        self,
    ):
        config, client = self.config(), FakeSMTP()
        with (
            patch(
                "sys.argv", ["watchdog", "--config", "/private/config", "--test-email"]
            ),
            patch.object(watchdog, "read_config", return_value=config),
            patch.object(watchdog.smtplib, "SMTP", return_value=client),
            patch.object(watchdog, "run") as incidents,
            patch.object(watchdog, "request") as probes,
        ):
            self.assertEqual(watchdog.main(), 0)
        self.assertEqual(
            client.messages[0][0]["Subject"], "Infrastructure alert delivery test"
        )
        incidents.assert_not_called()
        probes.assert_not_called()

    def config(self):
        return {
            "targets": [
                {"name": "application", "url": "https://application.example/health"}
            ],
            "alertEmail": {
                "host": "smtp.example.com",
                "port": 587,
                "tls": "starttls",
                "username": "fixture-user",
                "password": "fixture-password",
                "from": "alerts@example.com",
                "to": "operator@example.com",
            },
        }

    def test_starttls_precedes_auth_and_recovery_is_delivered(self):
        config, client = self.config(), FakeSMTP()
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / "state.json"
            with (
                patch.object(watchdog, "request", return_value=(503, b"")),
                patch.object(watchdog.smtplib, "SMTP", return_value=client) as factory,
            ):
                self.assertEqual(watchdog.run(config, state), 1)
            self.assertEqual(
                client.events, ["ehlo", "tls", "ehlo", "login", "send", "close"]
            )
            self.assertEqual(factory.call_args.kwargs["timeout"], 5)
            message, sender, recipients = client.messages[0]
            self.assertEqual(sender, "alerts@example.com")
            self.assertEqual(recipients, ["operator@example.com"])
            self.assertNotIn("fixture-password", message.as_string())
            self.assertEqual(message["Subject"], "Infrastructure checks failed")
            client.events.clear()
            with (
                patch.object(watchdog, "request", return_value=(200, b"")),
                patch.object(watchdog.smtplib, "SMTP", return_value=client),
            ):
                self.assertEqual(watchdog.run(config, state), 0)
            self.assertEqual(
                client.messages[-1][0]["Subject"], "Infrastructure checks recovered"
            )
            self.assertEqual(json.loads(state.read_text()), [])

    def test_implicit_tls_requires_validated_context_and_never_uses_plain_smtp(self):
        config, client = self.config()["alertEmail"], FakeSMTP()
        config.update(port=465, tls="implicit")
        with (
            patch.object(
                watchdog.smtplib, "SMTP_SSL", return_value=client
            ) as encrypted,
            patch.object(watchdog.smtplib, "SMTP") as plain,
        ):
            watchdog.send_email(config, "Infrastructure checks failed: fixture", True)
        plain.assert_not_called()
        context = encrypted.call_args.kwargs["context"]
        self.assertTrue(context.check_hostname)
        self.assertEqual(context.verify_mode, ssl.CERT_REQUIRED)
        self.assertGreaterEqual(context.minimum_version, ssl.TLSVersion.TLSv1_2)
        self.assertEqual(client.events, ["ehlo", "login", "send", "close"])

    def test_missing_starttls_or_invalid_certificate_never_authenticates(self):
        for error in [
            smtplib.SMTPNotSupportedError("fixture"),
            ssl.SSLCertVerificationError("fixture"),
        ]:
            with self.subTest(error=type(error).__name__):
                client = FakeSMTP(starttls_error=error)
                with (
                    patch.object(watchdog.smtplib, "SMTP", return_value=client),
                    self.assertRaises(OSError),
                ):
                    watchdog.send_email(self.config()["alertEmail"], "failed", True)
                self.assertNotIn("login", client.events)
                self.assertNotIn("send", client.events)

    def test_failed_auth_or_recipient_never_acknowledges_alert_and_hides_details(self):
        for client in [
            FakeSMTP(
                login_error=smtplib.SMTPAuthenticationError(535, b"fixture-password")
            ),
            FakeSMTP(rejected={"operator@example.com": (550, b"fixture-password")}),
        ]:
            with tempfile.TemporaryDirectory() as directory:
                state = Path(directory) / "state.json"
                with (
                    patch.object(watchdog, "request", return_value=(503, b"")),
                    patch.object(watchdog.smtplib, "SMTP", return_value=client),
                    self.assertRaisesRegex(
                        watchdog.WatchdogError, "^Alert delivery failed$"
                    ),
                ):
                    watchdog.run(self.config(), state)
                self.assertFalse(state.exists())

    def test_ambiguous_transport_cleartext_and_header_injection_fail_before_probes(
        self,
    ):
        mutations = [
            lambda c: c.update(alertWebhook="https://alerts.example/fixture"),
            lambda c: c["alertEmail"].update(tls="none"),
            lambda c: c["alertEmail"].update(verify=False),
            lambda c: c["alertEmail"].update(
                to="operator@example.com\r\nBcc: other@example.com"
            ),
            lambda c: c["alertEmail"].update(to="a@example.com,b@example.com"),
            lambda c: c["alertEmail"].update(host="smtp.example.com:587"),
            lambda c: c["alertEmail"].update(port=True),
            lambda c: c["alertEmail"].update(password="fixture\npassword"),
            lambda c: c["alertEmail"].update(username="x" * 257),
        ]
        for mutate in mutations:
            config = self.config()
            mutate(config)
            with (
                self.subTest(configKeys=list(config)),
                patch.object(watchdog, "request") as probe,
                patch.object(watchdog.smtplib, "SMTP") as smtp,
                self.assertRaises((watchdog.WatchdogError, ValueError)),
            ):
                watchdog.run(config, Path("/unused/state"))
            probe.assert_not_called()
            smtp.assert_not_called()

    def test_oversized_email_is_refused_before_connection(self):
        with (
            patch.object(watchdog.smtplib, "SMTP") as smtp,
            self.assertRaisesRegex(watchdog.WatchdogError, "exceeds"),
        ):
            watchdog.send_email(self.config()["alertEmail"], "x" * 32768, True)
        smtp.assert_not_called()

    def test_same_failure_is_not_resent_after_successful_acknowledgement(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / "state.json"
            state.write_text(json.dumps(["application"]))
            with (
                patch.object(watchdog, "request", return_value=(503, b"")),
                patch.object(watchdog, "send_email") as email,
            ):
                self.assertEqual(watchdog.run(self.config(), state), 1)
            email.assert_not_called()

    def test_runtime_credential_file_must_be_private_regular_and_bounded(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config"
            path.write_text(json.dumps(self.config()))
            path.chmod(0o600)
            self.assertEqual(watchdog.read_config(path), self.config())
            path.chmod(0o644)
            with self.assertRaises(watchdog.WatchdogError):
                watchdog.read_config(path)
            path.chmod(0o600)
            link = Path(directory) / "link"
            link.symlink_to(path)
            with self.assertRaises(OSError):
                watchdog.read_config(link)
            link.unlink()
            os.link(path, link)
            with self.assertRaises(watchdog.WatchdogError):
                watchdog.read_config(path)
            link.unlink()
            path.write_bytes(b"x" * 65537)
            with self.assertRaises(watchdog.WatchdogError):
                watchdog.read_config(path)

    def test_systemd_acl_credential_allows_only_root_and_the_service_user(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config"
            path.write_text(json.dumps(self.config()))
            path.chmod(0o440)
            info = list(path.stat())
            info[4:6] = [0, 0]
            entries = [
                (1, 4, 0xFFFFFFFF),
                (2, 4, 62000),
                (4, 0, 0xFFFFFFFF),
                (16, 4, 0xFFFFFFFF),
                (32, 0, 0xFFFFFFFF),
            ]
            acl = struct.pack("<I", 2) + b"".join(
                struct.pack("<HHI", *entry) for entry in entries
            )
            with (
                patch.object(watchdog.os, "fstat", return_value=os.stat_result(info)),
                patch.object(watchdog.os, "geteuid", return_value=62000),
                patch.object(watchdog.os, "getxattr", return_value=acl, create=True),
            ):
                self.assertEqual(watchdog.read_config(path), self.config())

    def test_group_readable_credential_rejects_missing_or_broader_acl(self):
        entries = [
            (1, 4, 0xFFFFFFFF),
            (2, 4, 62000),
            (4, 0, 0xFFFFFFFF),
            (16, 4, 0xFFFFFFFF),
            (32, 0, 0xFFFFFFFF),
        ]
        variants = [b"", b"\x02", struct.pack("<I", 3)]
        for index, replacement in [
            (1, (2, 4, 62001)),
            (2, (4, 4, 0xFFFFFFFF)),
            (4, (32, 4, 0xFFFFFFFF)),
        ]:
            changed = entries.copy()
            changed[index] = replacement
            variants.append(
                struct.pack("<I", 2)
                + b"".join(struct.pack("<HHI", *entry) for entry in changed)
            )
        variants.append(
            struct.pack("<I", 2)
            + b"".join(
                struct.pack("<HHI", *entry) for entry in entries + [(2, 4, 62001)]
            )
        )
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config"
            path.write_text(json.dumps(self.config()))
            path.chmod(0o440)
            info = list(path.stat())
            info[4:6] = [0, 0]
            for acl in variants:
                with (
                    self.subTest(acl=acl.hex()),
                    patch.object(
                        watchdog.os, "fstat", return_value=os.stat_result(info)
                    ),
                    patch.object(watchdog.os, "geteuid", return_value=62000),
                    patch.object(
                        watchdog.os, "getxattr", return_value=acl, create=True
                    ),
                    self.assertRaises(watchdog.WatchdogError),
                ):
                    watchdog.read_config(path)


if __name__ == "__main__":
    unittest.main()
