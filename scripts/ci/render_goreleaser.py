import argparse
import json
import re
import sys
from pathlib import Path

import yaml

TARGETS = {
    "gnu": ["x86_64-unknown-linux-gnu.2.28", "aarch64-unknown-linux-gnu.2.28"],
    "musl": ["x86_64-unknown-linux-musl", "aarch64-unknown-linux-musl"],
    "darwin": ["x86_64-apple-darwin", "aarch64-apple-darwin"],
}
TRIPLES = {"gnu": "unknown-linux-gnu", "musl": "unknown-linux-musl", "darwin": "apple-darwin"}
ARCH = '{{ if eq .Arch "amd64" }}x86_64{{ else }}aarch64{{ end }}'
NIX_LICENSES = {
    "0BSD": "bsd0", "Apache-2.0": "asl20", "BSD-2-Clause": "bsd2", "BSD-3-Clause": "bsd3",
    "GPL-3.0-only": "gpl3Only", "GPL-3.0-or-later": "gpl3Plus", "ISC": "isc", "MIT": "mit",
    "MPL-2.0": "mpl20", "Unlicense": "unlicense", "Zlib": "zlib",
}
QUARANTINE = """postflight_steps do
  on_macos do
    run "/usr/bin/xattr", args: ["-dr", "com.apple.quarantine", "{{ \"{{staged_path}}\" }}/%s"]
  end
end"""


def fail(message):
    raise SystemExit(message)


def release_package(metadata):
    members = [p for p in metadata["packages"] if p["id"] in metadata["workspace_members"]]
    wanted = ((metadata.get("metadata") or {}).get("release") or {}).get("package")
    candidates = [p for p in members if p["name"] == wanted] if wanted else \
        [p for p in members if any("bin" in t["kind"] for t in p["targets"])]
    if len(candidates) != 1:
        fail("Set [workspace.metadata.release] package to the crate that ships the binary")
    return candidates[0]


def settings(package, repository):
    release = (package.get("metadata") or {}).get("release") or {}
    binaries = [t["name"] for t in package["targets"] if "bin" in t["kind"]]
    binary = release.get("binary", binaries[0] if binaries else None)
    if binary not in binaries:
        fail(f"{package['name']} has no binary named {binary}")
    license_expression = package.get("license") or fail("Cargo.toml needs an SPDX license")
    first_license = re.split(r"\s+(?:OR|AND)\s+", license_expression.strip("()"))[0]
    if first_license not in NIX_LICENSES:
        fail(f"No Nix license mapping for {first_license}")
    maintainer = release.get("maintainer") or next(iter(package.get("authors") or []), None) or fail(
        "Set [package.metadata.release] maintainer")
    for name in release.get("extra-files", []):
        if Path(name).is_absolute() or ".." in Path(name).parts:
            fail(f"Extra file {name} must stay inside the crate")
    return {
        "name": package["name"],
        "binary": binary,
        "description": package.get("description") or fail("Cargo.toml needs a description"),
        "license": license_expression,
        "nix_license": NIX_LICENSES[first_license],
        "homepage": package.get("homepage") or package.get("repository") or f"https://github.com/{repository}",
        "repository": repository,
        "maintainer": maintainer,
        "section": release.get("section", "utils"),
        "features": release.get("features", []),
        "extra_files": release.get("extra-files", []),
        "depends": release.get("depends", {}),
        "optional": release.get("optional", []),
        "directory": str(Path(package["manifest_path"]).parent),
    }


def aur_maintainer(maintainer):
    return maintainer.replace("@", " at ").replace(".", " dot ")


def goreleaser(config, root, dist, sdk):
    owner, repository = config["repository"].split("/")
    features = [f"--features={','.join(config['features'])}"] if config["features"] else []
    relative = Path(config["directory"]).relative_to(root).as_posix()
    files = ["LICENSE*", "README*", *config["extra_files"]]
    builds = []
    for flavour, targets in TARGETS.items():
        environment = ["LIBZ_SYS_STATIC=1"]
        if flavour == "darwin":
            environment += [f"SDKROOT={sdk}", "MACOSX_DEPLOYMENT_TARGET=13.0", "CARGO_PROFILE_RELEASE_STRIP=false"]
        builds.append({
            "id": flavour, "builder": "rust", "binary": config["binary"], "dir": relative, "targets": targets,
            "flags": ["--release", "--locked", *features], "env": environment,
        })
    archives = [{
        "id": flavour, "ids": [flavour], "formats": ["tar.gz"], "files": files,
        "name_template": f"{{{{ .ProjectName }}}}-{ARCH}-{TRIPLES[flavour]}-v{{{{ .Version }}}}",
    } for flavour in TARGETS]
    install = "\n".join([
        f'install -Dm755 "./{config["binary"]}" "${{pkgdir}}/usr/bin/{config["binary"]}"',
        'install -Dm644 ./LICENSE* -t "${pkgdir}/usr/share/licenses/${pkgname}/"',
        *[f'install -Dm644 "./{name}" -t "${{pkgdir}}/usr/share/licenses/${{pkgname}}/"'
          for name in config["extra_files"]],
    ])
    common = {
        "homepage": config["homepage"], "description": config["description"], "license": config["license"],
        "maintainers": [aur_maintainer(config["maintainer"])], "skip_upload": True,
        "optdepends": config["optional"],
    }
    return {
        "version": 2,
        "project_name": config["name"],
        "dist": str(dist),
        "builds": builds,
        "archives": archives,
        "source": {"enabled": True, "name_template": "{{ .ProjectName }}-{{ .Version }}-source",
                   "prefix_template": "{{ .ProjectName }}-{{ .Version }}/"},
        "checksum": {"name_template": "checksums.txt", "algorithm": "sha256"},
        "changelog": {"disable": True},
        "release": {"github": {"owner": owner, "name": repository}},
        "homebrew_casks": [{
            "name": config["name"], "ids": ["musl", "darwin"], "binaries": [config["binary"]],
            "directory": "Casks", "homepage": config["homepage"], "description": config["description"],
            "license": config["license"], "skip_upload": True,
            "repository": {"owner": owner, "name": "homebrew-tap"},
            "custom_block": QUARANTINE % config["binary"],
        }],
        "aurs": [common | {
            "name": f"{config['name']}-bin", "ids": ["gnu"], "provides": [config["name"]],
            "conflicts": [config["name"]], "depends": config["depends"].get("arch", []),
            "git_url": f"ssh://aur@aur.archlinux.org/{config['name']}-bin.git", "package": install,
        }],
        "aur_sources": [common | {
            "name": config["name"], "arches": ["x86_64", "aarch64"], "makedepends": ["cargo"],
            "depends": ["gcc-libs", *config["depends"].get("arch", [])], "conflicts": [f"{config['name']}-bin"],
            "git_url": f"ssh://aur@aur.archlinux.org/{config['name']}.git",
            "prepare": "\n".join([
                'cd "${srcdir}/${pkgname}-${pkgver}"', "export RUSTUP_TOOLCHAIN=stable",
                'cargo fetch --locked --target "$(rustc -vV | sed -n \'s/host: //p\')"',
            ]),
            "build": "\n".join([
                'cd "${srcdir}/${pkgname}-${pkgver}"', "export RUSTUP_TOOLCHAIN=stable CARGO_TARGET_DIR=target",
                'export CFLAGS="${CFLAGS//-flto=auto/}" CXXFLAGS="${CXXFLAGS//-flto=auto/}" LDFLAGS="${LDFLAGS//-flto=auto/}"',
                f"cargo build --frozen --release --package {config['name']}",
            ]),
            "package": "\n".join(['cd "${srcdir}/${pkgname}-${pkgver}"', install.replace(
                f'"./{config["binary"]}"', f'"target/release/{config["binary"]}"')]),
        }],
        "nix": [{
            "name": config["name"], "ids": ["musl", "darwin"], "path": f"pkgs/{config['name']}/default.nix",
            "homepage": config["homepage"], "description": config["description"],
            "license": config["nix_license"], "main_program": config["binary"], "skip_upload": True,
            "repository": {"owner": owner, "name": "nur-packages"},
        }],
        "nfpms": [],
        "report_sizes": True,
    }


def main(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("--metadata", required=True, type=Path)
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--dist", required=True, type=Path)
    parser.add_argument("--sdk", required=True, type=Path)
    parser.add_argument("--config", required=True, type=Path)
    parser.add_argument("--summary", required=True, type=Path)
    args = parser.parse_args(argv)
    if not re.fullmatch(r"fredrir/[A-Za-z0-9_.-]+", args.repository):
        fail("Only fredrir repositories can be released")
    metadata = json.loads(args.metadata.read_text())
    config = settings(release_package(metadata), args.repository)
    args.config.write_text(yaml.safe_dump(goreleaser(config, args.root.resolve(), args.dist, args.sdk), sort_keys=False))
    args.summary.write_text(json.dumps({k: v for k, v in config.items() if k != "directory"}, indent=2) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
