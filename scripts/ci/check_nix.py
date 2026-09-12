#!/usr/bin/env python3
import json
import platform
import re
import subprocess

from policy import PolicyError


def native_system():
    architecture = {"x86_64": "x86_64", "aarch64": "aarch64"}.get(platform.machine())
    if platform.system() != "Linux" or not architecture:
        raise PolicyError("native Linux Nix validation is required")
    return f"{architecture}-linux"


def host_targets(hosts, system):
    if any(not re.fullmatch(r"[A-Za-z0-9_-]+", name) for name in hosts):
        raise PolicyError("unsafe NixOS configuration name")
    return [
        f".#nixosConfigurations.{name}.config.system.build.toplevel"
        for name, target in sorted(hosts.items())
        if target == system
    ]


def main():
    system = native_system()
    subprocess.run(["nix", "flake", "check", "--no-update-lock-file"], check=True)
    result = subprocess.run(
        [
            "nix",
            "eval",
            "--json",
            ".#nixosConfigurations",
            "--no-update-lock-file",
            "--apply",
            "hosts: builtins.mapAttrs (_: host: host.config.nixpkgs.hostPlatform.system) hosts",
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    targets = host_targets(json.loads(result.stdout), system)
    if targets:
        subprocess.run(
            ["nix", "build", "--no-link", "--no-update-lock-file", *targets], check=True
        )


if __name__ == "__main__":
    main()
