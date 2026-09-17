import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SHIM = Path(__file__).resolve().parents[2] / "images/runner-rust/nix-hash"
KNOWN = {b"test": "020ay2q1av2xs4n842rb3d7vz8qms1dcb87a5yd6azaci20x11lz"}


class NixHashTests(unittest.TestCase):
    def hash(self, content, *arguments):
        with tempfile.NamedTemporaryFile(delete=False) as handle:
            handle.write(content)
        try:
            return subprocess.run([sys.executable, str(SHIM), *arguments, handle.name],
                                  capture_output=True, text=True, check=False)
        finally:
            os.unlink(handle.name)

    def test_known_digest_matches_nix(self):
        for content, expected in KNOWN.items():
            self.assertEqual(self.hash(content, "--type", "sha256", "--flat", "--base32").stdout.strip(), expected)

    @unittest.skipUnless(shutil.which("nix-hash"), "nix-hash is not installed")
    def test_random_content_matches_the_real_nix_hash(self):
        for size in [0, 1, 31, 32, 33, 4096, 65537]:
            content = os.urandom(size)
            with self.subTest(size=size), tempfile.NamedTemporaryFile() as handle:
                handle.write(content)
                handle.flush()
                real = subprocess.run(["nix-hash", "--type", "sha256", "--flat", "--base32", handle.name],
                                      capture_output=True, text=True, check=True).stdout.strip()
                self.assertEqual(self.hash(content, "--type", "sha256", "--flat", "--base32").stdout.strip(), real)

    def test_unsupported_invocations_fail(self):
        for arguments in [("--type", "sha512", "--flat", "--base32"), ("--type", "sha256", "--base32"), ()]:
            with self.subTest(arguments=arguments):
                self.assertNotEqual(self.hash(b"x", *arguments).returncode, 0)


if __name__ == "__main__":
    unittest.main()
