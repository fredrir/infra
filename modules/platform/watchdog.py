import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime
import json
from pathlib import Path
import time
import urllib.error
import urllib.parse
import urllib.request


class WatchdogError(ValueError):
    pass


def https_url(value):
    url = urllib.parse.urlsplit(value)
    if url.scheme != 'https' or not url.hostname or url.username or url.password or url.fragment:
        raise WatchdogError('HTTPS endpoint required')
    return value


class HTTPSRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, file, code, message, headers, new_url):
        raise WatchdogError('Redirect refused')


def request(url, *, headers=None, payload=None):
    body = None if payload is None else json.dumps(payload).encode()
    request_headers = dict(headers or {})
    if body is not None:
        request_headers['Content-Type'] = 'application/json'
    request = urllib.request.Request(https_url(url), data=body, headers=request_headers)
    try:
        response = urllib.request.build_opener(HTTPSRedirect).open(request, timeout=15)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        data = response.read(65537)
        if len(data) > 65536:
            raise WatchdogError('Response exceeds limit')
        return response.status, data


def validate_config(config):
    if set(config) - {'targets', 'heartbeats', 'alertWebhook', 'deadmanURL'}:
        raise WatchdogError('Unknown configuration key')
    if not config.get('alertWebhook'):
        raise WatchdogError('Alert credential required')
    https_url(config['alertWebhook'])
    if config.get('deadmanURL'):
        https_url(config['deadmanURL'])
    checks = config.get('targets', []) + config.get('heartbeats', [])
    if not checks or len(checks) > 32:
        raise WatchdogError('Between one and 32 checks required')
    names = set()
    for check in checks:
        if set(check) - {'name', 'url', 'expectedStatus', 'timestampField', 'maxAgeSeconds', 'authorization'}:
            raise WatchdogError('Unknown check key')
        if not isinstance(check.get('name'), str) or not check['name'] or len(check['name']) > 80 or check['name'] in names:
            raise WatchdogError('Unique check names required')
        names.add(check['name'])
        https_url(check['url'])
    for check in config.get('heartbeats', []):
        if not isinstance(check.get('timestampField'), str) or not check['timestampField']:
            raise WatchdogError('Heartbeat timestamp field required')
        age = check.get('maxAgeSeconds')
        if type(age) is not int or not 60 <= age <= 604800:
            raise WatchdogError('Heartbeat age must be between 60 seconds and seven days')
    return config


def timestamp(value):
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        return value
    if not isinstance(value, str):
        raise WatchdogError('Heartbeat timestamp required')
    parsed = datetime.fromisoformat(value.replace('Z', '+00:00'))
    if parsed.tzinfo is None:
        raise WatchdogError('Heartbeat timezone required')
    return parsed.timestamp()


def check_health(config, now):
    def probe(item):
        kind, check = item
        try:
            headers = {'Authorization': check['authorization']} if check.get('authorization') else None
            status, data = request(check['url'], headers=headers)
            healthy = status == check.get('expectedStatus', 200)
            if kind == 'heartbeat':
                value = json.loads(data)
                for field in check['timestampField'].split('.'):
                    value = value[field]
                age = now - timestamp(value)
                healthy = healthy and -60 <= age <= check['maxAgeSeconds']
            return None if healthy else check['name']
        except (OSError, ValueError, KeyError, TypeError):
            return check['name']
    checks = [('health', check) for check in config.get('targets', [])] + [('heartbeat', check) for check in config.get('heartbeats', [])]
    with ThreadPoolExecutor(max_workers=8) as executor:
        return sorted(name for name in executor.map(probe, checks) if name is not None)


def run(config, state_path):
    validate_config(config)
    failed = check_health(config, time.time())
    previous = json.loads(state_path.read_text()) if state_path.exists() else None
    if previous != failed and (failed or previous):
        try:
            status, _ = request(config['alertWebhook'], payload={'text': 'Infrastructure checks failed: ' + ', '.join(failed) if failed else 'Infrastructure checks recovered'})
            if not 200 <= status < 300:
                raise WatchdogError('Alert rejected')
        except (OSError, ValueError):
            raise WatchdogError('Alert delivery failed') from None
    if not failed and config.get('deadmanURL'):
        try:
            status, _ = request(config['deadmanURL'])
            if not 200 <= status < 300:
                raise WatchdogError('Deadman confirmation rejected')
        except (OSError, ValueError):
            raise WatchdogError('Deadman confirmation failed') from None
    state_path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    temporary = state_path.with_suffix('.tmp')
    temporary.write_text(json.dumps(failed))
    temporary.chmod(0o600)
    temporary.replace(state_path)
    print('watchdog: ' + (f'{len(failed)} failed checks' if failed else 'healthy'))
    return 1 if failed else 0


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--config', type=Path, required=True)
    parser.add_argument('--state', type=Path, required=True)
    args = parser.parse_args()
    try:
        return run(json.loads(args.config.read_text()), args.state)
    except (OSError, ValueError, KeyError, TypeError):
        print('watchdog: configuration, state, or delivery failed')
        return 2


if __name__ == '__main__':
    raise SystemExit(main())
