import argparse
import base64
import fcntl
import hashlib
import hmac
import importlib.util
import json
import os
import re
import resource
import signal
import stat
import subprocess
import time
from datetime import datetime
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location(
    "platform_watchdog", ROOT / "modules/platform/watchdog.py"
)
WATCHDOG = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(WATCHDOG)
KEYS = {
    "usernameKey": "PLATFORM_WATCHDOG_SMTP_USERNAME",
    "passwordKey": "PLATFORM_WATCHDOG_SMTP_PASSWORD",
    "recipientKey": "PLATFORM_ALERT_RECIPIENT",
}


class CredentialError(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise CredentialError(message)


def private_directory(path):
    path = Path(path)
    info = path.lstat()
    require(
        stat.S_ISDIR(info.st_mode)
        and info.st_uid == os.geteuid()
        and stat.S_IMODE(info.st_mode) == 0o700,
        "Private directory required",
    )
    return path


def private_json(path):
    private_directory(Path(path).parent)
    return WATCHDOG.read_config(Path(path))


def write_json(path, value, *, fresh=False):
    path = Path(path)
    private_directory(path.parent)
    if fresh:
        descriptor = os.open(
            path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600
        )
        target = path
    else:
        private_json(path)
        target = path.with_name(path.name + ".tmp")
        descriptor = os.open(
            target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600
        )
    try:
        with os.fdopen(descriptor, "w") as stream:
            json.dump(value, stream, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        if not fresh:
            target.replace(path)
        descriptor = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    except BaseException:
        if target != path:
            target.unlink(missing_ok=True)
        raise


def settings(path):
    value = private_json(path)
    require(
        set(value)
        == {
            "schemaVersion",
            "awsAccountId",
            "region",
            "iamUser",
            "doppler",
            "watchdog",
            "target",
        },
        "Exact mail settings required",
    )
    require(
        value["schemaVersion"] == 1
        and re.fullmatch(r"[0-9]{12}", value["awsAccountId"]),
        "Verified AWS account required",
    )
    require(
        value["region"] == "eu-north-1"
        and value["iamUser"] == "fredrir-platform-alerts-smtp",
        "Dedicated regional SMTP identity required",
    )
    require(
        value["doppler"] == {"project": "infra", "config": "ops", **KEYS},
        "Scoped Doppler keys required",
    )
    config = value["watchdog"]
    require(
        set(config) <= {"targets", "heartbeats", "alertEmail", "deadmanURL"}
        and "alertEmail" in config,
        "Email watchdog settings required",
    )
    require(
        set(config["alertEmail"]) == {"host", "port", "tls", "from", "to"},
        "Credential-free mail settings required",
    )
    require(
        config["alertEmail"]["host"] == "email-smtp.eu-north-1.amazonaws.com"
        and config["alertEmail"]["port"] == 587
        and config["alertEmail"]["tls"] == "starttls"
        and config["alertEmail"]["from"] == "alerts@fredrir.com",
        "Verified platform SMTP endpoint required",
    )
    WATCHDOG.validate_config(
        config
        | {
            "alertEmail": config["alertEmail"]
            | {"username": "validation", "password": "validation"}
        }
    )
    require(
        value["target"]
        == {
            "sshAlias": "linode",
            "hostname": "localhost",
            "tailscaleIPv4": "100.86.241.75",
            "configPath": "/var/lib/platform-watchdog-config/config.json",
        },
        "Verified external host required",
    )
    return value


def fingerprint(value):
    return hashlib.sha256(
        json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()


def smtp_password(secret, region):
    require(
        isinstance(secret, str)
        and len(secret) == 40
        and re.fullmatch(r"[A-Za-z0-9/+]+", secret),
        "Long-term IAM secret required",
    )
    require(region == "eu-north-1", "Approved SMTP region required")
    signature = ("AWS4" + secret).encode()
    for message in ["11111111", region, "ses", "aws4_request", "SendRawEmail"]:
        signature = hmac.new(signature, message.encode(), hashlib.sha256).digest()
    return base64.b64encode(bytes([4]) + signature).decode()


class Services:
    def __init__(self, config):
        self.config = config
        self.environment = {
            key: value
            for key, value in os.environ.items()
            if key in {"PATH", "HOME", "TMPDIR", "SSL_CERT_FILE", "NIX_SSL_CERT_FILE"}
        }
        self.environment.update(
            {"AWS_MAX_ATTEMPTS": "1", "AWS_PAGER": "", "AWS_CLI_AUTO_PROMPT": "off"}
        )

    def call(self, command, data=None):
        try:
            result = subprocess.run(
                command,
                input=data,
                capture_output=True,
                cwd=ROOT,
                env=self.environment,
                timeout=30,
            )
        except (OSError, subprocess.SubprocessError):
            raise CredentialError("Bounded credential operation failed") from None
        require(
            result.returncode == 0 and len(result.stdout) <= 262144,
            "Credential operation failed",
        )
        return result.stdout

    def aws(self, service, operation, *arguments):
        result = self.call(
            [
                "aws",
                service,
                operation,
                *arguments,
                "--region",
                self.config["region"],
                "--output",
                "json",
                "--no-cli-pager",
                "--cli-connect-timeout",
                "5",
                "--cli-read-timeout",
                "15",
            ]
        )
        return json.loads(result) if result.strip() else {}

    def doppler(self, arguments, data=None):
        return self.call(
            [
                "doppler",
                "secrets",
                *arguments,
                "--project",
                "infra",
                "--config",
                "ops",
                "--no-read-env",
                "--no-check-version",
                "--no-verify-tls=false",
                "--attempts",
                "1",
                "--timeout",
                "10s",
                "--api-host",
                "https://api.doppler.com",
            ],
            data,
        )

    def names(self):
        value = json.loads(self.doppler(["--only-names", "--json"]))
        require(isinstance(value, dict), "Doppler names response invalid")
        return set(value)

    def get(self, key):
        require(key in KEYS.values(), "Scoped secret name required")
        return self.doppler(["get", key, "--plain", "--raw"]).decode().strip()

    def put(self, key, value):
        require(key in KEYS.values(), "Scoped secret name required")
        self.doppler(
            ["set", key, "--silent", "--no-interactive", "--visibility", "masked"],
            value.encode(),
        )

    def delete(self, keys):
        require(
            keys and set(keys) <= set(KEYS.values()), "Scoped secret names required"
        )
        self.doppler(["delete", *sorted(keys), "--yes", "--silent"])

    def access_keys(self):
        response = self.aws(
            "iam", "list-access-keys", "--user-name", self.config["iamUser"]
        )
        require(
            not response.get("IsTruncated", False),
            "Complete IAM key inventory required",
        )
        return response["AccessKeyMetadata"]


def expected_policy(config):
    mail = config["watchdog"]["alertEmail"]
    identity_prefix = (
        f"arn:aws:ses:{config['region']}:{config['awsAccountId']}:identity/"
    )
    return {
        "Version": "2012-10-17",
        "Statement": [
            {
                "Effect": "Allow",
                "Action": ["ses:SendRawEmail"],
                "Resource": [
                    identity_prefix + "fredrir.com",
                    identity_prefix + mail["to"],
                ],
                "Condition": {
                    "StringEquals": {"ses:FromAddress": mail["from"]},
                    "ForAllValues:StringEquals": {"ses:Recipients": [mail["to"]]},
                    "Null": {"ses:Recipients": "false"},
                },
            }
        ],
    }


def preflight(config, services):
    require(
        services.aws("sts", "get-caller-identity")["Account"] == config["awsAccountId"],
        "AWS account mismatch",
    )
    identity = services.aws(
        "sesv2", "get-email-identity", "--email-identity", "fredrir.com"
    )
    require(
        identity.get("VerifiedForSendingStatus") is True
        and identity.get("DkimAttributes", {}).get("Status") == "SUCCESS"
        and identity["DkimAttributes"].get("SigningEnabled") is True,
        "SES identity and DKIM must be verified",
    )
    account = services.aws("sesv2", "get-account")
    require(
        account.get("SendingEnabled") is True
        and account.get("EnforcementStatus") == "HEALTHY",
        "Healthy SES sending account required",
    )
    if not account.get("ProductionAccessEnabled"):
        recipient = services.aws(
            "sesv2",
            "get-email-identity",
            "--email-identity",
            config["watchdog"]["alertEmail"]["to"],
        )
        require(
            recipient.get("VerifiedForSendingStatus") is True,
            "SES sandbox recipient must be verified",
        )
    policy_arn = f"arn:aws:iam::{config['awsAccountId']}:policy/platform/{config['iamUser']}-send"
    user = services.aws("iam", "get-user", "--user-name", config["iamUser"])["User"]
    require(
        user["Arn"]
        == f"arn:aws:iam::{config['awsAccountId']}:user/platform/{config['iamUser']}"
        and user.get("PermissionsBoundary", {}).get("PermissionsBoundaryArn")
        == policy_arn,
        "Scoped SMTP user and boundary required",
    )
    attached = services.aws(
        "iam", "list-attached-user-policies", "--user-name", config["iamUser"]
    )
    require(
        not attached.get("IsTruncated")
        and [policy["PolicyArn"] for policy in attached["AttachedPolicies"]]
        == [policy_arn],
        "Exact SMTP policy attachment required",
    )
    policy = services.aws("iam", "get-policy", "--policy-arn", policy_arn)["Policy"]
    document = services.aws(
        "iam",
        "get-policy-version",
        "--policy-arn",
        policy_arn,
        "--version-id",
        policy["DefaultVersionId"],
    )["PolicyVersion"]["Document"]
    require(
        document == expected_policy(config),
        "SMTP authority differs from approved sender and recipient",
    )


def receipt_for(path, config):
    receipt = private_json(path)
    require(
        receipt["schemaVersion"] == 1
        and receipt["kind"] == "smtp-credential-issue"
        and receipt["settingsSHA256"] == fingerprint(config)
        and receipt["iamUser"] == config["iamUser"],
        "Credential receipt mismatch",
    )
    require(
        set(receipt["newDopplerKeys"]) <= set(KEYS.values()),
        "Credential receipt keys invalid",
    )
    return receipt


def recover(config, services, path, receipt):
    require(
        receipt["status"] != "stored",
        "Stored credentials require an explicit rotation procedure",
    )
    try:
        require(
            services.aws("sts", "get-caller-identity")["Account"]
            == config["awsAccountId"],
            "Recovery AWS account mismatch",
        )
        keys = services.access_keys()
        key_id = receipt.get("accessKeyId")
        if key_id is None:
            candidates = [
                key
                for key in keys
                if receipt["startedAt"] - 5
                <= datetime.fromisoformat(
                    key["CreateDate"].replace("Z", "+00:00")
                ).timestamp()
                <= receipt["startedAt"] + 60
            ]
            require(
                len(keys) == len(candidates) == 1,
                "Ambiguous key creation requires IAM reconciliation",
            )
            key_id = candidates[0]["AccessKeyId"]
            receipt["accessKeyId"] = key_id
            write_json(path, receipt)
        require(
            re.fullmatch(r"AKIA[A-Z0-9]{16}", key_id), "Receipt access key ID invalid"
        )
        for attempt in range(3):
            try:
                if any(key["AccessKeyId"] == key_id for key in services.access_keys()):
                    services.aws(
                        "iam",
                        "update-access-key",
                        "--user-name",
                        config["iamUser"],
                        "--access-key-id",
                        key_id,
                        "--status",
                        "Inactive",
                    )
                    services.aws(
                        "iam",
                        "delete-access-key",
                        "--user-name",
                        config["iamUser"],
                        "--access-key-id",
                        key_id,
                    )
                require(
                    not any(
                        key["AccessKeyId"] == key_id for key in services.access_keys()
                    ),
                    "IAM key revocation unconfirmed",
                )
                break
            except CredentialError:
                if attempt == 2:
                    raise
                time.sleep(0.25)
        remaining = services.names() & set(receipt["newDopplerKeys"])
        if remaining:
            username_matches = (
                KEYS["usernameKey"] in remaining
                and services.get(KEYS["usernameKey"]) == key_id
            )
            require(
                KEYS["usernameKey"] not in remaining or username_matches,
                "Doppler SMTP owner changed",
            )
            require(
                KEYS["passwordKey"] not in remaining or username_matches,
                "Unbound Doppler password requires reconciliation",
            )
            require(
                KEYS["recipientKey"] not in remaining
                or services.get(KEYS["recipientKey"])
                == config["watchdog"]["alertEmail"]["to"],
                "Doppler recipient changed",
            )
            services.delete(remaining)
        require(
            not (services.names() & set(receipt["newDopplerKeys"])),
            "Doppler rollback unconfirmed",
        )
        receipt["status"] = "rolled-back"
    except (OSError, ValueError, KeyError, TypeError):
        receipt["status"] = "recovery-required"
    write_json(path, receipt)
    return receipt["status"] == "rolled-back"


def issue(config, services, path):
    preflight(config, services)
    require(
        not services.access_keys(),
        "Initial provisioning requires an empty dedicated IAM user",
    )
    names = services.names()
    require(
        not (names & {KEYS["usernameKey"], KEYS["passwordKey"]}),
        "Existing SMTP secrets must not be overwritten",
    )
    recipient = config["watchdog"]["alertEmail"]["to"]
    if KEYS["recipientKey"] in names:
        require(
            services.get(KEYS["recipientKey"]) == recipient,
            "Existing recipient differs from approved policy",
        )
    receipt = {
        "schemaVersion": 1,
        "kind": "smtp-credential-issue",
        "settingsSHA256": fingerprint(config),
        "iamUser": config["iamUser"],
        "startedAt": int(time.time()),
        "status": "creating",
        "newDopplerKeys": sorted(set(KEYS.values()) - names),
    }
    write_json(path, receipt, fresh=True)
    try:
        created = services.aws(
            "iam", "create-access-key", "--user-name", config["iamUser"]
        )["AccessKey"]
        key_id = created["AccessKeyId"]
        require(
            created["UserName"] == config["iamUser"]
            and created["Status"] == "Active"
            and re.fullmatch(r"AKIA[A-Z0-9]{16}", key_id),
            "Created IAM key identity mismatch",
        )
        receipt.update({"accessKeyId": key_id, "status": "persisting"})
        write_json(path, receipt)
        password = smtp_password(created["SecretAccessKey"], config["region"])
        del created
        services.put(KEYS["usernameKey"], key_id)
        services.put(KEYS["passwordKey"], password)
        if KEYS["recipientKey"] not in names:
            services.put(KEYS["recipientKey"], recipient)
        require(
            services.get(KEYS["usernameKey"]) == key_id
            and hmac.compare_digest(services.get(KEYS["passwordKey"]), password)
            and services.get(KEYS["recipientKey"]) == recipient,
            "Doppler credential readback mismatch",
        )
        require(
            [
                key["AccessKeyId"]
                for key in services.access_keys()
                if key["Status"] == "Active"
            ]
            == [key_id],
            "Unexpected active SMTP keys",
        )
        completed = receipt | {"status": "stored"}
        write_json(path, completed)
    except BaseException:
        recover(config, services, path, receipt)
        raise CredentialError(
            "SMTP provisioning failed; inspect private recovery receipt"
        ) from None
    return {
        "status": "stored",
        "smtpCredentialsInDoppler": True,
        "awsSecretPersisted": False,
        "mailSent": False,
    }


def runtime_config(config, services, receipt):
    require(receipt["status"] == "stored", "Stored credential receipt required")
    preflight(config, services)
    username, password = (
        services.get(KEYS["usernameKey"]),
        services.get(KEYS["passwordKey"]),
    )
    require(
        username == receipt["accessKeyId"]
        and services.get(KEYS["recipientKey"])
        == config["watchdog"]["alertEmail"]["to"],
        "Stored SMTP settings mismatch",
    )
    require(
        any(
            key["AccessKeyId"] == username and key["Status"] == "Active"
            for key in services.access_keys()
        ),
        "Active SMTP key required",
    )
    value = config["watchdog"] | {
        "alertEmail": config["watchdog"]["alertEmail"]
        | {"username": username, "password": password}
    }
    return WATCHDOG.validate_config(value)


REMOTE_CONFIG = """import json,os,socket,stat,subprocess,sys,uuid
assert os.geteuid()==0 and socket.gethostname()=='localhost'
result=subprocess.run(['/usr/bin/tailscale','ip','-4'],capture_output=True,timeout=5,check=True)
assert result.stdout.strip()==b'100.86.241.75'
data=sys.stdin.buffer.read(65537);assert 0<len(data)<=65536
value=json.loads(data);assert 'alertEmail' in value and 'alertWebhook' not in value
parent='/var/lib/platform-watchdog-config'
try:os.mkdir(parent,0o700)
except FileExistsError:pass
fd=os.open(parent,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
info=os.fstat(fd);assert info.st_uid==0 and stat.S_IMODE(info.st_mode)==0o700
try:
 existing=os.open('config.json',os.O_RDONLY|os.O_NOFOLLOW,dir_fd=fd)
except FileNotFoundError:existing=None
if existing is not None:
 info=os.fstat(existing);assert stat.S_ISREG(info.st_mode) and info.st_uid==0 and info.st_nlink==1 and stat.S_IMODE(info.st_mode)==0o600
 with os.fdopen(existing,'rb') as source:assert source.read(65537)==data
else:
 temporary='.config-'+uuid.uuid4().hex
 target=os.open(temporary,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600,dir_fd=fd)
 try:
  with os.fdopen(target,'wb') as output:output.write(data);output.flush();os.fsync(output.fileno())
  os.link(temporary,'config.json',src_dir_fd=fd,dst_dir_fd=fd,follow_symlinks=False)
 finally:os.unlink(temporary,dir_fd=fd)
 os.fsync(fd)
os.close(fd)
print('Private watchdog config installed; service unchanged')
"""


def deliver_config(config, services, value):
    arguments = [
        "ssh",
        "-T",
        "-l",
        "root",
        "-o",
        "HostName=100.86.241.75",
        "-o",
        "HostKeyAlias=linode",
        "-o",
        "BatchMode=yes",
        "-o",
        "StrictHostKeyChecking=yes",
        "-o",
        "ForwardAgent=no",
        "-o",
        "ClearAllForwardings=yes",
        "-o",
        "ConnectTimeout=10",
        "linode",
        "/usr/bin/python3",
        "-I",
        "-c",
    ]
    import shlex

    services.call(arguments + [shlex.quote(REMOTE_CONFIG)], json.dumps(value).encode())
    return {"configInstalled": True, "serviceStarted": False, "mailSent": False}


def main(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "command",
        choices=[
            "check",
            "issue",
            "recover",
            "verify",
            "test-email",
            "deliver-config",
            "plan-input",
        ],
    )
    parser.add_argument("settings", type=Path)
    parser.add_argument("--receipt", type=Path)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args(argv)
    os.umask(0o077)
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))

    def interrupted(signum, frame):
        raise CredentialError("Credential operation interrupted")

    signal.signal(signal.SIGTERM, interrupted)
    try:
        config = settings(args.settings)
        if args.command == "check":
            print(json.dumps({"settingsValid": True, "networkUsed": False}))
            return 0
        services = Services(config)
        lock = os.open(
            args.settings.parent / ".smtp-provision.lock",
            os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW,
            0o600,
        )
        with os.fdopen(lock, "r+"):
            metadata = os.fstat(lock)
            require(
                stat.S_ISREG(metadata.st_mode)
                and metadata.st_uid == os.geteuid()
                and stat.S_IMODE(metadata.st_mode) == 0o600
                and metadata.st_nlink == 1,
                "Private lock file required",
            )
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            require(
                args.receipt is not None or args.command == "plan-input",
                "Private receipt path required",
            )
            if args.command == "issue":
                result = issue(config, services, args.receipt)
            elif args.command == "plan-input":
                require(args.output is not None, "Private output path required")
                recipient = services.get(KEYS["recipientKey"])
                require(
                    recipient == config["watchdog"]["alertEmail"]["to"],
                    "Recipient differs from approved settings",
                )
                write_json(
                    args.output, {"platform_mail_recipient": recipient}, fresh=True
                )
                result = {"privatePlanInputWritten": True}
            else:
                receipt = receipt_for(args.receipt, config)
                if args.command == "recover":
                    require(
                        recover(config, services, args.receipt, receipt),
                        "Recovery incomplete; inspect private receipt",
                    )
                    result = {"status": "rolled-back"}
                else:
                    value = runtime_config(config, services, receipt)
                    if args.command == "test-email":
                        WATCHDOG.send_email(
                            value["alertEmail"],
                            "Infrastructure alert delivery test. No incident or recovery is being reported.",
                            False,
                            test=True,
                        )
                        result = {"smtpAccepted": True, "inboxAcceptancePending": True}
                    elif args.command == "deliver-config":
                        result = deliver_config(config, services, value)
                    else:
                        result = {"credentialChecksPassed": True, "mailSent": False}
        print(json.dumps(result))
        return 0
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
        print(
            json.dumps(
                {
                    "error": "Mail credential operation failed; no credential values logged"
                }
            )
        )
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
