#!/usr/bin/env python3
import os
import urllib.error

from pipeline import api
from policy import INFRA_ID, OWNER_ID, SHA, PolicyError


if (
    os.environ.get("GITHUB_REPOSITORY_ID") != str(INFRA_ID)
    or os.environ.get("GITHUB_REPOSITORY_OWNER_ID") != str(OWNER_ID)
    or os.environ.get("GITHUB_EVENT_NAME") != "push"
    or os.environ.get("GITHUB_REF") != "refs/heads/main"
    or os.environ.get("GITHUB_REF_PROTECTED") != "true"
):
    raise PolicyError("infrastructure promotion is not authorized")

repository = os.environ["GITHUB_REPOSITORY"]
revision = os.environ["GITHUB_SHA"]
if not SHA.fullmatch(revision):
    raise PolicyError("invalid infrastructure revision")
try:
    current = api("GET", f"repos/{repository}/git/ref/heads/deploy")
except urllib.error.HTTPError as error:
    if error.code != 404:
        raise
    api("POST", f"repos/{repository}/git/refs", {"ref": "refs/heads/deploy", "sha": revision})
else:
    if current["object"]["sha"] != revision:
        api("PATCH", f"repos/{repository}/git/refs/heads/deploy", {"sha": revision, "force": False})
