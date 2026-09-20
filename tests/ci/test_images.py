import hashlib
import json
import os
import re
import subprocess
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
TAG_IMAGE = ROOT / 'scripts/ci/tag-image.sh'
INDEX = json.dumps({'schemaVersion': 2, 'mediaType': 'application/vnd.oci.image.index.v1+json', 'manifests': []}).encode()
DIGEST = 'sha256:' + hashlib.sha256(INDEX).hexdigest()


class Registry:
    def __init__(self, manifests=None, status=None):
        self.manifests = dict(manifests or {})
        self.status = status
        self.requests = []
        registry = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, format, *args):
                pass

            def reply(self, status, body=b''):
                self.send_response(status)
                self.send_header('Content-Length', str(len(body)))
                self.end_headers()
                if self.command != 'HEAD':
                    self.wfile.write(body)

            def handle_request(self):
                body = self.rfile.read(int(self.headers.get('Content-Length', 0)))
                registry.requests.append((self.command, self.path, dict(self.headers), body))
                if registry.status:
                    return self.reply(registry.status)
                if self.path.startswith('/token?'):
                    return self.reply(200, json.dumps({'token': 'registry-token'}).encode())
                if self.headers.get('Authorization') != 'Bearer registry-token':
                    return self.reply(401)
                if self.command == 'PUT':
                    registry.manifests[self.path] = body
                    return self.reply(201)
                manifest = registry.manifests.get(self.path)
                return self.reply(200, manifest) if manifest is not None else self.reply(404)

            do_GET = do_HEAD = do_PUT = handle_request

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.url = f'http://127.0.0.1:{self.server.server_address[1]}'

    def close(self):
        self.server.shutdown()
        self.server.server_close()


class TagImageTests(unittest.TestCase):
    def tag(self, registry, **changes):
        environment = {'IMAGE': f'ghcr.io/fredrir/example@{DIGEST}', 'TAG': 'inputs-' + 'a' * 64, 'GITHUB_ACTOR': 'fredrir',
                       'REGISTRY_TOKEN': 'workflow-token', 'REGISTRY_URL': registry.url} | changes
        return subprocess.run(['bash', str(TAG_IMAGE)], env=os.environ | environment, capture_output=True, text=True, check=False)

    def registry(self, **arguments):
        registry = Registry(**arguments)
        self.addCleanup(registry.close)
        return registry

    def test_the_published_manifest_gains_the_tag_unchanged(self):
        registry = self.registry(manifests={f'/v2/fredrir/example/manifests/{DIGEST}': INDEX})
        result = self.tag(registry)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(registry.manifests['/v2/fredrir/example/manifests/inputs-' + 'a' * 64], INDEX)
        method, _, headers, _ = registry.requests[-1]
        self.assertEqual((method, headers['Content-Type']), ('PUT', 'application/vnd.oci.image.index.v1+json'))
        self.assertIn('scope=repository:fredrir/example:pull,push', registry.requests[0][1])
        self.assertNotIn('workflow-token', result.stdout + result.stderr)

    def test_a_manifest_that_does_not_match_its_digest_is_never_tagged(self):
        registry = self.registry(manifests={f'/v2/fredrir/example/manifests/{DIGEST}': INDEX + b' '})
        self.assertNotEqual(self.tag(registry).returncode, 0)
        self.assertFalse([request for request in registry.requests if request[0] == 'PUT'])

    def test_foreign_images_mutable_references_and_malformed_tags_refuse(self):
        registry = self.registry(manifests={f'/v2/fredrir/example/manifests/{DIGEST}': INDEX})
        for change in [{'IMAGE': f'ghcr.io/other/example@{DIGEST}'}, {'IMAGE': 'ghcr.io/fredrir/example:latest'},
                       {'TAG': '../escape'}, {'TAG': ''}, {'REGISTRY_TOKEN': ''}]:
            with self.subTest(change=change):
                self.assertNotEqual(self.tag(registry, **change).returncode, 0)
        self.assertFalse(registry.requests)


class CatalogTests(unittest.TestCase):
    def test_every_build_input_is_declared_and_triggers_the_workflow(self):
        workflow = yaml.safe_load((ROOT / '.github/workflows/images.yml').read_text())
        triggers = workflow[True]['push']['paths']
        catalog = yaml.safe_load((ROOT / 'images/catalog.yaml').read_text())
        self.assertEqual(len({entry['image'] for entry in catalog}), len(catalog))
        for entry in catalog:
            with self.subTest(image=entry['image']):
                covered = lambda path: any(path == declared or path.startswith(declared + '/') for declared in entry['inputs'])
                self.assertTrue(covered(entry['dockerfile']))
                for declared in entry['inputs']:
                    self.assertTrue((ROOT / declared).exists(), declared)
                    self.assertIn(declared + '/**' if (ROOT / declared).is_dir() else declared, triggers)
                recipe = (ROOT / entry['dockerfile']).read_text().replace('\\\n', ' ')
                copied = [source for line in re.findall(r'^(?:COPY|ADD)\s+(?!.*--from=)(.+)$', recipe, re.MULTILINE)
                          for source in [word for word in line.split() if not word.startswith('--')][:-1]]
                mounted = [mount.group(1) for mount in re.finditer(r'--mount=type=bind,source=([^,\s]+)', recipe)]
                for source in copied + mounted:
                    self.assertTrue(covered(source), source)


if __name__ == '__main__':
    unittest.main()
