import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

HOOK = Path(__file__).resolve().parents[2] / "platform/components/backup-job/backup.sh"
KUBECTL = r'''#!/usr/bin/env bash
set -euo pipefail
shift
case "$1 $2" in
  "get deployment")
    if [[ ${!#} == json ]]; then jq -n --arg app "$3" '{spec: {selector: {matchLabels: {app: $app}}}}'
    else jq -r --arg name "$3" '.[$name]' "$FAKE_STATE"; fi ;;
  "get pods") echo '{"items":[]}' ;;
  wait\ *) exec sleep "${FAKE_WAIT_SECONDS:-0}" > /dev/null 2>&1 ;;
  "scale deployment")
    replicas=$(printf '%s\n' "$@" | sed -n 's/^--replicas=//p')
    state=$(jq --arg name "$3" --argjson replicas "$replicas" '.[$name] = $replicas' "$FAKE_STATE")
    printf '%s\n' "$state" > "$FAKE_STATE"
    [[ ${FAIL_AFTER_SCALE:-} != 1 || $replicas != 0 ]] ;;
  *) exit 2 ;;
esac
'''
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
CURL = r'''#!/usr/bin/env python3
import os,stat,sys
from pathlib import Path
p=Path(sys.argv[sys.argv.index('--config')+1])
token=os.environ['BACKUP_HEARTBEAT_TOKEN']
assert token not in ' '.join(sys.argv)
assert stat.S_IMODE(p.stat().st_mode)==0o600
assert p.read_text()=='header = "Authorization: Bearer '+token+'"\n'
Path(os.environ['OBSERVATION']).write_text(str(p))
sys.exit(int(os.environ['CURL_STATUS']))
'''
WRITE_DUMP = 'for arg; do case $arg in --file=*|--archive=*) echo dump > "${arg#*=}";; esac; done\n'
FAKES = {
    "kubectl": KUBECTL,
    "timeout": TIMEOUT,
    "curl": CURL,
    "restic": "#!/bin/bash\nexit 0\n",
    "pg_dump": '#!/bin/bash\n[[ -z "${FAKE_DUMP_FAILS:-}" ]] || exit 1\n' + WRITE_DUMP,
    "pg_restore": "#!/bin/bash\nexit 0\n",
    "mongodump": "#!/bin/bash\n" + WRITE_DUMP + 'echo "$FAKE_MONGODUMP_LOG" >&2\n',
    "mongorestore": '#!/bin/bash\nexit "${FAKE_MONGORESTORE_STATUS:-0}"\n',
    "du": '#!/bin/bash\nprintf \'%s\\t/files\\n\' "$FAKE_DU_BYTES"\n',
}


class BackupTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        tools = tempfile.TemporaryDirectory()
        cls.addClassCleanup(tools.cleanup)
        cls.binaries = Path(tools.name)
        for name, source in FAKES.items():
            (cls.binaries / name).write_text(source)
            (cls.binaries / name).chmod(0o755)

    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.state = self.root / "state.json"
        self.exported = self.root / "work/source"
        self.files = self.root / "files"
        self.files.mkdir()
        self.account = self.root / "service-account"
        self.account.mkdir()
        for name in ["namespace", "ca.crt", "token"]:
            (self.account / name).write_text("parser")

    def run_hook(self, script, *arguments, **environment):
        return subprocess.run(
            ["bash", str(HOOK.with_name(script)), *arguments], capture_output=True, text=True, check=False, timeout=30,
            env={**os.environ, "PATH": str(self.binaries) + os.pathsep + os.environ["PATH"], "TMPDIR": str(self.root)} | environment)

    def backup(self, kind, *arguments, writers=None, **environment):
        writers = writers or {"review": 1}
        shutil.rmtree(self.root / "work", ignore_errors=True)
        self.state.write_text(json.dumps(writers))
        result = self.run_hook(
            "backup.sh", *arguments, BACKUP_WORK_DIR=str(self.root / "work"), BACKUP_WRITERS=" ".join(writers),
            BACKUP_KIND=kind, BACKUP_PROJECT="parser", BACKUP_SERVICE_ACCOUNT_DIR=str(self.account),
            BACKUP_FILES_DIR=str(self.files), FAKE_STATE=str(self.state), **environment)
        self.assertEqual(json.loads(self.state.read_text()), writers)
        return result

    def test_export_resumes_only_originally_running_writers_and_rejects_partial_backups(self):
        writers = {"review": 1, "worker": 0}
        result = self.backup("postgres", "export", writers=writers)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.exported / "SHA256SUMS").exists())
        for failure in [{"FAKE_DUMP_FAILS": "1"}, {"FAIL_AFTER_SCALE": "1"}]:
            with self.subTest(failure=failure):
                self.assertNotEqual(self.backup("postgres", "export", writers=writers, **failure).returncode, 0)
                self.assertFalse((self.exported / "SHA256SUMS").exists())

    def test_missing_projected_token_refuses_before_stopping_writers(self):
        (self.account / "token").unlink()
        self.assertNotEqual(self.backup("postgres", "export").returncode, 0)
        self.assertFalse((self.exported / "SHA256SUMS").exists())

    def test_mongodb_export_requires_dumped_collections_and_a_readable_archive(self):
        dumped = "done dumping myAppDB.posts (3 documents)"
        for log, restore_status, accepted in [(dumped, "0", True), ("no collections to dump", "0", False), (dumped, "1", False)]:
            with self.subTest(log=log, restore_status=restore_status):
                result = self.backup("mongodb", "export", MONGODB_URI="mongodb://host/db",
                                     FAKE_MONGODUMP_LOG=log, FAKE_MONGORESTORE_STATUS=restore_status)
                self.assertEqual(result.returncode == 0, accepted, result.stderr)
                self.assertEqual((self.exported / "SHA256SUMS").exists(), accepted)

    @unittest.skipUnless(shutil.which("sqlite3"), "sqlite3 required")
    def test_sqlite_export_snapshots_a_wal_database(self):
        subprocess.run(["sqlite3", str(self.files / "metadata.db"),
                        "PRAGMA journal_mode=WAL; CREATE TABLE nar(id); INSERT INTO nar VALUES (1);"],
                       check=True, capture_output=True)
        result = self.backup("sqlite", "export")
        self.assertEqual(result.returncode, 0, result.stderr)
        rows = subprocess.run(["sqlite3", str(self.exported / "metadata.db"), "SELECT count(*) FROM nar;"],
                              capture_output=True, text=True, check=True)
        self.assertEqual(rows.stdout.strip(), "1")

    def test_oversized_local_files_fail_before_tar(self):
        result = self.backup("postgres", "export", BACKUP_LOCAL_FILES="true", BACKUP_LOCAL_FILES_MAX_BYTES="1024",
                             FAKE_DU_BYTES="999999999")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("scratch budget", result.stderr)
        self.assertFalse((self.exported / "local-files.tar").exists())

    def test_local_files_within_budget_are_archived_without_excluded_paths(self):
        for name in ["keep.txt", "metadata.db", "metadata.db-wal"]:
            (self.files / name).write_text(name)
        result = self.backup("postgres", "export", BACKUP_LOCAL_FILES="true", BACKUP_LOCAL_FILES_MAX_BYTES="1024",
                             BACKUP_LOCAL_FILES_EXCLUDE="./metadata.db*", FAKE_DU_BYTES="512")
        self.assertEqual(result.returncode, 0, result.stderr)
        listing = subprocess.run(["tar", "--list", "--file", str(self.exported / "local-files.tar")],
                                 capture_output=True, text=True, check=True).stdout
        self.assertIn("./keep.txt", listing)
        self.assertNotIn("metadata.db", listing)

    def test_killed_export_still_resumes_writers(self):
        result = self.backup("postgres", FAKE_WAIT_SECONDS="5", BACKUP_EXPORT_TIMEOUT="1", BACKUP_KILL_GRACE="1",
                             BACKUP_PREFLIGHT_TIMEOUT="5")
        self.assertNotEqual(result.returncode, 0)

    def test_heartbeat_failure_does_not_fail_a_completed_backup(self):
        result = self.backup("postgres", BACKUP_HEARTBEAT_TOKEN="too-short", BACKUP_PREFLIGHT_TIMEOUT="5")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("heartbeat failed", result.stderr)

    def test_heartbeat_token_stays_out_of_arguments_and_curl_failure_is_preserved(self):
        observation = self.root / "observation"
        for status in [0, 22]:
            with self.subTest(curl_status=status):
                result = self.run_hook("heartbeat.sh", "parser", BACKUP_HEARTBEAT_TOKEN="dummy_backup_token_" * 4,
                                       OBSERVATION=str(observation), CURL_STATUS=str(status))
                self.assertEqual(result.returncode, status, result.stderr)
                self.assertFalse(Path(observation.read_text()).exists())


if __name__ == "__main__":
    unittest.main()
