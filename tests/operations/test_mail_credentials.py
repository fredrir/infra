import importlib.util
import io
import json
import os
import subprocess
import tempfile
import types
import unittest
from contextlib import redirect_stdout
from datetime import UTC, datetime
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location(
    "mail_credentials", ROOT / "scripts/operations/mail_credentials.py"
)
mail = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(mail)
KEY_ID = "AKIA0123456789ABCDEF"
SECRET = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"


def fixture():
    return {
        "schemaVersion": 1,
        "awsAccountId": "123456789012",
        "region": "eu-north-1",
        "iamUser": "fredrir-platform-alerts-smtp",
        "doppler": {"project": "infra", "config": "ops", **mail.KEYS},
        "watchdog": {
            "targets": [{"name": "public", "url": "https://example.com/"}],
            "alertEmail": {
                "host": "email-smtp.eu-north-1.amazonaws.com",
                "port": 587,
                "tls": "starttls",
                "from": "alerts@fredrir.com",
                "to": "operator@example.net",
            },
        },
        "target": {
            "sshAlias": "linode",
            "hostname": "localhost",
            "tailscaleIPv4": "100.86.241.75",
            "configPath": "/var/lib/platform-watchdog-config/config.json",
        },
    }


class FakeServices:
    def __init__(self, config):
        self.config, self.secrets, self.keys = config, {}, []
        self.create_count, self.fail_put, self.lost_response, self.fail_delete = (
            0,
            None,
            False,
            False,
        )
        self.account, self.verified, self.policy = (
            config["awsAccountId"],
            True,
            mail.expected_policy(config),
        )

    def aws(self, service, operation, *arguments):
        policy_arn = f"arn:aws:iam::{self.config['awsAccountId']}:policy/platform/{self.config['iamUser']}-send"
        if operation == "get-caller-identity":
            return {"Account": self.account}
        if operation == "get-email-identity":
            return {
                "VerifiedForSendingStatus": self.verified,
                "DkimAttributes": {"Status": "SUCCESS", "SigningEnabled": True},
            }
        if operation == "get-account":
            return {
                "SendingEnabled": True,
                "EnforcementStatus": "HEALTHY",
                "ProductionAccessEnabled": False,
            }
        if operation == "get-user":
            return {
                "User": {
                    "Arn": f"arn:aws:iam::{self.config['awsAccountId']}:user/platform/{self.config['iamUser']}",
                    "PermissionsBoundary": {"PermissionsBoundaryArn": policy_arn},
                }
            }
        if operation == "list-attached-user-policies":
            return {"AttachedPolicies": [{"PolicyArn": policy_arn}]}
        if operation == "get-policy":
            return {"Policy": {"DefaultVersionId": "v1"}}
        if operation == "get-policy-version":
            return {"PolicyVersion": {"Document": self.policy}}
        if operation == "create-access-key":
            self.create_count += 1
            key = {
                "UserName": self.config["iamUser"],
                "AccessKeyId": KEY_ID,
                "Status": "Active",
                "CreateDate": datetime.now(UTC).isoformat(),
            }
            self.keys.append(key)
            if self.lost_response:
                raise mail.CredentialError("response lost")
            return {"AccessKey": key | {"SecretAccessKey": SECRET}}
        if operation == "update-access-key":
            self.keys[0]["Status"] = "Inactive"
            return {}
        if operation == "delete-access-key":
            if self.fail_delete:
                raise mail.CredentialError("deletion unavailable")
            self.keys = []
            return {}
        raise AssertionError(operation)

    def names(self):
        return set(self.secrets)

    def access_keys(self):
        return list(self.keys)

    def get(self, key):
        return self.secrets[key]

    def put(self, key, value):
        self.secrets[key] = value
        if key == self.fail_put:
            raise mail.CredentialError("response lost after secret write")

    def delete(self, keys):
        for key in keys:
            self.secrets.pop(key, None)


class CredentialTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.directory.chmod(0o700)
        self.path = self.directory / "receipt.json"
        self.config = fixture()
        self.services = FakeServices(self.config)

    def test_smtp_derivation_matches_independent_openssl_vector(self):
        self.assertEqual(
            mail.smtp_password(SECRET, "eu-north-1"),
            "BPfy85A/4ChxYc0s4mIG5bpIlvRl5AP1uupODe8KaHpf",
        )
        with self.assertRaises(mail.CredentialError):
            mail.smtp_password(SECRET, "us-east-1")

    def test_both_ses_identity_resources_keep_exact_sender_and_every_recipient_constraints(
        self,
    ):
        statement = mail.expected_policy(self.config)["Statement"][0]
        self.assertEqual(
            statement["Resource"],
            [
                "arn:aws:ses:eu-north-1:123456789012:identity/fredrir.com",
                "arn:aws:ses:eu-north-1:123456789012:identity/operator@example.net",
            ],
        )
        self.assertEqual(statement["Action"], ["ses:SendRawEmail"])
        self.assertEqual(
            statement["Condition"],
            {
                "StringEquals": {"ses:FromAddress": "alerts@fredrir.com"},
                "ForAllValues:StringEquals": {
                    "ses:Recipients": ["operator@example.net"]
                },
                "Null": {"ses:Recipients": "false"},
            },
        )

    def test_incomplete_or_broadened_ses_authority_fails_before_key_creation(self):
        for failure in [
            "sender-only",
            "recipient-domain",
            "another-account",
            "from-recipient",
            "extra-recipient",
            "missing-recipients",
        ]:
            with self.subTest(failure=failure):
                services = FakeServices(self.config)
                statement = services.policy["Statement"][0]
                if failure == "sender-only":
                    statement["Resource"].pop()
                elif failure == "recipient-domain":
                    statement["Resource"][1] = (
                        "arn:aws:ses:eu-north-1:123456789012:identity/example.net"
                    )
                elif failure == "another-account":
                    statement["Resource"][1] = (
                        "arn:aws:ses:eu-north-1:000000000000:identity/operator@example.net"
                    )
                elif failure == "from-recipient":
                    statement["Condition"]["StringEquals"]["ses:FromAddress"] = (
                        "operator@example.net"
                    )
                elif failure == "extra-recipient":
                    statement["Condition"]["ForAllValues:StringEquals"][
                        "ses:Recipients"
                    ].append("other@example.net")
                else:
                    del statement["Condition"]["Null"]
                with self.assertRaises(mail.CredentialError):
                    mail.issue(self.config, services, self.path)
                self.assertEqual(services.create_count, 0)
                self.assertFalse(self.path.exists())

    def test_success_stores_only_regional_smtp_credentials_and_private_recipient(self):
        result = mail.issue(self.config, self.services, self.path)
        self.assertEqual(result["status"], "stored")
        self.assertFalse(result["mailSent"])
        self.assertEqual(set(self.services.secrets), set(mail.KEYS.values()))
        self.assertNotIn(SECRET, self.services.secrets.values())
        text = self.path.read_text()
        for value in [
            SECRET,
            self.services.get(mail.KEYS["passwordKey"]),
            self.config["watchdog"]["alertEmail"]["to"],
        ]:
            self.assertNotIn(value, text)
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)
        value = mail.runtime_config(
            self.config, self.services, mail.receipt_for(self.path, self.config)
        )
        self.assertEqual(value["alertEmail"]["username"], KEY_ID)

    def test_partial_doppler_write_revokes_iam_key_and_removes_its_new_secrets(self):
        self.services.fail_put = mail.KEYS["passwordKey"]
        with self.assertRaises(mail.CredentialError):
            mail.issue(self.config, self.services, self.path)
        self.assertEqual(self.services.keys, [])
        self.assertEqual(self.services.secrets, {})
        self.assertEqual(mail.private_json(self.path)["status"], "rolled-back")

    def test_lost_create_response_recovers_only_the_new_dedicated_user_key(self):
        self.services.lost_response = True
        with self.assertRaises(mail.CredentialError):
            mail.issue(self.config, self.services, self.path)
        self.assertEqual(self.services.create_count, 1)
        self.assertEqual(self.services.keys, [])
        self.assertEqual(mail.private_json(self.path)["status"], "rolled-back")

    def test_failed_revocation_is_explicit_and_never_claims_cleanup(self):
        self.services.fail_put, self.services.fail_delete = (
            mail.KEYS["usernameKey"],
            True,
        )
        with patch.object(mail.time, "sleep"), self.assertRaises(mail.CredentialError):
            mail.issue(self.config, self.services, self.path)
        self.assertEqual(mail.private_json(self.path)["status"], "recovery-required")
        self.assertEqual(self.services.keys[0]["Status"], "Inactive")

    def test_recovery_refuses_another_aws_account(self):
        self.services.fail_put, self.services.fail_delete = (
            mail.KEYS["usernameKey"],
            True,
        )
        with patch.object(mail.time, "sleep"), self.assertRaises(mail.CredentialError):
            mail.issue(self.config, self.services, self.path)
        receipt = mail.private_json(self.path)
        self.services.account, self.services.fail_delete = "000000000000", False
        self.assertFalse(mail.recover(self.config, self.services, self.path, receipt))
        self.assertEqual(len(self.services.keys), 1)

    def test_existing_keys_or_secrets_are_never_overwritten(self):
        for target in ["iam", "doppler"]:
            with self.subTest(target=target):
                services = FakeServices(self.config)
                if target == "iam":
                    services.keys = [{"AccessKeyId": "existing"}]
                else:
                    services.secrets[mail.KEYS["usernameKey"]] = "existing"
                with self.assertRaises(mail.CredentialError):
                    mail.issue(self.config, services, self.path)
                self.assertEqual(services.create_count, 0)
        self.assertFalse(self.path.exists())

    def test_unverified_domain_and_changed_authority_fail_before_key_creation(self):
        for failure in ["identity", "policy", "account"]:
            with self.subTest(failure=failure):
                services = FakeServices(self.config)
                if failure == "identity":
                    services.verified = False
                elif failure == "policy":
                    services.policy["Statement"][0]["Resource"] = ["*"]
                else:
                    services.account = "000000000000"
                with self.assertRaises(mail.CredentialError):
                    mail.issue(self.config, services, self.path)
                self.assertEqual(services.create_count, 0)

    def test_existing_approved_recipient_survives_rollback(self):
        self.services.secrets[mail.KEYS["recipientKey"]] = self.config["watchdog"][
            "alertEmail"
        ]["to"]
        self.services.fail_put = mail.KEYS["passwordKey"]
        with self.assertRaises(mail.CredentialError):
            mail.issue(self.config, self.services, self.path)
        self.assertEqual(
            self.services.secrets, {mail.KEYS["recipientKey"]: "operator@example.net"}
        )

    def test_stored_receipt_cannot_be_used_as_implicit_credential_rotation(self):
        mail.issue(self.config, self.services, self.path)
        with self.assertRaises(mail.CredentialError):
            mail.recover(
                self.config, self.services, self.path, mail.private_json(self.path)
            )
        self.assertEqual(self.services.keys[0]["Status"], "Active")

    def test_receipt_commit_failure_revokes_the_unacknowledged_credential(self):
        original = mail.write_json

        def write(path, value, **kwargs):
            if value.get("status") == "stored":
                raise OSError("fixture disk failure")
            return original(path, value, **kwargs)

        with (
            patch.object(mail, "write_json", side_effect=write),
            self.assertRaises(mail.CredentialError),
        ):
            mail.issue(self.config, self.services, self.path)
        self.assertEqual(self.services.keys, [])
        self.assertEqual(self.services.secrets, {})
        self.assertEqual(mail.private_json(self.path)["status"], "rolled-back")

    def test_remote_install_is_private_idempotent_and_refuses_changed_existing_data(
        self,
    ):
        target = self.directory / "remote"
        script = mail.REMOTE_CONFIG.replace(
            "parent='/var/lib/platform-watchdog-config'", f"parent={str(target)!r}"
        )
        original_fstat = os.fstat

        def root_stat(descriptor):
            result = list(original_fstat(descriptor))
            result[4] = 0
            return os.stat_result(result)

        value = {"alertEmail": {"password": "fixture-password"}}

        def install(payload):
            with (
                patch.object(mail.os, "geteuid", return_value=0),
                patch.object(mail.os, "fstat", side_effect=root_stat),
                patch("socket.gethostname", return_value="localhost"),
                patch.object(
                    mail.subprocess,
                    "run",
                    return_value=subprocess.CompletedProcess(
                        [], 0, b"100.86.241.75\n", b""
                    ),
                ),
                patch(
                    "sys.stdin",
                    types.SimpleNamespace(
                        buffer=io.BytesIO(json.dumps(payload).encode())
                    ),
                ),
                redirect_stdout(io.StringIO()),
            ):
                exec(script, {})

        install(value)
        self.assertEqual((target / "config.json").stat().st_mode & 0o777, 0o600)
        self.assertEqual((target / "config.json").stat().st_nlink, 1)
        install(value)
        with self.assertRaises(AssertionError):
            install({"alertEmail": {"password": "changed"}})
        self.assertEqual(json.loads((target / "config.json").read_text()), value)
        self.assertEqual({path.name for path in target.iterdir()}, {"config.json"})

    def test_cli_filters_credentials_and_sends_secret_only_over_stdin(self):
        with patch.dict(
            os.environ,
            {
                "DOPPLER_TOKEN": "parent-secret",
                "AWS_SECRET_ACCESS_KEY": "parent-aws",
                "HTTP_PROXY": "http://untrusted",
            },
        ):
            services = mail.Services(self.config)
        with patch.object(
            mail.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ) as run:
            services.put(mail.KEYS["passwordKey"], "fixture-password")
        arguments = run.call_args.args[0]
        self.assertNotIn("fixture-password", arguments)
        self.assertEqual(run.call_args.kwargs["input"], b"fixture-password")
        self.assertFalse(
            set(run.call_args.kwargs["env"])
            & {"DOPPLER_TOKEN", "AWS_SECRET_ACCESS_KEY", "HTTP_PROXY"}
        )
        self.assertIn("--no-verify-tls=false", arguments)

    def test_settings_reject_public_permissions_wrong_target_and_embedded_credentials(
        self,
    ):
        path = self.directory / "settings.json"
        mail.write_json(path, self.config, fresh=True)
        self.assertEqual(mail.settings(path), self.config)
        path.chmod(0o644)
        with self.assertRaises(ValueError):
            mail.settings(path)
        path.chmod(0o600)
        for field in ["target", "credentials"]:
            value = fixture()
            if field == "target":
                value["target"]["sshAlias"] = "other-host"
            else:
                value["watchdog"]["alertEmail"]["password"] = "unexpected"
            mail.write_json(path, value)
            with self.assertRaises(mail.CredentialError):
                mail.settings(path)


if __name__ == "__main__":
    unittest.main()
