import hashlib
import json
import os
import re
import stat
import sys
from itertools import islice
from pathlib import Path


def require(value):
    if not value:
        raise ValueError("Reviewed private deployment differs")


def verify(directory, expected):
    require(
        os.geteuid() == 0
        and re.fullmatch(
            r"/var/lib/infra-evacuation/llunde/recurring-backup-deploy-[0-9TZ]+",
            directory,
        )
    )
    return verify_tree(directory, expected)


def verify_tree(directory, expected, owner=0):
    descriptor = os.open("/", os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in Path(directory).parts[1:]:
            following = os.open(
                part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor
            )
            os.close(descriptor)
            descriptor = following
            info = os.fstat(descriptor)
            require(
                info.st_uid in (0, owner) and stat.S_IMODE(info.st_mode) & 0o022 == 0
            )
        require(stat.S_IMODE(os.fstat(descriptor).st_mode) == 0o700)

        def read(name):
            require(re.fullmatch(r"(?:helpers/)?[a-zA-Z0-9_.-]+", name))
            parent = os.dup(descriptor)
            try:
                parts = name.split("/")
                if len(parts) == 2:
                    nested = os.open(
                        parts[0],
                        os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                        dir_fd=parent,
                    )
                    os.close(parent)
                    parent = nested
                    info = os.fstat(parent)
                    require(
                        info.st_uid == owner and stat.S_IMODE(info.st_mode) == 0o700
                    )
                fd = os.open(
                    parts[-1],
                    os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK,
                    dir_fd=parent,
                )
                with os.fdopen(fd, "rb") as stream:
                    info = os.fstat(stream.fileno())
                    require(
                        stat.S_ISREG(info.st_mode)
                        and info.st_uid == owner
                        and info.st_nlink == 1
                        and stat.S_IMODE(info.st_mode) == 0o600
                        and 0 < info.st_size <= 1024**2
                    )
                    return stream.read(1024**2 + 1)
            finally:
                os.close(parent)

        raw = read("manifest.json")
        require(hashlib.sha256(raw).hexdigest() == expected)
        manifest = json.loads(raw)
        require(
            manifest["schemaVersion"] == 1
            and manifest["kind"] == "recurring-backup-deployment"
            and manifest["remoteDirectory"] == directory
            and 1 <= len(manifest["files"]) <= 40
        )
        actual = set()
        entries = list(islice(Path(directory).rglob("*"), 43))
        require(len(entries) <= 42)
        for path in entries:
            name = str(path.relative_to(directory))
            info = path.lstat()
            require(not stat.S_ISLNK(info.st_mode))
            if stat.S_ISDIR(info.st_mode):
                require(
                    name == "helpers"
                    and info.st_uid == owner
                    and stat.S_IMODE(info.st_mode) == 0o700
                )
            else:
                require(stat.S_ISREG(info.st_mode))
                actual.add(name)
        require(actual == set(manifest["files"]) | {"manifest.json"})
        for name, wanted in manifest["files"].items():
            data = read(name)
            require(
                len(data) == wanted["bytes"]
                and hashlib.sha256(data).hexdigest() == wanted["sha256"]
            )
        return manifest
    finally:
        os.close(descriptor)


if __name__ == "__main__":
    verify(*sys.argv[1:])
    print("Reviewed deployment verified")
