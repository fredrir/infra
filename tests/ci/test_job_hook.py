import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

HOOK = Path(__file__).resolve().parents[2] / 'images/runner-rust/ci-job-started.sh'
OWNER_ID = '114402558'
REPOSITORY = 'fredrir/nsql'


class JobHookTests(unittest.TestCase):
    def run_hook(self, pool, event, ref, protected='false', head=REPOSITORY, owner=OWNER_ID):
        with tempfile.TemporaryDirectory() as directory:
            payload = Path(directory) / 'event.json'
            payload.write_text(json.dumps({'pull_request': {'head': {'repo': {'full_name': head}}}}))
            environment = {
                'PATH': os.environ['PATH'],
                'CI_POOL': pool,
                'GITHUB_EVENT_NAME': event,
                'GITHUB_REF': ref,
                'GITHUB_REF_PROTECTED': protected,
                'GITHUB_REPOSITORY': REPOSITORY,
                'GITHUB_REPOSITORY_OWNER_ID': owner,
                'GITHUB_EVENT_PATH': str(payload),
            }
            return subprocess.run(['bash', '-e', str(HOOK)], env=environment, capture_output=True, text=True, check=False)

    def assert_allowed(self, *args, **kwargs):
        result = self.run_hook(*args, **kwargs)
        self.assertEqual(result.returncode, 0, result.stderr)

    def assert_denied(self, *args, **kwargs):
        result = self.run_hook(*args, **kwargs)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('::error::', result.stderr)

    def test_pull_request_pool_accepts_only_same_repository_pull_requests(self):
        self.assert_allowed('pr', 'pull_request', 'refs/pull/7/merge')
        self.assert_denied('pr', 'pull_request', 'refs/pull/7/merge', head='attacker/nsql')
        self.assert_denied('pr', 'pull_request_target', 'refs/heads/main', protected='true')
        self.assert_denied('pr', 'push', 'refs/heads/main', protected='true')

    def test_main_pool_accepts_only_the_protected_main_branch(self):
        self.assert_allowed('main', 'push', 'refs/heads/main', protected='true')
        self.assert_allowed('main', 'workflow_dispatch', 'refs/heads/main', protected='true')
        self.assert_denied('main', 'push', 'refs/heads/main')
        self.assert_denied('main', 'push', 'refs/heads/feature', protected='true')
        self.assert_denied('main', 'pull_request', 'refs/pull/7/merge')
        self.assert_denied('main', 'push', 'refs/tags/v1.0.0', protected='true')

    def test_release_pool_accepts_protected_tags_and_main_dry_runs(self):
        self.assert_allowed('release', 'push', 'refs/tags/v1.2.3', protected='true')
        self.assert_allowed('release', 'workflow_dispatch', 'refs/heads/main', protected='true')
        self.assert_denied('release', 'push', 'refs/tags/v1.2.3')
        self.assert_denied('release', 'push', 'refs/tags/nightly', protected='true')
        self.assert_denied('release', 'push', 'refs/heads/main', protected='true')
        self.assert_denied('release', 'workflow_dispatch', 'refs/heads/feature', protected='true')
        self.assert_denied('release', 'pull_request', 'refs/pull/7/merge')

    def test_unknown_pools_and_foreign_owners_are_refused(self):
        self.assert_denied('', 'push', 'refs/heads/main', protected='true')
        self.assert_denied('build', 'push', 'refs/heads/main', protected='true')
        self.assert_denied('main', 'push', 'refs/heads/main', protected='true', owner='1')


if __name__ == '__main__':
    unittest.main()
