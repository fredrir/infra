import os
import socket
import subprocess
import unittest
from datetime import datetime, timezone
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[2] / 'scripts/ci/build-cache-args.sh'
CACHE = {'BUILDKIT_CACHE_BUCKET': 'ci-example-main', 'AWS_ACCESS_KEY_ID': 'GK' + 'a' * 24,
         'AWS_SECRET_ACCESS_KEY': 'b' * 64, 'IMAGE': 'ghcr.io/fredrir/example/web'}


class BuildCacheArgumentTests(unittest.TestCase):
    def setUp(self):
        self.listener = socket.socket()
        self.addCleanup(self.listener.close)
        self.listener.bind(('127.0.0.1', 0))
        self.listener.listen()
        self.endpoint = f'http://127.0.0.1:{self.listener.getsockname()[1]}'

    def arguments(self, environment):
        inherited = {k: v for k, v in os.environ.items() if not k.startswith(('BUILDKIT_CACHE_', 'AWS_'))}
        return subprocess.run(
            ['bash', '-c', 'source "$1"; printf "%s\\0" "${cache_args[@]}"', 'args', str(SCRIPT)],
            env=inherited | environment, capture_output=True, check=False,
        )

    def test_reachable_cache_imports_and_exports_every_layer(self):
        result = self.arguments(CACHE | {'BUILDKIT_CACHE_ENDPOINT': self.endpoint})
        self.assertEqual(result.returncode, 0, result.stderr)
        values = result.stdout.decode().split('\0')
        store = f'type=s3,region=garage,bucket=ci-example-main,endpoint_url={self.endpoint},use_path_style=true,prefix=buildkit/,name=example-web-' + datetime.now(timezone.utc).strftime('%G-%V')
        self.assertEqual(values[values.index('--import-cache') + 1], store)
        self.assertEqual(values[values.index('--export-cache') + 1], store + ',mode=max,touch_refresh=1m,ignore-error=true')
        self.assertNotIn(CACHE['AWS_SECRET_ACCESS_KEY'], result.stdout.decode())

    def test_disabled_unconfigured_or_unreachable_cache_builds_without_it(self):
        reachable = CACHE | {'BUILDKIT_CACHE_ENDPOINT': self.endpoint}
        self.assertNotIn(b'--import-cache', self.arguments(reachable | {'LAYER_CACHE': 'false'}).stdout)
        self.listener.close()
        for environment in [{'IMAGE': CACHE['IMAGE']}, reachable]:
            with self.subTest(environment=sorted(environment)):
                result = self.arguments(environment)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertNotIn(b'--import-cache', result.stdout)

    def test_malformed_or_incomplete_configuration_refuses(self):
        complete = CACHE | {'BUILDKIT_CACHE_ENDPOINT': self.endpoint}
        for change in [{'BUILDKIT_CACHE_BUCKET': 'toolchains'}, {'BUILDKIT_CACHE_ENDPOINT': 'https://example.com:443/x'},
                       {'IMAGE': 'docker.io/library/example'}, {'AWS_SECRET_ACCESS_KEY': ''}]:
            with self.subTest(change=change):
                self.assertNotEqual(self.arguments(complete | change).returncode, 0)


if __name__ == '__main__':
    unittest.main()
