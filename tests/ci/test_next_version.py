import contextlib
import io
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "scripts/ci"))

import next_version  # noqa: E402

SINGLE = '[package]\nname = "nsql"\nversion = "0.1.13"\n\n[dependencies]\nfoo = { version = "1" }\n'
WORKSPACE = '[workspace]\nmembers = ["a"]\n\n[workspace.package]\nversion = "1.2.0" # shared\n\n[package]\nname = "a"\nversion.workspace = true\n'


class NextVersionTests(unittest.TestCase):
    def bump(self, manifest, tags):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "Cargo.toml"
            path.write_text(manifest)
            released = Path(directory) / "tags"
            released.write_text("\n".join(tags))
            with contextlib.redirect_stdout(io.StringIO()) as printed:
                next_version.main([str(path), "--tags", str(released)])
            return printed.getvalue().strip(), path.read_text()

    def test_patch_follows_the_latest_release_tag_unless_the_manifest_is_ahead(self):
        for current, tags, expected in [
            ("0.1.13", ["v0.1.12", "v0.1.13", "v0.1.14-rc.1", "nightly"], "0.1.14"),
            ("0.1.13", ["v0.1.13", "v0.1.20"], "0.1.21"),
            ("0.1.13", [], "0.1.13"),
            ("0.2.0", ["v0.1.13"], "0.2.0"),
        ]:
            with self.subTest(current=current, tags=tags):
                printed, manifest = self.bump(SINGLE.replace("0.1.13", current), tags)
                self.assertEqual(printed, expected)
                self.assertEqual(manifest, SINGLE.replace("0.1.13", expected))

    def test_workspace_version_is_bumped_in_place(self):
        printed, manifest = self.bump(WORKSPACE, ["v1.2.0"])
        self.assertEqual(printed, "1.2.1")
        self.assertEqual(manifest, WORKSPACE.replace("1.2.0", "1.2.1"))

    def test_prerelease_manifest_versions_are_refused(self):
        with self.assertRaises(SystemExit):
            self.bump(SINGLE.replace("0.1.13", "0.2.0-beta.1"), ["v0.1.13"])


if __name__ == "__main__":
    unittest.main()
