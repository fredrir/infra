import argparse
import datetime
import http.client
import json
import os
import pathlib
import re
import sys
import urllib.error
import urllib.parse
import urllib.request


WRITER_TAG = "tag:ci-bazel-writer"
MINIMUM_AGE = datetime.timedelta(hours=1)
MAXIMUM_DELETIONS = 100
API_URL = "https://api.tailscale.com/api/v2"


class CleanupError(Exception):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, newurl):
        fp.close()
        raise CleanupError("API redirect refused")


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise CleanupError("Duplicate API field")
        result[key] = value
    return result


class API:
    def __init__(self, base=API_URL):
        self.base = base
        self.opener = urllib.request.build_opener(NoRedirect())
        self.token = None

    def request(self, method, path, form=None):
        headers = {"Accept": "application/json"}
        if self.token:
            headers["Authorization"] = "Bearer " + self.token
        data = None
        if form is not None:
            data = urllib.parse.urlencode(form).encode()
            headers["Content-Type"] = "application/x-www-form-urlencoded"
        request = urllib.request.Request(self.base + path, data=data, headers=headers, method=method)
        try:
            with self.opener.open(request, timeout=15) as response:
                if response.status not in (200, 204):
                    raise CleanupError("Unexpected API status")
                if response.headers.get("Link"):
                    raise CleanupError("Paginated API response refused")
                body = response.read((8 << 20) + 1)
                if len(body) > 8 << 20:
                    raise CleanupError("API response too large")
        except urllib.error.HTTPError as error:
            error.close()
            if error.code == 404 and re.fullmatch(r"/device/[A-Za-z0-9_-]+(?:\?fields=all)?", path):
                return None
            raise CleanupError(f"API {method} failed with HTTP {error.code}") from None
        except (urllib.error.URLError, TimeoutError, OSError, http.client.HTTPException):
            raise CleanupError(f"API {method} unavailable") from None
        if method == "DELETE" and body.strip() in (b"", b"null", b"{}"):
            return None
        try:
            value = json.loads(body, object_pairs_hook=unique_object)
        except (ValueError, UnicodeError):
            raise CleanupError("Invalid API document") from None
        if not isinstance(value, dict):
            raise CleanupError("Invalid API object")
        return value

    def authenticate(self, client_id, client_secret):
        self.token = None
        if not client_id or not client_secret.startswith("tskey-client-"):
            raise CleanupError("Dedicated cleanup OAuth credentials required")
        token = self.request("POST", "/oauth/token", {
            "grant_type": "client_credentials",
            "client_id": client_id,
            "client_secret": client_secret,
            "scope": "devices:core",
            "tags": WRITER_TAG,
        })
        if (not isinstance(token.get("access_token"), str) or not token["access_token"]
                or not isinstance(token.get("token_type"), str)
                or token["token_type"].lower() != "bearer"
                or token.get("scope") != "devices:core"
                or type(token.get("expires_in")) is not int
                or not 60 <= token["expires_in"] <= 3600):
            raise CleanupError("Cleanup OAuth scope or lifetime differs")
        self.token = token["access_token"]


def eligible(device, now):
    if not isinstance(device, dict):
        raise CleanupError("Invalid device entry")
    if device.get("tags") != [WRITER_TAG] or device.get("isExternal") is not False:
        return False
    node_id, created = device.get("nodeId"), device.get("created")
    if not isinstance(node_id, str) or not re.fullmatch(r"[A-Za-z0-9_-]{4,128}", node_id):
        raise CleanupError("Writer node ID missing or invalid")
    if not isinstance(created, str) or not re.fullmatch(
            r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})", created):
        raise CleanupError("Writer creation time missing or invalid")
    try:
        instant = datetime.datetime.fromisoformat(created.replace("Z", "+00:00"))
    except ValueError:
        raise CleanupError("Writer creation time invalid") from None
    return now - instant >= MINIMUM_AGE


def sweep(api, report, apply=False, now=None, progress=None):
    now = now or datetime.datetime.now(datetime.timezone.utc)
    if now.tzinfo is None or now.utcoffset() is None:
        raise CleanupError("Cleanup time must include timezone")
    report.update({"at": now.isoformat(), "apply": apply, "minimum_age_seconds": 3600, "devices": []})
    inventory = api.request("GET", "/tailnet/-/devices?fields=all")
    if set(inventory) != {"devices"} or not isinstance(inventory["devices"], list):
        raise CleanupError("Incomplete device inventory")
    candidates, seen = [], set()
    for device in inventory["devices"]:
        if isinstance(device, dict) and isinstance(device.get("nodeId"), str):
            if device["nodeId"] in seen:
                raise CleanupError("Duplicate node ID in inventory")
            seen.add(device["nodeId"])
        if eligible(device, now):
            candidates.append(device)
    candidates.sort(key=lambda item: (datetime.datetime.fromisoformat(item["created"].replace("Z", "+00:00")), item["nodeId"]))
    report["examined"] = len(inventory["devices"])
    report["eligible"] = len(candidates)
    report["deferred"] = max(0, len(candidates) - MAXIMUM_DELETIONS)
    if progress:
        progress(report)
    for device in candidates[:MAXIMUM_DELETIONS]:
        node_id = device["nodeId"]
        receipt = {"node_id": node_id, "created": device["created"], "result": "candidate"}
        report["devices"].append(receipt)
        if progress:
            progress(report)
        current = api.request("GET", "/device/" + node_id + "?fields=all")
        if current is None:
            receipt["result"] = "already_absent"
        elif (not eligible(current, now) or current["nodeId"] != node_id
                or current["created"] != device["created"]):
            receipt["result"] = "changed"
        elif apply:
            api.request("DELETE", "/device/" + node_id)
            receipt["result"] = "deleted"
        else:
            receipt["result"] = "would_delete"
        if progress:
            progress(report)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--apply", action="store_true")
    parser.add_argument("--report", type=pathlib.Path, required=True)
    args = parser.parse_args()
    report = {"schema": 1, "writer_tag": WRITER_TAG}
    result = 0
    def save(receipt):
        temporary = args.report.with_name(args.report.name + ".tmp")
        temporary.write_text(json.dumps(receipt, indent=2) + "\n")
        temporary.replace(args.report)
    try:
        api = API()
        api.authenticate(os.environ.get("TAILSCALE_CACHE_CLEANUP_CLIENT_ID", ""),
                         os.environ.get("TAILSCALE_CACHE_CLEANUP_CLIENT_SECRET", ""))
        sweep(api, report, args.apply, progress=save)
    except CleanupError as error:
        report["error"] = str(error)
        result = 1
    save(report)
    print(json.dumps(report))
    return result


if __name__ == "__main__":
    sys.exit(main())
