import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts/ci/next_version.py"
SINGLE = '[package]\nname = "nsql"\nversion = "0.1.13"\n\n[dependencies]\nfoo = { version = "1" }\n'
WORKSPACE = '[workspace]\nmembers = ["a"]\n\n[workspace.package]\nversion = "1.2.0" # shared\n\n[package]\nname = "a"\nversion.workspace = true\n'


class NextVersionTests(unittest.TestCase):
    def bump(self, manifest, tags):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "Cargo.toml"
            path.write_text(manifest)
            (Path(directory) / "tags").write_text("\n".join(tags))
            result = subprocess.run([sys.executable, str(SCRIPT), str(path), "--tags", str(Path(directory) / "tags")],
                                    capture_output=True, text=True, check=False)
            return result, path.read_text()

    def test_patch_follows_the_latest_release_tag(self):
        result, manifest = self.bump(SINGLE, ["v0.1.12", "v0.1.13", "v0.1.14-rc.1", "nightly"])
        self.assertEqual(result.stdout.strip(), "0.1.14")
        self.assertIn('version = "0.1.14"', manifest)
        self.assertIn('foo = { version = "1" }', manifest)

    def test_first_release_and_manual_bumps_keep_the_manifest_version(self):
        self.assertEqual(self.bump(SINGLE, [])[0].stdout.strip(), "0.1.13")
        result, manifest = self.bump(SINGLE.replace("0.1.13", "0.2.0"), ["v0.1.13"])
        self.assertEqual(result.stdout.strip(), "0.2.0")
        self.assertEqual(manifest, SINGLE.replace("0.1.13", "0.2.0"))

    def test_tagged_manifest_version_moves_past_the_latest_tag(self):
        self.assertEqual(self.bump(SINGLE, ["v0.1.13", "v0.1.20"])[0].stdout.strip(), "0.1.21")

    def test_workspace_version_is_bumped_in_place(self):
        result, manifest = self.bump(WORKSPACE, ["v1.2.0"])
        self.assertEqual(result.stdout.strip(), "1.2.1")
        self.assertIn('version = "1.2.1" # shared', manifest)
        self.assertIn("version.workspace = true", manifest)

    def test_prerelease_manifest_versions_are_refused(self):
        result, _ = self.bump(SINGLE.replace("0.1.13", "0.2.0-beta.1"), ["v0.1.13"])
        self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
