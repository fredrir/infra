from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
from pathlib import Path


class RecoveryError(ValueError):
    pass


def private_file(path: Path) -> Path:
    if not path.is_absolute() or path.is_symlink():
        raise RecoveryError(
            "Credential and recovery paths must be absolute regular files"
        )
    info = path.stat()
    if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077:
        raise RecoveryError(
            "Credential and recovery files must have mode 0600 or stricter"
        )
    if info.st_uid != os.geteuid() or not info.st_size:
        raise RecoveryError(
            "Credential and recovery files must be nonempty and owned by the operator"
        )
    return path


def private_directory(path: Path, *, create: bool = False) -> Path:
    if not path.is_absolute() or path.is_symlink():
        raise RecoveryError("Recovery directory must be an absolute nonsymlink path")
    if create:
        path.mkdir(mode=0o700, parents=False, exist_ok=False)
    info = path.stat()
    if (
        not stat.S_ISDIR(info.st_mode)
        or info.st_mode & 0o077
        or info.st_uid != os.geteuid()
    ):
        raise RecoveryError(
            "Recovery directory must be owned by the operator with mode 0700"
        )
    return path


def digest(path: Path) -> str:
    with path.open("rb") as handle:
        return hashlib.file_digest(handle, "sha256").hexdigest()


def checked(
    command: list[str], *, env: dict[str, str] | None = None, stdout=None
) -> bytes:
    result = subprocess.run(
        command, env=env, stdout=stdout or subprocess.PIPE, stderr=subprocess.PIPE
    )
    if result.returncode:
        raise RecoveryError(
            f"{Path(command[0]).name} failed with exit status {result.returncode}; sensitive stderr withheld"
        )
    return result.stdout or b""


def write_private(path: Path, content: bytes) -> None:
    with os.fdopen(
        os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb"
    ) as handle:
        handle.write(content)


def etcd_bundle(snapshot: Path, token: Path, destination: Path, version: str) -> dict:
    private_file(snapshot)
    private_file(token)
    if not re.fullmatch(r"v1\.\d+\.\d+\+k3s\d+", version):
        raise RecoveryError("A concrete K3s version is required")
    private_directory(destination, create=True)
    try:
        for source, name in [(snapshot, "snapshot"), (token, "server-token")]:
            with (
                source.open("rb") as handle,
                os.fdopen(
                    os.open(
                        destination / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600
                    ),
                    "wb",
                ) as target,
            ):
                shutil.copyfileobj(handle, target)
        manifest = {
            "schemaVersion": 1,
            "kind": "k3s-etcd",
            "k3sVersion": version,
            "createdAt": int(time.time()),
            "files": {
                name: digest(destination / name)
                for name in ["snapshot", "server-token"]
            },
        }
        write_private(
            destination / "manifest.json",
            (json.dumps(manifest, indent=2) + "\n").encode(),
        )
        return manifest
    except BaseException:
        shutil.rmtree(destination)
        raise


def verify_bundle(directory: Path) -> dict:
    private_directory(directory)
    manifest = json.loads(private_file(directory / "manifest.json").read_text())
    if (
        manifest.get("schemaVersion") != 1
        or manifest.get("kind") != "k3s-etcd"
        or set(manifest.get("files", {})) != {"snapshot", "server-token"}
    ):
        raise RecoveryError("Unsupported recovery manifest")
    if not re.fullmatch(r"v1\.\d+\.\d+\+k3s\d+", manifest.get("k3sVersion", "")):
        raise RecoveryError("Recovery manifest has no concrete K3s version")
    for name, expected in manifest["files"].items():
        if digest(private_file(directory / name)) != expected:
            raise RecoveryError(f"Recovery file integrity check failed: {name}")
    return manifest


def import_etcd_tar(archive: Path, destination: Path) -> dict:
    private_file(archive)
    private_directory(destination, create=True)
    expected = {"snapshot": 16 * 1024**3, "server-token": 65536, "manifest.json": 16384}
    seen = set()
    try:
        with tarfile.open(archive, "r:*") as source:
            for member in source:
                name = member.name.removeprefix("./")
                if member.isdir() and name in {"", "."}:
                    continue
                if (
                    name not in expected
                    or name in seen
                    or not member.isfile()
                    or not 0 < member.size <= expected[name]
                ):
                    raise RecoveryError(
                        "Archive must contain only one regular snapshot, token and manifest"
                    )
                handle = source.extractfile(member)
                with (
                    handle,
                    os.fdopen(
                        os.open(
                            destination / name,
                            os.O_WRONLY | os.O_CREAT | os.O_EXCL,
                            0o600,
                        ),
                        "wb",
                    ) as target,
                ):
                    shutil.copyfileobj(handle, target)
                seen.add(name)
        if seen != set(expected):
            raise RecoveryError("Archive is missing recovery material")
        return verify_bundle(destination)
    except BaseException:
        shutil.rmtree(destination)
        raise


def etcd_restore_plan(directory: Path, data_directory: Path, version: str) -> dict:
    manifest = verify_bundle(directory)
    if manifest["k3sVersion"] != version:
        raise RecoveryError("Restore must start on the snapshot's K3s version")
    if (
        not data_directory.is_absolute()
        or data_directory.is_symlink()
        or data_directory.exists()
    ):
        raise RecoveryError(
            "Restore requires a new, nonexistent absolute data directory"
        )
    return {
        "kind": "review-only-etcd-restore",
        "requires": [
            "All original K3s servers stopped or fenced",
            "Target has no active K3s process",
            "Original networking configuration and encryption settings restored",
            "Run reset on one replacement server only",
            "Rejoin other servers with empty database directories after verification",
        ],
        "command": [
            "k3s",
            "server",
            "--data-dir",
            str(data_directory),
            "--cluster-reset",
            "--cluster-reset-restore-path",
            str(directory / "snapshot"),
            "--token-file",
            str(directory / "server-token"),
        ],
        "next": [
            "Remove cluster-reset flags for normal startup",
            "Verify API readiness, etcd members and secrets decryption",
            "Restore application data separately and verify health before DNS cutover",
        ],
    }


def postgres_environment(
    pgpass: Path, host: str, database: str, user: str, port: int
) -> dict[str, str]:
    private_file(pgpass)
    if (
        not re.fullmatch(r"[a-zA-Z0-9._:-]+", host)
        or not database
        or not user
        or not 1 <= port <= 65535
    ):
        raise RecoveryError(
            "Explicit PostgreSQL host, database, user and valid port are required"
        )
    env = {key: value for key, value in os.environ.items() if not key.startswith("PG")}
    env.update(
        PGPASSFILE=str(pgpass),
        PGHOST=host,
        PGDATABASE=database,
        PGUSER=user,
        PGPORT=str(port),
        PGCONNECT_TIMEOUT="10",
    )
    return env


def postgres_backup(destination: Path, env: dict[str, str]) -> dict:
    private_directory(destination, create=True)
    try:
        version = checked(["pg_dump", "--version"], env=env).decode().strip()
        path = destination / "database.dump"
        with os.fdopen(
            os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb"
        ) as output:
            checked(
                ["pg_dump", "--format=custom", "--no-owner", "--no-acl"],
                env=env,
                stdout=output,
            )
        checked(["pg_restore", "--list", str(path)], env=env)
        manifest = {
            "schemaVersion": 1,
            "kind": "postgres-logical",
            "createdAt": int(time.time()),
            "toolVersion": version,
            "sha256": digest(path),
        }
        write_private(
            destination / "manifest.json",
            (json.dumps(manifest, indent=2) + "\n").encode(),
        )
        return manifest
    except BaseException:
        shutil.rmtree(destination)
        raise


def postgres_restore(
    directory: Path, env: dict[str, str], expected_tables: list[str]
) -> None:
    private_directory(directory)
    manifest = json.loads(private_file(directory / "manifest.json").read_text())
    path = private_file(directory / "database.dump")
    if manifest.get("kind") != "postgres-logical" or manifest.get("sha256") != digest(
        path
    ):
        raise RecoveryError("PostgreSQL dump integrity verification failed")
    if not expected_tables or not all(
        re.fullmatch(r"[a-z_][a-z0-9_]*\.[a-z_][a-z0-9_]*", name)
        for name in expected_tables
    ):
        raise RecoveryError(
            "Explicit schema-qualified expected table names are required"
        )
    count = checked(
        [
            "psql",
            "-X",
            "--no-psqlrc",
            "-At",
            "--set=ON_ERROR_STOP=1",
            "--command",
            "SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND c.relkind IN ('r','p','v','m','S');",
        ],
        env=env,
    ).strip()
    if count != b"0":
        raise RecoveryError("Restore target must be an empty, isolated database")
    checked(
        [
            "pg_restore",
            "--exit-on-error",
            "--single-transaction",
            "--no-owner",
            "--no-acl",
            "--dbname",
            env["PGDATABASE"],
            str(path),
        ],
        env=env,
    )
    for name in expected_tables:
        schema, table = name.split(".")
        checked(
            [
                "psql",
                "-X",
                "--set=ON_ERROR_STOP=1",
                "--command",
                f'SELECT count(*) FROM "{schema}"."{table}";',
            ],
            env=env,
        )


def backup_metrics(
    path: Path,
    job: str,
    maximum_age: int,
    *,
    success: bool = False,
    now: int | None = None,
) -> None:
    if not re.fullmatch(r"[a-z][a-z0-9_-]{0,62}", job) or maximum_age < 60:
        raise RecoveryError("A bounded metric job identifier and RPO are required")
    label = f'{{backup_job="{job}"}}'
    previous = ""
    if path.exists():
        for line in path.read_text().splitlines():
            if line.startswith("restic_last_success_timestamp" + label + " "):
                previous = line + "\n"
    body = (
        f"restic_expected_job{label} 1\nrestic_max_age_seconds{label} {maximum_age}\n"
    )
    body += (
        f"restic_last_success_timestamp{label} {now if now is not None else int(time.time())}\n"
        if success
        else previous
    )
    with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as output:
        temporary = Path(output.name)
        output.write(body)
    temporary.chmod(0o644)
    temporary.replace(path)


def main(argv=None) -> int:
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)
    bundle = sub.add_parser("etcd-bundle")
    bundle.add_argument("--snapshot", type=Path, required=True)
    bundle.add_argument("--token-file", type=Path, required=True)
    bundle.add_argument("--destination", type=Path, required=True)
    bundle.add_argument("--k3s-version", required=True)
    verify = sub.add_parser("verify-bundle")
    verify.add_argument("directory", type=Path)
    importer = sub.add_parser("import-etcd-tar")
    importer.add_argument("--archive", type=Path, required=True)
    importer.add_argument("--destination", type=Path, required=True)
    restore = sub.add_parser("etcd-restore-plan")
    restore.add_argument("directory", type=Path)
    restore.add_argument("--data-directory", type=Path, required=True)
    restore.add_argument("--k3s-version", required=True)
    for command in ["volume-export", "volume-verify", "volume-restore"]:
        volume = sub.add_parser(command)
        volume.add_argument("--database-bundle", type=Path, required=True)
        volume.add_argument("--project", required=True)
        volume.add_argument("--volume", required=True)
        if command == "volume-export":
            volume.add_argument("--source", type=Path, required=True)
            volume.add_argument("--drain-assertion", type=Path, required=True)
            volume.add_argument("--max-bytes", type=int, required=True)
        else:
            volume.add_argument("--directory", type=Path, required=True)
        if command != "volume-verify":
            volume.add_argument("--destination", type=Path, required=True)
            volume.add_argument("--execute", action="store_true", required=True)
    for command in ["postgres-backup", "postgres-restore"]:
        pg = sub.add_parser(command)
        pg.add_argument("--directory", type=Path, required=True)
        pg.add_argument("--pgpass-file", type=Path, required=True)
        pg.add_argument("--host", required=True)
        pg.add_argument("--database", required=True)
        pg.add_argument("--user", required=True)
        pg.add_argument("--port", type=int, default=5432)
        pg.add_argument("--execute", action="store_true", required=True)
        if command == "postgres-restore":
            pg.add_argument("--expect-table", action="append", required=True)
    metric = sub.add_parser("backup-metrics")
    metric.add_argument("--file", type=Path, required=True)
    metric.add_argument("--job", required=True)
    metric.add_argument("--maximum-age", type=int, required=True)
    metric.add_argument("--success", action="store_true")
    args = parser.parse_args(argv)
    try:
        if args.command == "etcd-bundle":
            result = etcd_bundle(
                args.snapshot, args.token_file, args.destination, args.k3s_version
            )
        elif args.command == "verify-bundle":
            result = verify_bundle(args.directory)
        elif args.command == "import-etcd-tar":
            result = import_etcd_tar(args.archive, args.destination)
        elif args.command == "etcd-restore-plan":
            result = etcd_restore_plan(
                args.directory, args.data_directory, args.k3s_version
            )
        elif args.command == "backup-metrics":
            backup_metrics(args.file, args.job, args.maximum_age, success=args.success)
            result = {"updated": True}
        elif args.command.startswith("volume-"):
            import volume_recovery

            if args.command == "volume-export":
                result = volume_recovery.export_volume(
                    args.source,
                    args.destination,
                    args.database_bundle,
                    args.drain_assertion,
                    args.project,
                    args.volume,
                    args.max_bytes,
                )
            elif args.command == "volume-verify":
                result = volume_recovery.verify_volume(
                    args.directory, args.database_bundle, args.project, args.volume
                )
            else:
                result = volume_recovery.restore_volume(
                    args.directory,
                    args.database_bundle,
                    args.destination,
                    args.project,
                    args.volume,
                )
        else:
            env = postgres_environment(
                args.pgpass_file, args.host, args.database, args.user, args.port
            )
            if args.command == "postgres-backup":
                result = postgres_backup(args.directory, env)
            else:
                postgres_restore(args.directory, env, args.expect_table)
                result = {"restoreChecksPassed": True}
        print(json.dumps(result, indent=2))
        return 0
    except (
        RecoveryError,
        OSError,
        ValueError,
        KeyError,
        TypeError,
        tarfile.TarError,
    ) as exc:
        print(f"Recovery operation refused: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
