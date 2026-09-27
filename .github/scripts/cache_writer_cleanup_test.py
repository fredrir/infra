import copy
import datetime
import http.server
import json
import threading
import unittest
import urllib.parse

import cache_writer_cleanup as cleanup


NOW = datetime.datetime(2026, 9, 28, 4, tzinfo=datetime.timezone.utc)


def device(node_id="node-old", age=7200, **changes):
    value = {
        "nodeId": node_id,
        "tags": [cleanup.WRITER_TAG],
        "isExternal": False,
        "created": (NOW - datetime.timedelta(seconds=age)).isoformat(),
        "connectedToControl": True,
    }
    return value | changes


class CleanupTest(unittest.TestCase):
    def setUp(self):
        self.requests = []
        self.routes = {}
        owner = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                self.handle_request()

            do_POST = do_DELETE = do_GET

            def handle_request(self):
                body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
                owner.requests.append((self.command, self.path, self.headers.get("Authorization"), body))
                code, value, headers = owner.routes.get((self.command, self.path), (500, {}, {}))
                self.send_response(code)
                for key, value_header in headers.items():
                    self.send_header(key, value_header)
                self.end_headers()
                self.wfile.write(value if isinstance(value, bytes) else json.dumps(value).encode())

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.001})
        self.thread.start()
        self.api = cleanup.API(f"http://127.0.0.1:{self.server.server_port}")
        self.api.token = "fixture-access-token"

    def tearDown(self):
        self.server.shutdown()
        self.thread.join()
        self.server.server_close()

    def inventory(self, devices):
        self.routes[("GET", "/tailnet/-/devices?fields=all")] = (200, {"devices": devices}, {})
        for value in devices:
            if isinstance(value, dict) and "nodeId" in value:
                self.routes[("GET", "/device/" + value["nodeId"] + "?fields=all")] = (200, copy.deepcopy(value), {})
                self.routes[("DELETE", "/device/" + value["nodeId"])] = (204, b"", {})

    def deletes(self):
        return [path for method, path, _, _ in self.requests if method == "DELETE"]

    def run_sweep(self, apply=True):
        report = {}
        cleanup.sweep(self.api, report, apply, NOW)
        return report

    def test_only_old_single_tag_writers_are_removed_even_when_connected(self):
        self.inventory([
            device(), device("node-young", age=3599), device("node-future", age=-1),
            device("node-reader", tags=["tag:ci-bazel-reader"]),
            device("node-mixed", tags=[cleanup.WRITER_TAG, "tag:infra-reconciler"]),
            device("node-external", isExternal=True),
            device("node-unmarked", isExternal=None),
            device("node-duplicate-tag", tags=[cleanup.WRITER_TAG, cleanup.WRITER_TAG]),
        ])
        report = self.run_sweep()
        self.assertEqual(self.deletes(), ["/device/node-old"])
        self.assertEqual(report["devices"][0]["result"], "deleted")
        self.assertEqual(len(self.requests), 3)
        self.assertTrue(all(request[2] == "Bearer fixture-access-token" for request in self.requests))

    def test_exact_age_boundary_and_timezone_offset(self):
        self.inventory([device(created="2026-09-28T05:00:00+02:00")])
        self.run_sweep()
        self.assertEqual(self.deletes(), ["/device/node-old"])

    def test_preview_refreshes_without_deleting(self):
        self.inventory([device()])
        report = self.run_sweep(apply=False)
        self.assertEqual(self.deletes(), [])
        self.assertEqual(report["devices"][0]["result"], "would_delete")

    def test_changed_tag_age_identity_or_external_status_prevents_deletion(self):
        for changes in [
            {"tags": ["tag:ci-bazel-reader"]},
            {"tags": [cleanup.WRITER_TAG, "tag:server"]},
            {"created": NOW.isoformat()}, {"nodeId": "node-other"},
            {"created": "2026-09-27T01:00:00Z"}, {"isExternal": True},
        ]:
            with self.subTest(changes=changes):
                self.inventory([device()])
                self.routes[("GET", "/device/node-old?fields=all")] = (200, device(**changes), {})
                self.assertEqual(self.run_sweep()["devices"][0]["result"], "changed")
                self.assertEqual(self.deletes(), [])

    def test_missing_nodes_are_idempotent(self):
        self.inventory([device()])
        self.routes[("GET", "/device/node-old?fields=all")] = (404, {}, {})
        self.assertEqual(self.run_sweep()["devices"][0]["result"], "already_absent")
        self.assertEqual(self.deletes(), [])
        self.inventory([device()])
        self.routes[("DELETE", "/device/node-old")] = (404, {}, {})
        self.assertEqual(self.run_sweep()["devices"][0]["result"], "deleted")

    def test_invalid_writer_metadata_aborts_before_any_deletion(self):
        for changes in [
            {"created": ""}, {"created": "yesterday"}, {"created": "2026-09-28T01:00:00"},
            {"created": "2026-99-28T01:00:00Z"}, {"created": 1}, {"nodeId": "../../other"},
        ]:
            with self.subTest(changes=changes):
                self.inventory([device(), device("node-bad", **changes)])
                with self.assertRaises(cleanup.CleanupError):
                    self.run_sweep()
                self.assertEqual(self.deletes(), [])

    def test_partial_or_paginated_or_duplicate_inventory_cannot_delete(self):
        for body, headers in [
            ({"devices": [device()], "next": "page2"}, {}),
            ({"devices": [device()], "nextToken": "page2"}, {}),
            ({"devices": [device()]}, {"Link": '<https://other.invalid/>; rel="next"'}),
            ({"devices": [device(), device()]}, {}),
            ({"devices": [device(), device(tags=["tag:ci-bazel-reader"])]}, {}),
            ({"devices": None}, {}),
            (b'{"devices":[],"devices":[]}', {}),
            (b'{"devices":[', {}),
        ]:
            with self.subTest(body=body):
                self.routes[("GET", "/tailnet/-/devices?fields=all")] = (200, body, headers)
                with self.assertRaises(cleanup.CleanupError):
                    self.run_sweep()
                self.assertEqual(self.deletes(), [])

    def test_large_inventory_makes_bounded_oldest_first_progress(self):
        nodes = [device(f"node-{index:03}", age=3600 + index) for index in range(101)]
        self.inventory(nodes)
        report = self.run_sweep()
        self.assertEqual(report["eligible"], 101)
        self.assertEqual(report["deferred"], 1)
        self.assertEqual(self.deletes(), [f"/device/node-{index:03}" for index in range(100, 0, -1)])
        self.inventory([nodes[0]])
        self.assertEqual(self.run_sweep()["deferred"], 0)
        self.assertEqual(len(self.deletes()), 101)
        self.assertEqual(self.deletes()[-1], "/device/node-000")

    def test_rate_limit_after_deletion_preserves_progress_and_stops(self):
        nodes = [device(f"node-{index}") for index in range(3)]
        self.inventory(nodes)
        self.routes[("DELETE", "/device/node-1")] = (429, {}, {})
        progress, report = [], {}
        with self.assertRaises(cleanup.CleanupError):
            cleanup.sweep(self.api, report, True, NOW, lambda current: progress.append(copy.deepcopy(current)))
        self.assertEqual(self.deletes(), ["/device/node-0", "/device/node-1"])
        self.assertEqual(progress[-1]["devices"][0]["result"], "deleted")
        self.inventory(nodes[1:])
        self.assertEqual(len(self.run_sweep()["devices"]), 2)
        self.assertEqual(self.deletes()[-2:], ["/device/node-1", "/device/node-2"])

    def test_http_failure_stops_and_never_echoes_response_secrets(self):
        for code in [401, 403, 429, 500]:
            with self.subTest(code=code):
                self.inventory([device()])
                self.routes[("GET", "/device/node-old?fields=all")] = (code, {"secret": "sensitive-value"}, {})
                with self.assertRaises(cleanup.CleanupError) as caught:
                    self.run_sweep()
                self.assertNotIn("sensitive-value", str(caught.exception))
                self.assertEqual(self.deletes(), [])

    def test_redirect_cannot_leak_authorization(self):
        self.routes[("GET", "/tailnet/-/devices?fields=all")] = (302, {}, {"Location": self.api.base + "/other"})
        with self.assertRaises(cleanup.CleanupError):
            self.run_sweep()
        self.assertEqual(len(self.requests), 1)

    def test_oauth_requests_only_dedicated_writer_deletion_authority(self):
        self.api.token = None
        self.routes[("POST", "/oauth/token")] = (200, {
            "access_token": "fixture-access-token", "token_type": "Bearer",
            "scope": "devices:core", "expires_in": 3600,
        }, {})
        self.api.authenticate("cleanup-client", "tskey-client-fixture")
        self.assertEqual(self.api.token, "fixture-access-token")
        method, path, authorization, body = self.requests[0]
        self.assertEqual((method, path, authorization), ("POST", "/oauth/token", None))
        self.assertEqual(urllib.parse.parse_qs(body.decode()), {
            "grant_type": ["client_credentials"], "client_id": ["cleanup-client"],
            "client_secret": ["tskey-client-fixture"], "scope": ["devices:core"],
            "tags": [cleanup.WRITER_TAG],
        })

    def test_oauth_broader_authority_and_malformed_expiry_are_rejected(self):
        for change in [{"scope": "all"}, {"scope": "devices:core policy_file"},
                       {"expires_in": True}, {"expires_in": 3601}, {"token_type": []}]:
            with self.subTest(change=change):
                self.routes[("POST", "/oauth/token")] = (200, {
                    "access_token": "fixture-access-token", "token_type": "Bearer",
                    "scope": "devices:core", "expires_in": 3600,
                } | change, {})
                with self.assertRaises(cleanup.CleanupError):
                    self.api.authenticate("cleanup-client", "tskey-client-fixture")

    def test_other_credentials_are_not_sent(self):
        for key in ["", "tskey-auth-bootstrap", "tskey-api-personal"]:
            with self.subTest(key=key):
                with self.assertRaises(cleanup.CleanupError):
                    self.api.authenticate("cleanup-client", key)
        self.assertEqual(self.requests, [])


if __name__ == "__main__":
    unittest.main()
