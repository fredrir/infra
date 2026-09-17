import base64
import struct
import unittest
from pathlib import Path

KEYS = Path(__file__).resolve().parents[2] / "ssh/admin_keys"


def embedded_type(blob):
    (length,) = struct.unpack(">I", blob[:4])
    return blob[4:4 + length].decode()


class AdminKeysTests(unittest.TestCase):
    def test_every_line_is_a_public_key_matching_its_declared_type(self):
        lines = KEYS.read_text().splitlines()
        self.assertTrue(lines)
        for line in lines:
            with self.subTest(line=line):
                key_type, blob, *_ = line.split()
                self.assertEqual(embedded_type(base64.b64decode(blob, validate=True)), key_type)

    def test_keys_are_unique_and_newline_terminated(self):
        text = KEYS.read_text()
        self.assertTrue(text.endswith("\n"))
        blobs = [line.split()[1] for line in text.splitlines()]
        self.assertEqual(len(blobs), len(set(blobs)))


if __name__ == "__main__":
    unittest.main()
