import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("platform_recovery", ROOT / "scripts/operations/recovery.py")
recovery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(recovery)


class EtcdBackupContract(unittest.TestCase):
    def test_host_archive_is_verifiable_and_failed_upload_has_no_success_metric(self):
        for failure in [False, True]:
            with self.subTest(upload_failure=failure), tempfile.TemporaryDirectory() as directory:
                temporary = Path(directory).resolve()
                binaries = temporary / "bin"
                binaries.mkdir()
                mocks = {
                    "id": "print(0)\n",
                    "k3s": """import os, sys
from pathlib import Path
if sys.argv[1:] == ['--version']:
    print('k3s version v1.36.3+k3s1 (fixture)')
else:
    location = Path(sys.argv[sys.argv.index('--data-dir') + 1]) / 'server/db/snapshots'
    location.mkdir(parents=True, exist_ok=True)
    (location / (sys.argv[sys.argv.index('--name') + 1] + '.snapshot')).write_bytes(b'fixture snapshot')
""",
                    "sha256sum": "import hashlib, sys\nfrom pathlib import Path\nprint(hashlib.sha256(Path(sys.argv[1]).read_bytes()).hexdigest(), sys.argv[1])\n",
                    "restic": "import os, sys\nfrom pathlib import Path\nPath(os.environ['ARCHIVE']).write_bytes(sys.stdin.buffer.read())\nraise SystemExit(int(os.environ['UPLOAD_FAILURE']))\n",
                }
                for name, body in mocks.items():
                    executable = binaries / name
                    executable.write_text(f"#!{sys.executable}\n{body}")
                    executable.chmod(0o700)
                data = temporary / "k3s"
                (data / "server").mkdir(parents=True)
                (data / "server/token").write_bytes(b"fixture agent-independent server token")
                credentials = temporary / "credentials"
                credentials.write_bytes(b"fixture credential")
                metrics = temporary / "metrics"
                archive = temporary / "platform-etcd.tar"
                environment = os.environ | {
                    "PATH": str(binaries) + os.pathsep + os.environ["PATH"],
                    "PLATFORM_K3S_DATA_DIR": str(data), "PLATFORM_METRICS_DIR": str(metrics),
                    "RESTIC_REPOSITORY_FILE": str(credentials), "RESTIC_PASSWORD_FILE": str(credentials),
                    "ARCHIVE": str(archive), "UPLOAD_FAILURE": str(int(failure)),
                    "COPYFILE_DISABLE": "1",
                }
                result = subprocess.run(["bash", str(ROOT / "modules/platform/etcd-backup.sh")], env=environment, capture_output=True, text=True)
                self.assertEqual(result.returncode, int(failure), result.stderr)
                self.assertEqual((metrics / "k3s-etcd-success.prom").exists(), not failure)
                restored = temporary / "restored"
                restored.mkdir(mode=0o700)
                with tarfile.open(archive) as bundle:
                    files = [member for member in bundle if member.isfile()]
                    self.assertEqual({Path(member.name).name for member in files}, {"snapshot", "server-token", "manifest.json"})
                    for member in files:
                        self.assertEqual(member.mode, 0o600)
                        target = restored / Path(member.name).name
                        target.write_bytes(bundle.extractfile(member).read())
                        target.chmod(0o600)
                manifest = recovery.verify_bundle(restored)
                self.assertEqual(manifest["k3sVersion"], "v1.36.3+k3s1")
                self.assertFalse(list(data.glob("etcd-backup.*")))
                self.assertFalse(list((data / "server/db/snapshots").iterdir()))


if __name__ == "__main__":
    unittest.main()
