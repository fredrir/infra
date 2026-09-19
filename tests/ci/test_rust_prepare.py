import os
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts/ci/rust-prepare.sh"


class RustPrepareTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        tools = tempfile.TemporaryDirectory()
        cls.addClassCleanup(tools.cleanup)
        cls.binaries = Path(tools.name)
        for tool in ["rustup", "sccache"]:
            (cls.binaries / tool).write_text("#!/bin/sh\nexit 0\n")
            (cls.binaries / tool).chmod(0o755)

    def prepare(self, clippy="", test="", **extra):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            environment = {"PATH": f"{self.binaries}:{os.environ['PATH']}", "RUNNER_TEMP": directory,
                           "GITHUB_ENV": str(base / "env"), "CLIPPY_ARGS": clippy, "TEST_ARGS": test} | extra
            result = subprocess.run(["bash", str(SCRIPT)], env=environment, capture_output=True, text=True, check=False)
            if result.returncode:
                return result, None, ""
            loaded = subprocess.run(["bash", "-c", 'source "$1"; printf "%s\\0" "${CLIPPY_FLAGS[@]}"; printf "|"; printf "%s\\0" "${TEST_FLAGS[@]}"',
                                     "load", str(base / "rust-args.sh")], capture_output=True, text=True, check=True).stdout
            clippy_part, test_part = loaded.split("|")
            env = (base / "env").read_text() if (base / "env").exists() else ""
            return result, ([v for v in clippy_part.split("\0") if v], [v for v in test_part.split("\0") if v]), env

    def test_arguments_become_separate_flags(self):
        result, (clippy, test), _ = self.prepare("--all-features", "--features vendored-openssl,cli --no-fail-fast")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(clippy, ["--all-features"])
        self.assertEqual(test, ["--features", "vendored-openssl,cli", "--no-fail-fast"])

    def test_shell_syntax_and_cargo_overrides_are_refused(self):
        for value in ["--features $(id)", "--all-features; curl x", "--config build.rustc-wrapper=x",
                      "--manifest-path ../other/Cargo.toml", "-Zunstable-options", "`id`"]:
            with self.subTest(value=value):
                result, _, _ = self.prepare(test=value)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("::error::", result.stdout)

    def test_unreachable_cache_disables_the_compiler_wrapper(self):
        result, _, env = self.prepare(RUSTC_WRAPPER="sccache", SCCACHE_ENDPOINT="http://127.0.0.1:9/", SCCACHE_BUCKET="b")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("RUSTC_WRAPPER=\n", env)
        self.assertIn("::warning::", result.stdout)


if __name__ == "__main__":
    unittest.main()
