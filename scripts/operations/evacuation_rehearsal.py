import argparse
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import shutil
import signal
import socket
import stat
import subprocess
import tarfile
import time
import uuid

from evacuation_images import SERVICE_USERS, USERS, open_private, private_directory, read_json, validate_plan


MAX_INPUT = 64 * 1024**2
MAX_WORK = 1024**3
SERVICES = ["llunde-postgres", "llunde-valkey", "llunde-frontend", "caddy"]
MOCK_CADDY = "{\n admin off\n auto_https off\n}\nhttp://:8080 {\n bind 127.0.0.1\n respond backend-fixture\n}\nhttp://:8081 {\n bind 127.0.0.1\n respond \"frontend-fixture {header.X-Forwarded-Proto} {header.X-Forwarded-For}\"\n}\n"


class RehearsalError(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise RehearsalError(message)


def digest_file(path, owner):
    with open_private(path, owner, MAX_INPUT) as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def prepare_inputs(candidate, source, destination):
    candidate = private_directory(candidate, os.geteuid())
    source = private_directory(source, os.geteuid())
    plan = read_json(candidate / "staging.json", os.geteuid())
    for service in SERVICES:
        validate_plan(plan, service)
        require(re.fullmatch(r"sha256:[a-f0-9]{64}", plan["services"][service]["runtimeImage"]), "Verified runtime image required")
    metadata = {}
    files = {"Caddyfile": candidate / "units/Caddyfile"}
    for kind, suffix, target in [("postgres", "dump", "database.dump"), ("valkey", "rdb", "dump.rdb")]:
        reference = plan["data"][kind]["restoreProof"]
        require(re.fullmatch(kind + r"/[0-9]{8}T[0-9]{6}Z\.json", reference), "Approved source receipt required")
        receipt = read_json(source / reference, os.geteuid())
        path = (source / reference).with_suffix("." + suffix)
        require(receipt["source"] == f"fredrir-05/llunde-{kind}" and receipt["restoreVerified"] is True and receipt["exitCode"] == 0 and receipt["format"] == ("custom" if kind == "postgres" else "rdb"), "Successful native backup receipt required")
        require(path.stat().st_size == receipt["bytes"] and digest_file(path, os.geteuid()) == receipt["sha256"], "Backup receipt digest mismatch")
        metadata[kind] = {key: receipt[key] for key in ["createdAt", "source", "format", "bytes", "sha256"]}
        if kind == "postgres":
            metadata[kind]["tables"] = receipt["restore"]["tables"]
            metadata[kind]["constraints"] = receipt["restore"]["constraints"]
        files[target] = path
    require(digest_file(files["Caddyfile"], os.geteuid()) == plan["unitSHA256"]["units/Caddyfile"], "Candidate Caddy digest mismatch")
    destination = Path(destination)
    private_directory(destination.parent, os.geteuid())
    require(not destination.exists() and not destination.is_symlink(), "Fresh rehearsal bundle required")
    manifest = {"schemaVersion": 1, "kind": "isolated-target-rehearsal", "target": "fredrir-09", "hostname": "cloud-server-10643982", "images": {name: plan["services"][name]["runtimeImage"] for name in SERVICES}, "sources": metadata, "files": {}, "candidateSHA256": digest_file(candidate / "staging.json", os.geteuid())}
    destination.mkdir(mode=0o700)
    try:
        for name, path in files.items():
            with open_private(path, os.geteuid(), MAX_INPUT) as original:
                content = original.read()
            with os.fdopen(os.open(destination / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), "wb") as output:
                output.write(content)
            manifest["files"][name] = {"sha256": hashlib.sha256(content).hexdigest(), "bytes": len(content)}
        with os.fdopen(os.open(destination / "manifest.json", os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), "w") as output:
            json.dump(manifest, output, indent=2)
            output.write("\n")
    except BaseException:
        shutil.rmtree(destination)
        raise
    return {"inputFiles": 3, "productionCredentials": False, "sourceWritersStopped": False}


def validate_inputs(directory, owner=0):
    directory = private_directory(directory, owner)
    manifest = read_json(directory / "manifest.json", owner)
    require(manifest["schemaVersion"] == 1 and manifest["kind"] == "isolated-target-rehearsal" and manifest["target"] == "fredrir-09" and manifest["hostname"] == "cloud-server-10643982", "Rehearsal identity mismatch")
    require(set(manifest["images"]) == set(SERVICES) and all(re.fullmatch(r"sha256:[a-f0-9]{64}", value) for value in manifest["images"].values()), "Exact reviewed rehearsal images required")
    require(set(manifest["files"]) == {"database.dump", "dump.rdb", "Caddyfile"} and {path.name for path in directory.iterdir()} == set(manifest["files"]) | {"manifest.json"}, "Unexpected rehearsal inputs")
    for name, metadata in manifest["files"].items():
        require((directory / name).stat().st_size == metadata["bytes"] and digest_file(directory / name, owner) == metadata["sha256"], "Rehearsal input checksum mismatch")
    require(manifest["sources"]["postgres"]["sha256"] == manifest["files"]["database.dump"]["sha256"] and manifest["sources"]["valkey"]["sha256"] == manifest["files"]["dump.rdb"]["sha256"], "Native backup provenance mismatch")
    with open_private(directory / "database.dump", owner, MAX_INPUT) as stream:
        require(stream.read(5) == b"PGDMP", "Native PostgreSQL custom dump required")
    with open_private(directory / "dump.rdb", owner, MAX_INPUT) as stream:
        require(re.fullmatch(b"REDIS[0-9]{4}", stream.read(9)), "Native RDB header required")
    return manifest


def verify_aof_archive(path):
    require(path.stat().st_size <= MAX_INPUT, "AOF archive exceeds budget")
    entries, total = {}, 0
    with tarfile.open(path, "r:") as archive:
        for item in archive:
            name = item.name.removeprefix("./")
            require(name == "." or (name and all(part not in ["", ".", ".."] for part in name.split("/")) and not name.startswith("/")), "AOF path escaped")
            require(name not in entries and len(entries) < 128 and (item.isdir() or item.isfile()) and not item.sparse and 0 <= item.size <= MAX_INPUT, "Invalid AOF archive entry")
            require(item.uid == 999 and item.gid in [999, 1000], "Namespace ownership missing")
            require(item.mode & ~0o777 == 0, "Unexpected AOF special permission bits")
            if item.isdir():
                require(name in [".", "appendonlydir"] and item.size == 0, "Unexpected AOF directory")
            total += item.size
            require(total <= MAX_INPUT, "AOF payload exceeds budget")
            record = {"uid": item.uid, "gid": item.gid, "mode": format(item.mode, "04o"), "size": item.size, "type": "directory" if item.isdir() else "file"}
            if item.isfile():
                require(name.startswith("appendonlydir/") and re.fullmatch(r"appendonlydir/appendonly\.aof(?:\.manifest|\.[0-9]+\.(?:base\.rdb|base\.aof|incr\.aof))", name), "Unexpected AOF persistence file")
                with archive.extractfile(item) as stream:
                    record["sha256"] = hashlib.file_digest(stream, "sha256").hexdigest()
            entries[name] = record
    require("appendonlydir/appendonly.aof.manifest" in entries and any(name.endswith(".incr.aof") for name in entries), "Complete multipart AOF required")
    return entries


def podman_argv(user, arguments):
    require(user in USERS, "Reserved service user required")
    return ["sudo", "-n", "-u", user, "env", "-i", "PATH=/usr/bin:/bin:/usr/sbin:/sbin", f"HOME=/home/{user}", f"USER={user}", f"XDG_RUNTIME_DIR=/run/user/{USERS[user]}", f"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/{USERS[user]}/bus", "podman", *arguments]


def container_arguments(name, image, memory, extra):
    require(re.fullmatch(r"infra-rehearsal-[a-f0-9]{12}-[a-z-]+", name) and re.fullmatch(r"sha256:[a-f0-9]{64}", image), "Private container identity required")
    return ["run", "--detach", "--name", name, "--pull=never", "--network=none", "--restart=no", "--timeout=300", "--stop-timeout=15", f"--memory={memory}m", f"--memory-swap={memory}m", "--cpus=1", "--pids-limit=128", "--security-opt=no-new-privileges", *extra, image]


HTTP_PROBE = '''import hashlib,http.client,json,sys
query=json.loads(sys.argv[1])
connection=http.client.HTTPConnection('127.0.0.1',query['port'],timeout=5)
connection.request('GET',query['path'],headers=query['headers'])
response=connection.getresponse();body=response.read(1048577)
assert len(body)<=1048576
result={'status':response.status,'location':response.getheader('Location'),'sha256':hashlib.sha256(body).hexdigest(),'bytes':len(body)}
if query.get('expectedBody') is not None:assert body.decode()==query['expectedBody']
if query.get('html'):assert b'<html' in body.lower()
connection.close();print(json.dumps(result))
'''


class Pilot:
    def __init__(self, inputs):
        self.inputs = Path(inputs)
        self.manifest = validate_inputs(self.inputs)
        self.token = uuid.uuid4().hex[:12]
        self.deadline = time.monotonic() + 300
        self.containers, self.workspaces = [], {}
        self.report = None
        self.command_count = 0
        self.last_command = None
        self.last_stdout, self.last_stderr = b"", b""
        self.size_check = 0
        self.result = {"schemaVersion": 1, "target": "fredrir-09", "rehearsal": self.token, "images": self.manifest["images"], "sources": self.manifest["sources"], "productionCredentials": False, "sourceWritersStopped": False, "cloudflaredStarted": False, "backendStarted": False, "fullCrossUserRoutingVerified": False, "passed": False}

    def call(self, argv, data=None, timeout=30, check=True, cleanup=False, output=None):
        remaining = 30 if cleanup else self.deadline - time.monotonic()
        require(remaining > 0, "Rehearsal deadline exceeded")
        if not cleanup and time.monotonic() - self.size_check > 10:
            used = sum(path.stat().st_size for root in self.workspaces.values() for path in root.rglob("*") if path.is_file() and not path.is_symlink())
            require(used <= MAX_WORK, "Private workspace exceeds sampled budget")
            self.size_check = time.monotonic()
        try:
            if not cleanup:
                self.last_command = {"argv": argv, "stage": self.result.get("stage", self.result.get("phase", "preflight")), "exitCode": None}
                self.last_stdout, self.last_stderr = b"", b""
            completed = subprocess.run(argv, input=data, stdout=output or subprocess.PIPE, stderr=subprocess.PIPE, cwd="/tmp", env={"PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "LANG": "C", "LC_ALL": "C"}, timeout=min(timeout, remaining))
        except (OSError, subprocess.SubprocessError):
            raise RehearsalError("Bounded rehearsal command failed") from None
        if not cleanup:
            self.last_command["exitCode"] = completed.returncode
            self.last_stdout, self.last_stderr = (completed.stdout or b"")[:65536], (completed.stderr or b"")[:65536]
            if "podman" in argv and "inspect" in argv and "--format" not in argv:
                self.last_stdout = b"Full container inspection omitted from diagnostics\n"
        if check:
            if completed.returncode:
                self.record_failure()
            require(completed.returncode == 0, "Isolated rehearsal command failed")
        return completed

    def record_failure(self):
        if self.report and self.last_command:
            self.command_count += 1
            prefix = self.report / f"command-{self.command_count}"
            prefix.with_suffix(".json").write_text(json.dumps(self.last_command, indent=2) + "\n")
            prefix.with_suffix(".stdout").write_bytes(self.last_stdout)
            prefix.with_suffix(".stderr").write_bytes(self.last_stderr)
            self.result["failureEvidence"] = prefix.name

    def podman(self, user, arguments, **kwargs):
        return self.call(podman_argv(user, arguments), **kwargs)

    def preflight(self):
        require(os.geteuid() == 0 and socket.gethostname() == "cloud-server-10643982", "Verified fredrir-09 root required")
        base = Path("/var/lib/infra-evacuation/llunde")
        installed = read_json(base / "staging.json", 0)
        for service in SERVICES:
            validate_plan(installed, service)
            require(installed["services"][service]["runtimeImage"] == self.manifest["images"][service], "Installed candidate image mismatch")
        require(installed["unitSHA256"]["units/Caddyfile"] == self.manifest["files"]["Caddyfile"]["sha256"], "Installed candidate Caddy mismatch")
        for marker in ["stage-approved", "restore-approved", "source-fenced", "edge-approved"]:
            require(not (base / marker).exists() and not (base / marker).is_symlink(), "Dormant applications required")
        self.report = self.inputs.parent / f"report-{self.token}"
        self.report.mkdir(mode=0o700)
        for user, uid in USERS.items():
            account = pwd.getpwnam(user)
            require(account.pw_uid == account.pw_gid == uid and account.pw_dir == f"/home/{user}", "Service identity mismatch")
            require(not self.podman(user, ["ps", "--all", "--format", "{{.Names}}"]).stdout.strip(), "Empty rehearsal container store required")
            require(self.podman(user, ["info", "--format", "{{.Host.Security.Rootless}}"]).stdout.strip() == b"true", "Rootless runtime required")
            workspace = Path(f"/home/{user}/.infra-rehearsal-{self.token}")
            require(not workspace.exists() and not workspace.is_symlink(), "Fresh private workspace required")
            workspace.mkdir(mode=0o700)
            os.chown(workspace, uid, uid)
            self.workspaces[user] = workspace
        for service, image in self.manifest["images"].items():
            self.podman(SERVICE_USERS[service], ["image", "exists", image])

    def data_directory(self, name):
        user = "llunde-backend"
        path = self.workspaces[user] / name
        path.mkdir(mode=0o700)
        os.chown(path, USERS[user], USERS[user])
        self.podman(user, ["unshare", "chown", "999:999", str(path)])
        return path

    def start(self, service, suffix, memory, extra, command=None):
        user, image = SERVICE_USERS[service], self.manifest["images"][service]
        name = f"infra-rehearsal-{self.token}-{suffix}"
        self.containers.append((user, name))
        self.podman(user, container_arguments(name, image, memory, extra) + (command or []))
        document = json.loads(self.podman(user, ["inspect", name]).stdout)[0]
        settings = document["HostConfig"]
        require(settings["NetworkMode"] == "none" and settings["Memory"] == memory * 1024**2 and settings["PidsLimit"] == 128 and not settings.get("PortBindings"), "Container isolation differs from request")
        require(settings.get("NanoCpus") == 1000000000 or (settings.get("CpuQuota") == 100000 and settings.get("CpuPeriod") == 100000), "Container CPU limit differs from request")
        require(document["Image"].removeprefix("sha256:") == image.removeprefix("sha256:"), "Container image differs from approved source")
        forbidden = {"DOPPLER_TOKEN", "TUNNEL_TOKEN", "DB_PASSWORD", "POSTGRES_PASSWORD", "CF_API_TOKEN", "AWS_SECRET_ACCESS_KEY"}
        require(not any(value.split("=", 1)[0] in forbidden for value in document["Config"].get("Env", [])), "Production credential environment forbidden")
        require(all(Path(mount["Source"]).resolve().is_relative_to(self.workspaces[user]) for mount in document.get("Mounts", []) if mount["Type"] == "bind"), "Unexpected host bind mount")
        return user, name

    def stop(self, user, name):
        self.podman(user, ["stop", "--time=15", name])
        state = json.loads(self.podman(user, ["inspect", "--format", "{{json .State}}", name]).stdout)
        require(state["ExitCode"] == 0 and not state["Running"], "Graceful persistence shutdown failed")

    def remove(self, user, name):
        self.podman(user, ["rm", "--force", "--volumes", name])
        self.containers.remove((user, name))

    def wait(self, user, name, arguments, expected=None):
        for _ in range(40):
            response = self.podman(user, ["exec", name, *arguments], check=False)
            if response.returncode == 0 and (expected is None or response.stdout.strip() == expected):
                return
            time.sleep(0.5)
        raise RehearsalError("Private service did not become ready")

    def postgres(self):
        self.result["stage"] = "postgres-native-restore"
        path = self.data_directory("postgres")
        user, name = self.start("llunde-postgres", "postgres", 768, ["--user=999:999", "--cap-drop=ALL", "--env=POSTGRES_DB=llunde", "--env=POSTGRES_USER=llunde", "--env=POSTGRES_HOST_AUTH_METHOD=trust", "--env=PGDATA=/var/lib/postgresql/data/pgdata", "--volume", f"{path}:/var/lib/postgresql/data"], ["postgres", "-c", "shared_buffers=128MB"])
        self.wait(user, name, ["pg_isready", "--username=llunde", "--dbname=llunde"])
        with open_private(self.inputs / "database.dump", 0, MAX_INPUT) as source:
            dump = source.read()
        self.podman(user, ["exec", "-i", name, "pg_restore", "--list"], data=dump)
        self.podman(user, ["exec", "-i", name, "pg_restore", "--exit-on-error", "--single-transaction", "--no-owner", "--no-acl", "--username=llunde", "--dbname=llunde"], data=dump, timeout=90)
        query = "SELECT json_build_object('serverVersion',current_setting('server_version'),'tables',(SELECT count(*) FROM pg_tables WHERE schemaname='public'),'constraints',(SELECT count(*) FROM pg_constraint WHERE connamespace='public'::regnamespace));"
        receipt = json.loads(self.podman(user, ["exec", name, "psql", "--username=llunde", "--dbname=llunde", "--tuples-only", "--no-align", "--command", query]).stdout)
        require(all(receipt[key] == self.manifest["sources"]["postgres"][key] for key in ["tables", "constraints"]), "Restored PostgreSQL schema differs from source proof")
        self.stop(user, name)
        self.remove(user, name)
        self.result["postgres"] = receipt | {"nativeRestoreVerified": True, "network": "none"}

    def valkey(self):
        self.result["stage"] = "valkey-rdb-restore"
        user = "llunde-backend"
        rdb = self.data_directory("valkey-rdb")
        self.podman(user, ["unshare", "sh", "-c", 'umask 077; cat > "$1"; chown 999:999 "$1"', "sh", str(rdb / "dump.rdb")], data=(self.inputs / "dump.rdb").read_bytes())
        extra = ["--user=999:999", "--cap-drop=ALL", "--entrypoint=valkey-server", "--volume", f"{rdb}:/data"]
        user, name = self.start("llunde-valkey", "valkey-rdb", 192, extra, ["--dir", "/data", "--save", "", "--appendonly", "no", "--maxmemory", "64mb"])
        self.wait(user, name, ["valkey-cli", "PING"], b"PONG")
        self.podman(user, ["exec", name, "valkey-check-rdb", "/data/dump.rdb"])
        keyspace = self.podman(user, ["exec", name, "valkey-cli", "--raw", "INFO", "keyspace"]).stdout.decode().strip()
        self.stop(user, name)
        self.remove(user, name)
        self.result["stage"] = "valkey-synthetic-aof-write"
        original = self.data_directory("valkey-aof")
        user, name = self.start("llunde-valkey", "valkey-aof", 192, ["--user=999:999", "--cap-drop=ALL", "--entrypoint=valkey-server", "--volume", f"{original}:/data"], ["--dir", "/data", "--save", "", "--appendonly", "yes", "--appendfsync", "everysec", "--aof-load-truncated", "no", "--maxmemory", "64mb"])
        self.wait(user, name, ["valkey-cli", "PING"], b"PONG")
        key = "infra-rehearsal:" + self.token
        require(self.podman(user, ["exec", name, "valkey-cli", "SET", key, "synthetic-persistence", "EX", "1800"]).stdout.strip() == b"OK", "Synthetic persistence write failed")
        self.wait(user, name, ["valkey-cli", "--raw", "GET", key], b"synthetic-persistence")
        persistence = self.podman(user, ["exec", name, "valkey-cli", "--raw", "INFO", "persistence"]).stdout.decode()
        require("aof_last_write_status:ok" in persistence and "aof_rewrite_in_progress:0" in persistence, "AOF persistence not ready")
        self.stop(user, name)
        self.remove(user, name)
        self.result["stage"] = "valkey-synthetic-aof-archive"
        self.podman(user, ["unshare", "chgrp", "1000", str(original / "appendonlydir/appendonly.aof.manifest")])
        archive = self.workspaces[user] / "synthetic-aof.tar"
        with os.fdopen(os.open(archive, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb") as output:
            self.podman(user, ["unshare", "tar", "--numeric-owner", "--format=pax", "-C", str(original), "-cf", "-", "."], output=output)
        entries = verify_aof_archive(archive)
        require(any(item["gid"] == 1000 for item in entries.values()), "Mixed namespace group proof missing")
        restored = self.data_directory("valkey-restored")
        self.podman(user, ["unshare", "tar", "--numeric-owner", "--same-owner", "--same-permissions", "-C", str(restored), "-xf", "-"], data=archive.read_bytes())
        restored_archive = self.workspaces[user] / "restored-aof.tar"
        with os.fdopen(os.open(restored_archive, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb") as output:
            self.podman(user, ["unshare", "tar", "--numeric-owner", "--format=pax", "-C", str(restored), "-cf", "-", "."], output=output)
        require(verify_aof_archive(restored_archive) == entries, "Restored AOF bytes or namespace ownership differ")
        self.check_synthetic_aof(restored, entries)
        self.result["stage"] = "valkey-synthetic-aof-restart"
        user, name = self.start("llunde-valkey", "valkey-restored", 192, ["--user=999:999", "--cap-drop=ALL", "--entrypoint=valkey-server", "--volume", f"{restored}:/data"], ["--dir", "/data", "--save", "", "--appendonly", "yes", "--appendfsync", "everysec", "--aof-load-truncated", "no", "--maxmemory", "64mb"])
        self.wait(user, name, ["valkey-cli", "--raw", "GET", key], b"synthetic-persistence")
        ttl = int(self.podman(user, ["exec", name, "valkey-cli", "TTL", key]).stdout)
        require(0 < ttl <= 1800, "Restored synthetic TTL missing")
        self.stop(user, name)
        self.remove(user, name)
        self.result["valkey"] = {"nativeRdbRestored": True, "rdbKeyspace": keyspace, "syntheticAofRestored": True, "nativeAofCheckPreservedFiles": True, "ttlSeconds": ttl, "archiveSHA256": hashlib.sha256(archive.read_bytes()).hexdigest(), "namespaceOwnership": entries, "liveProductionAofCopied": False}

    def check_synthetic_aof(self, restored, entries):
        self.result["stage"] = "valkey-synthetic-aof-native-check"
        user = "llunde-backend"
        require(restored == self.workspaces[user] / "valkey-restored", "Only restored synthetic AOF may be checked")
        check_name = f"infra-rehearsal-{self.token}-aof-check"
        self.containers.append((user, check_name))
        arguments = container_arguments(check_name, self.manifest["images"]["llunde-valkey"], 192, ["--rm", "--user=999:999", "--cap-drop=ALL", "--entrypoint=valkey-check-aof", "--volume", f"{restored}:/data"])
        arguments.remove("--detach")
        self.podman(user, arguments + ["/data/appendonlydir/appendonly.aof.manifest"])
        self.containers.remove((user, check_name))
        checked_archive = self.workspaces[user] / "checked-aof.tar"
        with os.fdopen(os.open(checked_archive, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), "wb") as output:
            self.podman(user, ["unshare", "tar", "--numeric-owner", "--format=pax", "-C", str(restored), "-cf", "-", "."], output=output)
        require(verify_aof_archive(checked_archive) == entries, "Native AOF checker changed bytes, modes or namespace ownership")

    def http(self, user, name, port, path, headers, expected_status, body=None, html=False):
        pid = int(self.podman(user, ["inspect", "--format", "{{.State.Pid}}", name]).stdout)
        require(pid > 1, "Isolated network namespace missing")
        query = {"port": port, "path": path, "headers": headers, "expectedBody": body, "html": html}
        response = self.call(["nsenter", f"--net=/proc/{pid}/ns/net", "/usr/bin/python3", "-I", "-c", HTTP_PROBE, json.dumps(query)])
        receipt = json.loads(response.stdout)
        require(receipt["status"] == expected_status, "Isolated HTTP route mismatch")
        return receipt

    def web(self):
        self.result["stage"] = "frontend-image"
        user, name = self.start("llunde-frontend", "frontend", 256, [])
        for _ in range(30):
            try:
                receipt = self.http(user, name, 8080, "/", {"Host": "llunde.no"}, 200, html=True)
                break
            except RehearsalError:
                time.sleep(0.5)
        else:
            raise RehearsalError("Isolated frontend unavailable")
        self.remove(user, name)
        self.result["frontend"] = receipt | {"network": "none", "hostPorts": False}
        self.result["stage"] = "caddy-synthetic-routing"
        user = "edge"
        config = self.workspaces[user] / "Caddyfile"
        config.write_bytes((self.inputs / "Caddyfile").read_bytes())
        os.chown(config, USERS[user], USERS[user])
        config.chmod(0o400)
        mock = self.workspaces[user] / "Mockfile"
        mock.write_text(MOCK_CADDY)
        os.chown(mock, USERS[user], USERS[user])
        mock.chmod(0o400)
        user, name = self.start("caddy", "caddy", 256, ["--cap-drop=ALL", "--volume", f"{config}:/rehearsal/Caddyfile:ro", "--volume", f"{mock}:/rehearsal/Mockfile:ro", "--tmpfs=/data:size=16m", "--tmpfs=/config:size=4m", "--entrypoint=caddy"], ["run", "--config", "/rehearsal/Caddyfile", "--adapter", "caddyfile"])
        self.podman(user, ["exec", "--detach", name, "caddy", "run", "--config", "/rehearsal/Mockfile", "--adapter", "caddyfile"])
        headers = {"Host": "llunde.no", "CF-Connecting-IP": "192.0.2.10"}
        for _ in range(30):
            try:
                self.http(user, name, 8085, "/", headers, 200, "frontend-fixture https 192.0.2.10")
                break
            except RehearsalError:
                time.sleep(0.5)
        else:
            raise RehearsalError("Isolated Caddy unavailable")
        routes = []
        for port, path, host, cf, status, body in [(8085, "/", "llunde.no", False, 400, None), (8085, "/", "unknown.example", True, 404, None), (8085, "/fixture", "www.llunde.no", True, 301, None), (8085, "/health", "api.llunde.no", True, 403, None), (8085, "/ready", "api.llunde.no", True, 403, None), (8085, "/metrics", "api.llunde.no", True, 403, None), (8085, "/fixture", "api.llunde.no", True, 200, "backend-fixture"), (9101, "/metrics", "localhost", False, 200, "backend-fixture"), (9101, "/unexpected", "localhost", False, 403, None)]:
            request_headers = {"Host": host} | ({"CF-Connecting-IP": "192.0.2.10"} if cf else {})
            route = self.http(user, name, port, path, request_headers, status, body)
            if status == 301:
                require(route["location"] == "https://llunde.no/fixture", "Canonical redirect mismatch")
            routes.append({"port": port, "path": path, "host": host, "status": route["status"]})
        self.remove(user, name)
        self.result["caddy"] = {"routes": routes, "candidateSHA256": self.manifest["files"]["Caddyfile"]["sha256"], "syntheticBackends": True, "network": "none", "hostPorts": False}

    def cleanup(self):
        failures, retained_users = [], set()
        for user, name in self.containers:
            try:
                if self.podman(user, ["rm", "--force", "--ignore", "--volumes", name], cleanup=True, timeout=15, check=False).returncode:
                    failures.append(name)
                    retained_users.add(user)
            except RehearsalError:
                failures.append(name)
                retained_users.add(user)
        for user, path in self.workspaces.items():
            if user in retained_users:
                continue
            try:
                require(path == Path(f"/home/{user}/.infra-rehearsal-{self.token}") and not path.is_symlink(), "Cleanup path mismatch")
                shutil.rmtree(path)
            except (OSError, RehearsalError):
                failures.append(user)
        self.result["cleanupFailures"] = failures
        self.result["retainedWorkspaces"] = [str(self.workspaces[user]) for user in sorted(retained_users)]
        self.result["containersRemoved"] = not failures
        return not failures


def run_rehearsal(inputs):
    pilot = Pilot(inputs)
    started = time.monotonic()
    def interrupted(signum, frame):
        raise RehearsalError("Rehearsal interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    try:
        pilot.preflight()
        pilot.result["phase"] = "postgres"
        pilot.postgres()
        pilot.result["phase"] = "valkey"
        pilot.valkey()
        pilot.result["phase"] = "web"
        pilot.web()
        pilot.result["phase"] = "complete"
        pilot.result["passed"] = True
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError, tarfile.TarError) as error:
        pilot.result["error"] = "Isolated rehearsal failed; no production activation"
        pilot.result["reason"] = str(error) if isinstance(error, RehearsalError) else type(error).__name__
        pilot.record_failure()
    finally:
        if not pilot.cleanup():
            pilot.result["passed"] = False
        pilot.result["elapsedSeconds"] = round(time.monotonic() - started, 3)
        if pilot.report:
            (pilot.report / "result.json").write_text(json.dumps(pilot.result, indent=2) + "\n")
    return pilot.result


def main(argv=None):
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    prepare = commands.add_parser("prepare")
    prepare.add_argument("candidate")
    prepare.add_argument("source")
    prepare.add_argument("destination")
    run = commands.add_parser("run")
    run.add_argument("inputs")
    args = parser.parse_args(argv)
    os.umask(0o077)
    try:
        result = prepare_inputs(args.candidate, args.source, args.destination) if args.command == "prepare" else run_rehearsal(args.inputs)
        print(json.dumps(result, indent=2))
        return 0 if result.get("passed", True) else 1
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
        print(json.dumps({"error": "Rehearsal preparation or validation failed"}))
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
