import argparse
import hashlib
import io
import json
import os
import pwd
import re
import shutil
import socket
import sys
import time
from pathlib import Path

import evacuation_backup as backup
from evacuation_execution import Commands
from evacuation_staging import validate_plan
from evacuation_target import mounted_profile

PASSWORD = "isolated-online-recovery-only"
SESSION_PATTERNS = ("session:*", "user:*:sessions")
SESSION_SCRIPT = """local total=0
for _,pattern in ipairs({'session:*','user:*:sessions'}) do
  local cursor='0'
  local iterations=0
  repeat
    local batch=redis.call('SCAN',cursor,'MATCH',pattern,'COUNT',128)
    cursor=batch[1]
    iterations=iterations+1
    if iterations>10000 then return redis.error_reply('session scan budget exceeded') end
    for _,key in ipairs(batch[2]) do
      if string.match(key,'^session:') or string.match(key,'^user:.*:sessions$') then
        total=total+redis.call('DEL',key)
      else return redis.error_reply('session key scope differs') end
    end
  until cursor=='0'
end
return total
"""
COUNT_SCRIPT = SESSION_SCRIPT.replace(
    "total=total+redis.call('DEL',key)", "total=total+1"
)


def require(value, message):
    if not value:
        raise ValueError(message)


def recovery_inputs(directory, proof_path, candidate):
    bundle = backup.validate_bundle(directory)
    require(
        bundle["kind"] == "evacuation-online-state", "Online recovery input required"
    )
    manifest, proof = (
        backup.read_json(Path(directory) / "manifest.json"),
        backup.read_json(proof_path),
    )
    require(
        proof["schemaVersion"] == 1
        and proof["kind"] == "evacuation-independent-restore"
        and proof["verified"] is True
        and proof["independentHost"] is True
        and proof["bundle"] == bundle
        and re.fullmatch("[a-f0-9]{64}", proof["snapshotId"])
        and re.fullmatch("[a-f0-9]{64}", proof["archiveSHA256"]),
        "Independent off-host byte proof required",
    )
    plan = validate_plan(candidate)
    require(
        hashlib.sha256((Path(candidate) / "staging.json").read_bytes()).hexdigest()
        == manifest["candidateSHA256"],
        "Reviewed candidate differs from snapshot",
    )
    require(
        all(
            plan["services"][name]["runtimeImage"] == image
            for name, image in manifest["imageIDs"].items()
        ),
        "Reviewed datastore images differ",
    )
    return manifest, proof, plan


class NativeRestore:
    def __init__(self, directory, proof, candidate, workspace):
        self.directory = backup.private_directory(directory)
        self.manifest, self.proof, self.plan = recovery_inputs(
            directory, proof, candidate
        )
        self.workspace = Path(workspace).absolute()
        home = Path(pwd.getpwuid(os.geteuid()).pw_dir)
        require(
            sys.platform == "linux"
            and os.geteuid() > 0
            and socket.gethostname() != "cloud-server-10643982",
            "Independent non-root Linux host required",
        )
        require(
            self.workspace.parent == home / ".cache"
            and re.fullmatch(r"infra-online-restore\.[a-f0-9]{12}", self.workspace.name)
            and not os.path.lexists(self.workspace),
            "Fresh private admin workspace required",
        )
        require(
            shutil.disk_usage(self.workspace.parent).free >= 6 * 1024**3,
            "Six GiB recovery workspace required",
        )
        self.commands = Commands(780)
        self.cleanup_commands = None
        self.prepared = False
        self.names = []
        self.environment = [
            "env",
            "HOME=" + str(home),
            "USER=" + pwd.getpwuid(os.geteuid()).pw_name,
            "XDG_RUNTIME_DIR=/run/user/" + str(os.geteuid()),
            "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/"
            + str(os.geteuid())
            + "/bus",
        ]
        self.podman = [
            "podman",
            "--root",
            str(self.workspace / "storage"),
            "--runroot",
            str(self.workspace / "run"),
            "--storage-driver=vfs",
            "--cgroup-manager=systemd",
        ]
        self.result = {
            "schemaVersion": 1,
            "kind": "evacuation-online-native-restore",
            "operatorHost": socket.gethostname(),
            "snapshotId": self.proof["snapshotId"],
            "archiveSHA256": self.proof["archiveSHA256"],
            "candidateSHA256": self.manifest["candidateSHA256"],
            "images": self.manifest["imageIDs"],
            "consistency": "independent-datastore-points",
            "productionCredentials": False,
            "applicationStarted": False,
            "nativeRestoreVerified": False,
        }

    def call(self, args, *, data=None, timeout=20, check=True, cleanup=False):
        if cleanup and self.cleanup_commands is None:
            self.cleanup_commands = Commands(90)
        commands = self.cleanup_commands if cleanup else self.commands
        with io.BytesIO(data or b"") as source:
            if data is None:
                return commands.run(
                    self.environment + self.podman + args,
                    timeout=timeout,
                    check=check,
                    maximum=65536,
                )
            import tempfile

            with tempfile.TemporaryFile() as stream:
                stream.write(source.read())
                stream.seek(0)
                return commands.run(
                    self.environment + self.podman + args,
                    timeout=timeout,
                    source=stream,
                    check=check,
                    maximum=65536,
                )

    def preflight(self):
        self.workspace.mkdir(mode=0o700)
        self.prepared = True
        _, rootless = self.call(["info", "--format", "{{.Host.Security.Rootless}}"])
        require(rootless.strip() == b"true", "Rootless recovery runtime required")
        for name, image in self.manifest["imageIDs"].items():
            reference = self.plan["services"][name]["image"]
            require(
                re.fullmatch(
                    r"docker.io/(?:library/postgres|valkey/valkey)@sha256:[a-f0-9]{64}",
                    reference,
                ),
                "Fixed public datastore image required",
            )
            self.call(["pull", reference], timeout=240)
            _, actual = self.call(
                ["image", "inspect", "--format", "{{.Id}}", reference]
            )
            require(
                actual.decode().strip().removeprefix("sha256:")
                == image.removeprefix("sha256:"),
                "Pulled image config differs from snapshot",
            )

    def inspect(self, name):
        _, raw = self.call(["inspect", name])
        document = json.loads(raw)[0]
        require(
            document["Name"].lstrip("/") == name
            and re.fullmatch("[a-f0-9]{64}", document["Id"]),
            "Exact owned recovery container required",
        )
        return document

    def start(self, service, memory, extra, command):
        name = self.workspace.name.replace(".", "-") + "-" + service
        image = self.manifest["imageIDs"][service]
        self.names.append(name)
        args = [
            "run",
            "--detach",
            "--name",
            name,
            "--pull=never",
            "--network=none",
            "--read-only",
            "--user=999:999",
            "--cap-drop=ALL",
            "--security-opt=no-new-privileges",
            "--memory=" + str(memory) + "m",
            "--memory-swap=" + str(memory) + "m",
            "--cpus=1",
            "--pids-limit=128",
            "--timeout=120",
            "--tmpfs=/tmp:rw,nosuid,nodev,noexec,size=16m",
            "--tmpfs=/run:rw,nosuid,nodev,noexec,size=4m",
            *extra,
            image,
            *command,
        ]
        self.call(args)
        before = self.inspect(name)
        require(
            before["HostConfig"].get("ReadonlyRootfs") is True,
            "Read-only recovery container required",
        )
        proof = mounted_profile(before, image, self.workspace, memory)
        after = self.inspect(name)
        require(
            (after["Id"], after["State"]["Pid"], after["State"]["Running"])
            == (before["Id"], before["State"]["Pid"], True),
            "Recovery process changed during kernel proof",
        )
        credentials = {
            key.split("=", 1)[0]: key.split("=", 1)[1]
            for key in after["Config"].get("Env", [])
            if "=" in key
        }
        require(
            all(
                value == PASSWORD
                for key, value in credentials.items()
                if key in ("POSTGRES_PASSWORD", "PGPASSWORD")
            ),
            "Only isolated PostgreSQL credentials permitted",
        )
        return name, proof

    def data_directory(self, name):
        path = self.workspace / name
        path.mkdir(mode=0o700)
        self.call(["unshare", "chown", "999:999", str(path)])
        return path

    def wait(self, name, args, expected):
        for _ in range(40):
            code, output = self.call(["exec", name, *args], check=False)
            if code == 0 and output.strip() == expected:
                return
            time.sleep(0.25)
        raise ValueError("Isolated database did not become ready")

    def postgres(self):
        path = self.data_directory("postgres")
        name, kernel = self.start(
            "llunde-postgres",
            768,
            [
                "--tmpfs=/var/run/postgresql:rw,nosuid,nodev,noexec,size=4m,mode=1777",
                "--env=POSTGRES_USER=llunde",
                "--env=POSTGRES_DB=llunde",
                "--env=POSTGRES_PASSWORD=" + PASSWORD,
                "--env=PGDATA=/var/lib/postgresql/data/pgdata",
                "--volume",
                str(path) + ":/var/lib/postgresql/data",
            ],
            ["postgres", "-c", "shared_buffers=128MB"],
        )
        psql = [
            "psql",
            "--host=127.0.0.1",
            "--username=llunde",
            "--dbname=llunde",
            "--tuples-only",
            "--no-align",
        ]
        self.wait(
            name, ["env", "PGPASSWORD=" + PASSWORD, *psql, "--command=SELECT 1"], b"1"
        )
        with backup.open_private(
            self.directory / "database.dump", 128 * 1024**2
        ) as stream:
            self.call(
                [
                    "exec",
                    "-i",
                    "--env=PGPASSWORD=" + PASSWORD,
                    name,
                    "pg_restore",
                    "--exit-on-error",
                    "--single-transaction",
                    "--no-owner",
                    "--no-acl",
                    "--host=127.0.0.1",
                    "--username=llunde",
                    "--dbname=llunde",
                ],
                data=stream.read(),
                timeout=90,
            )
        query = "SELECT json_build_object('tables',(SELECT json_agg(tablename ORDER BY tablename) FROM pg_tables WHERE schemaname='public'),'constraints',(SELECT count(*) FROM pg_constraint WHERE connamespace='public'::regnamespace),'serverVersion',current_setting('server_version'));"
        _, raw = self.call(
            ["exec", "--env=PGPASSWORD=" + PASSWORD, name, *psql, "--command", query]
        )
        value = json.loads(raw)
        require(
            value["tables"] == ["flyway_schema_history", "users"]
            and value["constraints"] == 3
            and value["serverVersion"].startswith("17."),
            "Restored approved PostgreSQL schema differs",
        )
        self.remove(name)
        self.result["postgres"] = dict(
            value, kernelProof=kernel, nativeRestoreVerified=True
        )

    def valkey(self):
        path = self.data_directory("valkey")
        with backup.open_private(self.directory / "dump.rdb", 128 * 1024**2) as stream:
            self.call(
                [
                    "unshare",
                    "sh",
                    "-c",
                    'umask 077; cat > "$1"; chown 999:999 "$1"',
                    "sh",
                    str(path / "dump.rdb"),
                ],
                data=stream.read(),
            )
        name, kernel = self.start(
            "llunde-valkey",
            192,
            ["--entrypoint=valkey-server", "--volume", str(path) + ":/data"],
            [
                "--dir",
                "/data",
                "--save",
                "",
                "--appendonly",
                "no",
                "--maxmemory",
                "80mb",
                "--maxmemory-policy",
                "noeviction",
            ],
        )
        self.wait(name, ["valkey-cli", "PING"], b"PONG")
        self.call(["exec", name, "valkey-check-rdb", "/data/dump.rdb"])
        _, removed = self.call(
            ["exec", name, "valkey-cli", "--raw", "EVAL", SESSION_SCRIPT, "0"]
        )
        require(re.fullmatch(rb"[0-9]+\s*", removed), "Session invalidation failed")
        _, remaining = self.call(
            ["exec", name, "valkey-cli", "--raw", "EVAL", COUNT_SCRIPT, "0"]
        )
        require(remaining.strip() == b"0", "Restored session authority remains")
        self.remove(name)
        self.result["valkey"] = {
            "nativeRdbRestored": True,
            "sessionsInvalidated": int(removed),
            "sessionKeysRemaining": 0,
            "patterns": list(SESSION_PATTERNS),
            "isolatedCopyOnly": True,
            "kernelProof": kernel,
        }

    def remove(self, name, cleanup=False):
        require(name in self.names, "Owned container required for cleanup")
        self.call(
            ["rm", "--force", "--ignore", "--volumes", name],
            cleanup=cleanup,
            timeout=15,
        )
        code, _ = self.call(["container", "exists", name], cleanup=cleanup, check=False)
        require(code == 1, "Recovery container remains")
        self.names.remove(name)

    def cleanup(self):
        failures = []
        for name in list(self.names):
            try:
                self.remove(name, cleanup=True)
            except Exception:
                failures.append(name)
        self.result["cleanupFailures"] = failures
        self.result["containersRemoved"] = not failures
        self.result["privateWorkspaceRetained"] = True
        self.result["restoredDataRemoved"] = False
        self.result["imagesRemoved"] = False
        if not failures and getattr(self, "prepared", False):
            try:
                for name in ("postgres", "valkey"):
                    path = self.workspace / name
                    require(not path.is_symlink(), "Recovery cleanup directory changed")
                    self.call(
                        ["unshare", "rm", "-rf", "--", str(path)],
                        cleanup=True,
                        timeout=15,
                    )
                    require(not os.path.lexists(path), "Restored private data remains")
                self.result["restoredDataRemoved"] = True
                for image in self.manifest["imageIDs"].values():
                    code, _ = self.call(
                        ["image", "exists", image], cleanup=True, check=False
                    )
                    require(code in (0, 1), "Private image-store inspection failed")
                    if code == 0:
                        self.call(["rmi", "--force", image], cleanup=True, timeout=20)
                self.result["imagesRemoved"] = True
            except Exception:
                failures.append("private-data-or-image-cleanup")
        return not failures


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--input", required=True, type=Path)
    parser.add_argument("--independent-proof", required=True, type=Path)
    parser.add_argument("--candidate", required=True, type=Path)
    parser.add_argument("--workspace", required=True, type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    pilot = NativeRestore(
        args.input, args.independent_proof, args.candidate, args.workspace
    )
    try:
        pilot.preflight()
        pilot.postgres()
        pilot.valkey()
        pilot.result["nativeRestoreVerified"] = True
    except Exception as error:
        pilot.result["errorType"] = type(error).__name__
    finally:
        clean = pilot.cleanup()
        if pilot.prepared:
            backup.write_private(
                pilot.workspace / "result.json",
                json.dumps(pilot.result, indent=2).encode() + b"\n",
            )
    print(
        json.dumps(
            {
                "nativeRestoreVerified": pilot.result["nativeRestoreVerified"],
                "containersRemoved": clean,
            }
        )
    )
    raise SystemExit(0 if pilot.result["nativeRestoreVerified"] and clean else 1)


if __name__ == "__main__":
    main()
