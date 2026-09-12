import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from urllib.error import HTTPError


ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('external_watchdog', ROOT / 'modules/platform/watchdog.py')
watchdog = importlib.util.module_from_spec(spec)
spec.loader.exec_module(watchdog)


class WatchdogContract(unittest.TestCase):
    def config(self):
        return {'targets': [{'name': 'application', 'url': 'https://application.example/health'}], 'alertWebhook': 'https://alerts.example/fixture-credential'}

    def test_missing_alert_credential_prevents_probes(self):
        config = self.config()
        config.pop('alertWebhook')
        with patch.object(watchdog, 'request') as request, self.assertRaisesRegex(watchdog.WatchdogError, 'credential'):
            watchdog.run(config, Path('/unused/state'))
        request.assert_not_called()

    def test_explicit_expected_http_error_status_is_healthy(self):
        error = HTTPError('https://application.example/private', 403, 'Forbidden', {}, io.BytesIO(b'forbidden'))
        with patch('urllib.request.build_opener') as opener:
            opener.return_value.open.side_effect = error
            self.assertEqual(watchdog.request('https://application.example/private'), (403, b'forbidden'))

    def test_failed_probe_alert_delivery_must_succeed_before_acknowledgement(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / 'state.json'
            with patch.object(watchdog, 'request', side_effect=[OSError('probe failure'), OSError('secret URL withheld')]):
                with self.assertRaisesRegex(watchdog.WatchdogError, '^Alert delivery failed$'):
                    watchdog.run(self.config(), state)
            self.assertFalse(state.exists())
            with patch.object(watchdog, 'request', side_effect=[OSError('probe failure'), (204, b'')]):
                self.assertEqual(watchdog.run(self.config(), state), 1)
            self.assertEqual(json.loads(state.read_text()), ['application'])

    def test_missing_stale_and_future_heartbeats_fail(self):
        config = {'heartbeats': [{'name': 'control', 'url': 'https://deadman.example/status', 'timestampField': 'last_ping', 'maxAgeSeconds': 300}], 'alertWebhook': 'https://alerts.example/fixture'}
        for value, failed in [(950, []), (600, ['control']), (1200, ['control']), (None, ['control'])]:
            with self.subTest(value=value), patch.object(watchdog, 'request', return_value=(200, json.dumps({'last_ping': value}).encode())):
                self.assertEqual(watchdog.check_health(config, 1000), failed)

    def test_deadman_is_confirmed_only_when_all_checks_are_healthy(self):
        config = self.config() | {'deadmanURL': 'https://deadman.example/ping/fixture'}
        with tempfile.TemporaryDirectory() as directory, patch.object(watchdog, 'request', side_effect=[(200, b'ok'), (200, b'ok')]) as request:
            self.assertEqual(watchdog.run(config, Path(directory) / 'state.json'), 0)
            self.assertEqual(request.call_args.args[0], config['deadmanURL'])


if __name__ == '__main__':
    unittest.main()
