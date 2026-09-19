import json
import sys
import tempfile
import unittest
from pathlib import Path

import yaml

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "scripts/ci"))

import render_goreleaser  # noqa: E402


def package(name="nsql", release=None, license="0BSD", bins=("nsql",), manifest="/src/Cargo.toml"):
    targets = [{"name": b, "kind": ["bin"]} for b in bins] or [{"name": name, "kind": ["lib"]}]
    return {"id": f"path+file:///src#{name}@0.1.0", "name": name, "description": "Run SQL", "license": license,
            "repository": "https://github.com/fredrir/nsql", "homepage": None, "authors": [],
            "manifest_path": manifest, "targets": targets,
            "metadata": {"release": {"maintainer": "fredrir <fhansteen@gmail.com>"} | (release or {})}}


class RenderGoreleaserTests(unittest.TestCase):
    def render(self, packages, workspace=None, repository="fredrir/nsql"):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            (base / "metadata.json").write_text(json.dumps({
                "packages": packages, "workspace_members": [p["id"] for p in packages], "metadata": workspace}))
            render_goreleaser.main(["--metadata", str(base / "metadata.json"), "--root", "/src", "--repository", repository,
                                    "--dist", "/tmp/dist", "--sdk", "/tmp/sdk", "--config", str(base / "goreleaser.yaml"),
                                    "--summary", str(base / "summary.json")])
            return yaml.safe_load((base / "goreleaser.yaml").read_text()), json.loads((base / "summary.json").read_text())

    def refused(self, packages, **options):
        with self.assertRaises(SystemExit) as failure:
            self.render(packages, **options)
        return str(failure.exception)

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

    def test_release_metadata_reaches_the_builds_packages_and_publisher_summary(self):
        release = {"extra-files": ["THIRD-PARTY-LICENSES.md"], "section": "database", "features": ["vendored-openssl"],
                   "optional": ["neovim: inline editor"], "depends": {"arch": ["dbus"]}}
        config, summary = self.render([package(release=release)])
        for build in config["builds"]:
            self.assertEqual(build["flags"], ["--release", "--locked", "--features=vendored-openssl"])
        self.assertEqual(config["aurs"][0]["depends"], ["dbus"])
        self.assertIn("THIRD-PARTY-LICENSES.md", config["archives"][0]["files"])
        self.assertEqual(summary["section"], "database")
        self.assertEqual(summary["optional"], ["neovim: inline editor"])
        self.assertNotIn("directory", summary)

    def test_unsafe_inputs_are_refused(self):
        for packages, repository in [([package(release={"extra-files": ["../secret"]})], "fredrir/nsql"),
                                     ([package()], "attacker/nsql"),
                                     ([package(release={"binary": "other"})], "fredrir/nsql")]:
            with self.subTest(repository=repository):
                self.refused(packages, repository=repository)


if __name__ == "__main__":
    unittest.main()
