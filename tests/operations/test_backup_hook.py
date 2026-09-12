import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
HOOK = ROOT / "platform/components/backup-job/backup.sh"
KUBECTL = r'''#!/usr/bin/env python3
import json,os,sys
from pathlib import Path
p=Path(os.environ['FAKE_STATE']);s=json.loads(p.read_text());a=sys.argv[2:]
if a[:2]==['get','deployment']:
 name=a[2]
 if a[-1]=='json':print(json.dumps({'spec':{'selector':{'matchLabels':{'app':name}}}}))
 else:print(s[name])
elif a[:2]==['get','pods']:print('{"items":[]}')
elif a[0]=='wait':pass
elif a[:2]==['scale','deployment']:
 name=a[2];s[name]=int(next(v.split('=')[1]for v in a if v.startswith('--replicas=')));p.write_text(json.dumps(s))
 if os.environ.get('FAIL_AFTER_SCALE')=='1' and s[name]==0:sys.exit(1)
else:sys.exit(2)
'''


@unittest.skipUnless(shutil.which("bash") and shutil.which("jq"), "Native shell tools required")
class BackupHookTests(unittest.TestCase):
    def run_export(self, *, dump_failure=False, scale_failure=False, missing_token=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary = root / "bin"
            binary.mkdir()
            state = root / "state.json"
            state.write_text(json.dumps({"review": 1, "worker": 0}))
            service_account = root / "service-account"
            service_account.mkdir()
            (service_account / "namespace").write_text("parser")
            (service_account / "ca.crt").write_text("test certificate")
            if not missing_token:
                (service_account / "token").write_text("test token")
            commands = {
                "kubectl": KUBECTL,
                "pg_dump": "#!/bin/bash\nexit 1\n" if dump_failure else "#!/bin/bash\nfor arg; do case $arg in --file=*) echo dump > \"${arg#--file=}\";; esac; done\n",
                "pg_restore": "#!/bin/bash\nexit 0\n",
            }
            for name, source in commands.items():
                path = binary / name
                path.write_text(source)
                path.chmod(0o755)
            result = subprocess.run(
                ["bash", str(HOOK), "export"],
                env={
                    **os.environ,
                    "PATH": str(binary) + os.pathsep + os.environ["PATH"],
                    "BACKUP_WORK_DIR": str(root / "work"),
                    "BACKUP_WRITERS": "review worker",
                    "BACKUP_KIND": "postgres",
                    "BACKUP_SERVICE_ACCOUNT_DIR": str(service_account),
                    "FAKE_STATE": str(state),
                    "FAIL_AFTER_SCALE": "1" if scale_failure else "0",
                },
                capture_output=True,
                check=False,
                timeout=10,
            )
            return result, json.loads(state.read_text()), (root / "work/source/SHA256SUMS").exists()

    def test_export_resumes_only_originally_running_writers(self):
        result, state, checksum = self.run_export()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(state, {"review": 1, "worker": 0})
        self.assertTrue(checksum)

    def test_dump_failure_resumes_writers_and_rejects_backup(self):
        result, state, checksum = self.run_export(dump_failure=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(state, {"review": 1, "worker": 0})
        self.assertFalse(checksum)

    def test_interrupted_scale_response_still_resumes_changed_writer(self):
        result, state, checksum = self.run_export(scale_failure=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(state, {"review": 1, "worker": 0})
        self.assertFalse(checksum)

    def test_missing_projected_token_refuses_before_stopping_writers(self):
        result, state, checksum = self.run_export(missing_token=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(state, {"review": 1, "worker": 0})
        self.assertFalse(checksum)
