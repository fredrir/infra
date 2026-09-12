import hashlib
import gzip
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import stat
import tarfile
import tempfile
import types
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("evacuation_images", ROOT / "scripts/operations/evacuation_images.py")
images = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(images)


def encoded(document):
    return json.dumps(document).encode()


def digest(content):
    return "sha256:" + hashlib.sha256(content).hexdigest()


def descriptor(content):
    return {"digest": digest(content), "size": len(content)}


def fixture_archive(path, *, architecture="amd64", bad_layer=False, extra=None, link=False, compressed=False, bad_diff_id=False):
    contents = b"fixture layer payload"
    layer = gzip.compress(contents) if compressed else contents
    config = encoded({"architecture": architecture, "os": "linux", "rootfs": {"type": "layers", "diff_ids": [digest(b"wrong") if bad_diff_id else digest(contents)]}})
    layer_descriptor = descriptor(layer) | {"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip" if compressed else "application/vnd.oci.image.layer.v1.tar"}
    manifest = encoded({"schemaVersion": 2, "config": descriptor(config), "layers": [layer_descriptor]})
    index = encoded({"schemaVersion": 2, "manifests": [descriptor(manifest)]})
    members = {"oci-layout": encoded({"imageLayoutVersion": "1.0.0"}), "index.json": index}
    for content in (layer, config, manifest):
        members["blobs/sha256/" + digest(content).removeprefix("sha256:")] = content
    if bad_layer:
        members["blobs/sha256/" + digest(layer).removeprefix("sha256:")] = b"tampered layer payload"
    if extra:
        members[extra] = b"unexpected"
    with tarfile.open(path, "w") as archive:
        for name, contents in members.items():
            member = tarfile.TarInfo(name)
            member.size = len(contents)
            archive.addfile(member, io.BytesIO(contents))
        if link:
            member = tarfile.TarInfo("linked")
            member.type, member.linkname = tarfile.SYMTYPE, "/etc/passwd"
            archive.addfile(member)
    path.chmod(0o600)
    return digest(config)


def fixture_plan():
    return {"source": {"id": "fredrir-05", "hostname": "llunde-01", "providerId": 132168416}, "target": {"id": "fredrir-09", "hostname": "cloud-server-10643982", "architecture": "amd64"}, "authorization": {"targetInstall": True, "sourceStop": False, "cutover": False}, "services": {"caddy": {"user": "edge", "image": "ghcr.io/fredrir/llunde-caddy@sha256:" + "a" * 64}}, "users": {"edge": {"uid": 2000, "gid": 2000}}}


def fixture_docker(path, *, link_target=None, altered_layer=False):
    layer = b"raw layer fixture"
    name = digest(layer).removeprefix('sha256:') + '.tar'
    config = encoded({'architecture': 'amd64', 'os': 'linux', 'rootfs': {'type': 'layers', 'diff_ids': [digest(layer)]}})
    config_name = digest(config).removeprefix('sha256:') + '.json'
    manifest = encoded([{'Config': config_name, 'RepoTags': None, 'Layers': [name]}])
    with tarfile.open(path, 'w') as archive:
        for filename, content in [(name, b'tampered' if altered_layer else layer), (config_name, config), ('manifest.json', manifest)]:
            member = tarfile.TarInfo(filename)
            member.size = len(content)
            archive.addfile(member, io.BytesIO(content))
        if link_target is not None:
            member = tarfile.TarInfo('a' * 64 + '/layer.tar')
            member.type = tarfile.SYMTYPE
            member.linkname = '../' + name if link_target == 'valid' else link_target
            archive.addfile(member)
    path.chmod(0o600)
    return digest(config)


class ImageArchiveTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.archive = self.root / "image.oci.tar"
        self.plan = fixture_plan()

    def bundle(self):
        config = fixture_archive(self.archive)
        manifest = {"schemaVersion": 1, "kind": "standalone-image-archive", "service": "caddy", "user": "edge", "source": "fredrir-05", "target": "fredrir-09", "sourceImage": self.plan["services"]["caddy"]["image"], "configID": config, "archiveBytes": self.archive.stat().st_size, "archiveSHA256": hashlib.sha256(self.archive.read_bytes()).hexdigest()}
        images.write_json(self.root / "manifest.json", manifest)
        return manifest

    def test_complete_archive_verifies_exact_configuration_and_all_layers(self):
        manifest = self.bundle()
        self.assertEqual(images.verify_bundle(self.root, self.plan, "caddy"), manifest)
        self.assertEqual(set(self.root.iterdir()), {self.archive, self.root / "manifest.json"})

    def test_modified_layer_wrong_architecture_and_config_are_rejected(self):
        for options in ({"bad_layer": True}, {"architecture": "arm64"}, {}):
            with self.subTest(options=options):
                config = fixture_archive(self.archive, **options)
                expected = config if options else "sha256:" + "0" * 64
                with self.archive.open("rb") as stream, self.assertRaises(images.ImageError):
                    images.verify_oci(stream, expected)

    def test_gzip_diff_id_and_decompression_budget_are_verified(self):
        config = fixture_archive(self.archive, compressed=True)
        with self.archive.open("rb") as stream:
            self.assertEqual(images.verify_oci(stream, config), hashlib.sha256(self.archive.read_bytes()).hexdigest())
        config = fixture_archive(self.archive, compressed=True, bad_diff_id=True)
        with self.archive.open("rb") as stream, self.assertRaises(images.ImageError):
            images.verify_oci(stream, config)
        config = fixture_archive(self.archive, compressed=True)
        with patch.object(images, "MAX_LAYER_BYTES", 1), self.archive.open("rb") as stream, self.assertRaises(images.ImageError):
            images.verify_oci(stream, config)

    def test_path_traversal_links_and_unreferenced_blobs_are_rejected(self):
        for options in ({"extra": "../../escape"}, {"link": True}, {"extra": "blobs/sha256/" + hashlib.sha256(b"unexpected").hexdigest()}):
            with self.subTest(options=options):
                config = fixture_archive(self.archive, **options)
                with self.archive.open("rb") as stream, self.assertRaises(images.ImageError):
                    images.verify_oci(stream, config)
        self.assertFalse((self.root.parent / "escape").exists())

    def test_bundle_checksum_and_service_scope_are_enforced(self):
        self.bundle()
        self.archive.write_bytes(self.archive.read_bytes() + b"trailing bytes")
        with self.assertRaises(images.ImageError):
            images.verify_bundle(self.root, self.plan, "caddy")
        self.plan["authorization"]["cutover"] = True
        with self.assertRaises(images.ImageError):
            images.validate_plan(self.plan, "caddy")

    def test_private_archive_links_and_world_readable_files_are_refused(self):
        self.bundle()
        self.archive.chmod(0o644)
        with self.assertRaises(images.ImageError):
            images.verify_bundle(self.root, self.plan, "caddy")
        self.archive.chmod(0o600)
        os.link(self.archive, self.root / "hardlink")
        with self.assertRaises(images.ImageError):
            images.open_private(self.archive, os.geteuid(), images.MAX_ARCHIVE_BYTES)

    def test_export_reuses_only_current_matching_archive(self):
        manifest = self.bundle()
        bundle = self.root / "images/caddy"
        bundle.mkdir(parents=True, mode=0o700)
        bundle.parent.chmod(0o700)
        self.archive.rename(bundle / self.archive.name)
        (self.root / "manifest.json").rename(bundle / "manifest.json")
        images.write_json(self.root / "staging.json", self.plan)
        with patch.object(images, "source_identity", return_value=manifest["configID"]):
            self.assertFalse(images.export_image(self.root, "caddy")["changed"])
        with patch.object(images, "source_identity", return_value="sha256:" + "0" * 64), self.assertRaises(images.ImageError):
            images.export_image(self.root, "caddy")

    def load_context(self, *, present=1, containers=b"", markers=False):
        manifest = self.bundle()
        calls = []

        def command(argv, **kwargs):
            calls.append((argv, kwargs))
            if "ps" in argv:
                return containers
            if "inspect" in argv:
                return encoded({"id": manifest["configID"], "architecture": "amd64", "os": "linux"})
            if "load" in argv:
                self.assertEqual(kwargs["input"].read(), self.archive.read_bytes())
                return b"Loaded fixture"
            self.fail("Unexpected image-store command")

        original_stat = images.Path.stat
        patches = [patch.object(images.os, "geteuid", return_value=0), patch.object(images.socket, "gethostname", return_value="cloud-server-10643982"), patch.object(images, "read_json", return_value=self.plan), patch.object(images, "verify_bundle", return_value=manifest), patch.object(images, "open_private", side_effect=lambda *args: self.archive.open("rb")), patch.object(images, "private_directory", side_effect=lambda path, *args: Path(path)), patch.object(images.Path, "stat", autospec=True, side_effect=lambda path, *args, **kwargs: types.SimpleNamespace(st_mode=stat.S_IFSOCK | 0o600, st_uid=2000) if str(path) == '/run/user/2000/bus' else original_stat(path, *args, **kwargs)), patch.object(images, "child_environment", return_value={}), patch.object(images.pwd, "getpwnam", return_value=types.SimpleNamespace(pw_uid=2000, pw_gid=2000, pw_dir="/home/edge")), patch.object(images, "checked_run", side_effect=command), patch.object(images.subprocess, "run", return_value=subprocess.CompletedProcess([], present)), patch.object(images.Path, "exists", return_value=markers), patch.object(images.Path, "is_symlink", return_value=False)]
        return calls, patches

    def test_load_only_imports_missing_image_and_never_starts_container(self):
        calls, patches = self.load_context()
        for context in patches:
            context.start()
            self.addCleanup(context.stop)
        result = images.load_image(self.root, "/var/lib/infra-evacuation/llunde/staging.json", "caddy")
        self.assertTrue(result["changed"])
        self.assertFalse(result["applicationStarted"])
        self.assertEqual([next(word for word in ("ps", "load", "inspect") if word in argv) for argv, _ in calls], ["ps", "load", "inspect"])
        self.assertTrue(all("edge" in argv and "DOPPLER_TOKEN" not in str(argv) for argv, _ in calls))

    def test_load_existing_image_is_idempotent(self):
        calls, patches = self.load_context(present=0)
        for context in patches:
            context.start()
            self.addCleanup(context.stop)
        result = images.load_image(self.root, "/var/lib/infra-evacuation/llunde/staging.json", "caddy")
        self.assertFalse(result["changed"])
        self.assertFalse(any("load" in argv for argv, _ in calls))

    def test_activated_checkpoint_refuses_image_store_commands(self):
        calls, patches = self.load_context(markers=True)
        for context in patches:
            context.start()
            self.addCleanup(context.stop)
        with self.assertRaises(images.ImageError):
            images.load_image(self.root, "/var/lib/infra-evacuation/llunde/staging.json", "caddy")
        self.assertEqual(calls, [])

    def test_existing_container_refuses_image_load(self):
        calls, patches = self.load_context(containers=b"unreviewed-container\n")
        for context in patches:
            context.start()
            self.addCleanup(context.stop)
        with self.assertRaises(images.ImageError):
            images.load_image(self.root, "/var/lib/infra-evacuation/llunde/staging.json", "caddy")
        self.assertFalse(any("load" in argv for argv, _ in calls))

    def test_source_transport_refuses_forwarding_and_unverified_host(self):
        argv = images.source_command("edge", ["save", "--uncompressed", "sha256:" + "a" * 64])
        for option in ("StrictHostKeyChecking=yes", "ForwardAgent=no", "ClearAllForwardings=yes"):
            self.assertIn(option, argv)
        self.assertIn("timeout --signal=TERM --kill-after=5s 210s", argv[-1])
        with patch.object(images, "checked_run", return_value=b"wrong-host\n"), self.assertRaises(images.ImageError):
            images.source_identity("caddy", self.plan['services']['caddy'])

    def test_child_environment_omits_unrelated_credentials(self):
        with patch.dict(os.environ, {'HOME': '/fixture/home', 'AWS_SECRET_ACCESS_KEY': 'fixture', 'DOPPLER_TOKEN': 'fixture'}, clear=True):
            self.assertEqual(set(images.child_environment()), {'PATH', 'HOME', 'LANG', 'LC_ALL'})

    def test_docker_normalization_preserves_exact_config_and_removes_legacy_links(self):
        config = fixture_docker(self.archive, link_target='valid')
        destination = self.root / 'normalized.tar'
        images.normalize_docker(self.archive, destination, config, os.geteuid())
        with destination.open('rb') as stream:
            self.assertEqual(images.verify_docker(stream, config), hashlib.sha256(destination.read_bytes()).hexdigest())
        with tarfile.open(destination) as archive:
            self.assertTrue(all(member.isfile() for member in archive))
            self.assertEqual(len(archive.getmembers()), 3)

    def test_docker_normalization_rejects_escaped_link_or_wrong_layer_bytes(self):
        for options in ({'link_target': '/etc/passwd'}, {'altered_layer': True}):
            with self.subTest(options=options):
                config = fixture_docker(self.archive, **options)
                destination = self.root / 'normalized.tar'
                with self.assertRaises(images.ImageError):
                    images.normalize_docker(self.archive, destination, config, os.geteuid())
                self.assertFalse(destination.exists())

    def test_docker_load_verifier_rejects_legacy_links_and_wrong_config_id(self):
        config = fixture_docker(self.archive, link_target='valid')
        with self.archive.open('rb') as stream, self.assertRaises(images.ImageError):
            images.verify_docker(stream, config)
        fixture_docker(self.archive)
        with self.archive.open('rb') as stream, self.assertRaises(images.ImageError):
            images.verify_docker(stream, 'sha256:' + '0' * 64)

    def translation_fixture(self):
        spec = importlib.util.spec_from_file_location('staging_fixture', ROOT / 'tests/operations/test_evacuation_staging.py')
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        plan = module.fixture_plan(self.root)
        plan['source']['hostname'] = 'llunde-01'
        plan['target']['hostname'] = 'cloud-server-10643982'
        for path in (self.root / 'units').rglob('*'):
            if path.is_file():
                if path.suffix == '.container':
                    with path.open('a') as stream:
                        stream.write('Environment=REGISTRY_AUTH_FILE=/run/infra-evacuation/llunde/' + images.SERVICE_USERS[path.stem] + '/registry.json\n')
                    plan['unitSHA256'][str(path.relative_to(self.root))] = hashlib.sha256(path.read_bytes()).hexdigest()
                path.chmod(0o600)
        (self.root / 'staging.json').write_text(json.dumps(plan))
        (self.root / 'staging.json').chmod(0o600)
        for service, item in plan['services'].items():
            directory = self.root / 'images' / service
            directory.mkdir(parents=True, mode=0o700)
            archive = directory / 'image.oci.tar'
            config = fixture_archive(archive)
            images.write_json(directory / 'manifest.json', {'schemaVersion': 1, 'kind': 'standalone-image-archive', 'service': service, 'user': item['user'], 'source': 'fredrir-05', 'target': 'fredrir-09', 'sourceImage': item['image'], 'configID': config, 'archiveBytes': archive.stat().st_size, 'archiveSHA256': hashlib.sha256(archive.read_bytes()).hexdigest()})
        return module.staging

    def test_translation_requires_all_receipts_and_keeps_source_candidates_unchanged(self):
        staging = self.translation_fixture()
        before = {p: p.read_bytes() for p in (self.root / 'units').rglob('*') if p.is_file()}
        destination = self.root / 'translated'
        result = images.translate_candidates(self.root, destination)
        self.assertEqual(result['translatedServices'], 6)
        self.assertEqual(before, {p: p.read_bytes() for p in before})
        compiled = staging.validate_plan(destination)
        for service, item in compiled['services'].items():
            text = (destination / 'units' / item['user'] / f'{service}.container').read_text()
            self.assertIn('Image=' + item['runtimeImage'] + '\nPull=never\n', text)
            self.assertNotIn('REGISTRY_AUTH_FILE', text)
        with self.assertRaises(images.ImageError):
            images.translate_candidates(self.root, destination)

    def test_missing_image_receipt_prevents_any_translated_output(self):
        self.translation_fixture()
        (self.root / 'images/caddy/manifest.json').unlink()
        destination = self.root / 'translated'
        with self.assertRaises(OSError):
            images.translate_candidates(self.root, destination)
        self.assertFalse(destination.exists())


if __name__ == "__main__":
    unittest.main()
