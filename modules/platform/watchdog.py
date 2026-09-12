import argparse
import json
import os
import pwd
import re
import smtplib
import ssl
import stat
import struct
import time
import urllib.error
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime
from email.errors import HeaderParseError
from email.headerregistry import Address
from email.message import EmailMessage
from email.policy import SMTP
from email.utils import formatdate
from pathlib import Path


class WatchdogError(ValueError):
    pass


def smtp_hostname(value):
    if (
        not isinstance(value, str)
        or not 1 <= len(value) <= 253
        or any(
            not re.fullmatch(r"[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?", label)
            for label in value.split(".")
        )
    ):
        raise WatchdogError("SMTP hostname required")
    return value


def mailbox(value):
    if (
        not isinstance(value, str)
        or not 3 <= len(value) <= 254
        or any(ord(character) < 33 or ord(character) > 126 for character in value)
    ):
        raise WatchdogError("Bare ASCII mailbox required")
    try:
        address = Address(addr_spec=value)
    except (ValueError, HeaderParseError):
        raise WatchdogError("Bare ASCII mailbox required") from None
    if not address.username or len(address.username) > 64 or address.addr_spec != value:
        raise WatchdogError("Bare ASCII mailbox required")
    smtp_hostname(address.domain)
    return value


def validate_email(config):
    keys = {"host", "port", "tls", "username", "password", "from", "to"}
    if not isinstance(config, dict) or set(config) != keys:
        raise WatchdogError("Exact SMTP configuration required")
    smtp_hostname(config["host"])
    if (
        type(config["port"]) is not int
        or not 1 <= config["port"] <= 65535
        or config["tls"] not in ("implicit", "starttls")
    ):
        raise WatchdogError("Explicit verified SMTP TLS required")
    for key, limit in [("username", 256), ("password", 1024)]:
        value = config[key]
        if (
            not isinstance(value, str)
            or not 1 <= len(value) <= limit
            or not value.strip()
            or any(ord(character) < 32 or ord(character) > 126 for character in value)
        ):
            raise WatchdogError("Bounded SMTP credential required")
    mailbox(config["from"])
    mailbox(config["to"])


def send_email(config, text, failed, *, test=False):
    validate_email(config)
    if type(test) is not bool:
        raise WatchdogError("Explicit delivery test flag required")
    message = EmailMessage(policy=SMTP)
    message["From"], message["To"] = config["from"], config["to"]
    message["Subject"] = (
        "Infrastructure alert delivery test"
        if test
        else (
            "Infrastructure checks failed"
            if failed
            else "Infrastructure checks recovered"
        )
    )
    message["Date"] = formatdate(usegmt=True)
    message.set_content(text)
    if len(message.as_bytes()) > 16384:
        raise WatchdogError("Alert email exceeds limit")
    context = ssl.create_default_context()
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    if config["tls"] == "implicit":
        client = smtplib.SMTP_SSL(
            config["host"], config["port"], timeout=5, context=context
        )
    else:
        client = smtplib.SMTP(config["host"], config["port"], timeout=5)
    with client:
        if client.ehlo()[0] != 250:
            raise WatchdogError("SMTP greeting rejected")
        if config["tls"] == "starttls":
            client.starttls(context=context)
            if client.ehlo()[0] != 250:
                raise WatchdogError("SMTP TLS greeting rejected")
        client.login(config["username"], config["password"])
        if client.send_message(
            message, from_addr=config["from"], to_addrs=[config["to"]]
        ):
            raise WatchdogError("Email recipient rejected")


def https_url(value):
    url = urllib.parse.urlsplit(value)
    if (
        url.scheme != "https"
        or not url.hostname
        or url.username
        or url.password
        or url.fragment
    ):
        raise WatchdogError("HTTPS endpoint required")
    return value


class HTTPSRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, file, code, message, headers, new_url):
        raise WatchdogError("Redirect refused")


def request(url, *, headers=None, payload=None):
    body = None if payload is None else json.dumps(payload).encode()
    request_headers = {"User-Agent": "fredrir-platform-watchdog/1.0", **(headers or {})}
    if body is not None:
        request_headers["Content-Type"] = "application/json"
    request = urllib.request.Request(https_url(url), data=body, headers=request_headers)
    try:
        response = urllib.request.build_opener(HTTPSRedirect).open(request, timeout=15)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        data = response.read(65537)
        if len(data) > 65536:
            raise WatchdogError("Response exceeds limit")
        return response.status, data


def validate_config(config):
    if not isinstance(config, dict) or set(config) - {
        "targets",
        "heartbeats",
        "localHeartbeats",
        "alertWebhook",
        "alertEmail",
        "deadmanURL",
    }:
        raise WatchdogError("Unknown configuration key")
    if sum(key in config for key in ("alertWebhook", "alertEmail")) != 1:
        raise WatchdogError("Exactly one alert credential transport required")
    if "alertEmail" in config:
        validate_email(config["alertEmail"])
    elif config.get("alertWebhook"):
        https_url(config["alertWebhook"])
    else:
        raise WatchdogError("Alert credential required")
    if config.get("deadmanURL"):
        https_url(config["deadmanURL"])
    checks = config.get("targets", []) + config.get("heartbeats", [])
    local_checks = config.get("localHeartbeats", [])
    if not checks + local_checks or len(checks + local_checks) > 32:
        raise WatchdogError("Between one and 32 checks required")
    names = set()
    for check in checks:
        if set(check) - {
            "name",
            "url",
            "expectedStatus",
            "timestampField",
            "maxAgeSeconds",
            "authorization",
        }:
            raise WatchdogError("Unknown check key")
        if (
            not isinstance(check.get("name"), str)
            or not check["name"]
            or len(check["name"]) > 80
            or check["name"] in names
            or any(ord(character) < 32 for character in check["name"])
        ):
            raise WatchdogError("Unique check names required")
        names.add(check["name"])
        https_url(check["url"])
    for check in config.get("heartbeats", []):
        if (
            not isinstance(check.get("timestampField"), str)
            or not check["timestampField"]
        ):
            raise WatchdogError("Heartbeat timestamp field required")
        age = check.get("maxAgeSeconds")
        if type(age) is not int or not 60 <= age <= 604800:
            raise WatchdogError(
                "Heartbeat age must be between 60 seconds and seven days"
            )
    for check in local_checks:
        if (
            set(check) != {"name", "job", "maxAgeSeconds"}
            or check["job"] != "llunde-backend-fredrir-09"
            or check["name"] in names
        ):
            raise WatchdogError("Exact local backup check required")
        if (
            not isinstance(check["name"], str)
            or not 1 <= len(check["name"]) <= 80
            or any(ord(character) < 32 for character in check["name"])
        ):
            raise WatchdogError("Local check name invalid")
        if (
            type(check["maxAgeSeconds"]) is not int
            or not 3600 <= check["maxAgeSeconds"] <= 86400
        ):
            raise WatchdogError("Backup age must be between one hour and one day")
        names.add(check["name"])
    return config


def timestamp(value):
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        return value
    if not isinstance(value, str):
        raise WatchdogError("Heartbeat timestamp required")
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise WatchdogError("Heartbeat timezone required")
    return parsed.timestamp()


def read_local_backup(check):
    owner = pwd.getpwnam("infra-backup-status").pw_uid
    descriptor = os.open("/", os.O_RDONLY | os.O_DIRECTORY)
    try:
        for name, expected in [
            ("var", 0),
            ("lib", 0),
            ("platform-backup-status", owner),
        ]:
            child = os.open(
                name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor
            )
            os.close(descriptor)
            descriptor = child
            info = os.fstat(descriptor)
            if info.st_uid != expected or stat.S_IMODE(info.st_mode) != 0o755:
                raise WatchdogError("Receipt directory ownership differs")
        fd = os.open(
            check["job"] + ".json",
            os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK,
            dir_fd=descriptor,
        )
        try:
            info = os.fstat(fd)
            if (
                not stat.S_ISREG(info.st_mode)
                or info.st_uid != owner
                or info.st_nlink != 1
                or stat.S_IMODE(info.st_mode) != 0o644
                or not 0 < info.st_size <= 4096
            ):
                raise WatchdogError("Receipt ownership differs")
            value = json.loads(os.read(fd, 4097))
        finally:
            os.close(fd)
    finally:
        os.close(descriptor)
    return value


def local_backup_fresh(check, now):
    value = read_local_backup(check)
    fields = {
        "schemaVersion",
        "job",
        "host",
        "snapshotId",
        "archiveSHA256",
        "recoveryPointAt",
        "completedAt",
        "receivedAt",
    }
    if (
        set(value) != fields
        or value["schemaVersion"] != 1
        or value["job"] != check["job"]
        or value["host"] != "fredrir-09"
    ):
        raise WatchdogError("Receipt identity differs")
    if not all(
        isinstance(value[key], str) and re.fullmatch("[a-f0-9]{64}", value[key])
        for key in ("snapshotId", "archiveSHA256")
    ):
        raise WatchdogError("Receipt hashes invalid")
    if not all(
        type(value[key]) is int
        for key in ("recoveryPointAt", "completedAt", "receivedAt")
    ):
        raise WatchdogError("Receipt times invalid")
    return (
        0 < value["recoveryPointAt"] <= value["completedAt"] <= value["receivedAt"] + 60
        and value["completedAt"] - value["recoveryPointAt"] <= 1800
        and -60 <= now - value["receivedAt"] <= check["maxAgeSeconds"]
        and -60 <= now - value["recoveryPointAt"] <= check["maxAgeSeconds"]
    )


def check_health(config, now):
    def probe(item):
        kind, check = item
        try:
            if kind == "local":
                return None if local_backup_fresh(check, now) else check["name"]
            headers = (
                {"Authorization": check["authorization"]}
                if check.get("authorization")
                else None
            )
            status, data = request(check["url"], headers=headers)
            healthy = status == check.get("expectedStatus", 200)
            if kind == "heartbeat":
                value = json.loads(data)
                for field in check["timestampField"].split("."):
                    value = value[field]
                age = now - timestamp(value)
                healthy = healthy and -60 <= age <= check["maxAgeSeconds"]
            return None if healthy else check["name"]
        except (OSError, ValueError, KeyError, TypeError):
            return check["name"]

    checks = (
        [("health", check) for check in config.get("targets", [])]
        + [("heartbeat", check) for check in config.get("heartbeats", [])]
        + [("local", check) for check in config.get("localHeartbeats", [])]
    )
    with ThreadPoolExecutor(max_workers=8) as executor:
        return sorted(name for name in executor.map(probe, checks) if name is not None)


def run(config, state_path):
    validate_config(config)
    failed = check_health(config, time.time())
    previous = json.loads(state_path.read_text()) if state_path.exists() else None
    if previous != failed and (failed or previous):
        try:
            text = (
                "Infrastructure checks failed: " + ", ".join(failed)
                if failed
                else "Infrastructure checks recovered"
            )
            if "alertEmail" in config:
                send_email(config["alertEmail"], text, bool(failed))
            else:
                status, _ = request(config["alertWebhook"], payload={"text": text})
                if not 200 <= status < 300:
                    raise WatchdogError("Alert rejected")
        except (OSError, ValueError):
            raise WatchdogError("Alert delivery failed") from None
    if not failed and config.get("deadmanURL"):
        try:
            status, _ = request(config["deadmanURL"])
            if not 200 <= status < 300:
                raise WatchdogError("Deadman confirmation rejected")
        except (OSError, ValueError):
            raise WatchdogError("Deadman confirmation failed") from None
    state_path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    temporary = state_path.with_suffix(".tmp")
    temporary.write_text(json.dumps(failed))
    temporary.chmod(0o600)
    temporary.replace(state_path)
    print("watchdog: " + (f"{len(failed)} failed checks" if failed else "healthy"))
    return 1 if failed else 0


def private_credential_mode(fd, info):
    mode = stat.S_IMODE(info.st_mode)
    if mode in (0o400, 0o600):
        return True
    if (
        mode != 0o440
        or info.st_uid != 0
        or info.st_gid != 0
        or not hasattr(os, "getxattr")
    ):
        return False
    entries = [
        (1, 4, 0xFFFFFFFF),
        (2, 4, os.geteuid()),
        (4, 0, 0xFFFFFFFF),
        (16, 4, 0xFFFFFFFF),
        (32, 0, 0xFFFFFFFF),
    ]
    expected = struct.pack("<I", 2) + b"".join(
        struct.pack("<HHI", *entry) for entry in entries
    )
    try:
        return os.getxattr(fd, "system.posix_acl_access") == expected
    except OSError:
        return False


def read_config(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        if (
            not stat.S_ISREG(info.st_mode)
            or info.st_uid not in (0, os.geteuid())
            or info.st_nlink != 1
            or not private_credential_mode(fd, info)
            or not 0 < info.st_size <= 65536
        ):
            raise WatchdogError("Private bounded credential file required")
        with os.fdopen(fd, "rb", closefd=False) as stream:
            data = stream.read(65537)
        if len(data) > 65536:
            raise WatchdogError("Credential file exceeds limit")
        return json.loads(data)
    finally:
        os.close(fd)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--state", type=Path)
    parser.add_argument("--test-email", action="store_true")
    args = parser.parse_args()
    if not args.test_email and args.state is None:
        parser.error("--state required")
    try:
        config = read_config(args.config)
        if args.test_email:
            validate_config(config)
            if "alertEmail" not in config:
                raise WatchdogError("Email transport required")
            send_email(
                config["alertEmail"],
                "Infrastructure alert delivery test. No incident or recovery is being reported.",
                False,
                test=True,
            )
            print("watchdog: delivery test accepted by SMTP")
            return 0
        return run(config, args.state)
    except (OSError, ValueError, KeyError, TypeError):
        print("watchdog: configuration, state, or delivery failed")
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
