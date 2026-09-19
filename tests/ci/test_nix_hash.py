import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SHIM = Path(__file__).resolve().parents[2] / "images/runner-rust/nix-hash"
NIX_DIGESTS = {
    b"": "0mdqa9w1p6cmli6976v4wi0sw9r4p5prkj7lzfd1877wk11c9c73",
    b"test": "020ay2q1av2xs4n842rb3d7vz8qms1dcb87a5yd6azaci20x11lz",
    bytes(65537): "07z0n280p2sncvj0v28axknrpi807smbjgmxqc38s9xy657k0rij",
}


class NixHashTests(unittest.TestCase):
    def hash(self, content, *arguments):
        with tempfile.NamedTemporaryFile() as handle:
            handle.write(content)
            handle.flush()
            return subprocess.run([sys.executable, str(SHIM), *arguments, handle.name],
                                  capture_output=True, text=True, check=False)

    def test_digests_match_nix(self):
        for content, expected in NIX_DIGESTS.items():
            with self.subTest(size=len(content)):
                self.assertEqual(self.hash(content, "--type", "sha256", "--flat", "--base32").stdout.strip(), expected)

    def test_unsupported_invocations_fail(self):
        for arguments in [("--type", "sha512", "--flat", "--base32"), ("--type", "sha256", "--base32"), ()]:
            with self.subTest(arguments=arguments):
                self.assertNotEqual(self.hash(b"x", *arguments).returncode, 0)


if __name__ == "__main__":
    unittest.main()
