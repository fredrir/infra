import os
import subprocess
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[2] / "scripts/ci/rust-target-cache.sh"
SECRET = "b" * 64
CURL = """#!/usr/bin/env python3
import os, shutil, sys
from pathlib import Path
arguments = sys.argv[1:]
with open(os.environ["S3_LOG"], "a") as log:
    log.write(" ".join(arguments) + "\\n")
stored = Path(os.environ["S3_ROOT"]) / arguments[-1].split("://", 1)[1].split("/", 1)[1]
option = lambda name: arguments[arguments.index(name) + 1] if name in arguments else None
if option("--upload-file"):
    stored.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(option("--upload-file"), stored)
elif option("--request") == "DELETE":
    stored.unlink(missing_ok=True)
elif not stored.exists():
    sys.exit(22)
elif option("--output"):
    shutil.copyfile(stored, option("--output"))
else:
    sys.stdout.write(stored.read_text())
"""


class RustTargetCacheTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.area = Path(self.directory.name)
        self.bucket = self.area / "s3/ci-example-main/target"
        binaries = self.area / "bin"
        binaries.mkdir()
        for name, content in [("curl", CURL), ("rustc", "#!/bin/sh\necho rustc 1.98.1\n")]:
            (binaries / name).write_text(content)
            (binaries / name).chmod(0o755)
        (self.area / "temp").mkdir()
        (self.area / "temp/rust-args.sh").write_text("declare -a CLIPPY_FLAGS=()\n")
        self.environment = {
            "PATH": str(binaries) + os.pathsep + os.environ["PATH"], "RUNNER_TEMP": str(self.area / "temp"),
            "S3_ROOT": str(self.area / "s3"), "S3_LOG": str(self.area / "s3.log"),
            "SCCACHE_ENDPOINT": "http://garage.invalid:3900", "SCCACHE_BUCKET": "ci-example-main",
            "AWS_ACCESS_KEY_ID": "GK" + "a" * 24, "AWS_SECRET_ACCESS_KEY": SECRET,
        }

    def workspace(self, name, lock="version = 4\n", output=None):
        directory = self.area / name
        directory.mkdir(exist_ok=True)
        (directory / "Cargo.lock").write_text(lock)
        if output is not None:
            (directory / "target/debug").mkdir(parents=True, exist_ok=True)
            (directory / "target/debug/artifact").write_text(output)
        return directory

    def run_cache(self, action, directory, **environment):
        return subprocess.run(["bash", str(SCRIPT), action], cwd=directory, env=self.environment | environment,
                              capture_output=True, text=True, check=False)

    def uploads(self):
        log = self.area / "s3.log"
        return log.read_text().count("--upload-file") if log.exists() else 0

    def test_saved_outputs_restore_into_a_fresh_checkout_without_exposing_the_key(self):
        result = self.run_cache("save", self.workspace("first", output="compiled"))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(list(self.bucket.glob("*.tar.zst"))), 1)
        fresh = self.workspace("second")
        result = self.run_cache("restore", fresh)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((fresh / "target/debug/artifact").read_text(), "compiled")
        self.assertNotIn(SECRET, (self.area / "s3.log").read_text() + result.stdout + result.stderr)

    def test_unchanged_dependencies_and_read_only_pools_never_upload(self):
        first = self.workspace("first", output="compiled")
        self.assertEqual(self.run_cache("save", first, SCCACHE_S3_RW_MODE="READ_ONLY").returncode, 0)
        self.assertEqual(self.uploads(), 0)
        self.assertEqual(self.run_cache("save", first).returncode, 0)
        self.assertEqual(self.uploads(), 2)
        self.assertEqual(self.run_cache("save", first).returncode, 0)
        self.assertEqual(self.uploads(), 2)
        self.assertEqual(self.run_cache("save", self.workspace("first", lock="version = 5\n")).returncode, 0)
        self.assertEqual(self.uploads(), 4)

    def test_outgrown_outputs_clear_the_cache_instead_of_uploading(self):
        first = self.workspace("first", output="compiled")
        self.assertEqual(self.run_cache("save", first).returncode, 0)
        changed = self.workspace("first", lock="version = 5\n")
        result = self.run_cache("save", changed, RUST_TARGET_CACHE_LIMIT_KIB="0")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(list(self.bucket.iterdir()), [])

    def test_missing_cache_or_configuration_never_fails_the_job(self):
        empty = self.workspace("empty")
        for action, environment in [("restore", {}), ("restore", {"AWS_SECRET_ACCESS_KEY": ""}), ("save", {})]:
            with self.subTest(action=action, environment=environment):
                result = self.run_cache(action, empty, **environment)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertFalse((empty / "target").exists())


if __name__ == "__main__":
    unittest.main()
