import base64
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'scripts/ci'))
import cache
from policy import PolicyError


class CacheConfigurationTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        (self.root / 'platform/components/cache').mkdir(parents=True)
        (self.root / 'platform/catalog').mkdir(parents=True)
        (self.root / 'platform/catalog/projects.json').write_text((ROOT / 'platform/catalog/projects.json').read_text())
        self.entry = {'url': 'https://cache.fredrir.com/portfolio', 'publicKey': 'portfolio-1:' + base64.b64encode(b'k' * 32).decode(), 'publicRead': False, 'uploadEnabled': False}
        self.document = {'schemaVersion': 1, 'enabled': True, 'caches': {'773018612': self.entry}}

    def configuration(self):
        (self.root / 'platform/components/cache/client.json').write_text(json.dumps(self.document))
        return cache.configuration(773018612, self.root)

    def test_disabled_cache_does_not_require_credentials_or_keys(self):
        self.document['enabled'] = False
        self.entry['publicKey'] = ''
        self.assertIsNone(self.configuration())

    def test_cache_must_use_reviewed_https_endpoint_and_signing_key(self):
        self.assertEqual(self.configuration()['name'], 'portfolio')
        for field, value in (('url', 'http://cache.fredrir.com/portfolio'), ('url', 'https://evil.example/portfolio'), ('publicKey', '')):
            with self.subTest(field=field, value=value):
                original = self.entry[field]
                self.entry[field] = value
                with self.assertRaises(PolicyError):
                    self.configuration()
                self.entry[field] = original

    def test_sibling_repository_cache_is_rejected(self):
        self.entry['url'] = 'https://cache.fredrir.com/y'
        with self.assertRaisesRegex(PolicyError, 'identity'):
            self.configuration()

    def test_project_signing_keys_cannot_be_shared(self):
        self.document['caches']['900286164'] = {**self.entry, 'url': 'https://cache.fredrir.com/y'}
        with self.assertRaisesRegex(PolicyError, 'distinct signing keys'):
            self.configuration()


class TokenScopeTests(unittest.TestCase):
    def token(self, caches, expires=200):
        body = {'exp': expires, 'https://jwt.attic.rs/v1': {'caches': caches}}
        return 'test.' + base64.urlsafe_b64encode(json.dumps(body).encode()).decode().rstrip('=') + '.test'

    def test_project_only_read_and_upload_claims_pass_scope_screen(self):
        cache.check_token_scope(self.token({'portfolio': {'r': 1}}), 'portfolio', now=100)
        cache.check_token_scope(self.token({'portfolio': {'r': 1, 'w': 1}}), 'portfolio', upload=True, now=100)

    def test_read_job_cannot_receive_upload_authority(self):
        with self.assertRaises(PolicyError):
            cache.check_token_scope(self.token({'portfolio': {'r': 1, 'w': 1}}), 'portfolio', now=100)

    def test_wildcard_sibling_or_administrative_authority_is_rejected(self):
        for grants in ({'*': {'w': 1}}, {'portfolio': {'w': 1}, 'y': {'r': 1}}, {'portfolio': {'w': 1, 'cc': 1}}, {'infra': {'w': 1}}):
            with self.subTest(grants=grants), self.assertRaises(PolicyError):
                cache.check_token_scope(self.token(grants), 'portfolio', upload=True, now=100)

    def test_expired_or_unknown_token_format_fails_closed(self):
        for token in ('opaque-new-format', self.token({'portfolio': {'w': 1}}, expires=90)):
            with self.subTest(token=token), self.assertRaises(PolicyError):
                cache.check_token_scope(token, 'portfolio', upload=True, now=100)

    def test_unknown_namespace_wide_authority_is_rejected(self):
        body = {'exp': 200, 'https://jwt.attic.rs/v1': {'caches': {'portfolio': {'w': 1}}, 'admin': True}}
        token = 'test.' + base64.urlsafe_b64encode(json.dumps(body).encode()).decode().rstrip('=') + '.test'
        with self.assertRaises(PolicyError):
            cache.check_token_scope(token, 'portfolio', upload=True, now=100)


class ClosureIntegrityTests(unittest.TestCase):
    def test_real_nix_closure_transfer_between_isolated_stores(self):
        executable = shutil.which('nix-store') or '/nix/var/nix/profiles/default/bin/nix-store'
        if not Path(executable).is_file():
            self.skipTest('Nix is not installed')
        with tempfile.TemporaryDirectory(prefix='infra-nar-test-') as directory:
            root = Path(directory).resolve()
            environment = {**os.environ, 'PATH': str(Path(executable).parent) + ':' + os.environ['PATH']}
            source = {**environment, 'NIX_REMOTE': f'local?root={root / "source-store"}'}
            target = {**environment, 'NIX_REMOTE': f'local?root={root / "target-store"}'}
            fixture = root / 'fixture'
            fixture.write_text('Nix closure transfer fixture\n')
            dependency = subprocess.run([executable, '--add', str(fixture)], env=source, check=True, text=True, capture_output=True).stdout.strip()
            expression = 'builtins.toFile "root" (builtins.storePath ' + json.dumps(dependency) + ')'
            path = subprocess.run([str(Path(executable).parent / 'nix'), 'eval', '--impure', '--raw', '--expr', expression], env=source, check=True, text=True, capture_output=True).stdout.strip()
            destination = root / 'artifact'
            destination.mkdir()
            manifest = cache.export_closure([path], destination, source)
            archive, metadata = cache.validated_closure(destination, manifest)
            self.assertEqual(set(metadata), {path, dependency})
            with archive.open('rb') as stream:
                subprocess.run([executable, '--import'], stdin=stream, stdout=subprocess.DEVNULL, env=target, check=True)
            self.assertEqual(cache.path_metadata(list(metadata), target), metadata)

    def test_modified_closure_export_is_rejected_before_import(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            archive = root / 'closure.export'
            archive.write_bytes(b'export fixture')
            declaration = root / 'closure.json'
            declaration.write_text(json.dumps({'schemaVersion': 1, 'paths': {'/nix/store/' + '0' * 32 + '-fixture': {}}}))
            manifest = {'closureExportSha256': cache.digest(archive), 'closureManifestSha256': cache.digest(declaration)}
            cache.validated_closure(root, manifest)
            archive.write_bytes(b'modified export')
            with self.assertRaisesRegex(PolicyError, 'digest mismatch'):
                cache.validated_closure(root, manifest)

    def test_closure_references_must_be_complete(self):
        path = '/nix/store/' + '0' * 32 + '-fixture'
        metadata = {path: {'narHash': 'sha256-placeholder', 'narSize': 1, 'references': ['/nix/store/' + '1' * 32 + '-missing']}}
        with patch.object(cache, 'capture', return_value=json.dumps(metadata)), self.assertRaisesRegex(PolicyError, 'incomplete'):
            cache.path_metadata([path])
