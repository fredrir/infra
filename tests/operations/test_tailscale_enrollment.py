import copy
import importlib.util
import io
import json
import os
import shlex
import subprocess
import sys
import tempfile
import unittest
from datetime import UTC, datetime
from pathlib import Path
from unittest.mock import MagicMock, Mock, patch

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location(
    "tailscale_enrollment", ROOT / "scripts/operations/tailscale_enrollment.py"
)
enrollment = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(enrollment)
NOW = 1789171200
AUTH_KEY = "tskey-" + "auth-" + "x" * 48
ENVIRONMENT = {
    "TS_API_CLIENT_ID": "fixture-client",
    "TS_API_CLIENT_SECRET": "fixture-oauth-secret",
}


def iso(value):
    return datetime.fromtimestamp(value, UTC).isoformat()


class FakeAPI:
    def __init__(
        self, *, mutate=None, fail_create=False, fail_delete=False, drift=False
    ):
        self.calls, self.keys = [], {}
        self.mutate, self.fail_create, self.fail_delete, self.drift = (
            mutate,
            fail_create,
            fail_delete,
            drift,
        )

    def __call__(self, method, path, **kwargs):
        self.calls.append((method, path, copy.deepcopy(kwargs)))
        if path == "/oauth/token":
            return {
                "access_token": "fixture-token",
                "token_type": "Bearer",
                "scope": "auth_keys",
                "expires_in": 3600,
            }
        if method == "POST":
            payload = kwargs["document"]
            result = {
                "id": "kFixture123",
                "key": AUTH_KEY,
                "created": iso(NOW),
                "expires": iso(NOW + 600),
                "description": payload["description"],
                "capabilities": copy.deepcopy(payload["capabilities"]),
                "invalid": False,
            }
            if self.mutate:
                self.mutate(result)
            self.keys[result["id"]] = {
                name: value for name, value in result.items() if name != "key"
            }
            if self.fail_create:
                raise enrollment.EnrollmentError("lost creation response")
            return result
        if method == "DELETE":
            if self.fail_delete:
                raise enrollment.EnrollmentError("revocation unavailable")
            self.keys.pop(path.rsplit("/", 1)[1], None)
            return {}
        if path == "/tailnet/-/keys":
            return {"keys": [{"id": key_id} for key_id in self.keys]}
        result = copy.deepcopy(self.keys.get(path.rsplit("/", 1)[1]))
        if self.drift and result:
            result["capabilities"]["devices"]["create"]["reusable"] = True
        return result


class EnrollmentTests(unittest.TestCase):
    def test_only_successful_key_deletion_accepts_json_null(self):
        cases = [
            ("DELETE", "/tailnet/-/keys/kFixture123", 200, b"null", True),
            ("DELETE", "/tailnet/-/keys/kFixture123", 204, b"", True),
            ("DELETE", "/tailnet/-/keys/kFixture123", 200, b"{}", True),
            ("GET", "/tailnet/-/keys/kFixture123", 200, b"null", False),
            ("GET", "/tailnet/-/keys", 200, b"null", False),
            ("POST", "/tailnet/-/keys", 200, b"null", False),
            ("DELETE", "/unrelated", 200, b"null", False),
            ("DELETE", "/tailnet/-/keys/kFixture123", 200, b"[]", False),
        ]
        for method, path, status, body, accepted in cases:
            with self.subTest(method=method, path=path, status=status, body=body):
                response = MagicMock(status=status)
                response.__enter__.return_value = response
                response.read.return_value = body
                with patch.object(
                    enrollment.urllib.request,
                    "build_opener",
                    return_value=Mock(open=Mock(return_value=response)),
                ):
                    if accepted:
                        self.assertEqual(
                            enrollment.request(method, path, token="fixture-token"), {}
                        )
                    else:
                        with self.assertRaises(enrollment.EnrollmentError):
                            enrollment.request(method, path, token="fixture-token")

    def test_empty_auth_key_inventory_accepts_null_and_rejects_malformed_responses(
        self,
    ):
        for response in [{"keys": None}, {"keys": []}]:
            with self.subTest(response=response):
                self.assertEqual(
                    enrollment.key_ids(Mock(return_value=response), "fixture-token"),
                    set(),
                )
        for response in [
            {},
            None,
            [],
            {"keys": {}},
            {"keys": ""},
            {"keys": [{"id": 1}]},
        ]:
            with self.subTest(response=response):
                with self.assertRaises(enrollment.EnrollmentError):
                    enrollment.key_ids(Mock(return_value=response), "fixture-token")

    def create(self, api, transport, role="control"):
        return enrollment.create_deliver(
            "fredrir-07",
            role,
            "fredrir-07",
            enrollment.KEY_FILE,
            environment=ENVIRONMENT,
            api=api,
            transport=transport,
            now=NOW,
        )

    def test_token_and_key_are_narrowed_to_one_role_before_delivery(self):
        for role in enrollment.ROLES:
            with self.subTest(role=role):
                api, transport = FakeAPI(), Mock()
                result = self.create(api, transport, role)
                form = api.calls[0][2]["form"]
                self.assertEqual(form["scope"], "auth_keys")
                self.assertEqual(form["tags"], enrollment.ROLES[role])
                creation = next(
                    call[2]["document"]
                    for call in api.calls
                    if call[:2] == ("POST", "/tailnet/-/keys")
                )
                self.assertEqual(creation["expirySeconds"], 600)
                self.assertEqual(
                    creation["capabilities"]["devices"]["create"],
                    {
                        "reusable": False,
                        "ephemeral": False,
                        "preauthorized": True,
                        "tags": [enrollment.ROLES[role]],
                    },
                )
                self.assertLessEqual(len(creation["description"]), 50)
                self.assertRegex(
                    creation["description"],
                    r"\Aenroll-fredrir-07-(control|worker)-[0-9a-f]{24}\Z",
                )
                self.assertEqual(
                    [call.args[0] for call in transport.call_args_list],
                    ["preflight", "deliver"],
                )
                self.assertEqual(transport.call_args.kwargs["payload"]["key"], AUTH_KEY)
                self.assertNotIn(AUTH_KEY, json.dumps(result))
                self.assertNotIn("fixture-oauth-secret", json.dumps(result))

    def test_unsafe_metadata_is_revoked_without_delivery(self):
        mutations = [
            lambda value: value["capabilities"]["devices"]["create"].update(
                tags=list(enrollment.ROLES.values())
            ),
            lambda value: value["capabilities"]["devices"]["create"].update(
                tags=["tag:platform-enrollment"]
            ),
            lambda value: value["capabilities"]["devices"]["create"].update(
                reusable=True
            ),
            lambda value: value["capabilities"]["devices"]["create"].update(
                ephemeral=True
            ),
            lambda value: value["capabilities"]["devices"]["create"].update(
                preauthorized=1
            ),
            lambda value: value.update(expires=iso(NOW + 3600)),
            lambda value: value.update(created="2026-09-12T00:00:00"),
            lambda value: value.update(created=iso(NOW - 120), expires=iso(NOW + 480)),
            lambda value: value.update(invalid=True),
        ]
        for mutate in mutations:
            with self.subTest(mutate=mutate):
                api, transport = FakeAPI(mutate=mutate), Mock()
                with self.assertRaises(enrollment.EnrollmentError):
                    self.create(api, transport)
                self.assertEqual(
                    [call.args[0] for call in transport.call_args_list], ["preflight"]
                )
                self.assertEqual(api.keys, {})

    def test_changed_get_metadata_prevents_delivery(self):
        api, transport = FakeAPI(drift=True), Mock()
        with self.assertRaises(enrollment.EnrollmentError):
            self.create(api, transport)
        self.assertEqual(api.keys, {})
        self.assertEqual(transport.call_count, 1)

    def test_failed_delivery_revokes_key_and_cleans_only_its_remote_receipt(self):
        api = FakeAPI()
        transport = Mock(
            side_effect=[None, enrollment.EnrollmentError("delivery interrupted"), None]
        )
        with self.assertRaises(enrollment.EnrollmentError) as error:
            self.create(api, transport)
        self.assertEqual(api.keys, {})
        self.assertEqual(
            [call.args[0] for call in transport.call_args_list],
            ["preflight", "deliver", "cleanup"],
        )
        self.assertEqual(transport.call_args.args[3], "kFixture123")
        self.assertNotIn(AUTH_KEY, str(error.exception))

    def test_lost_creation_response_uses_key_metadata_to_revoke_only_the_new_request(
        self,
    ):
        api, transport = FakeAPI(fail_create=True), Mock()
        api.keys["kExisting"] = {"description": "another request"}
        with self.assertRaises(enrollment.EnrollmentError):
            self.create(api, transport)
        self.assertEqual(set(api.keys), {"kExisting"})
        self.assertEqual(transport.call_count, 1)

    def test_cleanup_failures_are_reported_without_a_false_revocation_claim(self):
        api = FakeAPI(fail_delete=True)
        transport = Mock(
            side_effect=[
                None,
                enrollment.EnrollmentError("delivery interrupted"),
                enrollment.EnrollmentError("host unreachable"),
            ]
        )
        with self.assertRaisesRegex(
            enrollment.EnrollmentError,
            "API revocation unconfirmed; remote cleanup unconfirmed",
        ):
            self.create(api, transport)

    def test_unsafe_target_or_preexisting_remote_file_creates_no_key(self):
        for host, path in [
            ("-oProxyCommand=bad", enrollment.KEY_FILE),
            ("fredrir-07", "/run/../etc/secret"),
        ]:
            api, transport = FakeAPI(), Mock()
            with self.assertRaises(enrollment.EnrollmentError):
                enrollment.create_deliver(
                    "fredrir-07",
                    "control",
                    host,
                    path,
                    environment=ENVIRONMENT,
                    api=api,
                    transport=transport,
                    now=NOW,
                )
            self.assertEqual(api.calls, [])
        api = FakeAPI()
        with self.assertRaises(enrollment.EnrollmentError):
            self.create(
                api, Mock(side_effect=enrollment.EnrollmentError("preexisting key"))
            )
        self.assertFalse(
            any(call[:2] == ("POST", "/tailnet/-/keys") for call in api.calls)
        )

    def test_unused_key_revocation_is_bound_to_node_and_role(self):
        api, transport = FakeAPI(), Mock()
        report = self.create(api, transport)
        with self.assertRaises(enrollment.EnrollmentError):
            enrollment.revoke_unused(
                "fredrir-08",
                "control",
                "fredrir-08",
                enrollment.KEY_FILE,
                report["keyId"],
                environment=ENVIRONMENT,
                api=api,
                transport=transport,
                now=NOW,
            )
        self.assertIn(report["keyId"], api.keys)
        result = enrollment.revoke_unused(
            "fredrir-07",
            "control",
            "fredrir-07",
            enrollment.KEY_FILE,
            report["keyId"],
            environment=ENVIRONMENT,
            api=api,
            transport=transport,
            now=NOW,
        )
        self.assertEqual(api.keys, {})
        self.assertTrue(result["runtimeFileRemoved"])

    def test_ssh_stdin_is_the_only_credential_channel(self):
        runner = Mock(
            return_value=subprocess.CompletedProcess([], 0, b'{"result":"ok"}', b"")
        )
        environment = ENVIRONMENT | {
            "PATH": "/usr/bin",
            "DOPPLER_TOKEN": "fixture-doppler-token",
            "SSH_AUTH_SOCK": "/fixture/agent",
        }
        enrollment.remote(
            "deliver",
            "fredrir-07",
            enrollment.KEY_FILE,
            "kFixture123",
            "fredrir-07",
            "control",
            payload={"key": AUTH_KEY},
            runner=runner,
            environment=environment,
        )
        args, kwargs = runner.call_args.args[0], runner.call_args.kwargs
        self.assertIn(AUTH_KEY.encode(), kwargs["input"])
        self.assertNotIn(AUTH_KEY, " ".join(args))
        self.assertNotIn("fixture-oauth-secret", json.dumps(kwargs["env"]))
        self.assertNotIn("DOPPLER_TOKEN", kwargs["env"])
        self.assertIn("StrictHostKeyChecking=yes", args)
        self.assertIn("ForwardAgent=no", args)
        self.assertEqual(kwargs["env"]["SSH_AUTH_SOCK"], "/fixture/agent")

    def test_oauth_scope_widening_and_redirects_are_refused(self):
        api = Mock(
            return_value={
                "access_token": "fixture-token",
                "token_type": "Bearer",
                "scope": "all auth_keys",
                "expires_in": 3600,
            }
        )
        with self.assertRaises(enrollment.EnrollmentError):
            enrollment.access_token("control", ENVIRONMENT, api)
        with self.assertRaises(enrollment.EnrollmentError):
            enrollment.NoRedirect().redirect_request(
                None, None, 302, "", {}, "https://untrusted.invalid"
            )

    def test_administrator_ssh_alias_keeps_its_user(self):
        runner = Mock(
            return_value=subprocess.CompletedProcess([], 0, b'{"result":"ok"}', b"")
        )
        enrollment.remote(
            "preflight",
            "fredrir-09",
            "/run/platform/tailscale-auth-key",
            "",
            "fredrir-09",
            "worker",
            runner=runner,
            environment=ENVIRONMENT,
        )
        args = runner.call_args.args[0]
        self.assertEqual(args[-2], "fredrir-09")
        self.assertNotIn("root@fredrir-09", args)
        self.assertNotIn("-l", args)
        self.assertNotIn("fixture-oauth-secret", " ".join(args))


class RemoteInterpreterTests(unittest.TestCase):
    def test_invalid_interpreters_fail_before_transport_or_api_access(self):
        invalid = [
            "",
            "python3",
            "/",
            "//usr/bin/python3",
            "/usr//bin/python3",
            "/usr/./bin/python3",
            "/usr/../bin/python3",
            "/usr/bin/python3/",
            "/usr/bin/python3 -I",
            "/tmp/$(id)",
            "/tmp/python;id",
            "/tmp/`id`",
            "/tmp/python\n",
            "/tmp/python\x00",
            "/" + "x" * 4096,
        ]
        for interpreter in invalid:
            with self.subTest(interpreter=interpreter):
                runner = Mock()
                with self.assertRaisesRegex(enrollment.EnrollmentError, "absolute"):
                    enrollment.remote(
                        "preflight",
                        "fredrir-05",
                        enrollment.KEY_FILE,
                        "",
                        "fredrir-05",
                        "control",
                        remote_python=interpreter,
                        runner=runner,
                    )
                runner.assert_not_called()
                with (
                    patch.object(enrollment, "create_deliver") as create,
                    patch.object(enrollment.resource, "setrlimit"),
                    patch("sys.stderr", new_callable=io.StringIO) as error,
                ):
                    status = enrollment.main(
                        [
                            "create-deliver",
                            "--node",
                            "fredrir-05",
                            "--role",
                            "control",
                            "--remote-python",
                            interpreter,
                        ]
                    )
                self.assertEqual(status, 1)
                create.assert_not_called()
                self.assertEqual(
                    error.getvalue(), "Verified absolute remote Python path required\n"
                )

    def test_cli_binds_the_interpreter_for_delivery_and_cleanup(self):
        for interpreter in [
            None,
            "/nix/store/0123456789abc-python3-3.13.7/bin/python3",
        ]:
            for action, function in [
                ("create-deliver", "create_deliver"),
                ("revoke-unused", "revoke_unused"),
            ]:
                with self.subTest(interpreter=interpreter, action=action):
                    argv = [action, "--node", "fredrir-05", "--role", "control"]
                    if interpreter:
                        argv += ["--remote-python", interpreter]
                    if action == "revoke-unused":
                        argv += ["--key-id", "kFixture123"]
                    with (
                        patch.object(
                            enrollment, function, return_value={}
                        ) as operation,
                        patch.object(enrollment.resource, "setrlimit"),
                        patch("sys.stdout", new_callable=io.StringIO),
                    ):
                        self.assertEqual(enrollment.main(argv), 0)
                    transport = operation.call_args.kwargs["transport"]
                    runner = Mock(
                        return_value=subprocess.CompletedProcess(
                            [], 0, b'{"result":"ok"}', b""
                        )
                    )
                    transport(
                        "preflight",
                        "fredrir-05",
                        enrollment.KEY_FILE,
                        "",
                        "fredrir-05",
                        "control",
                        runner=runner,
                    )
                    command = runner.call_args.args[0][-1]
                    self.assertIn(
                        (interpreter or "/usr/bin/python3") + " -I -B -c", command
                    )
                    if interpreter:
                        self.assertNotIn("/usr/bin/python3", command)

    def run_shell_boundary(self, uid, probe_status=0):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            python, sudo = root / "python3", root / "sudo"
            delivered, escalated = root / "delivered.json", root / "escalated.json"
            python.write_text(
                f"#!{sys.executable}\nimport json, pathlib, sys\n"
                "assert sys.argv[1:4] == ['-I', '-B', '-c']\n"
                "if sys.argv[4] == 'import os; print(os.geteuid())':\n"
                f"    print({uid!r})\n    sys.exit({probe_status})\n"
                f"pathlib.Path({str(delivered)!r}).write_text(json.dumps({{'argv': sys.argv[5:], 'payload': json.load(sys.stdin)}}))\n"
                'print(\'{"result":"ok"}\')\n'
            )
            sudo.write_text(
                f"#!{sys.executable}\nimport json, os, pathlib, sys\n"
                "assert sys.argv[1:3] == ['-n', '--']\n"
                f"pathlib.Path({str(escalated)!r}).write_text(json.dumps(sys.argv[1:]))\n"
                "os.execv(sys.argv[3], sys.argv[3:])\n"
            )
            python.chmod(0o700)
            sudo.chmod(0o700)

            def runner(argv, **kwargs):
                command = argv[-1].replace(
                    "exec /usr/bin/sudo -n -- ",
                    "exec " + shlex.quote(str(sudo)) + " -n -- ",
                )
                return subprocess.run(["/bin/sh", "-c", command], check=False, **kwargs)

            error = None
            try:
                enrollment.remote(
                    "deliver",
                    "fredrir-05",
                    enrollment.KEY_FILE,
                    "kFixture123",
                    "fredrir-05",
                    "control",
                    payload={"key": AUTH_KEY},
                    remote_python=str(python),
                    runner=runner,
                    environment=ENVIRONMENT,
                )
            except enrollment.EnrollmentError as caught:
                error = str(caught)
            return (
                error,
                json.loads(delivered.read_text()) if delivered.exists() else None,
                json.loads(escalated.read_text()) if escalated.exists() else None,
            )

    def test_root_delivery_preserves_stdin_without_sudo(self):
        error, delivered, escalated = self.run_shell_boundary("0")
        self.assertIsNone(error)
        self.assertIsNone(escalated)
        self.assertEqual(delivered["payload"], {"key": AUTH_KEY})
        self.assertEqual(
            delivered["argv"],
            ["deliver", enrollment.KEY_FILE, "kFixture123", "fredrir-05", "control"],
        )

    def test_nonroot_delivery_escalates_once_with_selected_interpreter(self):
        error, delivered, escalated = self.run_shell_boundary("1000")
        self.assertIsNone(error)
        self.assertEqual(escalated[:2], ["-n", "--"])
        self.assertTrue(escalated[2].endswith("/python3"))
        self.assertEqual(escalated[3:6], ["-I", "-B", "-c"])
        self.assertEqual(delivered["payload"], {"key": AUTH_KEY})

    def test_failed_or_malformed_uid_probe_neither_delivers_nor_escalates(self):
        for uid, status in [
            ("0", 1),
            ("", 0),
            ("not-a-uid", 0),
            ("0\n1000", 0),
            ("-1", 0),
        ]:
            with self.subTest(uid=uid, status=status):
                error, delivered, escalated = self.run_shell_boundary(uid, status)
                self.assertEqual(
                    error, "SSH runtime key operation failed; output withheld"
                )
                self.assertIsNone(delivered)
                self.assertIsNone(escalated)


class RuntimeDeliveryTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name).resolve()
        self.key = self.root / "secrets/tailscale-auth-key"
        self.receipt = self.key.with_name(self.key.name + ".metadata.json")
        self.metadata = {"id": "kFixture123", "node": "fredrir-07", "role": "control"}

    def tearDown(self):
        self.temporary.cleanup()

    def run_remote(self, mode, payload=None, key_id="kFixture123"):
        script = enrollment.remote_program(str(self.root), os.geteuid(), sys.platform)
        return subprocess.run(
            [
                sys.executable,
                "-c",
                script,
                mode,
                str(self.key),
                key_id,
                "fredrir-07",
                "control",
            ],
            input=json.dumps(payload).encode() if payload else b"",
            capture_output=True,
            timeout=10,
        )

    def test_runtime_delivery_is_private_atomic_and_refuses_overwrite(self):
        self.assertEqual(self.run_remote("preflight").returncode, 0)
        self.assertFalse(self.key.parent.exists())
        payload = {"key": AUTH_KEY, "metadata": self.metadata}
        result = self.run_remote("deliver", payload)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.key.read_text().strip(), AUTH_KEY)
        self.assertEqual(self.key.stat().st_mode & 0o777, 0o400)
        self.assertEqual(self.key.stat().st_nlink, 1)
        self.assertEqual(self.receipt.stat().st_mode & 0o777, 0o400)
        self.assertEqual(self.key.parent.stat().st_mode & 0o777, 0o700)
        self.assertNotIn(AUTH_KEY.encode(), result.stdout + result.stderr)
        self.assertNotEqual(self.run_remote("deliver", payload).returncode, 0)
        self.assertNotEqual(
            self.run_remote("cleanup", key_id="kDifferent").returncode, 0
        )
        self.assertTrue(self.key.exists())
        self.assertEqual(self.run_remote("cleanup").returncode, 0)
        self.assertFalse(self.key.exists())
        self.assertFalse(self.receipt.exists())

    def test_runtime_symlink_parent_and_altered_key_are_not_followed_or_removed(self):
        outside = self.root / "outside"
        outside.mkdir()
        self.key.parent.symlink_to(outside)
        self.assertNotEqual(self.run_remote("preflight").returncode, 0)
        self.key.parent.unlink()
        payload = {"key": AUTH_KEY, "metadata": self.metadata}
        self.assertEqual(self.run_remote("deliver", payload).returncode, 0)
        self.key.chmod(0o600)
        self.key.write_text("a different credential")
        self.key.chmod(0o400)
        self.assertNotEqual(self.run_remote("cleanup").returncode, 0)
        self.assertEqual(self.key.read_text(), "a different credential")


if __name__ == "__main__":
    unittest.main()
