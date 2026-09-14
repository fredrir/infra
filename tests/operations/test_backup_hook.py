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
elif a[0]=='wait':
 import time
 time.sleep(float(os.environ.get('FAKE_WAIT_SECONDS','0')))
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


class BackupHeartbeatTests(unittest.TestCase):
    def test_curl_receives_private_config_and_failure_is_preserved(self):
        for failure in [0, 22]:
            with self.subTest(curl_status=failure), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                binary = root / "curl"
                binary.write_text(
                    "#!/usr/bin/env python3\n"
                    "import json,os,stat,sys\n"
                    "from pathlib import Path\n"
                    "p=Path(sys.argv[sys.argv.index('--config')+1])\n"
                    "token=os.environ['BACKUP_HEARTBEAT_TOKEN']\n"
                    "assert token not in ' '.join(sys.argv)\n"
                    "assert stat.S_IMODE(p.stat().st_mode)==0o600\n"
                    "assert p.read_text()=='header = \\\"Authorization: Bearer '+token+'\\\"\\n'\n"
                    "Path(os.environ['OBSERVATION']).write_text(json.dumps({'config':str(p)}))\n"
                    "sys.exit(int(os.environ['CURL_STATUS']))\n"
                )
                binary.chmod(0o755)
                observation = root / "observation.json"
                result = subprocess.run(
                    ["bash", str(HOOK.with_name("heartbeat.sh")), "parser"],
                    env={
                        **os.environ,
                        "PATH": str(root) + os.pathsep + os.environ["PATH"],
                        "TMPDIR": str(root),
                        "BACKUP_HEARTBEAT_TOKEN": "dummy_backup_token_" * 4,
                        "OBSERVATION": str(observation),
                        "CURL_STATUS": str(failure),
                    },
                    capture_output=True,
                    check=False,
                    timeout=10,
                )
                self.assertEqual(result.returncode, failure, result.stderr)
                config = Path(json.loads(observation.read_text())["config"])
                self.assertFalse(config.exists())


TIMEOUT = r'''#!/usr/bin/env python3
import signal,subprocess,sys,time
args=sys.argv[1:];kill_after=0.0
while args and args[0].startswith('--'):
 a=args.pop(0)
 if a.startswith('--kill-after='):kill_after=float(a.split('=',1)[1])
duration=float(args.pop(0));proc=subprocess.Popen(args);deadline=time.time()+duration
while proc.poll() is None and time.time()<deadline:time.sleep(0.05)
if proc.poll() is not None:sys.exit(proc.returncode)
proc.send_signal(signal.SIGTERM);hard=time.time()+kill_after
while proc.poll() is None and time.time()<hard:time.sleep(0.05)
if proc.poll() is None:
 proc.kill();proc.wait()
sys.exit(124)
'''

RESTIC = "#!/bin/bash\nexit 0\n"
MONGODUMP_OK = "#!/bin/bash\nfor arg; do case $arg in --archive=*) echo archive > \"${arg#--archive=}\";; esac; done\necho 'done dumping myAppDB.posts (3 documents)' >&2\n"
MONGODUMP_EMPTY = "#!/bin/bash\nfor arg; do case $arg in --archive=*) echo archive > \"${arg#--archive=}\";; esac; done\necho 'no collections to dump' >&2\n"
MONGORESTORE_OK = "#!/bin/bash\nexit 0\n"
MONGORESTORE_CORRUPT = "#!/bin/bash\nexit 1\n"


@unittest.skipUnless(shutil.which("bash") and shutil.which("jq"), "Native shell tools required")
class BackupGuardTests(unittest.TestCase):
    def build(self, root, commands, state=None):
        binary = root / "bin"
        binary.mkdir(exist_ok=True)
        fake_state = root / "state.json"
        fake_state.write_text(json.dumps(state if state is not None else {"review": 1}))
        service_account = root / "service-account"
        service_account.mkdir(exist_ok=True)
        (service_account / "namespace").write_text("parser")
        (service_account / "ca.crt").write_text("test certificate")
        (service_account / "token").write_text("test token")
        for name, source in {"kubectl": KUBECTL, **commands}.items():
            path = binary / name
            path.write_text(source)
            path.chmod(0o755)
        return binary, fake_state, service_account

    def invoke(self, root, binary, fake_state, service_account, kind, extra=None, argument="export", timeout=30):
        return subprocess.run(
            ["bash", str(HOOK)] + ([argument] if argument else []),
            env={
                **os.environ,
                "PATH": str(binary) + os.pathsep + os.environ["PATH"],
                "BACKUP_WORK_DIR": str(root / "work"),
                "BACKUP_WRITERS": "review",
                "BACKUP_KIND": kind,
                "BACKUP_PROJECT": "parser",
                "BACKUP_SERVICE_ACCOUNT_DIR": str(service_account),
                "FAKE_STATE": str(fake_state),
                "FAIL_AFTER_SCALE": "0",
                **(extra or {}),
            },
            capture_output=True,
            check=False,
            timeout=timeout,
        )

    def test_failed_guard_names_the_line_and_command(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary, state, account = self.build(root, {"pg_dump": "#!/bin/bash\nexit 1\n", "pg_restore": RESTIC})
            result = self.invoke(root, binary, state, account, "postgres")
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("backup.sh: line", result.stderr.decode())
            self.assertIn("pg_dump", result.stderr.decode())

    def test_oversized_local_files_fail_before_tar_and_resume_writers(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "files"
            source.mkdir()
            (source / "payload").write_text("x" * 4096)
            binary, state, account = self.build(
                root,
                {
                    "pg_dump": "#!/bin/bash\nfor arg; do case $arg in --file=*) echo dump > \"${arg#--file=}\";; esac; done\n",
                    "pg_restore": RESTIC,
                    "du": "#!/bin/bash\necho -e '999999999\\t/files'\n",
                },
            )
            result = self.invoke(
                root, binary, state, account, "postgres",
                {"BACKUP_LOCAL_FILES": "true", "BACKUP_LOCAL_FILES_MAX_BYTES": "1024", "BACKUP_FILES_DIR": str(source)},
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("scratch budget", result.stderr.decode())
            self.assertFalse((root / "work/source/local-files.tar").exists())
            self.assertEqual(json.loads(state.read_text()), {"review": 1})

    def test_local_files_within_budget_are_archived(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "files"
            source.mkdir()
            (source / "payload").write_text("x" * 16)
            binary, state, account = self.build(
                root,
                {
                    "pg_dump": "#!/bin/bash\nfor arg; do case $arg in --file=*) echo dump > \"${arg#--file=}\";; esac; done\n",
                    "pg_restore": RESTIC,
                    "du": "#!/bin/bash\necho -e '512\\t/files'\n",
                },
            )
            result = self.invoke(
                root, binary, state, account, "postgres",
                {"BACKUP_LOCAL_FILES": "true", "BACKUP_LOCAL_FILES_MAX_BYTES": "1024", "BACKUP_FILES_DIR": str(source)},
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertTrue((root / "work/source/local-files.tar").exists())

    def test_mongodb_export_rejects_a_dump_with_no_collections(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary, state, account = self.build(
                root, {"mongodump": MONGODUMP_EMPTY, "mongorestore": MONGORESTORE_OK})
            result = self.invoke(root, binary, state, account, "mongodb", {"MONGODB_URI": "mongodb://host/db"})
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(json.loads(state.read_text()), {"review": 1})

    def test_mongodb_export_rejects_an_unreadable_archive(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary, state, account = self.build(
                root, {"mongodump": MONGODUMP_OK, "mongorestore": MONGORESTORE_CORRUPT})
            result = self.invoke(root, binary, state, account, "mongodb", {"MONGODB_URI": "mongodb://host/db"})
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(json.loads(state.read_text()), {"review": 1})

    def test_mongodb_export_accepts_a_verified_archive(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary, state, account = self.build(
                root, {"mongodump": MONGODUMP_OK, "mongorestore": MONGORESTORE_OK})
            result = self.invoke(root, binary, state, account, "mongodb", {"MONGODB_URI": "mongodb://host/db"})
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertTrue((root / "work/source/SHA256SUMS").exists())

    @unittest.skipUnless(shutil.which("sqlite3"), "sqlite3 required")
    def test_sqlite_export_snapshots_a_wal_database(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "files"
            source.mkdir()
            database = source / "metadata.db"
            subprocess.run(
                ["sqlite3", str(database), "PRAGMA journal_mode=WAL; CREATE TABLE nar(id); INSERT INTO nar VALUES (1);"],
                check=True, capture_output=True)
            binary, state, account = self.build(root, {})
            result = self.invoke(
                root, binary, state, account, "sqlite", {"BACKUP_FILES_DIR": str(source)})
            self.assertEqual(result.returncode, 0, result.stderr)
            copy = root / "work/source/metadata.db"
            self.assertTrue(copy.exists())
            rows = subprocess.run(
                ["sqlite3", str(copy), "SELECT count(*) FROM nar;"], capture_output=True, text=True, check=True)
            self.assertEqual(rows.stdout.strip(), "1")

    def test_killed_export_still_resumes_writers(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary, state, account = self.build(root, {"restic": RESTIC, "timeout": TIMEOUT})
            result = self.invoke(
                root, binary, state, account, "postgres",
                {
                    "FAKE_WAIT_SECONDS": "5",
                    "BACKUP_EXPORT_TIMEOUT": "1",
                    "BACKUP_KILL_GRACE": "1",
                    "BACKUP_PREFLIGHT_TIMEOUT": "5",
                },
                argument=None,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(json.loads(state.read_text()), {"review": 1})

    def test_heartbeat_failure_does_not_fail_a_completed_backup(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary, state, account = self.build(
                root,
                {
                    "restic": RESTIC,
                    "timeout": TIMEOUT,
                    "pg_dump": "#!/bin/bash\nfor arg; do case $arg in --file=*) echo dump > \"${arg#--file=}\";; esac; done\n",
                    "pg_restore": RESTIC,
                },
            )
            result = self.invoke(
                root, binary, state, account, "postgres",
                {"BACKUP_HEARTBEAT_TOKEN": "too-short", "BACKUP_PREFLIGHT_TIMEOUT": "5"},
                argument=None,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("heartbeat failed", result.stderr.decode())
            self.assertEqual(json.loads(state.read_text()), {"review": 1})

    def test_excluded_paths_are_kept_out_of_the_local_files_archive(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "files"
            source.mkdir()
            (source / "keep.txt").write_text("keep")
            (source / "metadata.db").write_text("skip")
            (source / "metadata.db-wal").write_text("skip")
            binary, state, account = self.build(
                root,
                {
                    "pg_dump": "#!/bin/bash\nfor arg; do case $arg in --file=*) echo dump > \"${arg#--file=}\";; esac; done\n",
                    "pg_restore": RESTIC,
                    "du": "#!/bin/bash\necho -e '512\\t/files'\n",
                },
            )
            result = self.invoke(
                root, binary, state, account, "postgres",
                {
                    "BACKUP_LOCAL_FILES": "true",
                    "BACKUP_LOCAL_FILES_MAX_BYTES": "1024",
                    "BACKUP_FILES_DIR": str(source),
                    "BACKUP_LOCAL_FILES_EXCLUDE": "./metadata.db*",
                },
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            listing = subprocess.run(
                ["tar", "--list", "--file", str(root / "work/source/local-files.tar")],
                capture_output=True, text=True, check=True).stdout
            self.assertIn("./keep.txt", listing)
            self.assertNotIn("metadata.db", listing)
