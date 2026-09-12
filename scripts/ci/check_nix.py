#!/usr/bin/env python3
import subprocess


def main():
    subprocess.run(["nix", "flake", "check", "--no-update-lock-file"], check=True)
    subprocess.run(
        ["nix", "build", ".#default", "--no-link", "--no-update-lock-file"], check=True
    )


if __name__ == "__main__":
    main()
