#!/usr/bin/env python3
import argparse
import re
import subprocess

parser = argparse.ArgumentParser()
parser.add_argument("base")
parser.add_argument("head", nargs="?", default="HEAD")
args = parser.parse_args()
for revision in (args.base, args.head):
    if not re.fullmatch(r"[A-Za-z0-9_./-]+", revision) or revision.startswith("-"):
        parser.error("invalid revision")
result = subprocess.run(
    ["git", "diff", "--name-only", f"{args.base}...{args.head}", "--"],
    check=True,
    capture_output=True,
    text=True,
)
for path in result.stdout.splitlines():
    if re.match(
        r"^(\.github/|hosts/|modules/(tailscale|profiles|secrets|gitops-pull)/|platform/|tofu/|secrets/|tailscale/|flake\.(nix|lock)$)",
        path,
    ):
        print(path)
