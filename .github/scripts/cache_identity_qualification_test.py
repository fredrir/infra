import base64
import json
import unittest
import urllib.parse
from unittest.mock import patch

import cache_identity_qualification as probe


class IdentityTest(unittest.TestCase):
    def check(self, status, response, changes=None):
        claims = {"sub": "repo:fredrir@114402558/infra@1328085692:ref:refs/heads/main",
                  "ref": "refs/heads/main", "ref_protected": "true", "aud": "bazel-cache-check-writer",
                  "job_workflow_ref": "fredrir/infra/.github/workflows/cache-identity-negative.yml@refs/heads/main"}
        claims.update(changes or {})
        token = "header." + base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=") + ".signature"
        with patch.dict(probe.os.environ, {"ACTIONS_ID_TOKEN_REQUEST_URL": "https://pipelines.actions.githubusercontent.com/token?existing=value",
                                         "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "fixture-request-token"}), \
                patch.object(probe, "request_json", side_effect=[(200, {"value": token}), (status, response)]) as request:
            result = probe.denied_exchange("bazel-cache-check-writer", "fixture-client")
        github_request = request.call_args_list[0].args[0]
        self.assertEqual(urllib.parse.parse_qs(urllib.parse.urlsplit(github_request.full_url).query),
                         {"existing": ["value"], "audience": ["bazel-cache-check-writer"]})
        exchange = request.call_args_list[1].args[0]
        self.assertEqual(exchange.full_url, "https://controlplane.tailscale.com/api/v2/oauth/token-exchange")
        self.assertEqual(urllib.parse.parse_qs(exchange.data.decode(), keep_blank_values=True),
                         {"grant_type": ["authorization_code"], "code": [""], "client_id": ["fixture-client"], "jwt": [token]})
        self.assertNotIn(token, json.dumps(result))
        self.assertNotIn("fixture-request-token", json.dumps(result))
        self.assertNotIn("fixture-unexpected-access-token", json.dumps(result))
        return result

    def test_actual_denial_required(self):
        for status in (400, 401, 403):
            with self.subTest(status=status):
                self.assertTrue(self.check(status, {"error": "invalid_grant"})["passed"])
        for status, body in [(200, {"access_token": "fixture-unexpected-access-token"}),
                             (200, {}), (500, {}), (403, {"access_token": "fixture-unexpected-access-token"})]:
            with self.subTest(status=status):
                self.assertFalse(self.check(status, body)["passed"])

    def test_other_claim_mismatches_cannot_count_as_wrong_workflow_evidence(self):
        for changes in [{"aud": "wrong-audience"}, {"ref_protected": "false"},
                        {"sub": "repo:other:ref:refs/heads/main"}, {"ref": "refs/heads/topic"},
                        {"job_workflow_ref": "fredrir/infra/.github/workflows/check.yml@refs/heads/main"}]:
            with self.subTest(changes=changes):
                with self.assertRaises(probe.ProbeError):
                    self.check(403, {}, changes)


if __name__ == "__main__":
    unittest.main()
