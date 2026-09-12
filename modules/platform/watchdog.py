import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime
from email.headerregistry import Address
from email.errors import HeaderParseError
from email.message import EmailMessage
from email.policy import SMTP
from email.utils import formatdate
import json
import os
from pathlib import Path
import re
import smtplib
import ssl
import stat
import time
import urllib.error
import urllib.parse
import urllib.request


class WatchdogError(ValueError):
    pass


def smtp_hostname(value):
    if not isinstance(value, str) or not 1 <= len(value) <= 253 or any(not re.fullmatch(r'[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?', label) for label in value.split('.')):
        raise WatchdogError('SMTP hostname required')
    return value


def mailbox(value):
    if not isinstance(value, str) or not 3 <= len(value) <= 254 or any(ord(character) < 33 or ord(character) > 126 for character in value):
        raise WatchdogError('Bare ASCII mailbox required')
    try:
        address = Address(addr_spec=value)
    except (ValueError, HeaderParseError):
        raise WatchdogError('Bare ASCII mailbox required') from None
    if not address.username or len(address.username) > 64 or address.addr_spec != value:
        raise WatchdogError('Bare ASCII mailbox required')
    smtp_hostname(address.domain)
    return value


def validate_email(config):
    keys = {'host', 'port', 'tls', 'username', 'password', 'from', 'to'}
    if not isinstance(config, dict) or set(config) != keys:
        raise WatchdogError('Exact SMTP configuration required')
    smtp_hostname(config['host'])
    if type(config['port']) is not int or not 1 <= config['port'] <= 65535 or config['tls'] not in ('implicit', 'starttls'):
        raise WatchdogError('Explicit verified SMTP TLS required')
    for key, limit in [('username', 256), ('password', 1024)]:
        value = config[key]
        if not isinstance(value, str) or not 1 <= len(value) <= limit or not value.strip() or any(ord(character) < 32 or ord(character) > 126 for character in value):
            raise WatchdogError('Bounded SMTP credential required')
    mailbox(config['from'])
    mailbox(config['to'])


def send_email(config, text, failed):
    validate_email(config)
    message = EmailMessage(policy=SMTP)
    message['From'], message['To'] = config['from'], config['to']
    message['Subject'] = 'Infrastructure checks failed' if failed else 'Infrastructure checks recovered'
    message['Date'] = formatdate(usegmt=True)
    message.set_content(text)
    if len(message.as_bytes()) > 16384:
        raise WatchdogError('Alert email exceeds limit')
    context = ssl.create_default_context()
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    if config['tls'] == 'implicit':
        client = smtplib.SMTP_SSL(config['host'], config['port'], timeout=5, context=context)
    else:
        client = smtplib.SMTP(config['host'], config['port'], timeout=5)
    with client:
        if client.ehlo()[0] != 250:
            raise WatchdogError('SMTP greeting rejected')
        if config['tls'] == 'starttls':
            client.starttls(context=context)
            if client.ehlo()[0] != 250:
                raise WatchdogError('SMTP TLS greeting rejected')
        client.login(config['username'], config['password'])
        if client.send_message(message, from_addr=config['from'], to_addrs=[config['to']]):
            raise WatchdogError('Email recipient rejected')


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
    if not isinstance(config, dict) or set(config) - {'targets', 'heartbeats', 'alertWebhook', 'alertEmail', 'deadmanURL'}:
        raise WatchdogError('Unknown configuration key')
    if sum(key in config for key in ('alertWebhook', 'alertEmail')) != 1:
        raise WatchdogError('Exactly one alert credential transport required')
    if 'alertEmail' in config:
        validate_email(config['alertEmail'])
    elif config.get('alertWebhook'):
        https_url(config['alertWebhook'])
    else:
        raise WatchdogError('Alert credential required')
    if config.get('deadmanURL'):
        https_url(config['deadmanURL'])
    checks = config.get('targets', []) + config.get('heartbeats', [])
    if not checks or len(checks) > 32:
        raise WatchdogError('Between one and 32 checks required')
    names = set()
    for check in checks:
        if set(check) - {'name', 'url', 'expectedStatus', 'timestampField', 'maxAgeSeconds', 'authorization'}:
            raise WatchdogError('Unknown check key')
        if not isinstance(check.get('name'), str) or not check['name'] or len(check['name']) > 80 or check['name'] in names or any(ord(character) < 32 for character in check['name']):
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
            text = 'Infrastructure checks failed: ' + ', '.join(failed) if failed else 'Infrastructure checks recovered'
            if 'alertEmail' in config:
                send_email(config['alertEmail'], text, bool(failed))
            else:
                status, _ = request(config['alertWebhook'], payload={'text': text})
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


def read_config(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_uid not in (0, os.geteuid()) or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) not in (0o400, 0o600) or not 0 < info.st_size <= 65536:
            raise WatchdogError('Private bounded credential file required')
        with os.fdopen(fd, 'rb', closefd=False) as stream:
            data = stream.read(65537)
        if len(data) > 65536:
            raise WatchdogError('Credential file exceeds limit')
        return json.loads(data)
    finally:
        os.close(fd)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--config', type=Path, required=True)
    parser.add_argument('--state', type=Path, required=True)
    args = parser.parse_args()
    try:
        return run(read_config(args.config), args.state)
    except (OSError, ValueError, KeyError, TypeError):
        print('watchdog: configuration, state, or delivery failed')
        return 2


if __name__ == '__main__':
    raise SystemExit(main())
