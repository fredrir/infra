import os
import subprocess
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[2] / 'scripts/ci/build-test-target.sh'


class BuildTestTargetTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.workspace = Path(directory.name)
        (self.workspace / 'ci').mkdir()
        (self.workspace / 'ci/Containerfile').write_text('FROM scratch AS unit-tests\n')
        (self.workspace / 'bin').mkdir()
        buildctl = self.workspace / 'bin/buildctl-daemonless.sh'
        buildctl.write_text('#!/bin/sh\nprintf "%s\\n" "$BUILDKITD_FLAGS" > "$RECORD.flags"\nprintf "%s\\0" "$@" > "$RECORD"\n')
        buildctl.chmod(0o755)

    def build(self, *arguments, **environment):
        inherited = {k: v for k, v in os.environ.items() if not k.startswith(('BUILDKIT_CACHE_', 'AWS_'))}
        result = subprocess.run(['bash', str(SCRIPT), *arguments], cwd=self.workspace, capture_output=True, text=True, check=False,
                                env=inherited | {'PATH': f"{self.workspace / 'bin'}:{os.environ['PATH']}", 'RECORD': str(self.workspace / 'record'),
                                                 'IMAGE': 'ghcr.io/fredrir/example'} | environment)
        record = self.workspace / 'record'
        return result, record.read_text().split('\0')[:-1] if record.exists() else None

    def test_the_target_builds_from_its_recipe_with_extra_options_last(self):
        result, arguments = self.build('ci/Containerfile', 'unit-tests', '--opt', 'build-arg:CI_REVISION=a b', CONTEXT='.')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(arguments, ['build', '--frontend', 'dockerfile.v0', '--local', 'context=.', '--local', 'dockerfile=ci',
                                     '--opt', 'filename=Containerfile', '--opt', 'platform=linux/amd64', '--opt', 'target=unit-tests',
                                     '--opt', 'build-arg:CI_REVISION=a b'])
        self.assertIn('--oci-worker-net=host', (self.workspace / 'record.flags').read_text())

    def test_missing_escaping_or_malformed_inputs_never_build(self):
        for arguments in [['ci/Containerfile'], ['ci/Missing', 'unit-tests'], ['../ci/Containerfile', 'unit-tests'],
                          ['/etc/passwd', 'unit-tests'], ['ci/Containerfile', 'unit tests'], ['ci/Containerfile', '--opt']]:
            with self.subTest(arguments=arguments):
                result, recorded = self.build(*arguments)
                self.assertNotEqual(result.returncode, 0)
                self.assertIsNone(recorded)


if __name__ == '__main__':
    unittest.main()
