from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import stat
import tarfile
import time
from pathlib import Path, PurePosixPath

from recovery import (
    RecoveryError,
    digest,
    private_directory,
    private_file,
    write_private,
)

MAX_BYTES = 100 * 1024**3
MAX_ENTRIES = 100000
MAX_MANIFEST_BYTES = 32 * 1024**2


def identity(project, volume):
    if not re.fullmatch(r"[a-z][a-z0-9-]{0,29}", project) or not re.fullmatch(
        r"[a-z][a-z0-9-]{0,19}", volume
    ):
        raise RecoveryError("A concrete project and volume identity is required")


def read_manifest(path):
    private_file(path)
    if path.stat().st_size > MAX_MANIFEST_BYTES:
        raise RecoveryError("Recovery manifest exceeds the size budget")
    return json.loads(path.read_text())


def database_digest(directory):
    private_directory(directory)
    manifest = read_manifest(directory / "manifest.json")
    checksum = digest(private_file(directory / "database.dump"))
    if (
        manifest.get("schemaVersion") != 1
        or manifest.get("kind") != "postgres-logical"
        or manifest.get("sha256") != checksum
        or type(manifest.get("createdAt")) is not int
    ):
        raise RecoveryError("A verified paired PostgreSQL logical backup is required")
    return checksum, manifest["createdAt"]


def safe_name(name):
    path = PurePosixPath(name)
    if (
        not name
        or not path.parts
        or path.is_absolute()
        or str(path) != name
        or ".." in path.parts
        or "\\" in name
        or len(name) > 1024
        or any(ord(character) < 32 for character in name)
    ):
        raise RecoveryError("Volume contains an unsafe or unsupported path")
    return path


def inventory(source, max_bytes):
    if (
        not source.is_absolute()
        or source.resolve() != source
        or not source.is_dir()
        or source == Path("/")
    ):
        raise RecoveryError("Volume source must be an explicit nonsymlink directory")
    device = source.stat().st_dev
    records, size = {}, 0
    for directory, directories, files in os.walk(source, followlinks=False):
        for name in sorted(directories + files):
            path = Path(directory) / name
            relative = path.relative_to(source).as_posix()
            safe_name(relative)
            info = path.lstat()
            if (
                info.st_dev != device
                or not (stat.S_ISDIR(info.st_mode) or stat.S_ISREG(info.st_mode))
                or (stat.S_ISREG(info.st_mode) and info.st_nlink != 1)
            ):
                raise RecoveryError(
                    "Volume export accepts regular files and directories without links or cross-device mounts"
                )
            size += info.st_size if stat.S_ISREG(info.st_mode) else 0
            records[relative] = (
                info.st_ino,
                info.st_mode,
                info.st_size,
                info.st_mtime_ns,
                info.st_ctime_ns,
            )
            if len(records) > MAX_ENTRIES or size > max_bytes:
                raise RecoveryError("Volume exceeds its bounded export budget")
    return records


def export_volume(
    source, destination, database, assertion, project, volume, max_bytes, *, now=None
):
    identity(project, volume)
    if type(max_bytes) is not int or not 1 <= max_bytes <= MAX_BYTES:
        raise RecoveryError("Volume budget must be between one byte and 100 GiB")
    current = int(time.time()) if now is None else now
    evidence = read_manifest(assertion)
    if (
        set(evidence)
        != {
            "schemaVersion",
            "kind",
            "project",
            "volume",
            "verifiedAt",
            "writersStopped",
            "reportSha256",
        }
        or evidence.get("schemaVersion") != 1
        or evidence.get("kind") != "project-volume-drain"
        or evidence.get("project") != project
        or evidence.get("volume") != volume
        or evidence.get("writersStopped") is not True
        or not re.fullmatch(r"[0-9a-f]{64}", evidence.get("reportSha256", ""))
    ):
        raise RecoveryError(
            "A reviewed operator assertion that every volume writer is stopped is required"
        )
    stopped = evidence.get("verifiedAt")
    if type(stopped) is not int or not 0 <= current - stopped <= 3600:
        raise RecoveryError(
            "Writer-drain assertion must be timestamped within the last hour"
        )
    database_sha, database_created = database_digest(database)
    if not stopped <= database_created <= current:
        raise RecoveryError(
            "Paired PostgreSQL backup must be taken after the declared writer drain"
        )
    before = inventory(source, max_bytes)
    if destination.resolve().is_relative_to(source):
        raise RecoveryError("Recovery bundle must be outside the source volume")
    private_directory(destination, create=True)
    entries = []
    try:
        archive = destination / "files.tar"
        with (
            os.fdopen(
                os.open(archive, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb"
            ) as output,
            tarfile.open(fileobj=output, mode="w", format=tarfile.PAX_FORMAT) as target,
        ):
            for name, expected in sorted(before.items()):
                path = source / name
                member = tarfile.TarInfo(name)
                member.mtime = expected[3] // 1000000000
                if stat.S_ISDIR(expected[1]):
                    member.type, member.mode = tarfile.DIRTYPE, 0o700
                    target.addfile(member)
                    entries.append({"path": name, "kind": "directory"})
                else:
                    with os.fdopen(
                        os.open(path, os.O_RDONLY | os.O_NOFOLLOW), "rb"
                    ) as stream:
                        info = os.fstat(stream.fileno())
                        if (
                            info.st_ino,
                            info.st_mode,
                            info.st_size,
                            info.st_mtime_ns,
                            info.st_ctime_ns,
                        ) != expected:
                            raise RecoveryError(
                                "Volume changed while exporting; keep all writers stopped and retry"
                            )
                        checksum = hashlib.file_digest(stream, "sha256").hexdigest()
                        stream.seek(0)
                        member.size, member.mode = info.st_size, 0o600
                        target.addfile(member, stream)
                    entries.append(
                        {
                            "path": name,
                            "kind": "file",
                            "size": member.size,
                            "sha256": checksum,
                        }
                    )
        if inventory(source, max_bytes) != before:
            raise RecoveryError(
                "Volume changed while exporting; keep all writers stopped and retry"
            )
        manifest = {
            "schemaVersion": 1,
            "kind": "project-volume",
            "project": project,
            "volume": volume,
            "createdAt": current,
            "maxBytes": max_bytes,
            "databaseSha256": database_sha,
            "drainAssertion": evidence,
            "archiveSha256": digest(archive),
            "entries": entries,
        }
        encoded = (json.dumps(manifest, sort_keys=True) + "\n").encode()
        if len(encoded) > MAX_MANIFEST_BYTES:
            raise RecoveryError("Volume manifest exceeds the size budget")
        write_private(destination / "manifest.json", encoded)
        return verify_volume(destination, database, project, volume)
    except BaseException:
        shutil.rmtree(destination)
        raise


def verify_volume(directory, database, project, volume):
    identity(project, volume)
    private_directory(directory)
    manifest = read_manifest(directory / "manifest.json")
    if (
        manifest.get("schemaVersion") != 1
        or manifest.get("kind") != "project-volume"
        or manifest.get("project") != project
        or manifest.get("volume") != volume
    ):
        raise RecoveryError("Volume recovery identity does not match")
    maximum = manifest.get("maxBytes")
    if type(maximum) is not int or not 1 <= maximum <= MAX_BYTES:
        raise RecoveryError("Volume recovery budget is invalid")
    archive = private_file(directory / "files.tar")
    if archive.stat().st_size > maximum + MAX_ENTRIES * 4096 + 10240 or manifest.get(
        "archiveSha256"
    ) != digest(archive):
        raise RecoveryError("Volume archive integrity or size verification failed")
    database_sha, _ = database_digest(database)
    if manifest.get("databaseSha256") != database_sha:
        raise RecoveryError("Volume requires its matching PostgreSQL backup")
    entries = manifest.get("entries")
    if not isinstance(entries, list) or len(entries) > MAX_ENTRIES:
        raise RecoveryError("Invalid volume entry inventory")
    expected = {}
    for entry in entries:
        name = str(safe_name(entry["path"]))
        if name in expected or entry.get("kind") not in {"file", "directory"}:
            raise RecoveryError("Duplicate or unsupported volume entry")
        if entry["kind"] == "file" and (
            type(entry.get("size")) is not int or entry["size"] < 0
        ):
            raise RecoveryError("Volume manifest file size must be nonnegative")
        expected[name] = entry
    seen, total = set(), 0
    with tarfile.open(archive, "r:") as source:
        for member in source:
            name = str(safe_name(member.name))
            if (
                name in seen
                or name not in expected
                or not (member.isfile() or member.isdir())
            ):
                raise RecoveryError("Volume archive contains unexpected paths or links")
            entry = expected[name]
            if member.isdir():
                if entry["kind"] != "directory" or member.size:
                    raise RecoveryError("Volume directory does not match its manifest")
            else:
                if member.size < 0:
                    raise RecoveryError("Volume archive file size must be nonnegative")
                total += member.size
                if (
                    entry["kind"] != "file"
                    or type(entry.get("size")) is not int
                    or entry["size"] != member.size
                    or total > maximum
                ):
                    raise RecoveryError(
                        "Volume file size exceeds its manifest or budget"
                    )
                with source.extractfile(member) as stream:
                    if hashlib.file_digest(stream, "sha256").hexdigest() != entry.get(
                        "sha256"
                    ):
                        raise RecoveryError("Volume file integrity verification failed")
            seen.add(name)
    if seen != set(expected):
        raise RecoveryError("Volume archive is missing expected files")
    return {
        "kind": "verified-project-volume",
        "project": project,
        "volume": volume,
        "entries": len(seen),
        "bytes": total,
        "databaseSha256": database_sha,
        "archiveSha256": manifest["archiveSha256"],
    }


def restore_volume(directory, database, destination, project, volume):
    result = verify_volume(directory, database, project, volume)
    private_directory(destination, create=True)
    try:
        with tarfile.open(directory / "files.tar", "r:") as source:
            for member in source:
                if not (member.isdir() or member.isfile()):
                    raise RecoveryError(
                        "Volume restore accepts only regular files and directories"
                    )
                target = destination / str(safe_name(member.name))
                if member.isdir():
                    target.mkdir(mode=0o700, parents=True, exist_ok=True)
                else:
                    target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
                    with (
                        source.extractfile(member) as stream,
                        os.fdopen(
                            os.open(
                                target,
                                os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                                0o600,
                            ),
                            "wb",
                        ) as output,
                    ):
                        shutil.copyfileobj(stream, output)
        entries = read_manifest(directory / "manifest.json")["entries"]
        for entry in entries:
            if (
                entry["kind"] == "file"
                and digest(destination / entry["path"]) != entry["sha256"]
            ):
                raise RecoveryError("Restored volume content verification failed")
        return {
            **result,
            "restoreChecksPassed": True,
            "requires": [
                "Keep every old writer stopped or fenced",
                "Restore the paired PostgreSQL dump and verify application records",
                "Set reviewed runtime ownership before mounting the restored volume",
                "Verify application behavior before enabling writers",
            ],
        }
    except BaseException:
        shutil.rmtree(destination)
        raise
