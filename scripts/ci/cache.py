#!/usr/bin/env python3
import base64
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
from urllib.parse import urlsplit

from policy import ROOT, PolicyError, load_catalog


STORE_PATH = re.compile(r'/nix/store/[0-9a-df-np-sv-z]{32}-[A-Za-z0-9+._?=-]+\Z')


def configuration(repository_id, root=ROOT):
    document = json.loads((root / 'platform/components/cache/client.json').read_text())
    if document.get('schemaVersion') != 1 or type(document.get('enabled')) is not bool:
        raise PolicyError('invalid cache client contract')
    if not document['enabled']:
        return None
    entry = document['caches'].get(str(repository_id))
    if entry is None:
        raise PolicyError('repository has no approved cache')
    url = urlsplit(entry['url'])
    approved_names = {name for name, project in load_catalog(root)['projects'].items() if str(project['repositoryId']) == str(repository_id)}
    if url.path.removeprefix('/') not in approved_names:
        raise PolicyError('cache identity does not belong to this repository')
    if url.scheme != 'https' or url.netloc != 'cache.fredrir.com' or not re.fullmatch(r'/[a-z][a-z0-9-]{0,39}', url.path) or url.query or url.fragment:
        raise PolicyError('unapproved cache endpoint')
    try:
        name, key = entry['publicKey'].split(':', 1)
        if not re.fullmatch(r'[A-Za-z0-9._-]+', name) or len(base64.b64decode(key, validate=True)) != 32:
            raise ValueError()
    except (ValueError, KeyError):
        raise PolicyError('reviewed Ed25519 cache public key required') from None
    if any(other_id != str(repository_id) and other.get('publicKey') == entry['publicKey'] for other_id, other in document['caches'].items()):
        raise PolicyError('project caches must have distinct signing keys')
    if type(entry.get('publicRead')) is not bool or type(entry.get('uploadEnabled')) is not bool:
        raise PolicyError('explicit cache read and upload policy required')
    return {**entry, 'name': url.path[1:], 'endpoint': f'https://{url.netloc}'}


def check_token_scope(token, name, upload=False, now=None):
    try:
        if len(token) > 16384 or any(c.isspace() for c in token):
            raise ValueError()
        parts = token.split('.')
        if len(parts) != 3:
            raise ValueError()
        claim = json.loads(base64.urlsafe_b64decode(parts[1] + '=' * (-len(parts[1]) % 4)))
        namespace = claim['https://jwt.attic.rs/v1']
        if not isinstance(namespace, dict) or set(namespace) != {'caches'}:
            raise ValueError()
        caches = namespace['caches']
        rights = caches[name]
        allowed = {'r', 'w'} if upload else {'r'}
        required = 'w' if upload else 'r'
        if set(caches) != {name} or not set(rights) <= allowed or rights.get(required) != 1:
            raise ValueError()
        if any(type(value) not in (int, bool) or value not in (0, 1) for value in rights.values()):
            raise ValueError()
        if not isinstance(claim['exp'], int) or claim['exp'] <= (time.time() if now is None else now):
            raise ValueError()
    except (ValueError, KeyError, TypeError):
        raise PolicyError('cache token must have unexpired rights for this project cache only; server verification is still required') from None


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def capture(arguments, **kwargs):
    return subprocess.run(arguments, check=True, text=True, capture_output=True, **kwargs).stdout.strip()


@contextmanager
def read_environment(repository_id):
    entry = configuration(repository_id)
    environment = dict(os.environ)
    token = environment.pop('NIX_CACHE_READ_TOKEN', '')
    environment.pop('NIX_CACHE_UPLOAD_TOKEN', None)
    with tempfile.TemporaryDirectory(prefix='infra-cache-read-') as work:
        if entry:
            settings = [environment.get('NIX_CONFIG', ''), f"extra-substituters = {entry['url']}", f"extra-trusted-public-keys = {entry['publicKey']}", 'require-sigs = true', 'sandbox = true', 'sandbox-fallback = false']
            if not entry['publicRead']:
                check_token_scope(token, entry['name'])
                netrc = Path(work) / 'netrc'
                netrc.write_text(f'machine cache.fredrir.com password {token}\n')
                netrc.chmod(0o600)
                settings.append(f'netrc-file = {netrc}')
            environment['NIX_CONFIG'] = '\n'.join(settings)
            actual = json.loads(capture(['nix', 'config', 'show', '--json'], env=environment))
            if entry['url'] not in actual['substituters']['value'] or entry['publicKey'] not in actual['trusted-public-keys']['value'] or actual['require-sigs']['value'] is not True:
                raise PolicyError('effective Nix configuration does not enforce the approved cache key')
        yield environment


def path_metadata(paths, environment=None):
    information = json.loads(capture(['nix', 'path-info', '--recursive', '--json', *paths], env=environment))
    if isinstance(information, list):
        information = {item['path']: item for item in information}
    result = {}
    for path, item in information.items():
        if not STORE_PATH.fullmatch(path) or not isinstance(item.get('narSize'), int) or item['narSize'] <= 0 or not isinstance(item.get('narHash'), str):
            raise PolicyError('invalid exported Nix path metadata')
        result[path] = {key: item[key] for key in ('narHash', 'narSize', 'references')}
    if not result or len(result) > 10000 or sum(item['narSize'] for item in result.values()) > 8 * 1024 ** 3:
        raise PolicyError('Nix closure exceeds the pilot artifact budget')
    if any(not set(item['references']) <= set(result) for item in result.values()):
        raise PolicyError('exported Nix closure is incomplete')
    return result


def export_closure(paths, destination, environment):
    metadata = path_metadata(paths, environment)
    archive = destination / 'closure.export'
    with archive.open('wb') as stream:
        subprocess.run(['nix-store', '--export', *sorted(metadata)], check=True, stdout=stream, env=environment)
    declaration = destination / 'closure.json'
    declaration.write_text(json.dumps({'schemaVersion': 1, 'paths': metadata}, sort_keys=True) + '\n')
    return {'closureExportSha256': digest(archive), 'closureManifestSha256': digest(declaration)}


def validated_closure(directory, manifest):
    archive, declaration = directory / 'closure.export', directory / 'closure.json'
    for path, field in ((archive, 'closureExportSha256'), (declaration, 'closureManifestSha256')):
        if path.is_symlink() or not path.is_file() or digest(path) != manifest.get(field):
            raise PolicyError('Nix closure artifact digest mismatch')
    if archive.stat().st_size > 9 * 1024 ** 3 or declaration.stat().st_size > 4 * 1024 ** 2:
        raise PolicyError('Nix closure artifact exceeds the pilot budget')
    document = json.loads(declaration.read_text())
    metadata = document.get('paths', {})
    if document.get('schemaVersion') != 1 or not metadata or len(metadata) > 10000 or any(not STORE_PATH.fullmatch(path) for path in metadata):
        raise PolicyError('invalid Nix closure declaration')
    return archive, metadata


def upload(directory, manifest, repository_id):
    entry = configuration(repository_id)
    if not entry or not entry['uploadEnabled']:
        raise PolicyError('cache uploads are disabled')
    token = os.environ.get('NIX_CACHE_UPLOAD_TOKEN', '')
    check_token_scope(token, entry['name'], upload=True)
    archive, metadata = validated_closure(directory, manifest)
    environment = dict(os.environ)
    environment.pop('NIX_CACHE_UPLOAD_TOKEN', None)
    environment.pop('NIX_CACHE_READ_TOKEN', None)
    client = capture(['nix', 'build', '--inputs-from', str(ROOT), 'nixpkgs#attic-client', '--no-update-lock-file', '--no-link', '--print-out-paths'], env=environment)
    if not STORE_PATH.fullmatch(client):
        raise PolicyError('locked Attic client did not resolve to one Nix store path')
    with archive.open('rb') as stream:
        subprocess.run(['nix-store', '--import'], stdin=stream, stdout=subprocess.DEVNULL, check=True, env=environment)
    if path_metadata(sorted(metadata), environment) != metadata:
        raise PolicyError('imported Nix paths differ from tested build metadata')
    with tempfile.TemporaryDirectory(prefix='infra-cache-upload-') as work:
        config_directory = Path(work) / 'attic'
        config_directory.mkdir(mode=0o700)
        config = config_directory / 'config.toml'
        config.write_text(f'[servers.ci]\nendpoint = {json.dumps(entry["endpoint"])}\ntoken = {json.dumps(token)}\n')
        config.chmod(0o600)
        environment['XDG_CONFIG_HOME'] = work
        subprocess.run([f'{client}/bin/attic', 'push', f'ci:{entry["name"]}', '--stdin', '--no-closure', '--jobs', '1'], input='\n'.join(sorted(metadata)) + '\n', text=True, check=True, env=environment)
