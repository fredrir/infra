import base64
import copy
import json
import os
import shutil
import subprocess
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "platform/components/build-cache/provision.sh"
ADMIN_TOKEN = "admin-token-value-0123456789"
SERVICE_ACCOUNT_TOKEN = "service-account-token-value"
PROVISIONER = ("GK" + "0" * 24, "a" * 64)
NONE = {"read": False, "write": False, "owner": False}


def project_keys(project, seed):
    return {role: ("GK" + f"{seed}{index}".rjust(24, "0"), f"{seed}{index}".rjust(64, "b"))
            for index, role in enumerate(["rw", "ro", "release"], start=1)}


class FakeCluster:
    def __init__(self):
        self.lock = threading.Lock()
        self.requests = []
        self.layout = {"version": 0, "roles": [], "staged": []}
        self.keys = {}
        self.buckets = {}
        self.secrets = {}

    def bucket_view(self, bucket_id):
        bucket = self.buckets[bucket_id]
        return {"id": bucket_id, "globalAliases": [bucket["alias"]], "quotas": bucket["quotas"],
                "lifecycleRules": bucket["rules"],
                "keys": [{"accessKeyId": key, "name": self.keys[key]["name"], "permissions": permissions}
                         for key, permissions in bucket["permissions"].items() if any(permissions.values())]}

    def bucket_by_alias(self, alias):
        return next((bucket_id for bucket_id, bucket in self.buckets.items() if bucket["alias"] == alias), None)

    def handle(self, method, path, query, body):
        self.requests.append((method, path))
        if path == "/v2/GetClusterStatus":
            return 200, {"layoutVersion": self.layout["version"], "nodes": [{"id": "node-1", "isUp": True, "draining": False}]}
        if path == "/v2/GetClusterLayout":
            return 200, {"version": self.layout["version"], "roles": self.layout["roles"], "parameters": {},
                         "partitionSize": 0, "stagedRoleChanges": self.layout["staged"]}
        if path == "/v2/UpdateClusterLayout":
            self.layout["staged"] = body["roles"]
            return 200, {}
        if path == "/v2/ApplyClusterLayout":
            if body["version"] != self.layout["version"] + 1:
                return 400, {"code": "InvalidRequest"}
            self.layout.update(version=body["version"], roles=self.layout["staged"], staged=[])
            return 200, {}
        if path == "/v2/GetKeyInfo":
            key = self.keys.get(query["id"])
            if not key or key["deleted"]:
                return 404, {"code": "NoSuchAccessKey"}
            view = {"accessKeyId": query["id"], "name": key["name"]}
            if query.get("showSecretKey") == "true":
                view["secretAccessKey"] = key["secret"]
            return 200, view
        if path == "/v2/ImportKey":
            if body["accessKeyId"] in self.keys:
                return 409, {"code": "KeyAlreadyExists"}
            self.keys[body["accessKeyId"]] = {"name": body["name"], "secret": body["secretAccessKey"], "deleted": False}
            return 200, {"accessKeyId": body["accessKeyId"]}
        if path == "/v2/UpdateKey":
            self.keys[query["id"]]["name"] = body["name"]
            return 200, {}
        if path == "/v2/ListKeys":
            return 200, [{"id": key, "name": value["name"]} for key, value in self.keys.items() if not value["deleted"]]
        if path == "/v2/DeleteKey":
            self.keys[query["id"]]["deleted"] = True
            for bucket in self.buckets.values():
                bucket["permissions"].pop(query["id"], None)
            return 200, {}
        if path == "/v2/GetBucketInfo":
            bucket_id = self.bucket_by_alias(query["globalAlias"])
            return (200, self.bucket_view(bucket_id)) if bucket_id else (404, {"code": "NoSuchBucket"})
        if path == "/v2/CreateBucket":
            bucket_id = f"bucket-{len(self.buckets) + 1}"
            self.buckets[bucket_id] = {"alias": body["globalAlias"], "quotas": {}, "rules": None, "permissions": {}}
            return 200, self.bucket_view(bucket_id)
        if path == "/v2/UpdateBucket":
            rules = body["lifecycleRules"]
            for rule in rules:
                if rule["Status"] not in ["Enabled", "Disabled"] or not isinstance(rule["Expiration"]["Days"], int):
                    return 400, {"code": "InvalidRequest"}
            bucket = self.buckets[query["id"]]
            bucket.update(quotas=body["quotas"], rules=rules or None)
            return 200, self.bucket_view(query["id"])
        if path in ["/v2/AllowBucketKey", "/v2/DenyBucketKey"]:
            if body["accessKeyId"] not in self.keys or self.keys[body["accessKeyId"]]["deleted"]:
                return 404, {"code": "NoSuchAccessKey"}
            current = self.buckets[body["bucketId"]]["permissions"].setdefault(body["accessKeyId"], dict(NONE))
            for flag, value in body["permissions"].items():
                if value:
                    current[flag] = path == "/v2/AllowBucketKey"
            return 200, self.bucket_view(body["bucketId"])
        return 404, {"code": "UnknownEndpoint"}

    def secret_list(self):
        return {"items": [{"metadata": {"name": name, "labels": {"infra.fredrir.com/build-cache-project": project}},
                           "data": {field: base64.b64encode(value.encode()).decode() for field, value in data.items()}}
                          for name, (project, data) in self.secrets.items()]}

    def add_project(self, project, keys):
        self.secrets[f"build-cache-{project}"] = (project, {
            f"{role}_{part}": value for role, (key, secret) in keys.items() for part, value in [("id", key), ("secret", secret)]})

    def grants(self, alias):
        return {key: permissions for key, permissions in self.buckets[self.bucket_by_alias(alias)]["permissions"].items()
                if any(permissions.values())}


def server_for(cluster):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def respond(self, status, payload):
            data = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def dispatch(self, method):
            url = urlsplit(self.path)
            query = {key: values[0] for key, values in parse_qs(url.query).items()}
            length = int(self.headers.get("Content-Length") or 0)
            body = json.loads(self.rfile.read(length)) if length else None
            authorization = self.headers.get("Authorization")
            with cluster.lock:
                if url.path.startswith("/api/v1/"):
                    if authorization != f"Bearer {SERVICE_ACCOUNT_TOKEN}":
                        return self.respond(401, {})
                    if url.path != "/api/v1/namespaces/build-cache/secrets" or query.get("labelSelector") != "infra.fredrir.com/build-cache-project":
                        return self.respond(404, {})
                    return self.respond(200, cluster.secret_list())
                if authorization != f"Bearer {ADMIN_TOKEN}":
                    return self.respond(403, {"code": "Forbidden"})
                self.respond(*cluster.handle(method, url.path, query, body))

        def do_GET(self):
            self.dispatch("GET")

        def do_POST(self):
            self.dispatch("POST")

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


@unittest.skipUnless(shutil.which("bash") and shutil.which("jq") and shutil.which("curl"), "Native shell tools required")
class BuildCacheProvisionerTests(unittest.TestCase):
    def setUp(self):
        self.cluster = FakeCluster()
        self.server = server_for(self.cluster)
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        account = Path(self.directory.name)
        (account / "token").write_text(SERVICE_ACCOUNT_TOKEN)
        (account / "namespace").write_text("build-cache")
        (account / "ca.crt").write_text("unused for plain http")
        self.environment = {
            "PATH": os.environ["PATH"],
            "TMPDIR": self.directory.name,
            "GARAGE_ADMIN_URL": f"http://127.0.0.1:{self.server.server_port}",
            "GARAGE_ADMIN_TOKEN": ADMIN_TOKEN,
            "PROVISIONER_KEY_ID": PROVISIONER[0],
            "PROVISIONER_KEY_SECRET": PROVISIONER[1],
            "LAYOUT_CAPACITY_BYTES": "150000000000",
            "MAIN_QUOTA_BYTES": "21474836480",
            "RELEASE_QUOTA_BYTES": "10737418240",
            "TOOLCHAINS_QUOTA_BYTES": "5368709120",
            "EXPIRATION_DAYS": "14",
            "KUBERNETES_API_URL": f"http://127.0.0.1:{self.server.server_port}",
            "SERVICE_ACCOUNT_DIR": str(account),
            "WAIT_ATTEMPTS": "2",
            "WAIT_SECONDS": "0",
        }
        self.nsql = project_keys("nsql", 1)
        self.ui_box = project_keys("ui-box", 2)
        self.cluster.add_project("nsql", self.nsql)
        self.cluster.add_project("ui-box", self.ui_box)

    def provision(self):
        self.cluster.requests.clear()
        result = subprocess.run(["bash", str(SCRIPT)], env=self.environment, capture_output=True, text=True, check=False, timeout=60)
        secrets = [ADMIN_TOKEN, SERVICE_ACCOUNT_TOKEN, PROVISIONER[1]] + [secret for keys in [self.nsql, self.ui_box] for _, secret in keys.values()]
        for secret in secrets:
            self.assertNotIn(secret, result.stdout + result.stderr)
        return result

    def mutations(self):
        return [path for method, path in self.cluster.requests if method == "POST"]

    def test_fresh_cluster_gets_layout_keys_buckets_and_exact_grants(self):
        result = self.provision()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.cluster.layout["version"], 1)
        self.assertEqual(self.cluster.layout["roles"], [{"id": "node-1", "zone": "dc1", "capacity": 150000000000, "tags": []}])
        self.assertEqual({value["name"] for value in self.cluster.keys.values()}, {
            "build-cache-provisioner", "ci-nsql-rw", "ci-nsql-ro", "ci-nsql-release",
            "ci-ui-box-rw", "ci-ui-box-ro", "ci-ui-box-release"})
        owner = {PROVISIONER[0]: {"read": False, "write": False, "owner": True}}
        self.assertEqual(self.cluster.grants("ci-nsql-main"), owner | {
            self.nsql["rw"][0]: {"read": True, "write": True, "owner": False},
            self.nsql["ro"][0]: {"read": True, "write": False, "owner": False}})
        self.assertEqual(self.cluster.grants("ci-nsql-release"), owner | {
            self.nsql["release"][0]: {"read": True, "write": True, "owner": False}})
        self.assertEqual(self.cluster.grants("toolchains"), {PROVISIONER[0]: {"read": True, "write": True, "owner": True}} | {
            self.nsql["release"][0]: {"read": True, "write": False, "owner": False},
            self.ui_box["release"][0]: {"read": True, "write": False, "owner": False}})
        main = self.cluster.buckets[self.cluster.bucket_by_alias("ci-ui-box-main")]
        self.assertEqual(main["quotas"], {"maxSize": 21474836480, "maxObjects": None})
        self.assertEqual(main["rules"][0]["Expiration"], {"Days": 14})
        release = self.cluster.buckets[self.cluster.bucket_by_alias("ci-ui-box-release")]
        self.assertEqual(release["quotas"]["maxSize"], 10737418240)
        toolchains = self.cluster.buckets[self.cluster.bucket_by_alias("toolchains")]
        self.assertEqual(toolchains["quotas"]["maxSize"], 5368709120)
        self.assertIsNone(toolchains["rules"])

    def test_rerun_only_reasserts_bucket_settings(self):
        self.assertEqual(self.provision().returncode, 0)
        result = self.provision()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(set(self.mutations()), {"/v2/UpdateBucket"})

    def test_drifted_grants_are_narrowed_and_removed_projects_are_revoked(self):
        self.assertEqual(self.provision().returncode, 0)
        main = self.cluster.bucket_by_alias("ci-nsql-main")
        self.cluster.buckets[main]["permissions"][self.nsql["ro"][0]]["write"] = True
        self.cluster.buckets[main]["permissions"][self.ui_box["rw"][0]] = {"read": True, "write": True, "owner": False}
        del self.cluster.secrets["build-cache-ui-box"]
        result = self.provision()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.cluster.grants("ci-nsql-main")[self.nsql["ro"][0]], {"read": True, "write": False, "owner": False})
        self.assertTrue(all(self.cluster.keys[key]["deleted"] for key, _ in self.ui_box.values()))
        self.assertFalse(self.cluster.keys[PROVISIONER[0]]["deleted"])
        self.assertNotIn(self.ui_box["release"][0], self.cluster.grants("toolchains"))
        self.assertIn("/v2/DenyBucketKey", self.mutations())

    def test_changed_secrets_and_reused_ids_refuse_without_leaking(self):
        self.assertEqual(self.provision().returncode, 0)
        rotated = copy.deepcopy(self.nsql)
        rotated["rw"] = (rotated["rw"][0], "c" * 64)
        self.cluster.add_project("nsql", rotated)
        result = self.provision()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("rotate by generating a new key id", result.stderr)
        self.assertNotIn("c" * 64, result.stdout + result.stderr)
        self.cluster.add_project("nsql", self.nsql)
        self.cluster.keys[self.nsql["ro"][0]]["deleted"] = True
        result = self.provision()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("reuses a deleted key id", result.stderr)

    def test_malformed_project_secrets_are_rejected(self):
        for name, project, data in [
            ("build-cache-nsql", "Bad_Name", self.cluster.secrets["build-cache-nsql"][1]),
            ("other-name", "nsql", self.cluster.secrets["build-cache-nsql"][1]),
            ("build-cache-nsql", "nsql", {"rw_id": "GK1"}),
            ("build-cache-nsql", "nsql", {**self.cluster.secrets["build-cache-nsql"][1], "ro_id": "not-a-key"}),
        ]:
            with self.subTest(name=name, project=project):
                self.cluster.secrets = {name: (project, data)}
                self.assertNotEqual(self.provision().returncode, 0)

    def test_wrong_admin_token_fails_before_any_change(self):
        self.environment["GARAGE_ADMIN_TOKEN"] = "wrong-token-value-0000000"
        result = self.provision()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.mutations(), [])
        self.assertIn("did not become available", result.stderr)


if __name__ == "__main__":
    unittest.main()
