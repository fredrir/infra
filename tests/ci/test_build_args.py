import os
import subprocess
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[2] / 'scripts/ci/build-args.sh'


class BuildArgumentTests(unittest.TestCase):
    def arguments(self, value):
        return subprocess.run(
            ['bash', '-c', 'source "$1"; printf "%s\\0" "${build_args[@]}"', 'args', str(SCRIPT)],
            env={**os.environ, 'GITHUB_SHA': 'a' * 40, 'BUILD_ARGS': value},
            capture_output=True, check=False,
        )

    def test_public_values_stay_literal_single_arguments(self):
        result = self.arguments('VITE_TURNSTILE_SITE_KEY=public key\nAPP_VERSION=$(false); literal')
        self.assertEqual(result.returncode, 0, result.stderr)
        values = result.stdout.decode().split('\0')
        self.assertIn('build-arg:VITE_TURNSTILE_SITE_KEY=public key', values)
        self.assertIn('build-arg:APP_VERSION=$(false); literal', values)
        self.assertIn('build-arg:REVISION=' + 'a' * 40, values)

    def test_private_reserved_duplicate_and_malformed_arguments_refuse(self):
        for value in ['TOKEN=private', 'GIT_SHA=override', 'REVISION=override',
                      'VITE_A=one\nVITE_A=two', 'VITE_A', 'VITE_A=one\r']:
            with self.subTest(value=value):
                self.assertNotEqual(self.arguments(value).returncode, 0)
