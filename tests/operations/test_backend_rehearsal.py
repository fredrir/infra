import copy
import json
import sys
import tempfile
import time
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
import evacuation_backend_rehearsal as rehearsal


class BackendRehearsalTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def pilot(self, component="backend"):
        pilot = object.__new__(rehearsal.Rehearsal)
        pilot.nonce = "a" * 16
        pilot.prefix = "infra-backend-rehearsal-" + pilot.nonce
        pilot.workspace = self.root / "workspace"
        pilot.workspace.mkdir(exist_ok=True)
        (pilot.workspace / "postgres").mkdir(exist_ok=True)
        pilot.record = {}
        pilot.initialized = False
        pilot.output = self.root / "result.json"
        pilot.networks = {}
        pilot.containers = {
            pilot.prefix + "-" + component: {
                "id": "b" * 64,
                "component": component,
                "timeout": 120,
                "networks": [pilot.prefix + "-data"],
                "environment": {
                    "DB_PASSWORD": "synthetic-only",
                    "HOME": "/tmp",
                    "HOSTNAME": pilot.prefix + "-" + component,
                },
            }
        }
        return pilot

    def document(self, pilot, component="backend"):
        name = pilot.prefix + "-" + component
        document = {
            "Name": name,
            "Id": "b" * 64,
            "Image": rehearsal.IMAGES[component],
            "EffectiveCaps": None,
            "BoundingCaps": [],
            "Config": {
                "Labels": {"infra.backend-rehearsal": pilot.nonce},
                "User": "65534:65534" if component == "backend" else "999:999",
                "Timeout": 120,
                "Env": [
                    "DB_PASSWORD=synthetic-only",
                    "HOME=/tmp",
                    "HOSTNAME=" + name,
                ],
                "Hostname": name,
                "Entrypoint": ["/app/entrypoint.sh"],
                "Cmd": None,
            },
            "HostConfig": {
                "Privileged": False,
                "ReadonlyRootfs": True,
                "PortBindings": {},
                "Memory": rehearsal.MEMORY[component] * 1024**2,
                "MemorySwap": rehearsal.MEMORY[component] * 1024**2,
                "PidsLimit": rehearsal.PIDS[component],
                "CpuQuota": rehearsal.CPUS[component],
                "CpuPeriod": 100000,
                "SecurityOpt": ["no-new-privileges"],
            },
            "NetworkSettings": {"Networks": {pilot.prefix + "-data": {}}},
            "Mounts": [],
        }
        document["HostConfig"]["Tmpfs"] = {
            path: "rw,nosuid,nodev,mode=1777,size="
            + str(size)
            + "m"
            + (",noexec" if path != "/tmp" else "")
            for path, size in rehearsal.TMPFS[component].items()
        }
        return document

    def test_start_declares_exact_runtime_environment_before_and_after_start(self):
        for component in rehearsal.IMAGES:
            with self.subTest(component=component):
                pilot = self.pilot(component)
                pilot.containers = {}
                pilot.image_env = {component: {"PATH": "/usr/bin", "HOME": "/image"}}
                pilot.commands = SimpleNamespace(deadline=time.monotonic() + 120)
                created = {"started": False}

                def call(args, created=created, **kwargs):
                    if args[:2] == ["container", "exists"]:
                        return 1, b""
                    if args[0] == "create":
                        created["args"] = args
                        return 0, b"b" * 64
                    self.assertEqual(args, ["start", "b" * 64])
                    created["started"] = True
                    return 0, b""

                def inspect(name, pilot=pilot, component=component, created=created):
                    value = self.document(pilot, component)
                    env = rehearsal.environment(
                        [arg[6:] for arg in created["args"] if arg.startswith("--env=")]
                    )
                    if created["started"]:
                        env.setdefault("HOME", "/runtime-home")
                        env.setdefault("HOSTNAME", "b" * 12)
                    value["Config"]["Env"] = [
                        key + "=" + content for key, content in env.items()
                    ]
                    value["Config"]["Timeout"] = pilot.containers[name]["timeout"]
                    value["NetworkSettings"]["Networks"] = dict.fromkeys(
                        pilot.containers[name]["networks"]
                    )
                    value["State"] = {"Running": True, "Pid": 1234}
                    return value

                with (
                    patch.object(pilot, "call", side_effect=call),
                    patch.object(pilot, "inspect", side_effect=inspect),
                    patch.object(pilot, "save"),
                    patch.object(rehearsal, "kernel_profile", return_value={}),
                ):
                    name = pilot.start(component, {"DB_PASSWORD": "synthetic-only"}, [])
                self.assertIn("--hostname=" + name, created["args"])
                self.assertIn("--env=HOSTNAME=" + name, created["args"])
                self.assertIn("--env=HOME=/tmp", created["args"])
                self.assertNotIn("environmentMismatch", pilot.record)

    def test_runtime_environment_drift_reports_only_keys_and_rejects_every_delta(self):
        for mutation, expected in (
            (
                lambda env: env.append("DOPPLER_TOKEN=private-token"),
                {"addedKeys": ["DOPPLER_TOKEN"]},
            ),
            (lambda env: env.remove("HOME=/tmp"), {"missingKeys": ["HOME"]}),
            (
                lambda env: env.__setitem__(1, "HOME=/unexpected"),
                {"changedKeys": ["HOME"]},
            ),
            (
                lambda env: env.__setitem__(0, "DB_PASSWORD=private-replacement"),
                {"changedKeys": ["DB_PASSWORD"]},
            ),
        ):
            pilot = self.pilot()
            document = self.document(pilot)
            mutation(document["Config"]["Env"])
            with self.assertRaisesRegex(ValueError, "environment differs"):
                pilot.profile(document, document["Name"])
            difference = pilot.record["environmentMismatch"][document["Name"]]
            self.assertEqual(
                difference,
                {"addedKeys": [], "missingKeys": [], "changedKeys": []} | expected,
            )
            serialized = json.dumps(pilot.record)
            for secret in (
                "private-token",
                "private-replacement",
                "synthetic-only",
                "/unexpected",
            ):
                self.assertNotIn(secret, serialized)

    def test_malformed_environment_and_wrong_hostname_remain_rejected(self):
        for entry in (
            "HOME=/duplicate",
            "container=podman",
            "X" * 129 + "=bounded",
            "HOME=bad\x00value",
        ):
            pilot = self.pilot()
            document = self.document(pilot)
            document["Config"]["Env"].append(entry)
            with self.assertRaises(ValueError):
                pilot.profile(document, document["Name"])
            self.assertNotIn(entry, json.dumps(pilot.record))
        document = self.document(pilot)
        document["Config"]["Hostname"] = "unexpected"
        with self.assertRaisesRegex(ValueError, "hostname differs"):
            pilot.profile(document, document["Name"])

    def test_datastores_reject_every_host_bind_and_named_volume(self):
        pilot = self.pilot("postgres")
        document = self.document(pilot, "postgres")
        pilot.profile(document, document["Name"])
        for kind in ("bind", "volume"):
            document["Mounts"] = [
                {
                    "Type": kind,
                    "Source": "/home/llunde-backend/data/postgres",
                    "Destination": "/var/lib/postgresql/data",
                }
            ]
            with self.assertRaisesRegex(ValueError, "mounts and volumes"):
                pilot.profile(document, document["Name"])

    def test_backend_rejects_production_environment_external_network_mount_and_privilege(
        self,
    ):
        pilot = self.pilot()
        original = self.document(pilot)
        pilot.profile(original, original["Name"])
        changed = []
        value = copy.deepcopy(original)
        value["Config"]["Env"].append("DOPPLER_TOKEN=must-never-be-used")
        changed.append(value)
        value = copy.deepcopy(original)
        value["NetworkSettings"]["Networks"]["podman"] = {}
        changed.append(value)
        value = copy.deepcopy(original)
        value["Mounts"] = [
            {"Type": "bind", "Source": "/run/secrets", "Destination": "/run/secrets"}
        ]
        changed.append(value)
        value = copy.deepcopy(original)
        value["HostConfig"]["Privileged"] = True
        changed.append(value)
        value = copy.deepcopy(original)
        value["Config"]["Entrypoint"] = ["doppler"]
        changed.append(value)
        for document in changed:
            with self.subTest(document=document), self.assertRaises(ValueError):
                pilot.profile(document, document["Name"])

    def test_absent_capability_fields_are_not_accepted_as_explicit_empty_values(self):
        pilot = self.pilot()
        document = self.document(pilot)
        del document["EffectiveCaps"]
        with self.assertRaisesRegex(ValueError, "Explicit empty"):
            pilot.profile(document, document["Name"])

    def test_only_exact_default_network_synthetic_creation_time_is_normalized(self):
        before = {
            "networks": [
                {
                    "name": "podman",
                    "id": rehearsal.DEFAULT_NETWORK_ID,
                    "created": "before",
                    "internal": False,
                }
            ]
        }
        after = copy.deepcopy(before)
        after["networks"][0]["created"] = "after"
        self.assertEqual(
            rehearsal.comparable_baseline(before), rehearsal.comparable_baseline(after)
        )
        self.assertEqual(before["networks"][0]["created"], "before")
        after["networks"][0]["internal"] = True
        self.assertNotEqual(
            rehearsal.comparable_baseline(before), rehearsal.comparable_baseline(after)
        )
        before["networks"][0]["name"] = after["networks"][0]["name"] = "other"
        after["networks"][0]["internal"] = False
        self.assertNotEqual(
            rehearsal.comparable_baseline(before), rehearsal.comparable_baseline(after)
        )

    def test_wrong_password_acceptance_is_a_failure_even_before_happy_path(self):
        replies = iter([(401, {}, b""), (403, {}, b""), (201, {}, b""), (200, {}, b"")])
        with self.assertRaisesRegex(ValueError, "Incorrect synthetic password"):
            rehearsal.auth_flow(
                lambda *args, **kwargs: next(replies), "a" * 16, "synthetic-password"
            )

    def test_authenticated_flow_requires_correct_identity_and_revoked_session_denial(
        self,
    ):
        cookie = {
            "set-cookie": [
                "llunde_session="
                + "x" * 43
                + "; Max-Age=2592000; Path=/; Secure; HttpOnly; SameSite=Lax; $x-enc=URI_ENCODING"
            ]
        }
        for final_status in (401, 200):
            replies = iter(
                [
                    (401, {}, b""),
                    (403, {}, b""),
                    (201, {}, b""),
                    (401, {}, b""),
                    (200, cookie, b""),
                    (
                        200,
                        {},
                        json.dumps(
                            {
                                "email": "infra-rehearsal-"
                                + "a" * 16
                                + "@example.invalid"
                            }
                        ).encode(),
                    ),
                    (204, {}, b""),
                    (final_status, {}, b""),
                ]
            )
            request = lambda *args, values=replies, **kwargs: next(values)
            if final_status == 401:
                result = rehearsal.auth_flow(request, "a" * 16, "synthetic-password")
                self.assertTrue(result["authenticatedIdentityVerified"])
                self.assertFalse(result["browserCookieTransportVerified"])
                self.assertFalse(result["existingUserAccountVerified"])
            else:
                with self.assertRaisesRegex(ValueError, "Revoked synthetic session"):
                    rehearsal.auth_flow(request, "a" * 16, "synthetic-password")

    def test_http_and_cookie_bounds_fail_closed(self):
        self.assertEqual(
            rehearsal.response(
                b"HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n"
            )[0],
            401,
        )
        for raw in (
            b"not-http",
            b"HTTP/1.1 200 OK\r\nBad header\r\n\r\n",
            b"x" * 32769,
        ):
            with self.assertRaises(ValueError):
                rehearsal.response(raw)
        for suffix in (
            "; Path=/; HttpOnly; SameSite=Lax",
            "; Path=/; Secure; HttpOnly; SameSite=Lax; Domain=llunde.no",
        ):
            with self.assertRaises(ValueError):
                rehearsal.session_cookie(
                    {"set-cookie": ["llunde_session=" + "x" * 43 + suffix]}
                )

    def test_exact_ktor_cookie_encoding_preserves_security_and_hides_token(self):
        token = "synthetic-session-token-" + "x" * 43
        value = "llunde_session=" + token
        attributes = "; Max-Age=2592000; Path=/; Secure; HttpOnly; SameSite=Lax"
        diagnostics = {}
        result = rehearsal.session_cookie(
            {"set-cookie": [value + attributes + "; $x-enc=URI_ENCODING"]},
            diagnostics=diagnostics,
        )
        self.assertEqual(result, value)
        self.assertEqual(
            diagnostics,
            {
                "attributeNames": [
                    "Max-Age",
                    "Path",
                    "Secure",
                    "HttpOnly",
                    "SameSite",
                    "$x-enc",
                ]
            },
        )
        self.assertNotIn(token, json.dumps(diagnostics))
        for change in (
            attributes.replace("; Secure", ""),
            attributes.replace("; HttpOnly", ""),
            attributes.replace("Lax", "None"),
            attributes.replace("Path=/", "Path=/other"),
            attributes + "; Domain=llunde.no",
        ):
            with self.assertRaises(ValueError):
                rehearsal.session_cookie(
                    {"set-cookie": [value + change + "; $x-enc=URI_ENCODING"]}
                )

    def test_cookie_extension_duplicates_alternatives_and_controls_are_rejected(self):
        value = (
            "llunde_session=" + "x" * 43 + "; Path=/; Secure; HttpOnly; SameSite=Lax"
        )
        for suffix in (
            "; $x-enc=RAW",
            "; $x-enc=URI_ENCODING; $x-enc=URI_ENCODING",
            "; $Version=1",
            "; $unknown=private-value",
            "; Secure=false",
            "; Path=/other",
            "; other_cookie=private-value",
            "; $x-enc=URI_ENCODING\r\nInjected: private-value",
            "; " + "x" * 65 + "=private-value",
            "; x=y" * 16,
        ):
            diagnostics = {}
            with self.assertRaises(ValueError) as error:
                rehearsal.session_cookie(
                    {"set-cookie": [value + suffix]}, diagnostics=diagnostics
                )
            self.assertNotIn("private-value", str(error.exception))
            self.assertNotIn("private-value", json.dumps(diagnostics))

    def test_cookie_parser_error_is_sanitized_and_does_not_include_header(self):
        value = (
            "llunde_session=" + "x" * 43 + "; Path=/; Secure; HttpOnly; SameSite=Lax"
        )
        with (
            patch.object(
                rehearsal.http.cookies.SimpleCookie,
                "load",
                side_effect=rehearsal.http.cookies.CookieError("private-token"),
            ),
            self.assertRaisesRegex(
                rehearsal.RehearsalError, "Session cookie syntax differs"
            ) as error,
        ):
            rehearsal.session_cookie({"set-cookie": [value]})
        self.assertNotIn("private-token", str(error.exception))

    def test_unrelated_failure_does_not_reclassify_successful_cleanup(self):
        pilot = self.pilot()
        pilot.initialized = True

        def cleanup():
            pilot.record["cleanupVerified"] = True
            pilot.record["applicationStatesUnchanged"] = True
            return True

        with (
            patch.object(
                pilot,
                "preflight",
                side_effect=rehearsal.http.cookies.CookieError("unrelated failure"),
            ),
            patch.object(pilot, "cleanup", side_effect=cleanup),
            patch.object(pilot, "save") as save,
            patch.object(rehearsal.signal, "signal"),
            self.assertRaisesRegex(
                rehearsal.http.cookies.CookieError, "unrelated failure"
            ),
        ):
            pilot.run()
        save.assert_called_once()
        self.assertTrue(pilot.record["cleanupVerified"])
        self.assertNotIn("cleanupFailureType", pilot.record)
        self.assertFalse(pilot.record["passed"])

    def test_runtime_headroom_is_required_before_restore(self):
        rehearsal.capacity("MemAvailable: 4000000 kB\n", 2560 * 1024**2, 100 * 1024**2)
        for memory, limit, current in (
            ("MemAvailable: 1000 kB\n", 2560 * 1024**2, 0),
            ("MemAvailable: 4000000 kB\n", 128 * 1024**2, 0),
            ("MemAvailable: 4000000 kB\n", 2560 * 1024**2, 257 * 1024**2),
        ):
            with self.assertRaises(ValueError):
                rehearsal.capacity(memory, limit, current)

    def test_tmpfs_sizes_and_exact_mount_set_are_enforced(self):
        values = {
            path: "rw,nosuid,nodev,noexec,mode=1777,size=" + str(size) + "m"
            for path, size in rehearsal.TMPFS["postgres"].items()
        }
        rehearsal.tmpfs_profile(values, "postgres")
        for changed in (
            values
            | {"/var/lib/postgresql/data": "rw,nosuid,nodev,noexec,mode=1777,size=8g"},
            values | {"/unexpected": "size=1m"},
            values | {"/run/postgresql": "rw,nosuid,nodev,noexec,mode=700,size=4m"},
        ):
            with self.assertRaises(ValueError):
                rehearsal.tmpfs_profile(changed, "postgres")

    def test_postgres_uses_only_canonical_socket_mount_before_and_after_start(self):
        pilot = self.pilot("postgres")
        document = self.document(pilot, "postgres")
        name = document["Name"]
        self.assertIn("/run/postgresql", document["HostConfig"]["Tmpfs"])
        self.assertNotIn("/var/run/postgresql", document["HostConfig"]["Tmpfs"])
        pilot.profile(document, name)
        document["State"] = {"Running": True}
        pilot.profile(document, name)
        self.assertEqual(
            pilot.record["tmpfsProfiles"][name]["notRunning"],
            pilot.record["tmpfsProfiles"][name]["running"],
        )
        tmpfs = document["HostConfig"]["Tmpfs"]
        tmpfs["/var/run/postgresql"] = tmpfs.pop("/run/postgresql")
        with self.assertRaisesRegex(ValueError, "Exact ephemeral"):
            pilot.profile(document, name)
        self.assertIn(
            "/var/run/postgresql",
            json.dumps(pilot.record["tmpfsProfiles"][name]["running"]),
        )

    def test_tmpfs_failure_retains_paths_options_before_validation_without_other_config(
        self,
    ):
        pilot = self.pilot("postgres")
        document = self.document(pilot, "postgres")
        document["HostConfig"]["Tmpfs"].pop("/run/postgresql")
        document["Config"]["Labels"]["unrelated"] = "private-label"
        with self.assertRaisesRegex(ValueError, "Exact ephemeral"):
            pilot.profile(document, document["Name"])
        observed = json.dumps(pilot.record)
        self.assertIn("/var/lib/postgresql/data", observed)
        self.assertIn("size=128m", observed)
        for content in ("synthetic-only", "private-label", "DB_PASSWORD"):
            self.assertNotIn(content, observed)

    def test_tmpfs_diagnostic_redacts_unknown_options_and_bounds_every_field(self):
        diagnostic = rehearsal.tmpfs_diagnostic(
            {"/tmp": "rw,size=64m,context=private-value,password=private-password"}
        )
        self.assertEqual(
            diagnostic["mounts"][0]["options"],
            ["rw", "size=64m", "<unrecognized>", "<unrecognized>"],
        )
        for values in (
            None,
            {"/path" + str(index): "rw" for index in range(17)},
            {"/" + "x" * 128: "rw"},
            {"/../tmp": "rw"},
            {"/tmp": "x" * 1025},
            {"/tmp": "rw," * 32},
            {"/tmp": None},
        ):
            result = rehearsal.tmpfs_diagnostic(values)
            self.assertFalse(result["shapeValid"])
            self.assertLess(len(json.dumps(result)), 256)

    def test_unexpected_cleanup_error_still_persists_failure_after_known_initialization(
        self,
    ):
        pilot = self.pilot()
        pilot.initialized = True
        with (
            patch.object(pilot, "preflight", side_effect=ValueError("startup failed")),
            patch.object(
                pilot, "cleanup", side_effect=ZeroDivisionError("private text")
            ),
            patch.object(pilot, "save") as save,
            patch.object(rehearsal.signal, "signal"),
            self.assertRaisesRegex(ValueError, "Cleanup failed"),
        ):
            pilot.run()
        save.assert_called_once()
        self.assertFalse(pilot.record["passed"])
        self.assertEqual(pilot.record["cleanupFailureType"], "ZeroDivisionError")
        self.assertNotIn("private text", json.dumps(pilot.record))

    def test_unvalidated_output_path_is_not_written_on_preflight_failure(self):
        pilot = self.pilot()
        with (
            patch.object(pilot, "preflight", side_effect=ValueError("unsafe output")),
            patch.object(pilot, "cleanup", return_value=True),
            patch.object(pilot, "save") as save,
            patch.object(rehearsal.signal, "signal"),
        ):
            result = pilot.run()
        save.assert_not_called()
        self.assertFalse(result["passed"])

    def test_removed_container_is_verified_after_nonzero_client_status(self):
        pilot = self.pilot()
        value = self.document(pilot)
        name = value["Name"]
        replies = [
            (0, b""),
            (0, json.dumps([value]).encode()),
            (137, b""),
            (1, b""),
            (1, b""),
        ]
        with patch.object(pilot, "call", side_effect=replies):
            pilot.remove(name)
        self.assertFalse(pilot.containers)
        self.assertEqual(pilot.record["removalClientStatus"][name], 137)

    def test_cleanup_recovers_lost_create_reply_only_with_exact_nonce_image_and_name(
        self,
    ):
        pilot = self.pilot()
        value = self.document(pilot)
        name = value["Name"]
        pilot.containers[name]["id"] = None
        value["Config"]["Labels"]["infra.backend-rehearsal"] = "unrelated"
        with (
            patch.object(
                pilot, "call", side_effect=[(0, b""), (0, json.dumps([value]).encode())]
            ) as call,
            self.assertRaisesRegex(ValueError, "ownership"),
        ):
            pilot.remove(name)
        self.assertFalse(any(item.args[0][0] == "rm" for item in call.call_args_list))

    def test_failed_removal_retains_only_owned_runtime_identity_and_records_no_success(
        self,
    ):
        pilot = self.pilot()
        with (
            patch.object(
                pilot, "remove", side_effect=ValueError("owned container remains")
            ),
            patch.object(pilot, "call") as call,
        ):
            self.assertFalse(pilot.cleanup())
        call.assert_not_called()
        self.assertFalse(pilot.record["cleanup"]["containersRemoved"])
        self.assertEqual(len(pilot.record["cleanup"]["failures"]), 1)

    def test_receipt_does_not_persist_synthetic_password_or_environment(self):
        pilot = self.pilot()
        with patch.object(rehearsal, "durable_json") as write:
            pilot.save()
        serialized = json.dumps(write.call_args.args[1])
        self.assertNotIn("synthetic-only", serialized)
        self.assertNotIn("DB_PASSWORD", serialized)

    def test_requests_cannot_target_production_url_or_unreviewed_route(self):
        pilot = self.pilot()
        with patch.object(pilot, "call") as call:
            for method, path in (
                ("GET", "https://llunde.no"),
                ("DELETE", "/auth/me"),
                ("GET", "/admin"),
            ):
                with self.assertRaises(ValueError):
                    pilot.request(method, path)
        call.assert_not_called()


if __name__ == "__main__":
    unittest.main()
