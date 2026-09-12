import argparse
import base64
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import resource
import shlex
import shutil
import stat
import subprocess

from evacuation_staging import SECRETS, StagingError, validate_environment


SOURCE_FILES = {
    "doppler": ("doppler-token", 2001),
    "database": ("llunde-backend-db-env", 2001),
    "tunnel": ("llunde-tunnel", 2000),
}
MAX_RESPONSE = 100000
METADATA_FIELDS = {"uid", "gid", "mode", "device", "inode", "size", "mtimeNs", "ctimeNs", "links"}


def require(condition, message):
    if not condition:
        raise StagingError(message)


def source_program(runtime_root="/run", root_uid=0, root_gid=0, directory_gid=96, hostname="llunde-01", owners=None):
    files = SOURCE_FILES if owners is None else {name: (filename, owners[name]) for name, (filename, _) in SOURCE_FILES.items()}
    return f"RUNTIME_ROOT = {runtime_root!r}\nROOT_UID = {root_uid!r}\nROOT_GID = {root_gid!r}\nDIRECTORY_GID = {directory_gid!r}\nHOSTNAME = {hostname!r}\nFILES = {files!r}\n" + '''import base64
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import resource
import socket
import stat
import sys

def require(condition):
    if not condition:
        raise ValueError('source contract')

def metadata(info):
    return {'uid': info.st_uid, 'gid': info.st_gid, 'mode': format(stat.S_IMODE(info.st_mode), '04o'), 'device': info.st_dev, 'inode': info.st_ino, 'size': info.st_size, 'mtimeNs': info.st_mtime_ns, 'ctimeNs': info.st_ctime_ns, 'links': info.st_nlink}

descriptors = []
try:
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    require(os.geteuid() == ROOT_UID and socket.gethostname() == HOSTNAME)
    root = Path(RUNTIME_ROOT)
    root_fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    descriptors.append(root_fd)
    info = os.fstat(root_fd)
    require(info.st_uid == ROOT_UID and stat.S_IMODE(info.st_mode) == 0o755)
    link = os.stat('secrets', dir_fd=root_fd, follow_symlinks=False)
    require(stat.S_ISLNK(link.st_mode) and link.st_uid == ROOT_UID and link.st_gid == ROOT_GID and link.st_nlink == 1)
    target = os.readlink('secrets', dir_fd=root_fd)
    require(re.fullmatch(re.escape(str(root)) + r'/secrets[.]d/[1-9][0-9]{0,9}', target) is not None)
    generation = target.rsplit('/', 1)[1]
    parent_fd = os.open('secrets.d', os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=root_fd)
    descriptors.append(parent_fd)
    generation_fd = os.open(generation, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent_fd)
    descriptors.append(generation_fd)
    directories = {}
    for name, fd in [('secrets.d', parent_fd), ('generation', generation_fd)]:
        info = os.fstat(fd)
        require(info.st_uid == ROOT_UID and info.st_gid == DIRECTORY_GID and stat.S_IMODE(info.st_mode) == 0o751)
        directories[name] = metadata(info)
    records = {}
    opened = []
    for name, (filename, owner) in FILES.items():
        fd = os.open(filename, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=generation_fd)
        descriptors.append(fd)
        before = os.fstat(fd)
        require(stat.S_ISREG(before.st_mode) and before.st_uid == owner and stat.S_IMODE(before.st_mode) == 0o400 and before.st_nlink == 1 and 0 < before.st_size <= 16384)
        data = os.read(fd, 16385)
        require(len(data) == before.st_size and metadata(before) == metadata(os.fstat(fd)))
        opened.append((filename, fd, metadata(before)))
        records[name] = {'path': str(root / 'secrets' / filename), 'resolvedPath': target + '/' + filename, 'metadata': metadata(before), 'data': base64.b64encode(data).decode('ascii')}
    for filename, fd, before in opened:
        require(before == metadata(os.fstat(fd)) == metadata(os.stat(filename, dir_fd=generation_fd, follow_symlinks=False)))
    require(metadata(link) == metadata(os.stat('secrets', dir_fd=root_fd, follow_symlinks=False)) and target == os.readlink('secrets', dir_fd=root_fd))
    require(directories['generation'] == metadata(os.stat(generation, dir_fd=parent_fd, follow_symlinks=False)))
    require(directories['secrets.d'] == metadata(os.stat('secrets.d', dir_fd=root_fd, follow_symlinks=False)))
    response = {'schemaVersion': 1, 'hostname': HOSTNAME, 'capturedAt': datetime.now(timezone.utc).isoformat(), 'generation': target, 'directories': directories, 'files': records}
    sys.stdout.write(json.dumps(response))
except (OSError, ValueError, TypeError):
    sys.stderr.write('Runtime source validation failed\\n')
    sys.exit(1)
finally:
    for fd in descriptors:
        os.close(fd)
'''


def child_environment(environment, *, ssh=False):
    allowed = {"PATH", "LANG", "LC_ALL"}
    if ssh:
        allowed |= {"HOME", "USER", "LOGNAME", "SSH_AUTH_SOCK"}
    return {key: value for key, value in environment.items() if key in allowed}


def private_command(argv, data, *, runner=subprocess.run, environment=None, ssh=False):
    environment = os.environ if environment is None else environment
    try:
        result = runner(argv, input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30, env=child_environment(environment, ssh=ssh))
    except (OSError, subprocess.SubprocessError):
        raise StagingError("Private relay command failed") from None
    require(result.returncode == 0 and len(result.stdout) <= MAX_RESPONSE, "Private relay command failed")
    return result.stdout


def validate_interpreter(source_python):
    require(source_python == "python3" or re.fullmatch(r"/nix/store/[0-9abcdfghijklmnpqrsvwxyz]{32}-python3-[0-9]+\.[0-9]+\.[0-9]+/bin/python3", source_python), "Reviewed source Python path required")


def read_source(*, source_python="python3", runner=subprocess.run, environment=None):
    validate_interpreter(source_python)
    program = source_program()
    invocation = shlex.join(["env", "-i", "PATH=/run/current-system/sw/bin:/usr/bin:/bin", source_python, "-I", "-c", program])
    command = "if [ \"$(id -u)\" = 0 ]; then exec " + invocation + "; else exec sudo -n " + invocation + "; fi"
    argv = ["ssh", "-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes", "-o", "ConnectTimeout=10", "fredrir-05", command]
    raw = private_command(argv, b"", runner=runner, environment=environment, ssh=True)
    try:
        document = json.loads(raw)
    except (UnicodeError, ValueError):
        raise StagingError("Invalid source metadata") from None
    return document


def decode_source(document):
    require(isinstance(document, dict) and document.get("schemaVersion") == 1 and document.get("hostname") == "llunde-01", "Source identity mismatch")
    generation = document.get("generation")
    require(isinstance(generation, str) and re.fullmatch(r"/run/secrets\.d/[1-9][0-9]{0,9}", generation), "Source generation mismatch")
    require(isinstance(document.get("files"), dict) and set(document["files"]) == set(SOURCE_FILES), "Exact runtime secrets required")
    require(isinstance(document.get("directories"), dict) and set(document["directories"]) == {"secrets.d", "generation"}, "Source directories missing")
    try:
        captured = datetime.fromisoformat(document["capturedAt"])
        require(captured.tzinfo is not None and abs((datetime.now(timezone.utc) - captured).total_seconds()) <= 120, "Source observation stale")
        for info in document["directories"].values():
            require(set(info) == METADATA_FIELDS and info["uid"] == 0 and info["gid"] == 96 and info["mode"] == "0751", "Source directory identity mismatch")
            require(all(type(info[field]) is int and info[field] >= 0 for field in METADATA_FIELDS - {"mode"}), "Invalid directory metadata")
        decoded, records = {}, {}
        for name, (filename, owner) in SOURCE_FILES.items():
            record = document["files"][name]
            require(set(record) == {"path", "resolvedPath", "metadata", "data"}, "Unexpected source fields")
            require(record["path"] == f"/run/secrets/{filename}" and record["resolvedPath"] == f"{generation}/{filename}", "Source path mismatch")
            info = record["metadata"]
            require(set(info) == METADATA_FIELDS, "Unexpected file metadata")
            require(info["uid"] == owner and info["mode"] == "0400" and info["links"] == 1 and type(info["size"]) is int and 0 < info["size"] <= 16384, "Source file identity mismatch")
            require(all(type(info[field]) is int and info[field] >= 0 for field in ["uid", "gid", "device", "inode", "mtimeNs", "ctimeNs"]), "Invalid source metadata")
            plaintext = base64.b64decode(record["data"], validate=True)
            require(len(plaintext) == info["size"], "Source size mismatch")
            validate_environment(plaintext, SECRETS[name][2])
            decoded[name] = plaintext
            records[name] = {key: record[key] for key in ["path", "resolvedPath", "metadata"]}
    except (KeyError, TypeError, UnicodeError, ValueError):
        raise StagingError("Invalid runtime source response") from None
    provenance = {"type": "runtime-ssh", "node": "fredrir-05", "hostname": "llunde-01", "capturedAt": document["capturedAt"], "generation": generation, "directories": document["directories"], "files": records, "readerSHA256": hashlib.sha256(source_program().encode()).hexdigest(), "repositorySecretProvenance": False}
    return decoded, provenance


def prepare_relay(recipient, destination, *, source_python="python3", source_reader=None, runner=subprocess.run, environment=None):
    validate_interpreter(source_python)
    require(re.fullmatch(r"age1[0-9a-z]{58}", recipient) is not None, "Invalid target recipient")
    destination = Path(destination).absolute()
    parent = destination.parent
    info = parent.lstat()
    require(stat.S_ISDIR(info.st_mode) and info.st_uid == os.geteuid() and stat.S_IMODE(info.st_mode) == 0o700, "Private destination parent required")
    require(not destination.exists() and not destination.is_symlink(), "Fresh bundle destination required")
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    decoded, provenance = decode_source(source_reader() if source_reader else read_source(source_python=source_python, runner=runner, environment=environment))
    provenance["interpreter"] = source_python
    ciphertexts = {}
    for name, plaintext in decoded.items():
        ciphertext = private_command(["age", "--recipient", recipient], plaintext, runner=runner, environment=environment)
        require(ciphertext.startswith(b"age-encryption.org/v1\n"), "Invalid encrypted output")
        ciphertexts[f"{name}.age"] = ciphertext
    manifest = {"schemaVersion": 1, "recipient": recipient, "target": "fredrir-09", "source": provenance, "files": {name: {"sha256": hashlib.sha256(data).hexdigest()} for name, data in ciphertexts.items()}}
    destination.mkdir(mode=0o700)
    try:
        for name, data in (ciphertexts | {"manifest.json": (json.dumps(manifest, indent=2) + "\n").encode()}).items():
            with os.fdopen(os.open(destination / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), "wb") as handle:
                handle.write(data)
                handle.flush()
                os.fsync(handle.fileno())
    except BaseException:
        shutil.rmtree(destination)
        raise
    return {"encryptedFiles": len(ciphertexts), "recipient": recipient, "target": "fredrir-09", "source": "fredrir-05 runtime", "repositorySecretProvenance": False, "applicationActivation": False}


def main(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("recipient")
    parser.add_argument("destination")
    parser.add_argument("--source-python", default="python3")
    args = parser.parse_args(argv)
    try:
        result = prepare_relay(args.recipient, args.destination, source_python=args.source_python)
        print(json.dumps(result))
        return 0
    except (StagingError, OSError, ValueError, KeyError, subprocess.SubprocessError):
        print(json.dumps({"error": "Secret relay failed; sensitive details withheld"}))
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
