import argparse
import functools
import hashlib
import json
import re
import shutil
import subprocess
import sys
import tarfile
from pathlib import Path

KEEP = 3
SIGNER = "fredrir/infra/.github/workflows/rust-release.yml"
STABLE = re.compile(r"v(\d+)\.(\d+)\.(\d+)")
ARCHES = {"x86_64": "amd64", "aarch64": "arm64"}
RPM_ARCHES = {"amd64": "x86_64", "arm64": "aarch64"}
APK_ARCHES = {"amd64": "x86_64", "arm64": "aarch64"}
CHANNEL_FILES = ["{name}.rb", "{name}.nix", "{name}-bin.pkgbuild", "{name}-bin.srcinfo", "{name}.pkgbuild", "{name}.srcinfo"]


def gh(*args):
    return subprocess.run(["gh", *args], check=True, capture_output=True, text=True).stdout


def stable_releases(releases, keep=KEEP):
    stable = [(tuple(map(int, m.groups())), r["tag_name"]) for r in releases
              if not r["draft"] and not r["prerelease"] and (m := STABLE.fullmatch(r["tag_name"]))
              and any(asset["name"] == "release.json" for asset in r.get("assets", []))]
    return [tag for _, tag in sorted(stable, reverse=True)[:keep]]


def checksums(path):
    entries = {}
    for line in path.read_text().splitlines():
        digest, name = line.split(maxsplit=1)
        entries[name.lstrip("*")] = digest
    return entries


def verify_checksum(path, expected):
    if hashlib.sha256(path.read_bytes()).hexdigest() != expected:
        raise SystemExit(f"Checksum mismatch for {path.name}")


def signer_digests(attestations):
    return {result["verificationResult"]["signature"]["certificate"]["buildSignerDigest"] for result in attestations}


@functools.cache
def signer_on_main(digest):
    if not re.fullmatch(r"[0-9a-f]{40}", digest):
        raise SystemExit(f"Unexpected signer digest {digest}")
    return json.loads(gh("api", f"repos/fredrir/infra/compare/{digest}...main"))["status"] in ("ahead", "identical")


def verify_attestation(path, repository, tag):
    output = gh("attestation", "verify", str(path), "--repo", repository, "--signer-workflow", SIGNER,
                "--source-ref", f"refs/tags/{tag}", "--format", "json")
    digests = signer_digests(json.loads(output))
    if not digests or not all(signer_on_main(digest) for digest in digests):
        raise SystemExit(f"{path.name} was not built by an infra revision on main")
    return digests


def nfpm_config(release, version, arch, payload):
    name = release["name"]
    recommends = [entry.split(":")[0].strip() for entry in release.get("optional", [])]
    depends = release.get("depends", {})
    contents = [{"src": str(payload / release["binary"]), "dst": f"/usr/bin/{release['binary']}",
                 "file_info": {"mode": 0o755}}]
    for license_file in sorted(payload.glob("LICENSE*")) + [payload / extra for extra in release.get("extra_files", [])]:
        contents.append({"src": str(license_file), "dst": f"/usr/share/licenses/{name}/{license_file.name}",
                         "file_info": {"mode": 0o644}})
    return {
        "name": name, "arch": arch, "platform": "linux", "version": version, "release": "1",
        "section": release.get("section", "utils"), "priority": "optional",
        "maintainer": release["maintainer"], "description": release["description"],
        "vendor": "fredrir", "homepage": release["homepage"], "license": release["license"],
        "contents": contents,
        "overrides": {
            "deb": {"recommends": recommends, "depends": depends.get("deb", [])},
            "rpm": {"recommends": recommends, "depends": depends.get("rpm", [])},
            "apk": {"depends": depends.get("apk", [])},
        },
        "rpm": {"signature": {"key_file": "${RPM_SIGNING_KEY}"}},
        "apk": {"signature": {"key_file": "${APK_SIGNING_KEY}", "key_name": "fredrir"}},
    }


def extract(archive, destination):
    destination.mkdir(parents=True)
    with tarfile.open(archive) as bundle:
        bundle.extractall(destination, filter="data")
    return destination


def package(release, version, tag_dir, work, site):
    name = release["name"]
    for triple_arch, arch in ARCHES.items():
        for flavour, packagers in [("gnu", ["deb", "rpm"]), ("musl", ["apk"])]:
            archive = tag_dir / f"{name}-{triple_arch}-unknown-linux-{flavour}-v{version}.tar.gz"
            payload = extract(archive, work / f"{name}-{version}-{arch}-{flavour}")
            config = work / f"{name}-{version}-{arch}-{flavour}.json"
            config.write_text(json.dumps(nfpm_config(release, version, arch, payload), indent=2))
            for packager in packagers:
                target = {"deb": site / "deb/pool/main" / name,
                          "rpm": site / "rpm" / RPM_ARCHES[arch],
                          "apk": site / "apk" / APK_ARCHES[arch] / f"{name}-{version}-r1.apk"}[packager]
                (target.parent if packager == "apk" else target).mkdir(parents=True, exist_ok=True)
                subprocess.run(["nfpm", "package", "--config", str(config), "--packager", packager, "--target", str(target)],
                               check=True, capture_output=True, text=True)


def collect(registry, work, site, channels):
    tools = {}
    for project in registry["projects"]:
        if project["visibility"] != "public":
            continue
        repository = project["repository"]
        tags = stable_releases(json.loads(gh("api", f"repos/{repository}/releases?per_page=100")))
        for position, tag in enumerate(tags):
            tag_dir = work / "releases" / project["project"] / tag
            tag_dir.mkdir(parents=True)
            gh("release", "download", tag, "--repo", repository, "--dir", str(tag_dir),
               "--pattern", "*-unknown-linux-*.tar.gz", "--pattern", "checksums.txt", "--pattern", "release.json",
               "--pattern", "*.rb", "--pattern", "*.nix", "--pattern", "*.pkgbuild", "--pattern", "*.srcinfo")
            sums = checksums(tag_dir / "checksums.txt")
            for path in sorted(tag_dir.iterdir()):
                if path.name == "checksums.txt":
                    continue
                if path.name not in sums:
                    raise SystemExit(f"{repository} {tag}: {path.name} is not listed in checksums.txt")
                verify_checksum(path, sums[path.name])
                verify_attestation(path, repository, tag)
            release = json.loads((tag_dir / "release.json").read_text())
            if release["repository"] != repository or release["name"] != project["project"]:
                raise SystemExit(f"{repository} {tag}: release metadata names another project")
            version = tag[1:]
            package(release, version, tag_dir, work / "payloads", site)
            if position == 0:
                target = channels / release["name"]
                target.mkdir(parents=True)
                for pattern in CHANNEL_FILES:
                    shutil.copy2(tag_dir / pattern.format(name=release["name"]), target)
                (target / "release.json").write_text(json.dumps(release | {"version": version, "tag": tag}, indent=2))
                tools[release["name"]] = {"repository": repository, "version": version, "binary": release["binary"],
                                          "taps": project.get("taps", [])}
    (channels / "tools.json").write_text(json.dumps(tools, indent=2, sort_keys=True) + "\n")
    return tools


def main(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("--registry", type=Path, required=True)
    parser.add_argument("--work", type=Path, required=True)
    parser.add_argument("--site", type=Path, required=True)
    parser.add_argument("--channels", type=Path, required=True)
    args = parser.parse_args(argv)
    for directory in [args.work, args.site, args.channels]:
        directory.mkdir(parents=True, exist_ok=False)
    tools = collect(json.loads(args.registry.read_text()), args.work, args.site, args.channels)
    print(f"Collected {', '.join(f'{k} {v['version']}' for k, v in sorted(tools.items())) or 'nothing'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
