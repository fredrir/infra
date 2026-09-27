import base64
import json
import os
import pathlib
import urllib.error
import urllib.parse
import urllib.request


class ProbeError(Exception):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, newurl):
        fp.close()
        raise ProbeError("Identity redirect refused")


def request_json(request):
    opener = urllib.request.build_opener(NoRedirect())
    try:
        with opener.open(request, timeout=15) as response:
            body = response.read(65537)
            if len(body) > 65536:
                raise ProbeError("Identity response too large")
            document = json.loads(body)
            if not isinstance(document, dict):
                raise ProbeError("Invalid identity response")
            return response.status, document
    except urllib.error.HTTPError as error:
        code = error.code
        try:
            document = json.loads(error.read(65536))
            if not isinstance(document, dict):
                document = {}
        except (ValueError, UnicodeError):
            document = {}
        finally:
            error.close()
        return code, document


def denied_exchange(identity, client_id):
    source = os.environ.get("ACTIONS_ID_TOKEN_REQUEST_URL", "")
    bearer = os.environ.get("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
    url = urllib.parse.urlsplit(source)
    if url.scheme != "https" or not url.hostname or not url.hostname.endswith(".actions.githubusercontent.com") or not bearer or not client_id:
        raise ProbeError("GitHub identity request unavailable")
    query = urllib.parse.parse_qs(url.query)
    query["audience"] = [identity]
    address = urllib.parse.urlunsplit(url._replace(query=urllib.parse.urlencode(query, doseq=True)))
    code, token = request_json(urllib.request.Request(address, headers={"Authorization": "Bearer " + bearer}))
    if code != 200 or not isinstance(token.get("value"), str):
        raise ProbeError("GitHub identity request failed")
    jwt = token["value"]
    payload = jwt.split(".")[1]
    claims = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))
    if not isinstance(claims, dict):
        raise ProbeError("Invalid identity claims")
    if (claims.get("sub") != "repo:fredrir@114402558/infra@1328085692:ref:refs/heads/main"
            or claims.get("ref") != "refs/heads/main"
            or claims.get("ref_protected") not in ("true", True)
            or claims.get("aud") != identity
            or claims.get("job_workflow_ref") != "fredrir/infra/.github/workflows/cache-identity-negative.yml@refs/heads/main"):
        raise ProbeError("Unexpected negative probe identity claims")
    data = urllib.parse.urlencode({"grant_type": "authorization_code", "code": "", "client_id": client_id, "jwt": jwt}).encode()
    code, response = request_json(urllib.request.Request("https://controlplane.tailscale.com/api/v2/oauth/token-exchange", data=data))
    result = {"identity": identity, "client_id": client_id, "http": code,
              "claims": {key: claims.get(key) for key in ("sub", "aud", "ref", "ref_protected", "job_workflow_ref")},
              "passed": code in (400, 401, 403) and not response.get("access_token")}
    if response.get("error") in ("invalid_grant", "invalid_request", "unauthorized_client", "access_denied", "invalid_token"):
        result["error_code"] = response["error"]
    return result


def main():
    receipt = {"run_id": os.environ.get("GITHUB_RUN_ID"), "revision": os.environ.get("GITHUB_SHA"), "results": []}
    try:
        for identity, variable in [("bazel-cache-check-writer", "CHECK_CLIENT_ID"), ("bazel-cache-cli-writer", "CLI_CLIENT_ID")]:
            receipt["results"].append(denied_exchange(identity, os.environ.get(variable, "")))
        receipt["passed"] = all(result["passed"] for result in receipt["results"])
    except ProbeError as error:
        receipt["passed"] = False
        receipt["failure"] = str(error)
    except (ValueError, IndexError, OSError):
        receipt["passed"] = False
        receipt["failure"] = "Identity probe failed"
    path = pathlib.Path(os.environ["RUNNER_TEMP"]) / "cache-identity-negative.json"
    path.write_text(json.dumps(receipt, indent=2) + "\n")
    print(json.dumps(receipt))
    return 0 if receipt["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
