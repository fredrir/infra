import argparse
import base64
import hashlib
import json
import os
import re
import resource
import select
import shlex
import shutil
import socket
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
from pathlib import Path

from evacuation_secret_relay import (
    METADATA_FIELDS,
    private_command,
    source_program,
    validate_interpreter,
)

REPOSITORY = "s3:s3.eu-north-1.amazonaws.com/llunde-pyparser-bucket/restic/llunde-01"
SOURCE_FILES = {"environment": ("restic-env", 0), "password": ("restic-password", 0)}
MAX_ARCHIVE = 2 * 1024**3
TARGET_RECIPIENT = "age1yqfxvrr3n48krcjjfes5syqz6pfwgmkr776hjqree8vlzuktmaws539jq9"


class BackupError(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise BackupError(message)


def private_directory(path):
    path = Path(path).absolute()
    info = path.lstat()
    require(
        stat.S_ISDIR(info.st_mode)
        and info.st_uid == os.geteuid()
        and stat.S_IMODE(info.st_mode) == 0o700,
        "Private owned directory required",
    )
    return path


def open_private(path, maximum=65536):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(descriptor)
        require(
            stat.S_ISREG(info.st_mode)
            and info.st_uid == os.geteuid()
            and stat.S_IMODE(info.st_mode) in {0o400, 0o600}
            and info.st_nlink == 1
            and 0 < info.st_size <= maximum,
            "Private regular file required",
        )
        return os.fdopen(descriptor, "rb")
    except BaseException:
        os.close(descriptor)
        raise


def read_json(path):
    with open_private(path) as source:
        return json.load(source)


def write_private(path, value):
    with os.fdopen(
        os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), "wb"
    ) as stream:
        stream.write(value)
        stream.flush()
        os.fsync(stream.fileno())


def digest(path, maximum=MAX_ARCHIVE):
    with open_private(path, maximum) as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def source_credentials(document):
    require(
        document["schemaVersion"] == 1
        and document["hostname"] == "llunde-01"
        and re.fullmatch(r"/run/secrets[.]d/[1-9][0-9]{0,9}", document["generation"]),
        "Source runtime identity differs",
    )
    require(
        set(document["files"]) == set(SOURCE_FILES),
        "Exact backup source files required",
    )
    data, records = {}, {}
    for name, (filename, owner) in SOURCE_FILES.items():
        record = document["files"][name]
        info = record["metadata"]
        require(
            set(info) == METADATA_FIELDS
            and info["uid"] == owner
            and info["mode"] == "0400"
            and info["links"] == 1
            and 0 < info["size"] <= 16384,
            "Source backup ownership differs",
        )
        require(
            record["path"] == "/run/secrets/" + filename
            and record["resolvedPath"] == document["generation"] + "/" + filename,
            "Source backup path differs",
        )
        raw = base64.b64decode(record["data"], validate=True)
        require(len(raw) == info["size"], "Source backup size differs")
        data[name] = raw.decode()
        records[name] = {
            key: record[key] for key in ["path", "resolvedPath", "metadata"]
        }
    environment = {}
    for line in data["environment"].splitlines():
        if not line.strip():
            continue
        words = shlex.split(line)
        require(
            len(words) == 1 and "=" in words[0], "Literal backup environment required"
        )
        key, value = words[0].split("=", 1)
        require(key not in environment, "Duplicate backup environment key")
        environment[key] = value
    required_keys = {"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}
    require(
        required_keys <= set(environment) <= required_keys | {"AWS_DEFAULT_REGION"},
        "Exact AWS backup keys required",
    )
    require(
        environment.get("AWS_DEFAULT_REGION", "eu-north-1") == "eu-north-1",
        "Source backup region differs",
    )
    password = data["password"].rstrip("\n")
    require(
        16 <= len(password) <= 1024 and all(32 <= ord(c) < 127 for c in password),
        "Backup password format invalid",
    )
    require(
        re.fullmatch(r"AKIA[A-Z0-9]{16}", environment["AWS_ACCESS_KEY_ID"])
        and re.fullmatch(r"[A-Za-z0-9/+]{40}", environment["AWS_SECRET_ACCESS_KEY"]),
        "Long-term scoped AWS credentials required",
    )
    return {
        "schemaVersion": 1,
        "repository": REPOSITORY,
        "region": "eu-north-1",
        "password": password,
        **{key: environment[key] for key in required_keys},
    }, {
        "type": "runtime-ssh",
        "source": "fredrir-05",
        "capturedAt": document["capturedAt"],
        "generation": document["generation"],
        "files": records,
        "repositorySecretProvenance": False,
    }


def validate_credentials(value):
    require(
        set(value)
        == {
            "schemaVersion",
            "repository",
            "region",
            "password",
            "AWS_ACCESS_KEY_ID",
            "AWS_SECRET_ACCESS_KEY",
        },
        "Exact backup credentials required",
    )
    require(
        value["schemaVersion"] == 1
        and value["repository"] == REPOSITORY
        and value["region"] == "eu-north-1",
        "Approved backup repository required",
    )
    require(
        re.fullmatch(r"AKIA[A-Z0-9]{16}", value["AWS_ACCESS_KEY_ID"])
        and re.fullmatch(r"[A-Za-z0-9/+]{40}", value["AWS_SECRET_ACCESS_KEY"]),
        "Scoped AWS credential format invalid",
    )
    require(
        isinstance(value["password"], str)
        and 16 <= len(value["password"]) <= 1024
        and all(32 <= ord(c) < 127 for c in value["password"]),
        "Backup password format invalid",
    )
    return value


def prepare_credentials(
    destination,
    recipient,
    source_python,
    expected_arn,
    *,
    reader=None,
    command=private_command,
):
    require(
        recipient != TARGET_RECIPIENT and re.fullmatch(r"age1[0-9a-z]{58}", recipient),
        "Distinct administrator recovery recipient required",
    )
    require(
        re.fullmatch(r"arn:aws:iam::[0-9]{12}:user/restic-llunde-01", expected_arn),
        "Verified backup principal required",
    )
    destination = Path(destination).absolute()
    private_directory(destination.parent)
    require(
        not destination.exists() and not destination.is_symlink(),
        "Fresh credential bundle required",
    )
    validate_interpreter(source_python)
    program = source_program(files=SOURCE_FILES)
    if reader is None:
        invocation = shlex.join(
            [
                "env",
                "-i",
                "PATH=/run/current-system/sw/bin:/usr/bin:/bin",
                source_python,
                "-I",
                "-c",
                program,
            ]
        )
        raw = command(
            [
                "ssh",
                "-T",
                "-o",
                "StrictHostKeyChecking=yes",
                "-o",
                "BatchMode=yes",
                "-o",
                "ForwardAgent=no",
                "-o",
                "ClearAllForwardings=yes",
                "-o",
                "ConnectTimeout=10",
                "fredrir-05",
                'if [ "$(id -u)" = 0 ]; then exec '
                + invocation
                + "; else exec sudo -n "
                + invocation
                + "; fi",
            ],
            b"",
            ssh=True,
        )
        document = json.loads(raw)
    else:
        document = reader()
    credentials, provenance = source_credentials(document)
    validate_credentials(credentials)
    environment = {
        "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
        "AWS_ACCESS_KEY_ID": credentials["AWS_ACCESS_KEY_ID"],
        "AWS_SECRET_ACCESS_KEY": credentials["AWS_SECRET_ACCESS_KEY"],
        "AWS_DEFAULT_REGION": "eu-north-1",
        "AWS_EC2_METADATA_DISABLED": "true",
        "AWS_MAX_ATTEMPTS": "1",
    }
    response = subprocess.run(
        [
            "aws",
            "sts",
            "get-caller-identity",
            "--output",
            "json",
            "--no-cli-pager",
            "--cli-connect-timeout",
            "5",
            "--cli-read-timeout",
            "10",
        ],
        capture_output=True,
        timeout=20,
        env=environment,
    )
    require(
        response.returncode == 0 and json.loads(response.stdout)["Arn"] == expected_arn,
        "Source credential principal differs",
    )
    encrypted = command(
        ["age", "--recipient", TARGET_RECIPIENT, "--recipient", recipient],
        json.dumps(credentials).encode(),
    )
    require(
        encrypted.startswith(b"age-encryption.org/v1\n"),
        "Encrypted credential output required",
    )
    manifest = {
        "schemaVersion": 1,
        "kind": "evacuation-backup-credentials",
        "repository": REPOSITORY,
        "recipients": [TARGET_RECIPIENT, recipient],
        "principal": expected_arn,
        "source": provenance,
        "ciphertextSHA256": hashlib.sha256(encrypted).hexdigest(),
    }
    destination.mkdir(mode=0o700)
    try:
        write_private(destination / "credentials.age", encrypted)
        write_private(
            destination / "manifest.json",
            (json.dumps(manifest, indent=2) + "\n").encode(),
        )
    except BaseException:
        shutil.rmtree(destination)
        raise
    return {
        "repository": REPOSITORY,
        "principal": expected_arn,
        "recipients": manifest["recipients"],
        "credentialDelivery": False,
    }


def decrypt_credentials(bundle, identity, *, command=private_command):
    bundle = private_directory(bundle)
    manifest = read_json(bundle / "manifest.json")
    require(
        manifest["schemaVersion"] == 1
        and manifest["kind"] == "evacuation-backup-credentials"
        and manifest["repository"] == REPOSITORY,
        "Credential manifest differs",
    )
    require(
        digest(bundle / "credentials.age", 65536) == manifest["ciphertextSHA256"],
        "Credential ciphertext changed",
    )
    with open_private(identity) as source:
        pass
    raw = command(
        [
            "age",
            "--decrypt",
            "--identity",
            str(identity),
            str(bundle / "credentials.age"),
        ],
        b"",
    )
    return validate_credentials(json.loads(raw))


def validate_bundle(directory):
    directory = private_directory(directory)
    manifest = read_json(directory / "manifest.json")
    kind = manifest.get("kind")
    if kind == "evacuation-paired-state":
        from evacuation_cutover import verify_pair

        verify_pair(directory)
        names = {"manifest.json", "database.dump", "valkey.tar"}
    elif kind == "isolated-target-rehearsal":
        names = {"manifest.json", "database.dump", "dump.rdb", "Caddyfile"}
        require(
            manifest["schemaVersion"] == 1
            and manifest["target"] == "fredrir-09"
            and set(manifest["files"]) == names - {"manifest.json"},
            "Rehearsal manifest differs",
        )
        for name, metadata in manifest["files"].items():
            require(
                digest(directory / name) == metadata["sha256"],
                "Rehearsal file checksum differs",
            )
    elif kind == "evacuation-online-state":
        from evacuation_recurring_backup import validate_online

        validate_online(directory, manifest)
        names = {"manifest.json", "database.dump", "dump.rdb"}
    else:
        raise BackupError("Approved recovery bundle required")
    require(
        {p.name for p in directory.iterdir()} == names,
        "Unexpected recovery bundle files",
    )
    files = {
        name: {
            "sha256": digest(directory / name),
            "bytes": (directory / name).stat().st_size,
        }
        for name in sorted(names)
    }
    require(
        sum(value["bytes"] for value in files.values()) < MAX_ARCHIVE - 65536,
        "Recovery bundle exceeds budget",
    )
    return {"kind": kind, "files": files}


class Restic:
    def __init__(self, credentials):
        value = validate_credentials(credentials)
        self.deadline = time.monotonic() + 900
        self.environment = {
            "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
            "RESTIC_REPOSITORY": REPOSITORY,
            "RESTIC_PASSWORD": value["password"],
            "AWS_ACCESS_KEY_ID": value["AWS_ACCESS_KEY_ID"],
            "AWS_SECRET_ACCESS_KEY": value["AWS_SECRET_ACCESS_KEY"],
            "AWS_DEFAULT_REGION": "eu-north-1",
            "AWS_EC2_METADATA_DISABLED": "true",
            "GOMAXPROCS": "2",
            "GOMEMLIMIT": "384MiB",
        }

    def call(self, arguments, *, source=None, output=None, maximum=262144):
        require(
            arguments and arguments[0] in {"cat", "backup", "dump", "version"},
            "Backup operation forbidden",
        )
        require(
            type(maximum) is int and 0 < maximum <= MAX_ARCHIVE,
            "Output budget required",
        )
        process = None
        try:
            process = subprocess.Popen(
                [
                    "restic",
                    "--no-cache",
                    "-o",
                    "s3.region=eu-north-1",
                    "-o",
                    "s3.connections=2",
                    "--limit-upload=10240",
                    "--limit-download=10240",
                    *arguments,
                ],
                stdin=source,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                env=self.environment,
            )
            deadline, total, chunks = self.deadline, 0, []
            while True:
                require(time.monotonic() < deadline, "Restic time budget exceeded")
                ready, _, _ = select.select([process.stdout], [], [], 1)
                if not ready:
                    continue
                chunk = os.read(process.stdout.fileno(), 65536)
                if not chunk:
                    break
                total += len(chunk)
                require(total <= maximum, "Restic output exceeds budget")
                if output is None:
                    chunks.append(chunk)
                else:
                    output.write(chunk)
            require(
                process.wait(timeout=max(1, deadline - time.monotonic())) == 0,
                "Restic operation failed; sensitive output withheld",
            )
            return b"".join(chunks)
        except (OSError, subprocess.SubprocessError):
            raise BackupError("Bounded Restic operation failed") from None
        finally:
            if process is not None:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=5)
                process.stdout.close()

    def identity(self):
        config = json.loads(self.call(["cat", "config"]))
        require(
            re.fullmatch(r"[a-f0-9]{64}", config["id"]) and config["version"] in {1, 2},
            "Existing Restic repository required",
        )
        return config["id"]


def require_upload_host():
    require(
        os.geteuid() == 0 and socket.gethostname() == "cloud-server-10643982",
        "Verified target09 required for upload",
    )


def backup(directory, restic, work_parent):
    require_upload_host()
    before = validate_bundle(directory)
    repository_id = restic.identity()
    with tempfile.TemporaryDirectory(
        prefix="restic-upload-", dir=private_directory(work_parent)
    ) as temporary:
        archive_path = Path(temporary) / "evacuation-state.tar"
        with (
            os.fdopen(
                os.open(archive_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb"
            ) as stream,
            tarfile.open(fileobj=stream, mode="w") as archive,
        ):
            for name in before["files"]:
                with open_private(Path(directory) / name, MAX_ARCHIVE) as source:
                    entry = tarfile.TarInfo(name)
                    entry.size, entry.mode = (
                        os.fstat(source.fileno()).st_size,
                        0o600,
                    )
                    archive.addfile(entry, source)
        require(
            validate_bundle(directory) == before,
            "Recovery bundle changed during archive creation",
        )
        archive_hash = digest(archive_path)
        tag = "evacuation-" + before["kind"]
        with open_private(archive_path, MAX_ARCHIVE) as source:
            rows = [
                json.loads(line)
                for line in restic.call(
                    [
                        "backup",
                        "--stdin",
                        "--stdin-filename",
                        "evacuation-state.tar",
                        "--host",
                        "fredrir-09",
                        "--tag",
                        tag,
                        "--json",
                    ],
                    source=source,
                ).splitlines()
                if line
            ]
        summaries = [row for row in rows if row.get("message_type") == "summary"]
        require(
            len(summaries) == 1
            and re.fullmatch(r"[a-f0-9]{64}", summaries[0].get("snapshot_id", "")),
            "Complete backup snapshot required",
        )
        return {
            "schemaVersion": 1,
            "kind": "evacuation-offhost-backup",
            "repository": REPOSITORY,
            "repositoryId": repository_id,
            "snapshotId": summaries[0]["snapshot_id"],
            "archiveSHA256": archive_hash,
            "archiveBytes": archive_path.stat().st_size,
            "bundle": before,
            "createdAt": int(time.time()),
            "host": "fredrir-09",
            "tag": tag,
            "independentRestoreVerified": False,
        }


def restore(receipt, destination, restic):
    require(
        receipt["schemaVersion"] == 1
        and receipt["kind"] == "evacuation-offhost-backup"
        and receipt["repository"] == REPOSITORY
        and receipt["host"] == "fredrir-09",
        "Backup receipt differs",
    )
    require(
        re.fullmatch(r"[a-f0-9]{64}", receipt["snapshotId"])
        and re.fullmatch(r"[a-f0-9]{64}", receipt["archiveSHA256"])
        and type(receipt["archiveBytes"]) is int
        and 0 < receipt["archiveBytes"] <= MAX_ARCHIVE,
        "Exact bounded snapshot required",
    )
    contracts = {
        "evacuation-paired-state": {"manifest.json", "database.dump", "valkey.tar"},
        "isolated-target-rehearsal": {
            "manifest.json",
            "database.dump",
            "dump.rdb",
            "Caddyfile",
        },
        "evacuation-online-state": {"manifest.json", "database.dump", "dump.rdb"},
    }
    require(
        receipt["bundle"]["kind"] in contracts
        and set(receipt["bundle"]["files"]) == contracts[receipt["bundle"]["kind"]]
        and receipt["tag"] == "evacuation-" + receipt["bundle"]["kind"],
        "Exact recovery file contract required",
    )
    require(
        all(
            re.fullmatch(r"[a-f0-9]{64}", value["sha256"])
            and type(value["bytes"]) is int
            and 0 < value["bytes"] < MAX_ARCHIVE
            for value in receipt["bundle"]["files"].values()
        ),
        "Recovery file bounds invalid",
    )
    require(
        restic.identity() == receipt["repositoryId"],
        "Restic repository identity differs",
    )
    snapshot = json.loads(restic.call(["cat", "snapshot", receipt["snapshotId"]]))
    require(
        snapshot["hostname"] == "fredrir-09"
        and snapshot["paths"] == ["/evacuation-state.tar"]
        and receipt["tag"] in snapshot["tags"],
        "Snapshot source metadata differs",
    )
    destination = Path(destination).absolute()
    private_directory(destination.parent)
    require(
        not destination.exists() and not destination.is_symlink(),
        "Fresh restore destination required",
    )
    destination.mkdir(mode=0o700)
    try:
        archive_path = destination / "restic-archive.tar"
        descriptor = os.open(archive_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "wb") as output:
            restic.call(
                ["dump", receipt["snapshotId"], "/evacuation-state.tar"],
                output=output,
                maximum=receipt["archiveBytes"],
            )
        require(
            archive_path.stat().st_size == receipt["archiveBytes"]
            and digest(archive_path) == receipt["archiveSHA256"],
            "Restored archive differs",
        )
        seen = set()
        with tarfile.open(archive_path, "r:") as archive:
            for member in archive:
                require(
                    member.name in receipt["bundle"]["files"]
                    and member.name not in seen
                    and member.isfile()
                    and member.size == receipt["bundle"]["files"][member.name]["bytes"],
                    "Unsafe recovery archive",
                )
                seen.add(member.name)
                descriptor = os.open(
                    destination / member.name,
                    os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                    0o600,
                )
                with (
                    archive.extractfile(member) as source,
                    os.fdopen(descriptor, "wb") as output,
                ):
                    shutil.copyfileobj(source, output, 1024**2)
        require(
            seen == set(receipt["bundle"]["files"]), "Recovery archive is incomplete"
        )
        archive_path.unlink()
        require(
            validate_bundle(destination) == receipt["bundle"], "Restored bundle differs"
        )
        return {
            "schemaVersion": 1,
            "kind": "evacuation-independent-restore",
            "snapshotId": receipt["snapshotId"],
            "archiveSHA256": receipt["archiveSHA256"],
            "bundle": receipt["bundle"],
            "verified": True,
            "operatorHost": socket.gethostname(),
            "independentHost": socket.gethostname() != "cloud-server-10643982",
            "applicationRestoreVerified": False,
        }
    except BaseException:
        shutil.rmtree(destination)
        raise


def main():
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="action", required=True)
    prepare = commands.add_parser("prepare-credentials")
    prepare.add_argument("destination", type=Path)
    prepare.add_argument("--recovery-recipient", required=True)
    prepare.add_argument("--source-python", required=True)
    prepare.add_argument("--expected-principal", required=True)
    for name in ["backup", "restore", "inspect-credentials"]:
        command = commands.add_parser(name)
        command.add_argument("--credentials", type=Path, required=True)
        command.add_argument("--identity", type=Path, required=True)
        if name != "inspect-credentials":
            command.add_argument("source", type=Path)
            command.add_argument("--output", type=Path, required=True)
        if name == "backup":
            command.add_argument("--work-parent", type=Path, required=True)
        if name == "restore":
            command.add_argument("--destination", type=Path, required=True)
    arguments = parser.parse_args()
    os.umask(0o077)
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    if arguments.action == "prepare-credentials":
        result = prepare_credentials(
            arguments.destination,
            arguments.recovery_recipient,
            arguments.source_python,
            arguments.expected_principal,
        )
    else:
        restic = Restic(decrypt_credentials(arguments.credentials, arguments.identity))
        if arguments.action == "inspect-credentials":
            result = {
                "repository": REPOSITORY,
                "repositoryId": restic.identity(),
                "credentialDelivery": False,
            }
        else:
            private_directory(arguments.output.parent)
            require(
                not arguments.output.exists() and not arguments.output.is_symlink(),
                "Fresh output receipt required",
            )
            result = (
                backup(arguments.source, restic, arguments.work_parent)
                if arguments.action == "backup"
                else restore(read_json(arguments.source), arguments.destination, restic)
            )
            write_private(
                arguments.output, (json.dumps(result, indent=2) + "\n").encode()
            )
    print(
        json.dumps(
            {
                key: value
                for key, value in result.items()
                if key not in {"bundle", "recipients"}
            }
        )
    )


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
        print("Backup operation failed; sensitive details withheld", file=sys.stderr)
        raise SystemExit(1)
