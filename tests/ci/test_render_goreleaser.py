import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts/ci/render_goreleaser.py"


def package(name="nsql", release=None, license="0BSD", bins=("nsql",), manifest="/src/Cargo.toml"):
    targets = [{"name": b, "kind": ["bin"]} for b in bins] or [{"name": name, "kind": ["lib"]}]
    return {"id": f"path+file:///src#{name}@0.1.0", "name": name, "description": "Run SQL", "license": license,
            "repository": "https://github.com/fredrir/nsql", "homepage": None, "authors": [],
            "manifest_path": manifest, "targets": targets,
            "metadata": {"release": {"maintainer": "fredrir <fhansteen@gmail.com>"} | (release or {})}}


class RenderGoreleaserTests(unittest.TestCase):
    def run_renderer(self, packages, workspace=None, repository="fredrir/nsql"):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            metadata = base / "metadata.json"
            metadata.write_text(json.dumps({"packages": packages, "workspace_members": [p["id"] for p in packages],
                                            "metadata": workspace}))
            result = subprocess.run([sys.executable, str(SCRIPT), "--metadata", str(metadata), "--root", "/src",
                                     "--repository", repository, "--dist", "/tmp/dist", "--sdk", "/tmp/sdk",
                                     "--config", str(base / "goreleaser.yaml"), "--summary", str(base / "summary.json")],
                                    capture_output=True, text=True, check=False)
            if result.returncode:
                return result, {}, {}
            return result, yaml.safe_load((base / "goreleaser.yaml").read_text()), json.loads((base / "summary.json").read_text())

    def render(self, packages, workspace=None, repository="fredrir/nsql"):
        result, config, summary = self.run_renderer(packages, workspace, repository)
        self.assertEqual(result.returncode, 0, result.stderr)
        return config, summary

    def refused(self, packages, workspace=None, repository="fredrir/nsql"):
        result, _, _ = self.run_renderer(packages, workspace, repository)
        self.assertNotEqual(result.returncode, 0)
        return result.stderr

    def test_every_linux_and_macos_target_is_built_by_flavour(self):
        config, _ = self.render([package(release={"features": ["vendored-openssl"]})])
        builds = {b["id"]: b for b in config["builds"]}
        self.assertEqual(builds["gnu"]["targets"], ["x86_64-unknown-linux-gnu.2.28", "aarch64-unknown-linux-gnu.2.28"])
        self.assertEqual(builds["musl"]["targets"], ["x86_64-unknown-linux-musl", "aarch64-unknown-linux-musl"])
        self.assertEqual(builds["darwin"]["targets"], ["x86_64-apple-darwin", "aarch64-apple-darwin"])
        for build in builds.values():
            self.assertEqual(build["builder"], "rust")
            self.assertEqual(build["flags"], ["--release", "--locked", "--features=vendored-openssl"])
            self.assertIn("LIBZ_SYS_STATIC=1", build["env"])
            self.assertFalse(any(e.startswith("CARGO_TARGET_DIR") for e in build["env"]))
        self.assertIn("SDKROOT=/tmp/sdk", builds["darwin"]["env"])
        self.assertIn("CARGO_PROFILE_RELEASE_STRIP=false", builds["darwin"]["env"])
        self.assertNotIn("SDKROOT=/tmp/sdk", builds["gnu"]["env"])

    def test_archives_and_publishers_never_mix_flavours(self):
        config, _ = self.render([package()])
        self.assertEqual({a["id"]: a["ids"] for a in config["archives"]}, {"gnu": ["gnu"], "musl": ["musl"], "darwin": ["darwin"]})
        self.assertIn("unknown-linux-musl-v{{ .Version }}", next(a for a in config["archives"] if a["id"] == "musl")["name_template"])
        self.assertEqual(config["homebrew_casks"][0]["ids"], ["musl", "darwin"])
        self.assertEqual(config["nix"][0]["ids"], ["musl", "darwin"])
        self.assertEqual(config["aurs"][0]["ids"], ["gnu"])
        for publisher in ["homebrew_casks", "aurs", "aur_sources", "nix"]:
            self.assertTrue(all(entry["skip_upload"] is True for entry in config[publisher]), publisher)
        self.assertEqual(config["nfpms"], [])
        self.assertTrue(config["changelog"]["disable"])
        self.assertTrue(config["source"]["enabled"])

    def test_source_package_builds_without_gcc_lto_and_from_the_locked_tree(self):
        config, _ = self.render([package()])
        source = config["aur_sources"][0]
        self.assertEqual(source["name"], "nsql")
        self.assertIn('${CFLAGS//-flto=auto/}', source["build"])
        self.assertIn("cargo build --frozen --release --package nsql", source["build"])
        self.assertIn('"target/release/nsql"', source["package"])
        self.assertEqual(config["aurs"][0]["name"], "nsql-bin")
        self.assertEqual(config["aurs"][0]["maintainers"], ["fredrir <fhansteen at gmail dot com>"])

    def test_cask_removes_quarantine_only_on_macos(self):
        config, _ = self.render([package()])
        block = config["homebrew_casks"][0]["custom_block"]
        self.assertIn("postflight_steps do", block)
        self.assertIn("on_macos do", block)
        self.assertIn('"#{staged_path}/nsql"', block)
        self.assertEqual(config["homebrew_casks"][0]["repository"], {"owner": "fredrir", "name": "homebrew-tap"})

    def test_nix_license_uses_the_first_spdx_alternative(self):
        config, summary = self.render([package(license="MIT OR Apache-2.0")])
        self.assertEqual(config["nix"][0]["license"], "mit")
        self.assertEqual(summary["license"], "MIT OR Apache-2.0")
        self.refused([package(license="LicenseRef-proprietary")])

    def test_workspace_must_name_the_released_crate_when_ambiguous(self):
        cli = package(name="tool-cli", bins=("tool",), manifest="/src/crates/tool-cli/Cargo.toml")
        other = package(name="tool-bench", bins=("bench",), manifest="/src/crates/tool-bench/Cargo.toml")
        self.assertIn("package", self.refused([cli, other]))
        config, summary = self.render([cli, other], workspace={"release": {"package": "tool-cli"}})
        self.assertEqual(config["builds"][0]["dir"], "crates/tool-cli")
        self.assertEqual(config["builds"][0]["binary"], "tool")
        self.assertEqual(summary["binary"], "tool")

    def test_summary_carries_packaging_metadata_for_the_publisher(self):
        release = {"extra-files": ["THIRD-PARTY-LICENSES.md"], "section": "database",
                   "optional": ["neovim: inline editor"], "depends": {"arch": ["dbus"]}}
        config, summary = self.render([package(release=release)])
        self.assertEqual(summary["section"], "database")
        self.assertEqual(summary["optional"], ["neovim: inline editor"])
        self.assertEqual(config["aurs"][0]["depends"], ["dbus"])
        self.assertIn("THIRD-PARTY-LICENSES.md", config["archives"][0]["files"])
        self.assertNotIn("directory", summary)

    def test_unsafe_inputs_are_refused(self):
        for packages, repository in [([package(release={"extra-files": ["../secret"]})], "fredrir/nsql"),
                                     ([package()], "attacker/nsql"),
                                     ([package(release={"binary": "other"})], "fredrir/nsql")]:
            with self.subTest(repository=repository):
                self.refused(packages, repository=repository)


if __name__ == "__main__":
    unittest.main()
