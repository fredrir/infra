#!/usr/bin/env python3
import os
from pathlib import Path
import tempfile

from pipeline import ROOT, registry_auth, verify_release_file


with tempfile.TemporaryDirectory(prefix="infra-release-check-") as directory:
    registry_auth(directory)
    os.environ["DOCKER_CONFIG"] = directory
    projects = ROOT / "platform/projects"
    for release in sorted([*projects.glob("*/release.json"), *projects.glob("*/releases/*.json")]):
        verify_release_file(release)
        print(f"Verified {release.relative_to(ROOT)}")
