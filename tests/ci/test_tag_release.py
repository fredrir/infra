import os
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts/ci/tag-release.sh"
ISOLATED = {**os.environ, "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1"}


def git(directory, *args):
    return subprocess.run(["git", "-C", str(directory), *args], env=ISOLATED, check=True,
                          capture_output=True, text=True).stdout.strip()


class TagReleaseTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        base = Path(self.directory.name)
        self.remote = base / "remote.git"
        self.work = base / "work"
        self.bin = base / "bin"
        self.bin.mkdir()
        (self.bin / "cargo").write_text("#!/bin/sh\necho \"$@\" >> \"$RUNNER_TEMP/cargo.log\"\n")
        (self.bin / "git-cliff").write_text("#!/bin/sh\nwhile [ $# -gt 0 ]; do case $1 in --tag) tag=$2;; --output) out=$2;; esac; shift; done\necho \"## $tag\" > \"$out\"\n")
        for tool in self.bin.iterdir():
            tool.chmod(0o755)
        subprocess.run(["git", "init", "--quiet", "--bare", "--initial-branch=main", str(self.remote)], env=ISOLATED, check=True)
        subprocess.run(["git", "init", "--quiet", "--initial-branch=main", str(self.work)], env=ISOLATED, check=True)
        git(self.work, "config", "user.email", "test@example.com")
        git(self.work, "config", "user.name", "test")
        (self.work / "Cargo.toml").write_text('[package]\nname = "nsql"\nversion = "0.1.13"\n')
        (self.work / "Cargo.lock").write_text("lock\n")
        git(self.work, "add", ".")
        git(self.work, "commit", "--quiet", "-m", "initial")
        git(self.work, "tag", "v0.1.13")
        git(self.work, "commit", "--quiet", "--allow-empty", "-m", "fix: things")
        git(self.work, "remote", "add", "origin", str(self.remote))
        git(self.work, "push", "--quiet", "origin", "main", "v0.1.13")

    def tearDown(self):
        self.directory.cleanup()

    def tag(self):
        environment = ISOLATED | {"PATH": f"{self.bin}:{os.environ['PATH']}", "RUNNER_TEMP": self.directory.name}
        return subprocess.run(["bash", str(SCRIPT)], cwd=self.work, env=environment, capture_output=True, text=True, check=False)

    def test_release_commit_and_tag_reach_the_remote(self):
        result = self.tag()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(git(self.remote, "log", "-1", "--format=%s", "main"), "release: v0.1.14")
        self.assertEqual(git(self.remote, "rev-parse", "v0.1.14^{commit}"), git(self.remote, "rev-parse", "main"))
        self.assertEqual(git(self.remote, "cat-file", "-t", "v0.1.14"), "tag")
        self.assertIn('version = "0.1.14"', git(self.remote, "show", "main:Cargo.toml"))
        self.assertEqual(git(self.remote, "show", "main:CHANGELOG.md"), "## v0.1.14")
        self.assertIn("update --workspace", (Path(self.directory.name) / "cargo.log").read_text())

    def test_manual_version_bump_is_tagged_as_written(self):
        (self.work / "Cargo.toml").write_text('[package]\nname = "nsql"\nversion = "0.2.0"\n')
        git(self.work, "commit", "--quiet", "-am", "feat: bump")
        result = self.tag()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Tagged v0.2.0", result.stdout)
        self.assertIn('version = "0.2.0"', git(self.remote, "show", "v0.2.0:Cargo.toml"))
        self.assertEqual(git(self.remote, "log", "-1", "--format=%s", "main"), "release: v0.2.0")

if __name__ == "__main__":
    unittest.main()
