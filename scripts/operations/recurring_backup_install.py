import argparse
import grp
import hashlib
import json
import os
import pwd
import re
import socket
import stat
import subprocess
import sys
import uuid
from pathlib import Path

from backup_status import ACCOUNT, DIRECTORY, authorized_key
from evacuation_execution import Commands, stopped, unit_state
from evacuation_guards import Filesystem, verify_installation

BASE = "/var/lib/platform-backup-install"
RECEIPT = BASE + "/receipt.json"
SSH_CONFIG = "/etc/ssh/sshd_config"
KEY = "/var/lib/infra-evacuation/llunde/identity/backup-status"
HOME = "/var/lib/platform-backup-status-home"
UNITS = ("restic-backups-llunde-backend.service", "restic-backups-llunde-backend.timer")
HOSTS = {"fredrir-09": "cloud-server-10643982", "fredrir-06": "localhost"}
FILES = {
    "fredrir-09": {
        KEY,
        KEY + ".pub",
        "/etc/infra-evacuation/llunde/backup-status-known-hosts",
    }
    | {"/etc/systemd/system/" + name for name in UNITS},
    "fredrir-06": {
        SSH_CONFIG,
        "/usr/local/libexec/infra-backup-status",
        "/etc/ssh/authorized_keys/infra-backup-status",
        "/usr/local/lib/platform-watchdog.py",
    },
}
ADMIN_ADDRESSES = ("62.92.106.173", "85.190.100.72")
RECEIVER_UNITS = (
    "apache2.service",
    "mongod.service",
    "pgbouncer.service",
    "postgresql@16-main.service",
    "pm2-appuser.service",
    "pm2-root.service",
    "github-runner.service",
)
SSH_EXPECTED = {
    "authenticationmethods": "publickey",
    "pubkeyauthentication": "yes",
    "authorizedkeyscommand": "none",
    "trustedusercakeys": "none",
    "authorizedkeysfile": "/etc/ssh/authorized_keys/infra-backup-status",
    "passwordauthentication": "no",
    "kbdinteractiveauthentication": "no",
    "permitemptypasswords": "no",
    "disableforwarding": "yes",
    "permittty": "no",
    "permituserrc": "no",
    "maxsessions": "1",
    "forcecommand": "/usr/bin/timeout -s TERM -k 1 8 /usr/bin/python3 -I /usr/local/libexec/infra-backup-status",
}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def host_identity(host):
    require(
        host in HOSTS and os.geteuid() == 0 and socket.gethostname() == HOSTS[host],
        "Verified root host required",
    )


def checked_bundle(directory, expected):
    directory = Path(directory)
    require(
        re.fullmatch(
            r"/var/lib/infra-evacuation/llunde/recurring-backup-deploy-[0-9TZ]+",
            str(directory),
        ),
        "Fixed deployment directory required",
    )
    fs = Filesystem()
    metadata, data = fs.read(str(directory / "manifest.json"))
    require(
        metadata["mode"] == "0600" and metadata["sha256"] == expected,
        "Reviewed deployment manifest required",
    )
    value = json.loads(data)
    require(
        value["schemaVersion"] == 1
        and value["kind"] == "recurring-backup-deployment"
        and value["remoteDirectory"] == str(directory),
        "Deployment manifest differs",
    )
    names = {
        str(p.relative_to(directory)) for p in directory.rglob("*") if not p.is_dir()
    }
    require(
        names == set(value["files"]) | {"manifest.json"},
        "Exact deployment inventory required",
    )
    for name, wanted in value["files"].items():
        require(
            not name.startswith("/")
            and all(part not in ("", ".", "..") for part in name.split("/")),
            "Deployment path escaped",
        )
        info, content = fs.read(str(directory / name))
        require(
            info["mode"] == "0600"
            and info["sha256"] == wanted["sha256"]
            and len(content) == wanted["bytes"],
            "Deployment file differs",
        )
    return value


def current_account():
    try:
        user = pwd.getpwnam(ACCOUNT)
    except KeyError:
        return None
    group = grp.getgrnam(ACCOUNT)
    return {
        "uid": user.pw_uid,
        "gid": user.pw_gid,
        "home": user.pw_dir,
        "shell": user.pw_shell,
        "gecos": user.pw_gecos,
        "groupGid": group.gr_gid,
        "groupMembers": group.gr_mem,
        "groups": sorted(os.getgrouplist(ACCOUNT, user.pw_gid)),
    }


def validate_account(account, nonce):
    require(
        account
        and 0 < account["uid"] < 1000
        and account["gid"] == account["groupGid"]
        and account["groups"] == [account["gid"]]
        and account["groupMembers"] == []
        and account["home"] == HOME
        and account["shell"] == "/bin/sh"
        and account["gecos"] == account_comment(nonce),
        "Owned receiver account differs",
    )
    return account


def account_comment(nonce):
    require(re.fullmatch("[a-f0-9]{16}", nonce), "Receiver ownership nonce differs")
    return "infra-backup-receiver-" + nonce


def effective(commands, config, user, address):
    _, raw = commands.run(
        [
            "/usr/sbin/sshd",
            "-T",
            "-f",
            str(config),
            "-C",
            "user=" + user + ",addr=" + address + ",host=fredrir-admin",
        ],
        maximum=65536,
    )
    require(raw and len(raw) <= 65536, "Complete native SSH configuration required")
    return raw


def ssh_contract(commands, original, candidate, users):
    commands.run(["/usr/sbin/sshd", "-t", "-f", str(candidate)], maximum=4096)
    require(
        "root" in users and ACCOUNT not in users and 1 <= len(users) <= 32,
        "Bounded administrator contexts required",
    )
    hashes = {}
    for user in users:
        require(
            re.fullmatch(r"[a-z_][a-z0-9_-]{0,31}", user), "Native account name invalid"
        )
        for address in ADMIN_ADDRESSES:
            before, after = (
                effective(commands, original, user, address),
                effective(commands, candidate, user, address),
            )
            require(before == after, "Existing administrator SSH settings changed")
            hashes[user + "@" + address] = hashlib.sha256(before).hexdigest()
    raw = effective(commands, candidate, ACCOUNT, "85.190.100.72")
    rows = dict(line.split(" ", 1) for line in raw.decode().splitlines())
    require(
        all(rows.get(key) == value for key, value in SSH_EXPECTED.items())
        and rows.get("permituserenvironment") == "no",
        "Restricted receiver SSH settings differ",
    )
    require(
        set(
            line.split(" ", 1)[1]
            for line in raw.decode().splitlines()
            if line.startswith("acceptenv ")
        )
        <= {"LANG", "LC_*"},
        "Unsafe receiver environment acceptance",
    )
    return {
        "adminContexts": hashes,
        "receiver": SSH_EXPECTED,
        "nativeSyntaxVerified": True,
    }


def replace_file(fs, path, data, expected, mode):
    with fs.parent_fd(path) as (parent, name):
        fs.metadata(path, expected)
        temporary = name + ".infra-backup-new"
        descriptor = os.open(
            temporary,
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
            mode,
            dir_fd=parent,
        )
        try:
            with os.fdopen(descriptor, "wb") as output:
                os.fchmod(output.fileno(), mode)
                if expected["exists"]:
                    os.fchown(output.fileno(), expected["uid"], expected["gid"])
                output.write(data)
                output.flush()
                os.fsync(output.fileno())
            fs.metadata(path, expected)
            os.replace(temporary, name, src_dir_fd=parent, dst_dir_fd=parent)
            os.fsync(parent)
        finally:
            try:
                os.unlink(temporary, dir_fd=parent)
            except FileNotFoundError:
                pass
    return fs.metadata(path)


def install_file(fs, receipt, path, data, mode=0o644, replace=False):
    before, original = fs.read(path)
    require(
        not before["exists"] or replace and before["mode"] == format(mode, "04o"),
        "Installation would overwrite unrelated file",
    )
    if before["exists"]:
        saved = BASE + "/original-" + str(len(receipt["files"]))
        fs.write(saved, original, 0o600)
    else:
        saved = None
    record = {
        "before": before,
        "original": saved,
        "desiredSHA256": hashlib.sha256(data).hexdigest(),
        "mode": format(mode, "04o"),
        "after": None,
    }
    receipt["files"][path] = record
    fs.save(RECEIPT, receipt)
    if before["exists"]:
        record["after"] = replace_file(fs, path, data, before, mode)
    else:
        record["after"] = fs.write(path, data, mode)
    fs.save(RECEIPT, receipt)


def dormant_target(commands):
    from evacuation_target import inert_markers_absent

    inert_markers_absent()
    result = {}
    for user, names in {
        "edge": ("caddy", "cloudflared"),
        "llunde-backend": ("llunde-backend", "llunde-postgres", "llunde-valkey"),
        "llunde-frontend": ("llunde-frontend",),
    }.items():
        for name in names:
            value = unit_state(commands, name + ".service", user)
            require(stopped(value), "Dormant application units required")
            result[user + "/" + name] = {
                key: value[key] for key in ("ActiveState", "SubState", "MainPID")
            }
    for name in UNITS:
        require(stopped(unit_state(commands, name)), "Inactive backup units required")
        _, raw = commands.run(
            ["systemctl", "is-enabled", name], check=False, maximum=1024
        )
        require(
            raw.strip() in (b"not-found", b"disabled", b"static", b""),
            "Backup timer must remain disabled",
        )
    return result


def reload_ssh(commands):
    commands.run(["/usr/sbin/sshd", "-t"], maximum=4096)
    commands.run(["systemctl", "reload", "ssh.service"], maximum=4096)
    commands.run(["systemctl", "is-active", "--quiet", "ssh.service"], maximum=4096)


def receiver_services(commands):
    return {
        name: {
            key: value
            for key, value in unit_state(commands, name).items()
            if key in ("ActiveState", "SubState", "MainPID")
        }
        for name in RECEIVER_UNITS
    }


def reconcile_key(fs, receipt, commands):
    expected_comment = "infra-backup-status:" + receipt["nonce"]
    private, _ = fs.read(KEY)
    public, contents = fs.read(KEY + ".pub")
    derived = None
    if private["exists"]:
        require(private["mode"] == "0600", "Private sender key mode differs")
        _, raw = commands.run(["ssh-keygen", "-y", "-f", KEY], maximum=1024)
        fields = raw.decode().split()
        require(
            len(fields) == 3 and fields[2] == expected_comment,
            "Generated sender key ownership differs",
        )
        derived = " ".join(fields[:2])
        authorized_key(derived)
    if public["exists"]:
        fields = contents.decode().split()
        require(
            public["mode"] == "0600"
            and len(fields) == 3
            and fields[2] == expected_comment,
            "Generated public key ownership differs",
        )
        candidate = " ".join(fields[:2])
        authorized_key(candidate)
        require(derived in (None, candidate), "Sender key pair differs")
        derived = candidate
    for path, info in ((KEY, private), (KEY + ".pub", public)):
        if info["exists"] and path not in receipt["files"]:
            receipt["files"][path] = {
                "before": {"exists": False},
                "original": None,
                "desiredSHA256": info["sha256"],
                "mode": "0600",
                "after": info,
            }
    fs.save(RECEIPT, receipt)
    return derived


def install(host, bundle, manifest, public_key=None, *, fs=None, commands=None):
    host_identity(host)
    fs, commands = fs or Filesystem(), commands or Commands(150)
    require(
        not fs.metadata(RECEIPT)["exists"],
        "Existing installation receipt requires verification or rollback",
    )
    created = {}
    fs.directory(BASE, 0o700, created)
    receipt = {
        "schemaVersion": 1,
        "kind": "recurring-backup-installation",
        "host": host,
        "status": "installing",
        "nonce": uuid.uuid4().hex[:16],
        "manifestSHA256": hashlib.sha256(
            (Path(bundle) / "manifest.json").read_bytes()
        ).hexdigest(),
        "files": {},
        "directories": created,
        "account": None,
        "accountPlanned": False,
        "keyPlanned": False,
        "timerEnabled": False,
    }
    fs.save(RECEIPT, receipt)
    if host == "fredrir-09":
        receipt["applicationBaseline"] = dormant_target(commands)
        verify_installation(
            "/var/lib/platform-evacuation/receipts/guards.json", require_loaded=True
        )
        require(
            not fs.metadata(KEY)["exists"] and not fs.metadata(KEY + ".pub")["exists"],
            "Fresh dedicated sender key required",
        )
        receipt["keyPlanned"] = True
        fs.save(RECEIPT, receipt)
        commands.run(
            [
                "ssh-keygen",
                "-q",
                "-t",
                "ed25519",
                "-N",
                "",
                "-C",
                "infra-backup-status:" + receipt["nonce"],
                "-f",
                KEY,
            ],
            maximum=4096,
        )
        public = reconcile_key(fs, receipt, commands)
        require(
            public and KEY in receipt["files"] and KEY + ".pub" in receipt["files"],
            "Complete sender key pair required",
        )
        receipt["publicKey"] = public
        fs.save(RECEIPT, receipt)
        for name in UNITS:
            install_file(
                fs,
                receipt,
                "/etc/systemd/system/" + name,
                (Path(bundle) / name).read_bytes(),
            )
        install_file(
            fs,
            receipt,
            "/etc/infra-evacuation/llunde/backup-status-known-hosts",
            (Path(bundle) / "known_hosts").read_bytes(),
            0o600,
        )
        commands.run(["systemctl", "daemon-reload"], maximum=4096)
        verify_installation(
            "/var/lib/platform-evacuation/receipts/guards.json", require_loaded=True
        )
        require(
            dormant_target(commands) == receipt["applicationBaseline"],
            "Application states changed",
        )
    else:
        require(public_key is not None, "Verified sender public key required")
        receipt["applicationBaseline"] = receiver_services(commands)
        fs.save(RECEIPT, receipt)
        key_line = authorized_key(public_key).encode()
        require(
            current_account() is None, "Existing receiver account cannot be adopted"
        )
        try:
            grp.getgrnam(ACCOUNT)
        except KeyError:
            pass
        else:
            raise ValueError("Existing receiver group cannot be adopted")
        for path in (
            "/usr/local/libexec/infra-backup-status",
            "/etc/ssh/authorized_keys/infra-backup-status",
            str(DIRECTORY),
            HOME,
        ):
            require(not os.path.lexists(path), "Fresh receiver paths required")
        _, original = fs.read(SSH_CONFIG)
        require(
            original is not None and b"infra-backup-status" not in original,
            "Unmodified SSH baseline required",
        )
        candidate = (
            original.rstrip(b"\n")
            + b"\n"
            + (Path(bundle) / "receiver.sshd").read_bytes()
        )
        fs.write(BASE + "/sshd-candidate", candidate, 0o600)
        users = sorted(
            {"root"}
            | {
                user.pw_name
                for user in pwd.getpwall()
                if user.pw_uid >= 1000
                and user.pw_name != "nobody"
                and user.pw_shell not in ("/usr/sbin/nologin", "/bin/false")
            }
        )
        receipt["sshProof"] = ssh_contract(
            commands, SSH_CONFIG, BASE + "/sshd-candidate", users
        )
        receipt["adminUsers"] = users
        receipt["accountPlanned"] = True
        fs.save(RECEIPT, receipt)
        commands.run(
            [
                "useradd",
                "--system",
                "--user-group",
                "--no-create-home",
                "--home-dir",
                HOME,
                "--shell",
                "/bin/sh",
                "--password",
                "!",
                "--comment",
                account_comment(receipt["nonce"]),
                ACCOUNT,
            ],
            maximum=4096,
        )
        receipt["account"] = validate_account(current_account(), receipt["nonce"])
        fs.save(RECEIPT, receipt)
        for path in (HOME, "/etc/ssh/authorized_keys", "/usr/local/libexec"):
            fs.directory(path, 0o755, receipt["directories"])
            fs.save(RECEIPT, receipt)
        fs.directory(str(DIRECTORY), 0o755, receipt["directories"])
        fs.save(RECEIPT, receipt)
        receipt["receiverDirectory"] = dict(
            receipt["directories"][str(DIRECTORY)],
            beforeGID=os.stat(DIRECTORY, follow_symlinks=False).st_gid,
            afterUID=receipt["account"]["uid"],
            afterGID=receipt["account"]["gid"],
        )
        fs.save(RECEIPT, receipt)
        with fs.directory_fd(str(DIRECTORY)) as descriptor:
            os.fchown(descriptor, receipt["account"]["uid"], receipt["account"]["gid"])
        receipt["directories"][str(DIRECTORY)].update(
            uid=receipt["account"]["uid"], gid=receipt["account"]["gid"]
        )
        fs.save(RECEIPT, receipt)
        install_file(
            fs,
            receipt,
            "/usr/local/libexec/infra-backup-status",
            (Path(bundle) / "backup_status.py").read_bytes(),
        )
        install_file(
            fs, receipt, "/etc/ssh/authorized_keys/infra-backup-status", key_line
        )
        install_file(
            fs,
            receipt,
            "/usr/local/lib/platform-watchdog.py",
            (Path(bundle) / "watchdog.py").read_bytes(),
            replace=True,
        )
        receipt["sshReloadRequired"] = True
        fs.save(RECEIPT, receipt)
        install_file(fs, receipt, SSH_CONFIG, candidate, replace=True)
        reload_ssh(commands)
        receipt["sshReloadVerified"] = True
        require(
            ssh_contract(
                commands,
                BASE + "/original-" + str(list(receipt["files"]).index(SSH_CONFIG)),
                SSH_CONFIG,
                users,
            )
            == receipt["sshProof"],
            "Installed SSH settings differ",
        )
        require(
            receiver_services(commands) == receipt["applicationBaseline"],
            "Receiver application states changed",
        )
        receipt["freshSSHAndForcedCommandAcceptance"] = False
    receipt["status"] = "installed"
    fs.save(RECEIPT, receipt)
    return receipt


def original_ssh_proof(commands, receipt):
    original = receipt["files"][SSH_CONFIG]["original"]
    commands.run(["/usr/sbin/sshd", "-t", "-f", original], maximum=4096)
    for context, expected in receipt["sshProof"]["adminContexts"].items():
        user, address = context.split("@")
        require(
            hashlib.sha256(effective(commands, original, user, address)).hexdigest()
            == expected,
            "Original SSH dependencies changed",
        )


def reconcile_directory(fs, receipt):
    wanted = receipt.get("receiverDirectory")
    if not wanted:
        return
    with fs.parent_fd(str(DIRECTORY)) as (parent, name):
        try:
            descriptor = os.open(
                name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent
            )
        except FileNotFoundError:
            require(
                receipt.get("directoriesRemoved") is True
                or receipt.get("directoryRemovalPlanned") is True,
                "Receiver directory disappeared",
            )
            return
        try:
            info = os.fstat(descriptor)
            require(
                info.st_ino == wanted["inode"]
                and info.st_dev == wanted["device"]
                and stat.S_IMODE(info.st_mode) == 0o755
                and (info.st_uid, info.st_gid)
                in (
                    (wanted["uid"], wanted["beforeGID"]),
                    (wanted["afterUID"], wanted["afterGID"]),
                ),
                "Receiver directory ownership changed",
            )
            require(
                not os.listdir(descriptor), "Receiver contains state; retain for review"
            )
            receipt["directories"][str(DIRECTORY)].update(
                uid=info.st_uid, gid=info.st_gid
            )
        finally:
            os.close(descriptor)
    fs.save(RECEIPT, receipt)


def matches_original(current, before):
    if not before["exists"]:
        return current == {"exists": False}
    return current["exists"] and all(
        current[key] == before[key] for key in ("sha256", "uid", "gid", "mode")
    )


def rollback(host, *, fs=None, commands=None):
    host_identity(host)
    fs, commands = fs or Filesystem(), commands or Commands(150)
    receipt = fs.read_json(RECEIPT)
    require(
        receipt["schemaVersion"] == 1
        and receipt["kind"] == "recurring-backup-installation"
        and receipt["host"] == host
        and receipt["status"] in ("installing", "installed", "rolling-back"),
        "Owned installation receipt required",
    )
    require(set(receipt["files"]) <= FILES[host], "Rollback file scope differs")
    require(
        set(receipt["directories"])
        <= {
            BASE,
            HOME,
            str(DIRECTORY),
            "/var",
            "/var/lib",
            "/etc",
            "/etc/ssh",
            "/etc/ssh/authorized_keys",
            "/usr",
            "/usr/local",
            "/usr/local/libexec",
        },
        "Rollback directory scope differs",
    )
    require(
        all(
            record["original"] is None
            or re.fullmatch(re.escape(BASE) + "/original-[0-9]", record["original"])
            for record in receipt["files"].values()
        ),
        "Rollback original path differs",
    )
    account = None
    if host == "fredrir-09":
        dormant_target(commands)
        if receipt["keyPlanned"] and not receipt.get("rollbackStarted"):
            reconcile_key(fs, receipt, commands)
    else:
        if receipt["accountPlanned"]:
            observed = current_account()
            if observed is not None:
                account = validate_account(observed, receipt["nonce"])
                require(
                    receipt["account"] in (None, account), "Receiver account changed"
                )
                receipt["account"] = account
                fs.save(RECEIPT, receipt)
                _, pids = commands.run(
                    ["ps", "-u", str(account["uid"]), "-o", "pid="],
                    check=False,
                    maximum=4096,
                )
                require(not pids.strip(), "Receiver session still active")
            elif receipt.get("accountDeletePlanned"):
                account = receipt["account"]
            else:
                require(receipt["account"] is None, "Receiver account disappeared")
                try:
                    grp.getgrnam(ACCOUNT)
                except KeyError:
                    pass
                else:
                    raise ValueError("Partial receiver group requires inspection")
        reconcile_directory(fs, receipt)
        if SSH_CONFIG in receipt["files"]:
            original_ssh_proof(commands, receipt)
            record = receipt["files"][SSH_CONFIG]
            current = fs.metadata(SSH_CONFIG)
            if (
                current != record["before"]
                and not record.get("rolledBack")
                and not (
                    record.get("rollbackPlanned")
                    and matches_original(current, record["before"])
                )
            ):
                require(
                    ssh_contract(
                        commands, record["original"], SSH_CONFIG, receipt["adminUsers"]
                    )
                    == receipt["sshProof"],
                    "SSH configuration drift prevents rollback",
                )
    untouched = set()
    for path, record in receipt["files"].items():
        current = fs.metadata(path)
        if record.get("rolledBack") is not None:
            require(
                current == record["rolledBack"],
                "Restored file changed; rollback refused",
            )
            untouched.add(path)
        elif record.get("rollbackPlanned") and matches_original(
            current, record["before"]
        ):
            record["rolledBack"] = current
            untouched.add(path)
        elif record["after"] is None and current == record["before"]:
            untouched.add(path)
        else:
            require(
                current["exists"]
                and current["sha256"] == record["desiredSHA256"]
                and current["mode"] == record["mode"]
                and (record["after"] is None or current == record["after"]),
                "Installation file changed; rollback refused",
            )
    receipt["status"], receipt["rollbackStarted"] = "rolling-back", True
    if (
        host == "fredrir-06"
        and SSH_CONFIG in receipt["files"]
        and SSH_CONFIG not in untouched
    ):
        receipt["rollbackSSHReloadNeeded"] = True
    fs.save(RECEIPT, receipt)
    for path, record in reversed(list(receipt["files"].items())):
        if path in untouched:
            continue
        current = fs.metadata(path)
        record["rollbackPlanned"] = True
        fs.save(RECEIPT, receipt)
        if record["before"]["exists"]:
            _, original = fs.read(record["original"])
            require(
                hashlib.sha256(original).hexdigest() == record["before"]["sha256"],
                "Original file changed",
            )
            replace_file(fs, path, original, current, int(record["before"]["mode"], 8))
        else:
            fs.unlink(path, current)
        record["rolledBack"] = fs.metadata(path)
        fs.save(RECEIPT, receipt)
    if host == "fredrir-09":
        commands.run(["systemctl", "daemon-reload"], maximum=4096)
    elif receipt.get("rollbackSSHReloadNeeded"):
        reload_ssh(commands)
        receipt["rollbackSSHReloadNeeded"] = False
        fs.save(RECEIPT, receipt)
    receipt["directoryRemovalPlanned"] = True
    fs.save(RECEIPT, receipt)
    receipt["retainedDirectories"] = fs.remove_directories(
        {key: value for key, value in receipt["directories"].items() if key != BASE}
    )
    receipt["directoriesRemoved"] = True
    fs.save(RECEIPT, receipt)
    if host == "fredrir-06" and account is not None:
        receipt["accountDeletePlanned"] = True
        fs.save(RECEIPT, receipt)
        if current_account() is not None:
            commands.run(["userdel", ACCOUNT], maximum=4096)
        try:
            group = grp.getgrnam(ACCOUNT)
        except KeyError:
            pass
        else:
            require(
                group.gr_gid == account["gid"] and not group.gr_mem,
                "Receiver group changed",
            )
            commands.run(["groupdel", ACCOUNT], maximum=4096)
    receipt["status"] = "rolled-back"
    fs.save(RECEIPT, receipt)
    return receipt


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=("install", "rollback"))
    parser.add_argument("host", choices=tuple(HOSTS))
    parser.add_argument("--bundle", type=Path)
    parser.add_argument("--manifest-sha256")
    parser.add_argument("--public-key")
    args = parser.parse_args()
    os.umask(0o077)
    try:
        if args.action == "install":
            require(
                args.bundle is not None and args.manifest_sha256 is not None,
                "Reviewed bundle required",
            )
            manifest = checked_bundle(args.bundle, args.manifest_sha256)
            result = install(args.host, args.bundle, manifest, args.public_key)
        else:
            result = rollback(args.host)
        print(
            json.dumps(
                {
                    key: result[key]
                    for key in ("host", "status", "publicKey")
                    if key in result
                }
            )
        )
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
        print(
            "Dormant backup installation failed; inspect private receipt",
            file=sys.stderr,
        )
        raise SystemExit(1)


if __name__ == "__main__":
    main()
