import argparse
import re
import sys
import tomllib
from pathlib import Path

RELEASE = re.compile(r"v(\d+)\.(\d+)\.(\d+)")
VERSION = re.compile(r'^(\s*version\s*=\s*")([^"]+)(".*)$')


def version_table(manifest):
    data = tomllib.loads(manifest)
    if isinstance(data.get("workspace", {}).get("package", {}).get("version"), str):
        return "workspace.package", data["workspace"]["package"]["version"]
    if isinstance(data.get("package", {}).get("version"), str):
        return "package", data["package"]["version"]
    raise SystemExit("Cargo.toml has no literal release version")


def next_version(current, tags):
    parsed = re.fullmatch(r"(\d+)\.(\d+)\.(\d+)", current)
    if not parsed:
        raise SystemExit(f"Cargo.toml version {current} is not a plain release version")
    released = sorted(tuple(map(int, m.groups())) for m in map(RELEASE.fullmatch, tags) if m)
    manual = tuple(map(int, parsed.groups()))
    if not released or (manual > released[-1] and manual not in released):
        return current
    major, minor, patch = released[-1]
    return f"{major}.{minor}.{patch + 1}"


def rewrite(manifest, table, version):
    lines = manifest.splitlines(keepends=True)
    current = None
    for index, line in enumerate(lines):
        header = re.fullmatch(r"\s*\[([^\[\]]+)\]\s*(#.*)?", line.rstrip("\n"))
        if header:
            current = header.group(1).strip()
            continue
        match = VERSION.match(line.rstrip("\n"))
        if current == table and match:
            lines[index] = f"{match.group(1)}{version}{match.group(3)}" + ("\n" if line.endswith("\n") else "")
            return "".join(lines)
    raise SystemExit(f"No version line in [{table}]")


def main(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("manifest", type=Path)
    parser.add_argument("--tags", type=Path, required=True)
    args = parser.parse_args(argv)
    manifest = args.manifest.read_text()
    table, current = version_table(manifest)
    version = next_version(current, args.tags.read_text().split())
    if version != current:
        args.manifest.write_text(rewrite(manifest, table, version))
    print(version)
    return 0


if __name__ == "__main__":
    sys.exit(main())
