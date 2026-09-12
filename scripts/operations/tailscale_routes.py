import argparse
import fcntl
import hashlib
import json
import os
import signal
import socket
import stat
import subprocess
import sys
import time
from pathlib import Path

ROUTES = ["10.60.0.5/32", "10.60.0.7/32", "10.60.0.8/32"]
NODES = {
    "fredrir-07": {
        "hostname": "ubuntu-8gb-hel1-1",
        "deviceId": "nKgogDp6v721CNTRL",
        "tailnetIP": "100.115.121.9",
        "privateIP": "10.60.0.7",
    },
    "fredrir-08": {
        "hostname": "ubuntu-8gb-hel1-2",
        "deviceId": "nMZbWwFtmv11CNTRL",
        "tailnetIP": "100.96.114.53",
        "privateIP": "10.60.0.8",
    },
}
SYSCTL_TEXT = b"net.ipv4.ip_forward=1\n"


class RouteError(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise RouteError(message)


def exact_file(path, mode):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(descriptor, "rb") as stream:
        info = os.fstat(stream.fileno())
        require(
            stat.S_ISREG(info.st_mode)
            and info.st_uid == os.geteuid()
            and stat.S_IMODE(info.st_mode) == mode
            and info.st_nlink == 1
            and info.st_size <= 65536,
            "Private owned file required",
        )
        return stream.read(65537)


def save(path, value, *, fresh=False):
    path = Path(path)
    temporary = path if fresh else path.with_name(path.name + ".tmp")
    descriptor = os.open(
        temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600
    )
    try:
        with os.fdopen(descriptor, "w") as stream:
            json.dump(value, stream, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        if not fresh:
            os.replace(temporary, path)
    finally:
        if not fresh:
            temporary.unlink(missing_ok=True)
    descriptor = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


class Router:
    def __init__(self, node):
        require(node in NODES, "Verified staging control plane required")
        self.node, self.identity = node, NODES[node]
        self.directory = Path("/var/lib/platform-api-routing")
        self.sysctl_file = Path("/etc/sysctl.d/90-platform-api-routing.conf")
        self.forward_file = Path("/proc/sys/net/ipv4/ip_forward")
        self.k3s_paths = [Path("/etc/rancher/k3s"), Path("/var/lib/rancher/k3s")]

    def call(self, arguments):
        try:
            result = subprocess.run(
                arguments,
                capture_output=True,
                timeout=15,
                cwd="/",
                env={
                    "PATH": "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
                    "LANG": "C",
                    "LC_ALL": "C",
                },
            )
        except (OSError, subprocess.SubprocessError):
            raise RouteError("Bounded route command failed") from None
        require(
            result.returncode == 0 and len(result.stdout) <= 262144,
            "Route command failed",
        )
        return result.stdout

    def snapshot(self):
        require(
            socket.gethostname() == self.identity["hostname"], "Host identity differs"
        )
        require(
            not any(path.exists() or path.is_symlink() for path in self.k3s_paths),
            "Route staging requires no K3s installation",
        )
        state = json.loads(self.call(["/usr/local/bin/tailscale", "status", "--json"]))
        own = state["Self"]
        require(
            state["BackendState"] == "Running"
            and own["ID"] == self.identity["deviceId"]
            and own["HostName"] == self.node
            and own["Tags"] == ["tag:platform-control"]
            and self.identity["tailnetIP"] in own["TailscaleIPs"],
            "Tagged transport identity differs",
        )
        preferences = json.loads(
            self.call(["/usr/local/bin/tailscale", "get", "--json"])
        )
        require(
            all(
                preferences.get(key) is value
                for key, value in {
                    "accept-routes": False,
                    "advertise-exit-node": False,
                    "ssh": False,
                    "snat-subnet-routes": True,
                }.items()
            ),
            "Transport preferences differ",
        )
        routes = preferences["advertise-routes"]
        require(
            routes == "" or routes.split(",") == ROUTES, "Unexpected advertised routes"
        )
        address = json.loads(
            self.call(["ip", "-j", "address", "show", "dev", "enp7s0"])
        )
        require(
            len(address) == 1
            and any(
                item.get("local") == self.identity["privateIP"]
                and item.get("prefixlen") == 32
                for item in address[0]["addr_info"]
            ),
            "Private interface identity differs",
        )
        for peer in [
            ip
            for ip in ["10.60.0.5", "10.60.0.7", "10.60.0.8"]
            if ip != self.identity["privateIP"]
        ]:
            route = json.loads(self.call(["ip", "-j", "route", "get", peer]))
            require(
                len(route) == 1
                and route[0].get("dev") == "enp7s0"
                and route[0].get("prefsrc") == self.identity["privateIP"],
                "Private peer route differs",
            )
        rules = self.call(["nft", "list", "ruleset"]).decode()
        require(
            "chain ts-forward" in rules
            and "chain ts-postrouting" in rules
            and "masquerade" in rules,
            "Tailscale forwarding and SNAT chains required",
        )
        forwarding = self.forward_file.read_text().strip()
        require(forwarding in {"0", "1"}, "IPv4 forwarding state invalid")
        exists = self.sysctl_file.exists() or self.sysctl_file.is_symlink()
        if exists:
            require(
                exact_file(self.sysctl_file, 0o644) == SYSCTL_TEXT,
                "Existing sysctl file differs",
            )
        return {
            "node": self.node,
            "identity": self.identity,
            "routes": routes,
            "ipForward": forwarding,
            "sysctlFileExists": exists,
            "privateReachabilityVerified": False,
            "unattachedBackend": "10.60.0.5",
        }

    def receipt_path(self):
        return self.directory / "receipt.json"

    def receipt(self):
        info = self.directory.lstat()
        require(
            stat.S_ISDIR(info.st_mode)
            and info.st_uid == os.geteuid()
            and stat.S_IMODE(info.st_mode) == 0o700,
            "Private routing directory required",
        )
        value = json.loads(exact_file(self.receipt_path(), 0o600))
        require(
            value["schemaVersion"] == 1
            and value["node"] == self.node
            and value["before"]["identity"] == self.identity,
            "Routing receipt identity differs",
        )
        return value

    def prepare_directory(self):
        try:
            self.directory.mkdir(mode=0o700)
        except FileExistsError:
            pass
        info = self.directory.lstat()
        require(
            stat.S_ISDIR(info.st_mode)
            and info.st_uid == os.geteuid()
            and stat.S_IMODE(info.st_mode) == 0o700,
            "Private routing directory required",
        )

    def restore(self, receipt):
        before = receipt["before"]
        self.snapshot()
        require(
            before["routes"] == ""
            and before["ipForward"] in {"0", "1"}
            and before["sysctlFileExists"] is False,
            "Original staging baseline required",
        )
        self.call(["/usr/local/bin/tailscale", "set", "--advertise-routes="])
        self.call(["sysctl", "-w", "net.ipv4.ip_forward=" + before["ipForward"]])
        if self.sysctl_file.exists() or self.sysctl_file.is_symlink():
            require(
                exact_file(self.sysctl_file, 0o644) == SYSCTL_TEXT,
                "Owned sysctl file changed",
            )
            self.sysctl_file.unlink()
        after = self.snapshot()
        require(after == before, "Rollback differs from original baseline")
        receipt = receipt | {"status": "rolled-back", "after": after}
        save(self.receipt_path(), receipt)
        return receipt

    def stage(self, policy_hash):
        require(
            isinstance(policy_hash, str)
            and len(policy_hash) == 64
            and all(c in "0123456789abcdef" for c in policy_hash),
            "Reviewed policy SHA256 required",
        )
        before = self.snapshot()
        self.prepare_directory()
        if self.receipt_path().exists():
            receipt = self.receipt()
            require(
                receipt["policySHA256"] == policy_hash,
                "Reviewed policy differs from receipt",
            )
            require(
                receipt["status"] == "staged" and before == receipt["after"],
                "Existing receipt requires explicit reconciliation",
            )
            return receipt
        require(
            before["routes"] == "" and before["sysctlFileExists"] is False,
            "Clean route staging baseline required",
        )
        receipt = {
            "schemaVersion": 1,
            "node": self.node,
            "policySHA256": policy_hash,
            "createdAt": int(time.time()),
            "status": "prepared",
            "before": before,
            "routeApproval": "not-performed",
        }
        save(self.receipt_path(), receipt, fresh=True)
        try:
            descriptor = os.open(
                self.sysctl_file,
                os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                0o644,
            )
            with os.fdopen(descriptor, "wb") as stream:
                os.fchmod(stream.fileno(), 0o644)
                stream.write(SYSCTL_TEXT)
                stream.flush()
                os.fsync(stream.fileno())
            self.call(["sysctl", "-w", "net.ipv4.ip_forward=1"])
            self.call(
                [
                    "/usr/local/bin/tailscale",
                    "set",
                    "--advertise-routes=" + ",".join(ROUTES),
                ]
            )
            after = self.snapshot()
            require(
                after["routes"].split(",") == ROUTES
                and after["ipForward"] == "1"
                and after["sysctlFileExists"],
                "Staged routes did not converge",
            )
            receipt = receipt | {"status": "staged", "after": after}
            save(self.receipt_path(), receipt)
            return receipt
        except BaseException:
            try:
                self.restore(receipt)
            except BaseException:
                save(self.receipt_path(), receipt | {"status": "recovery-required"})
            raise RouteError("Route staging failed; inspect private receipt") from None


def host_run(action, node, policy_hash):
    require(os.geteuid() == 0, "Root required")
    router = Router(node)
    if action == "inspect":
        return router.snapshot()
    descriptor = os.open(
        "/run/lock/platform-api-routing.lock",
        os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW,
        0o600,
    )
    with os.fdopen(descriptor, "w") as lock:
        info = os.fstat(lock.fileno())
        require(
            stat.S_ISREG(info.st_mode)
            and info.st_uid == 0
            and stat.S_IMODE(info.st_mode) == 0o600
            and info.st_nlink == 1,
            "Private routing lock required",
        )
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        return (
            router.stage(policy_hash)
            if action == "stage"
            else router.restore(router.receipt())
        )


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=["inspect", "stage", "rollback"])
    parser.add_argument("node", choices=sorted(NODES))
    parser.add_argument("--policy-sha256")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--local-host", action="store_true", help=argparse.SUPPRESS)
    arguments = parser.parse_args()
    signal.signal(
        signal.SIGTERM,
        lambda *_: (_ for _ in ()).throw(RouteError("Route operation interrupted")),
    )
    if arguments.local_host:
        print(
            json.dumps(
                host_run(arguments.action, arguments.node, arguments.policy_sha256)
            )
        )
        return
    require(arguments.output is not None, "Private output path required")
    parent = arguments.output.parent.lstat()
    require(
        stat.S_ISDIR(parent.st_mode)
        and parent.st_uid == os.geteuid()
        and stat.S_IMODE(parent.st_mode) == 0o700
        and not arguments.output.exists()
        and not arguments.output.is_symlink(),
        "Fresh private output required",
    )
    source = Path(__file__).read_text()
    command = [
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
        arguments.node,
        "sudo -n timeout --signal=TERM --kill-after=5s 80s /usr/bin/python3 -I - "
        + arguments.action
        + " "
        + arguments.node
        + " --local-host"
        + (
            " --policy-sha256=" + arguments.policy_sha256
            if arguments.policy_sha256
            else ""
        ),
    ]
    if arguments.policy_sha256:
        require(
            len(arguments.policy_sha256) == 64
            and all(c in "0123456789abcdef" for c in arguments.policy_sha256),
            "Policy SHA256 invalid",
        )
        policy = Path(__file__).resolve().parents[2] / "tailscale/policy.hujson"
        require(
            hashlib.sha256(policy.read_bytes()).hexdigest() == arguments.policy_sha256,
            "Repository policy differs from reviewed SHA256",
        )
    environment = {
        key: value
        for key, value in os.environ.items()
        if key in {"PATH", "HOME", "TMPDIR"}
    }
    result = subprocess.run(
        command, input=source.encode(), capture_output=True, timeout=90, env=environment
    )
    require(
        result.returncode == 0 and len(result.stdout) <= 65536,
        "Remote routing operation failed; inspect host receipt",
    )
    value = json.loads(result.stdout)
    save(arguments.output, value, fresh=True)
    print(
        json.dumps(
            {
                "node": arguments.node,
                "action": arguments.action,
                "status": value.get("status", "observed"),
                "receipt": str(arguments.output),
            }
        )
    )


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
        print("Routing operation failed; no route approval performed", file=sys.stderr)
        raise SystemExit(1)
