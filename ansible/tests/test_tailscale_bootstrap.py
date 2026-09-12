import copy
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
ROLE = ROOT / 'ansible/roles/platform'
ANSIBLE = shutil.which('ansible-playbook') or str(ROOT / '.venv/bin/ansible-playbook')


class TailscaleBootstrapTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.directory = Path(self.temporary.name)
        self.values = yaml.safe_load((ROLE / 'defaults/main.yml').read_text())
        self.values.update({
            'ansible_facts': {'service_mgr': 'systemd', 'distribution': 'Ubuntu', 'distribution_version': '24.04', 'distribution_major_version': '24', 'architecture': 'x86_64'},
            'ansible_python_interpreter': sys.executable,
            'platform_architecture': 'amd64',
            'platform_node_name': 'fredrir-07',
            'platform_tailscale_bootstrap_approved': True,
            'platform_tailscale_bootstrap_tags': ['tag:platform-control'],
            'platform_tailscale_auth_key_file': '/run/secrets/tailscale-auth-key',
            'platform_bootstrap_k3s': {'results': [{'stat': {'exists': False}}]},
            'platform_bootstrap_key': {'stat': {'exists': True, 'isreg': True, 'islnk': False, 'uid': 0, 'mode': '0400', 'nlink': 1, 'size': 40}},
        })

    def tearDown(self):
        self.temporary.cleanup()

    def run_tasks(self, tasks, values=None):
        playbook = self.directory / 'test.yml'
        playbook.write_text(yaml.safe_dump([{'hosts': 'all', 'gather_facts': False, 'vars': self.values if values is None else values, 'tasks': [{'ansible.builtin.include_tasks': str(ROLE / 'tasks' / task)} for task in tasks]}]))
        environment = os.environ | {'ANSIBLE_CONFIG': str(ROOT / 'ansible/ansible.cfg'), 'ANSIBLE_NOCOLOR': '1'}
        return subprocess.run([ANSIBLE, '-i', 'localhost,', '--connection', 'local', str(playbook)], capture_output=True, text=True, env=environment, timeout=30)

    def assert_succeeds(self, result):
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def assert_refused(self, result):
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('"evaluated_to": false', result.stdout)

    def test_supported_operating_systems_and_architectures_need_no_node_address(self):
        for distribution, version, architecture, machine in [('Ubuntu', '24.04', 'amd64', 'x86_64'), ('Ubuntu', '26.04', 'arm64', 'aarch64'), ('Debian', '13.6', 'arm64', 'aarch64')]:
            with self.subTest(distribution=distribution, architecture=architecture):
                values = copy.deepcopy(self.values)
                values['ansible_facts'].update(distribution=distribution, distribution_version=version, distribution_major_version=version.split('.')[0], architecture=machine)
                values['platform_architecture'] = architecture
                self.assert_succeeds(self.run_tasks(['tailscale-bootstrap-contract.yml', 'tailscale-bootstrap-credentials.yml'], values))

    def test_preflight_refuses_unsupported_or_unapproved_enrollment(self):
        changes = [
            {'platform_tailscale_bootstrap_approved': False},
            {'platform_tailscale_bootstrap_tags': []},
            {'platform_tailscale_bootstrap_tags': ['tag:platform-control', 'tag:platform-worker']},
            {'platform_tailscale_bootstrap_tags': ['tag:platform-enrollment']},
            {'platform_tailscale_bootstrap_tags': ['tag:infra']},
            {'platform_tailscale_version': '1.90.9'},
            {'platform_node_name': 'ubuntu-8gb'},
            {'platform_architecture': 'arm64'},
            {'platform_tailscale_auth_key_file': '/run/../etc/key'},
            {'ansible_facts': self.values['ansible_facts'] | {'distribution_version': '22.04'}},
            {'ansible_facts': self.values['ansible_facts'] | {'service_mgr': 'openrc'}},
        ]
        for change in changes:
            with self.subTest(change=change):
                self.assert_refused(self.run_tasks(['tailscale-bootstrap-contract.yml'], self.values | change))

    def test_preflight_refuses_existing_k3s_or_unprotected_key_metadata(self):
        for metadata in [{'exists': False}, {'mode': '0644'}, {'uid': 1000}, {'islnk': True}, {'nlink': 2}, {'size': 0}]:
            with self.subTest(metadata=metadata):
                values = copy.deepcopy(self.values)
                values['platform_bootstrap_key']['stat'].update(metadata)
                self.assert_refused(self.run_tasks(['tailscale-bootstrap-credentials.yml'], values))
        values = self.values | {'platform_bootstrap_k3s': {'results': [{'stat': {'exists': True}}]}}
        self.assert_refused(self.run_tasks(['tailscale-bootstrap-credentials.yml'], values))

    def mock_transport(self, **changes):
        state = self.directory / 'state.json'
        state.write_text(json.dumps({'running': False, 'tags': [], 'rejectTags': False, 'failLogin': False, 'preferences': {'ssh': False, 'accept-routes': False, 'advertise-routes': '', 'advertise-exit-node': False, 'hostname': 'fredrir-07', 'fixture': 'private-fixture-must-not-be-logged'}} | changes))
        executable = self.directory / 'tailscale'
        executable.write_text(f'#!{sys.executable}\n' + '''import json
from pathlib import Path
import sys

directory = Path(__file__).parent
state = json.loads((directory / 'state.json').read_text())
args = sys.argv[1:]
with (directory / 'calls.jsonl').open('a') as calls:
    calls.write(json.dumps(args) + '\\n')
if args == ['status', '--json']:
    print(json.dumps({'BackendState': 'Running' if state['running'] else 'NeedsLogin', 'Self': {'Tags': state['tags']}}))
elif args[:1] in [['up'], ['set']]:
    tags = next((arg.split('=', 1)[1].split(',') for arg in args if arg.startswith('--advertise-tags=')), state['tags'])
    if args[0] == 'set' and any(arg.startswith('--advertise-tags=') for arg in args):
        sys.exit(2)
    if not state['rejectTags']:
        state['tags'] = tags
    state['running'] = True
    state['preferences'].update({'ssh': False, 'accept-routes': '--accept-routes=true' in args, 'advertise-routes': '', 'advertise-exit-node': False})
    state['preferences']['hostname'] = next(arg.split('=', 1)[1] for arg in args if arg.startswith('--hostname='))
    if args[0] == 'up' and state['failLogin']:
        (directory / 'state.json').write_text(json.dumps(state))
        sys.exit(1)
elif args == ['get', '--json']:
    print(json.dumps(state['preferences']))
elif args == ['down']:
    state['running'] = False
elif args == ['ip', '-4']:
    print('100.100.0.7')
else:
    sys.exit(2)
(directory / 'state.json').write_text(json.dumps(state))
''')
        executable.chmod(0o700)
        self.values['platform_tailscale_cli'] = str(executable)

    def calls(self):
        return [json.loads(line) for line in (self.directory / 'calls.jsonl').read_text().splitlines()]

    def test_repeated_enrollment_logs_in_once_without_exposing_credentials(self):
        self.mock_transport()
        result = self.run_tasks(['tailscale-bootstrap-enroll.yml', 'tailscale-bootstrap-enroll.yml'])
        self.assert_succeeds(result)
        logins = [call for call in self.calls() if call[0] == 'up']
        self.assertEqual(len(logins), 1)
        self.assertIn('--auth-key=file:/run/secrets/tailscale-auth-key', logins[0])
        self.assertIn('--ssh=false', logins[0])
        self.assertIn('--accept-routes=false', logins[0])
        self.assertIn('--advertise-tags=tag:platform-control', logins[0])
        self.assertIn('--hostname=fredrir-07', logins[0])
        self.assertFalse(any(call[0] == 'set' for call in self.calls()))
        self.assertNotIn('private-fixture-must-not-be-logged', result.stdout + result.stderr)

    def test_preference_drift_is_repaired_once_without_reauthentication(self):
        self.mock_transport(running=True, tags=['tag:platform-control'], preferences={'ssh': True, 'accept-routes': True, 'advertise-routes': '10.0.0.0/8', 'advertise-exit-node': True, 'hostname': 'ubuntu-8gb'})
        self.assert_succeeds(self.run_tasks(['tailscale-bootstrap-enroll.yml', 'tailscale-bootstrap-enroll.yml']))
        self.assertEqual(len([call for call in self.calls() if call[0] == 'set']), 1)
        self.assertFalse(any(call[0] == 'up' for call in self.calls()))
        self.assertEqual(json.loads((self.directory / 'state.json').read_text())['preferences']['hostname'], 'fredrir-07')

    def test_existing_untagged_identity_is_not_adopted(self):
        self.mock_transport(running=True)
        self.assert_refused(self.run_tasks(['tailscale-bootstrap-enroll.yml']))
        self.assertEqual(self.calls(), [['status', '--json']])

    def test_worker_accepts_routes_without_advertising_them(self):
        self.mock_transport()
        self.values['platform_tailscale_bootstrap_tags'] = ['tag:platform-worker']
        self.assert_succeeds(self.run_tasks(['tailscale-bootstrap-enroll.yml']))
        login = next(call for call in self.calls() if call[0] == 'up')
        self.assertIn('--accept-routes=true', login)
        self.assertIn('--advertise-routes=', login)
        self.assertIn('--advertise-exit-node=false', login)

    def test_failed_tag_authorization_disconnects_new_identity(self):
        self.mock_transport(rejectTags=True)
        result = self.run_tasks(['tailscale-bootstrap-enroll.yml'])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Transport disconnected', result.stdout)
        self.assertIn(['down'], self.calls())
        self.assertFalse(json.loads((self.directory / 'state.json').read_text())['running'])

    def test_failed_login_disconnects_partial_enrollment(self):
        self.mock_transport(failLogin=True)
        result = self.run_tasks(['tailscale-bootstrap-enroll.yml'])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(['down'], self.calls())
        self.assertFalse(json.loads((self.directory / 'state.json').read_text())['running'])

    def test_bootstrap_playbook_has_valid_ansible_syntax(self):
        environment = os.environ | {'ANSIBLE_CONFIG': str(ROOT / 'ansible/ansible.cfg')}
        result = subprocess.run([ANSIBLE, '-i', 'localhost,', str(ROOT / 'ansible/tailscale-bootstrap.yml'), '--syntax-check'], capture_output=True, text=True, env=environment, timeout=30)
        self.assert_succeeds(result)


if __name__ == '__main__':
    unittest.main()
