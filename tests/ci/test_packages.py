import hashlib
import json
import os
import subprocess
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/packages"))

import publish_channels as channels  # noqa: E402
import collect  # noqa: E402
import pages as package_site  # noqa: E402

RELEASE = {"name": "nsql", "binary": "nsql", "description": "Run SQL", "license": "0BSD",
           "homepage": "https://github.com/fredrir/nsql", "repository": "fredrir/nsql",
           "maintainer": "fredrir <fhansteen@gmail.com>", "section": "database",
           "extra_files": ["THIRD-PARTY-LICENSES.md"], "optional": ["neovim: editor"], "depends": {"arch": ["dbus"]}}
DIGEST = "a" * 40


def release(tag, prerelease=False, draft=False, assets=("release.json",)):
    return {"tag_name": tag, "prerelease": prerelease, "draft": draft, "assets": [{"name": a} for a in assets]}


class ReleaseSelectionTests(unittest.TestCase):
    def test_only_the_newest_attested_stable_releases_are_kept(self):
        releases = [release("v0.1.9"), release("v0.1.10"), release("v0.2.0-rc.1", prerelease=True),
                    release("v0.1.11", draft=True), release("v0.1.8"), release("v0.1.13", assets=("nsql.tar.xz",)),
                    release("nightly")]
        self.assertEqual(collect.stable_releases(releases), ["v0.1.10", "v0.1.9", "v0.1.8"])

    def test_checksum_files_accept_binary_markers(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "checksums.txt"
            path.write_text(f"{'1' * 64}  a.tar.gz\n{'2' * 64} *b.rb\n")
            self.assertEqual(collect.checksums(path), {"a.tar.gz": "1" * 64, "b.rb": "2" * 64})

    def test_attestations_must_come_from_infra_main(self):
        attestation = json.dumps([{"verificationResult": {"signature": {"certificate": {"buildSignerDigest": DIGEST}}}}])
        collect.signer_on_main.cache_clear()
        for status, accepted in [("ahead", True), ("identical", True), ("behind", False), ("diverged", False)]:
            with self.subTest(status=status):
                collect.signer_on_main.cache_clear()
                responses = {"attestation": attestation, "api": json.dumps({"status": status})}
                with patch.object(collect, "gh", side_effect=lambda *a: responses[a[0]]) as gh:
                    if accepted:
                        self.assertEqual(collect.verify_attestation(Path("x.tar.gz"), "fredrir/nsql", "v1.0.0"), {DIGEST})
                    else:
                        with self.assertRaises(SystemExit):
                            collect.verify_attestation(Path("x.tar.gz"), "fredrir/nsql", "v1.0.0")
                    command = gh.call_args_list[0].args
                    self.assertEqual(command[command.index("--signer-workflow") + 1], collect.SIGNER)
                    self.assertEqual(command[command.index("--source-ref") + 1], "refs/tags/v1.0.0")

    def test_attestation_without_signer_is_refused(self):
        with patch.object(collect, "gh", return_value="[]"), self.assertRaises(SystemExit):
            collect.verify_attestation(Path("x"), "fredrir/nsql", "v1.0.0")


class PackageConfigurationTests(unittest.TestCase):
    def test_packages_install_the_binary_and_licenses_with_format_specific_relations(self):
        with tempfile.TemporaryDirectory() as directory:
            payload = Path(directory)
            for name in ["nsql", "LICENSE", "THIRD-PARTY-LICENSES.md"]:
                (payload / name).write_text(name)
            config = collect.nfpm_config(RELEASE, "0.1.14", "arm64", payload)
        destinations = {entry["dst"]: entry["file_info"]["mode"] for entry in config["contents"]}
        self.assertEqual(destinations, {"/usr/bin/nsql": 0o755, "/usr/share/licenses/nsql/LICENSE": 0o644,
                                        "/usr/share/licenses/nsql/THIRD-PARTY-LICENSES.md": 0o644})
        self.assertEqual(config["overrides"]["deb"]["recommends"], ["neovim"])
        self.assertEqual(config["overrides"]["deb"]["depends"], [])
        self.assertEqual(config["apk"]["signature"]["key_name"], "fredrir")
        self.assertEqual(config["rpm"]["signature"]["key_file"], "${RPM_SIGNING_KEY}")
        self.assertEqual((config["arch"], config["version"], config["release"]), ("arm64", "0.1.14", "1"))

    def test_collect_verifies_every_asset_before_packaging(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            payloads = {}
            for triple in ["x86_64-unknown-linux-gnu", "aarch64-unknown-linux-gnu", "x86_64-unknown-linux-musl", "aarch64-unknown-linux-musl"]:
                source = base / "src" / triple
                source.mkdir(parents=True)
                (source / "nsql").write_text("binary")
                archive = base / f"nsql-{triple}-v0.1.14.tar.gz"
                with tarfile.open(archive, "w:gz") as bundle:
                    bundle.add(source / "nsql", arcname="nsql")
                payloads[archive.name] = archive.read_bytes()
            payloads["release.json"] = json.dumps(RELEASE).encode()
            for name in collect.CHANNEL_FILES:
                payloads[name.format(name="nsql")] = b"generated"
            payloads["checksums.txt"] = "".join(f"{hashlib.sha256(data).hexdigest()}  {name}\n"
                                                for name, data in payloads.items()).encode()
            verified = []

            def gh(*args):
                if args[0] == "api":
                    return json.dumps([release("v0.1.14")])
                target = Path(args[args.index("--dir") + 1])
                for name, data in payloads.items():
                    (target / name).write_bytes(data)
                return ""

            registry = {"projects": [{"project": "nsql", "repository": "fredrir/nsql", "id": 1, "visibility": "public", "taps": ["homebrew-nsql"]},
                                     {"project": "secret", "repository": "fredrir/secret", "id": 2, "visibility": "private"}]}
            with patch.object(collect, "gh", side_effect=gh), \
                    patch.object(collect, "verify_attestation", side_effect=lambda p, r, t: verified.append(p.name)), \
                    patch.object(collect, "package") as packaged:
                tools = collect.collect(registry, base / "work", base / "site", base / "channels")
            self.assertEqual(sorted(verified), sorted(n for n in payloads if n != "checksums.txt"))
            packaged.assert_called_once()
            self.assertEqual(tools, {"nsql": {"repository": "fredrir/nsql", "version": "0.1.14", "binary": "nsql", "taps": ["homebrew-nsql"]}})
            self.assertTrue((base / "channels/nsql/nsql-bin.pkgbuild").exists())

            payloads["release.json"] = json.dumps(RELEASE | {"repository": "fredrir/other"}).encode()
            payloads["checksums.txt"] = "".join(f"{hashlib.sha256(data).hexdigest()}  {name}\n"
                                                for name, data in payloads.items() if name != "checksums.txt").encode()
            with patch.object(collect, "gh", side_effect=gh), patch.object(collect, "verify_attestation"), \
                    patch.object(collect, "package"), self.assertRaises(SystemExit):
                collect.collect(registry, base / "work2", base / "site2", base / "channels2")

    def test_tampered_assets_are_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "a"
            path.write_text("tampered")
            with self.assertRaises(SystemExit):
                collect.verify_checksum(path, hashlib.sha256(b"original").hexdigest())


class SiteTests(unittest.TestCase):
    TOOLS = {"nsql": {"repository": "fredrir/nsql", "version": "0.1.14", "binary": "nsql", "taps": []}}

    def test_installer_is_valid_shell_and_rejects_unknown_input(self):
        with tempfile.TemporaryDirectory() as directory:
            script = Path(directory) / "install.sh"
            script.write_text(package_site.installer(self.TOOLS))
            self.assertEqual(subprocess.run(["sh", "-n", str(script)]).returncode, 0)
            self.assertIn("nsql) repository=fredrir/nsql version=0.1.14 binary=nsql ;;", script.read_text())
            for arguments in [[], ["other"], ["nsql", "1.0;rm"]]:
                with self.subTest(arguments=arguments):
                    result = subprocess.run(["sh", str(script), *arguments], capture_output=True, text=True,
                                            env={"PATH": "/usr/bin:/bin", "HOME": directory})
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("install.sh:", result.stderr)

    def test_installer_refuses_unsafe_registry_values(self):
        for tool in [{"repository": "evil/x", "version": "1", "binary": "x"},
                     {"repository": "fredrir/x", "version": "1;id", "binary": "x"},
                     {"repository": "fredrir/x", "version": "1", "binary": "x y"}]:
            with self.subTest(tool=tool), self.assertRaises(SystemExit):
                package_site.installer({"x": tool})


class ChannelTests(unittest.TestCase):
    NUR = "{ pkgs ? import <nixpkgs> { } }:\n{\n  lib = import ./lib { inherit pkgs; };\n  example-package = pkgs.callPackage ./pkgs/example-package { };\n}\n"

    def test_nur_index_gains_each_package_once(self):
        updated = channels.nur_index(self.NUR, "nsql")
        self.assertIn("  nsql = pkgs.callPackage ./pkgs/nsql { };\n}", updated)
        self.assertEqual(channels.nur_index(updated, "nsql"), updated)

    def test_taps_are_updated_with_the_latest_casks_only_when_changed(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            isolated = {**os.environ, "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1"}
            remotes = {}
            for tap in ["homebrew-tap", "homebrew-nsql"]:
                remote = base / f"{tap}.git"
                seed = base / f"seed-{tap}"
                subprocess.run(["git", "init", "-q", "--bare", "-b", "main", str(remote)], check=True, env=isolated)
                subprocess.run(["git", "init", "-q", "-b", "main", str(seed)], check=True, env=isolated)
                (seed / "README.md").write_text("tap")
                subprocess.run(["git", "-C", str(seed), "add", "."], check=True, env=isolated)
                subprocess.run(["git", "-C", str(seed), "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"], check=True, env=isolated)
                subprocess.run(["git", "-C", str(seed), "push", "-q", str(remote), "main"], check=True, env=isolated)
                remotes[f"fredrir/{tap}"] = remote
            (base / "channels/nsql").mkdir(parents=True)
            (base / "channels/nsql/nsql.rb").write_text('cask "nsql" do\nend\n')

            def checkout(repository, token, destination):
                subprocess.run(["git", "clone", "-q", str(remotes[repository]), str(destination)], check=True, env=isolated)
                return "main"

            tools = {"nsql": {"version": "0.1.14", "taps": ["homebrew-nsql"]}}
            with patch.dict(os.environ, isolated), patch.object(channels, "github_checkout", side_effect=checkout):
                channels.publish_taps(tools, base / "channels", base / "work1", lambda scope: "token")
                channels.publish_taps(tools, base / "channels", base / "work2", lambda scope: "token")
            for remote in remotes.values():
                log = subprocess.run(["git", "--git-dir", str(remote), "log", "--format=%s", "main"],
                                     capture_output=True, text=True, check=True, env=isolated).stdout.split("\n")
                self.assertEqual(log[:2], ["Update nsql 0.1.14", "init"])
                shown = subprocess.run(["git", "--git-dir", str(remote), "show", "main:Casks/nsql.rb"],
                                       capture_output=True, text=True, check=True, env=isolated).stdout
                self.assertIn('cask "nsql"', shown)


if __name__ == "__main__":
    unittest.main()
