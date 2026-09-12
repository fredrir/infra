from __future__ import annotations

import argparse
import json
import os
import re
import resource
import shlex
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
import uuid
from datetime import UTC, datetime
from pathlib import PurePosixPath

API = "https://api.tailscale.com/api/v2"
ROLES = {"control": "tag:platform-control", "worker": "tag:platform-worker"}
TTL = 600
KEY_FILE = "/run/secrets/tailscale-auth-key"
KEY_ID = re.compile(r"[A-Za-z0-9_-]{4,128}")


class EnrollmentError(ValueError):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise EnrollmentError("API redirect refused")


def request(method, path, *, token=None, document=None, form=None):
    headers = {"Accept": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    data = None
    if document is not None:
        data = json.dumps(document).encode()
        headers["Content-Type"] = "application/json"
    if form is not None:
        data = urllib.parse.urlencode(form).encode()
        headers["Content-Type"] = "application/x-www-form-urlencoded"
    try:
        with urllib.request.build_opener(NoRedirect).open(
            urllib.request.Request(
                API + path, data=data, headers=headers, method=method
            ),
            timeout=20,
        ) as response:
            raw = response.read(65537)
            if response.status not in [200, 201, 204] or len(raw) > 65536:
                raise EnrollmentError("Unexpected API response")
            result = json.loads(raw) if raw else {}
            if (
                result is None
                and method == "DELETE"
                and response.status in [200, 204]
                and re.fullmatch(r"/tailnet/-/keys/[A-Za-z0-9_-]{4,128}", path)
            ):
                return {}
            if not isinstance(result, dict):
                raise EnrollmentError("Unexpected API response structure")
            return result
    except urllib.error.HTTPError as error:
        if error.code == 404 and method in ["GET", "DELETE"] and "/keys/" in path:
            return None
        raise EnrollmentError(
            f"API {method} failed with HTTP {error.code}; response withheld"
        ) from None
    except (urllib.error.URLError, TimeoutError, OSError, json.JSONDecodeError):
        raise EnrollmentError(
            f"API {method} failed; sensitive details withheld"
        ) from None


def validate_target(node, role, host, key_file):
    if not re.fullmatch(r"fredrir-[0-9]{2}", node) or role not in ROLES:
        raise EnrollmentError("Concrete fleet node and role required")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", host):
        raise EnrollmentError("Verified SSH host alias required")
    path = PurePosixPath(key_file)
    if (
        not re.fullmatch(r"/run/(?:[A-Za-z0-9_-]+/)*[A-Za-z0-9_-]+", key_file)
        or str(path) != key_file
    ):
        raise EnrollmentError("Private runtime key path under /run required")


def timestamp(value):
    if not isinstance(value, str):
        raise EnrollmentError("Key timestamp missing")
    try:
        result = datetime.fromisoformat(value.replace("Z", "+00:00"))
        if result.tzinfo is None:
            raise ValueError
        return result.timestamp()
    except ValueError:
        raise EnrollmentError("Key timestamp must include a timezone") from None


def validate_metadata(document, role, description, now, *, fresh=True):
    if (
        not isinstance(document, dict)
        or not isinstance(document.get("id"), str)
        or not KEY_ID.fullmatch(document["id"])
    ):
        raise EnrollmentError("Key identifier missing or malformed")
    expected = {
        "devices": {
            "create": {
                "reusable": False,
                "ephemeral": False,
                "preauthorized": True,
                "tags": [ROLES[role]],
            }
        }
    }
    if (
        document.get("capabilities") != expected
        or document.get("description") != description
    ):
        raise EnrollmentError(
            "Key metadata does not match the exact node and role contract"
        )
    if any(
        type(document["capabilities"]["devices"]["create"][name]) is not bool
        for name in ["reusable", "ephemeral", "preauthorized"]
    ):
        raise EnrollmentError("Key capability flags must be booleans")
    created, expires = (
        timestamp(document.get("created")),
        timestamp(document.get("expires")),
    )
    if not 1 <= expires - created <= TTL + 5:
        raise EnrollmentError("Key lifetime exceeds the ten-minute contract")
    if fresh and (
        document.get("invalid", False) is not False
        or document.get("revoked") not in [None, "", "0001-01-01T00:00:00Z"]
        or not -60 <= now - created <= 60
        or not 60 <= expires - now <= TTL + 65
    ):
        raise EnrollmentError("Key is invalid, stale or outside the allowed clock skew")
    return {
        key: document[key]
        for key in ["id", "created", "expires", "description", "capabilities"]
    }


def access_token(role, environment, api=request):
    prefix = (
        "TS_API"
        if environment.get("TS_API_CLIENT_ID")
        or environment.get("TS_API_CLIENT_SECRET")
        else "TAILSCALE_ENROLL"
    )
    client_id, secret = (
        environment.get(prefix + "_CLIENT_ID"),
        environment.get(prefix + "_CLIENT_SECRET"),
    )
    if not client_id or not secret:
        raise EnrollmentError(
            "Complete TS_API_CLIENT_ID/SECRET or TAILSCALE_ENROLL_CLIENT_ID/SECRET pair required"
        )
    result = api(
        "POST",
        "/oauth/token",
        form={
            "grant_type": "client_credentials",
            "client_id": client_id,
            "client_secret": secret,
            "scope": "auth_keys",
            "tags": ROLES[role],
        },
    )
    if (
        not isinstance(result.get("access_token"), str)
        or not result["access_token"]
        or result.get("token_type", "").lower() != "bearer"
        or set(result.get("scope", "").split()) != {"auth_keys"}
        or type(result.get("expires_in")) is not int
        or not 60 <= result["expires_in"] <= 3600
    ):
        raise EnrollmentError("OAuth token did not return the requested narrow scope")
    return result["access_token"]


def remote_program(runtime_root="/run", owner=0, system="linux"):
    return (
        f"RUNTIME_ROOT = {runtime_root!r}\nOWNER = {owner!r}\nSYSTEM = {system!r}\n"
        + """import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys
import uuid

def check_directory(path):
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != OWNER or info.st_mode & 0o022:
        raise ValueError('directory')

def private_write(path, data):
    temporary = path.with_name('.' + path.name + '.' + uuid.uuid4().hex)
    try:
        with os.fdopen(os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o400), 'wb') as output:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        os.link(temporary, path, follow_symlinks=False)
    finally:
        temporary.unlink(missing_ok=True)

try:
    mode, target, key_id, node, role = sys.argv[1:]
    target, root = Path(target), Path(RUNTIME_ROOT)
    receipt = target.with_name(target.name + '.metadata.json')
    if os.geteuid() != OWNER or sys.platform != SYSTEM or not target.is_absolute() or not target.is_relative_to(root) or '..' in target.parts or mode not in ['preflight', 'deliver', 'cleanup']:
        raise ValueError('target')
    current = root
    check_directory(current)
    for part in target.parent.relative_to(root).parts:
        current = current / part
        if current.exists() or current.is_symlink():
            check_directory(current)
        elif mode == 'deliver':
            current.mkdir(mode=0o700)
            check_directory(current)
    if mode in ['preflight', 'deliver'] and any(path.exists() or path.is_symlink() for path in [target, receipt]):
        raise ValueError('existing key')
    if mode == 'deliver':
        raw = sys.stdin.buffer.read(8193)
        if len(raw) > 8192:
            raise ValueError('payload')
        payload = json.loads(raw)
        metadata, key = payload['metadata'], payload['key']
        if metadata['id'] != key_id or metadata['node'] != node or metadata['role'] != role or not re.fullmatch(r'tskey-auth-[A-Za-z0-9_-]{20,250}', key):
            raise ValueError('payload identity')
        private_write(receipt, json.dumps(metadata | {'keySha256': hashlib.sha256(key.encode()).hexdigest()}).encode())
        try:
            private_write(target, key.encode() + b'\\n')
        except BaseException:
            receipt.unlink()
            raise
    if mode == 'cleanup' and (receipt.exists() or receipt.is_symlink()):
        info = receipt.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid != OWNER or info.st_mode & 0o077 or info.st_nlink != 1 or info.st_size > 4096:
            raise ValueError('receipt')
        metadata = json.loads(receipt.read_text())
        if metadata['id'] != key_id or metadata['node'] != node or metadata['role'] != role:
            raise ValueError('cleanup identity')
        if target.exists() or target.is_symlink():
            info = target.lstat()
            if not stat.S_ISREG(info.st_mode) or info.st_uid != OWNER or info.st_mode & 0o077 or info.st_nlink != 1 or info.st_size > 512 or hashlib.sha256(target.read_bytes().strip()).hexdigest() != metadata['keySha256']:
                raise ValueError('key')
            target.unlink()
        receipt.unlink()
    elif mode == 'cleanup' and (target.exists() or target.is_symlink()):
        raise ValueError('missing receipt')
    print(json.dumps({'result': 'ok'}))
except BaseException:
    sys.stderr.write('Runtime key operation failed; details withheld\\n')
    sys.exit(1)
"""
    )


def remote(
    mode,
    host,
    key_file,
    key_id,
    node,
    role,
    *,
    payload=None,
    runner=subprocess.run,
    environment=None,
):
    environment = os.environ if environment is None else environment
    child_environment = {
        key: value
        for key, value in environment.items()
        if key in ["PATH", "HOME", "USER", "LOGNAME", "SSH_AUTH_SOCK", "LANG", "LC_ALL"]
    }
    python_command = shlex.join(
        ["/usr/bin/python3", "-c", remote_program(), mode, key_file, key_id, node, role]
    )
    command = (
        'if [ "$(/usr/bin/id -u)" = 0 ]; then exec '
        + python_command
        + "; else exec /usr/bin/sudo -n -- "
        + python_command
        + "; fi"
    )
    try:
        result = runner(
            [
                "ssh",
                "-T",
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
                host,
                command,
            ],
            input=json.dumps(payload).encode() if payload else b"",
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=30,
            env=child_environment,
        )
        if result.returncode or json.loads(result.stdout) != {"result": "ok"}:
            raise EnrollmentError("SSH runtime key operation failed; output withheld")
    except (subprocess.SubprocessError, OSError, json.JSONDecodeError):
        raise EnrollmentError(
            "SSH runtime key operation failed; output withheld"
        ) from None


def revoke_key(api, token, key_id):
    api("DELETE", "/tailnet/-/keys/" + urllib.parse.quote(key_id, safe=""), token=token)


def key_ids(api, token):
    result = api("GET", "/tailnet/-/keys", token=token)
    if not isinstance(result, dict) or "keys" not in result:
        raise EnrollmentError("Unexpected auth-key inventory")
    keys = result.get("keys")
    if keys is None:
        keys = []
    if (
        not isinstance(keys, list)
        or len(keys) > 1000
        or any(
            not isinstance(item, dict)
            or not isinstance(item.get("id"), str)
            or not KEY_ID.fullmatch(item["id"])
            for item in keys
        )
    ):
        raise EnrollmentError("Unexpected auth-key inventory")
    return {item["id"] for item in keys}


def create_deliver(
    node,
    role,
    host,
    key_file,
    *,
    environment=None,
    api=request,
    transport=remote,
    now=None,
):
    validate_target(node, role, host, key_file)
    environment = os.environ if environment is None else environment

    def current():
        return datetime.now(UTC).timestamp() if now is None else now

    token = access_token(role, environment, api)
    transport("preflight", host, key_file, "", node, role)
    existing_keys = key_ids(api, token)
    description = f"enroll-{node}-{role}-{uuid.uuid4().hex[:24]}"
    key_id, delivery_attempted = None, False
    try:
        capabilities = {
            "devices": {
                "create": {
                    "reusable": False,
                    "ephemeral": False,
                    "preauthorized": True,
                    "tags": [ROLES[role]],
                }
            }
        }
        result = api(
            "POST",
            "/tailnet/-/keys",
            token=token,
            document={
                "capabilities": capabilities,
                "expirySeconds": TTL,
                "description": description,
            },
        )
        if (
            isinstance(result.get("id"), str)
            and KEY_ID.fullmatch(result["id"])
            and result["id"] not in existing_keys
        ):
            key_id = result["id"]
        if key_id is None:
            raise EnrollmentError("API did not identify a new key")
        metadata = validate_metadata(result, role, description, current())
        key = result.get("key")
        if not isinstance(key, str) or not re.fullmatch(
            r"tskey-auth-[A-Za-z0-9_-]{20,250}", key
        ):
            raise EnrollmentError("API did not return a node auth key")
        verified = api("GET", "/tailnet/-/keys/" + key_id, token=token)
        if validate_metadata(verified, role, description, current()) != metadata:
            raise EnrollmentError("Created key metadata changed before delivery")
        receipt = {**metadata, "node": node, "role": role}
        delivery_attempted = True
        transport(
            "deliver",
            host,
            key_file,
            key_id,
            node,
            role,
            payload={"key": key, "metadata": receipt},
        )
        return {
            "kind": "delivered-tailscale-key",
            "node": node,
            "role": role,
            "keyId": key_id,
            "expires": metadata["expires"],
            "keyFile": key_file,
            "oneTime": True,
            "requires": "Run bootstrap immediately; revoke and remove an unused key",
        }
    except BaseException:
        cleanup_failures = []
        try:
            if key_id:
                revoke_key(api, token, key_id)
            else:
                recovered = []
                for candidate in key_ids(api, token) - existing_keys:
                    item = api("GET", "/tailnet/-/keys/" + candidate, token=token)
                    if item is not None and item.get("description") == description:
                        revoke_key(api, token, candidate)
                        recovered.append(candidate)
                if not recovered:
                    cleanup_failures.append(
                        "creation outcome and API revocation unconfirmed"
                    )
        except BaseException:
            cleanup_failures.append("API revocation unconfirmed")
        if delivery_attempted:
            try:
                transport("cleanup", host, key_file, key_id, node, role)
            except BaseException:
                cleanup_failures.append("remote cleanup unconfirmed")
        detail = (
            "; ".join(cleanup_failures)
            or "created key revoked; no usable delivered key retained"
        )
        raise EnrollmentError(
            f"Enrollment key preparation failed; {detail}; request {description}"
        ) from None


def revoke_unused(
    node,
    role,
    host,
    key_file,
    key_id,
    *,
    environment=None,
    api=request,
    transport=remote,
    now=None,
):
    validate_target(node, role, host, key_file)
    if not KEY_ID.fullmatch(key_id):
        raise EnrollmentError("Concrete key ID required")
    token = access_token(role, os.environ if environment is None else environment, api)
    document = api("GET", "/tailnet/-/keys/" + key_id, token=token)
    if document is not None:
        description = document.get("description", "")
        if not re.fullmatch(
            f"enroll-{re.escape(node)}-{re.escape(role)}-[0-9a-f]{{24}}", description
        ):
            raise EnrollmentError("Key does not belong to this bootstrap target")
        validate_metadata(
            document,
            role,
            description,
            now or datetime.now(UTC).timestamp(),
            fresh=False,
        )
        revoke_key(api, token, key_id)
    transport("cleanup", host, key_file, key_id, node, role)
    return {
        "kind": "revoked-tailscale-key",
        "node": node,
        "role": role,
        "keyId": key_id,
        "runtimeFileRemoved": True,
    }


def main(argv=None):
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=["create-deliver", "revoke-unused"])
    parser.add_argument("--node", required=True)
    parser.add_argument("--role", required=True, choices=ROLES)
    parser.add_argument("--host")
    parser.add_argument("--key-file", default=KEY_FILE)
    parser.add_argument("--key-id")
    args = parser.parse_args(argv)
    try:
        host = args.host or args.node
        if args.action == "create-deliver":
            if args.key_id:
                raise EnrollmentError("Key ID is only accepted for revocation")
            result = create_deliver(args.node, args.role, host, args.key_file)
        else:
            if not args.key_id:
                raise EnrollmentError("Key ID required for revocation")
            result = revoke_unused(
                args.node, args.role, host, args.key_file, args.key_id
            )
        print(json.dumps(result, sort_keys=True))
        return 0
    except EnrollmentError as error:
        print(str(error), file=sys.stderr)
        return 1
    except Exception:
        print("Unexpected response; sensitive details withheld", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
