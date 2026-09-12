import argparse
import json
import os
import re
import signal
import socket
import subprocess
import time
from pathlib import Path

from evacuation_rehearsal import (
    HTTP_PROBE,
    SERVICE_USERS,
    USERS,
    Pilot,
    RehearsalError,
    capability_projection,
    container_arguments,
    require,
)

PORTS = (8080, 8081, 8085, 9101)


def require_free_ports(ports=PORTS):
    sockets = []
    try:
        for port in ports:
            connection = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            sockets.append(connection)
            connection.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            connection.bind(("127.0.0.1", port))
    except OSError:
        raise RehearsalError("Candidate loopback port is already occupied") from None
    finally:
        for connection in sockets:
            connection.close()


def validate_container(document, service, image, workspace):
    require(
        service in ("llunde-frontend", "caddy"), "Only frontend and Caddy may start"
    )
    settings = document["HostConfig"]
    require(
        document["Image"].removeprefix("sha256:") == image.removeprefix("sha256:"),
        "Approved runtime image required",
    )
    require(
        settings["Memory"] == settings["MemorySwap"] == 256 * 1024**2
        and settings["PidsLimit"] == 128,
        "Container memory and task limits differ",
    )
    require(
        settings.get("NanoCpus") == 1000000000
        or (settings.get("CpuQuota") == 100000 and settings.get("CpuPeriod") == 100000),
        "Container CPU limit differs",
    )
    require(
        settings.get("Privileged") is False
        and "no-new-privileges" in settings.get("SecurityOpt", []),
        "Unprivileged container with NNP required",
    )
    require(not document["Config"].get("Secrets"), "Runtime secret mounts forbidden")
    forbidden = {
        "DOPPLER_TOKEN",
        "TUNNEL_TOKEN",
        "DB_PASSWORD",
        "POSTGRES_PASSWORD",
        "CF_API_TOKEN",
        "AWS_ACCESS_KEY_ID",
        "AWS_SECRET_ACCESS_KEY",
    }
    require(
        not any(
            value.split("=", 1)[0] in forbidden
            for value in document["Config"].get("Env", [])
        ),
        "Production credential environment forbidden",
    )
    binds = [mount for mount in document.get("Mounts", []) if mount["Type"] == "bind"]
    if service == "llunde-frontend":
        require(
            settings["NetworkMode"] == "pasta"
            and settings["PortBindings"]
            == {"8080/tcp": [{"HostIp": "127.0.0.1", "HostPort": "8081"}]},
            "Exact frontend loopback port mapping required",
        )
        require(not binds, "Frontend host mounts forbidden")
    else:
        require(
            settings["NetworkMode"] == "host" and not settings.get("PortBindings"),
            "Caddy candidate host network required",
        )
        require(
            len(binds) == 1
            and Path(binds[0]["Source"]).resolve() == workspace.resolve() / "Caddyfile"
            and binds[0]["Destination"] == "/rehearsal/Caddyfile"
            and binds[0]["RW"] is False,
            "Exact read-only candidate Caddyfile required",
        )
        capabilities = capability_projection(document)
        require(
            all(
                isinstance(capabilities[key], list)
                and {cap.removeprefix("CAP_") for cap in capabilities[key]}
                == {"NET_BIND_SERVICE"}
                for key in ("EffectiveCaps", "BoundingCaps")
            ),
            "Caddy requires only NET_BIND_SERVICE",
        )
    require(
        document["State"]["Running"] is True
        and type(document["State"]["Pid"]) is int
        and document["State"]["Pid"] > 1,
        "Running owned container required",
    )


def validate_listeners(output, caddy_pid):
    endpoints, caddy_endpoints = set(), set()
    for line in output.splitlines():
        fields = line.split()
        require(len(fields) >= 5, "Unexpected listening socket response")
        endpoint = fields[3]
        if endpoint.rsplit(":", 1)[-1] in {str(port) for port in PORTS}:
            endpoints.add(endpoint)
        if re.search(rf"\bpid={caddy_pid},", line):
            caddy_endpoints.add(endpoint)
    require(
        endpoints == {"127.0.0.1:8081", "127.0.0.1:8085", "127.0.0.1:9101"},
        "Candidate listeners must remain exactly on loopback",
    )
    require(
        caddy_endpoints == {"127.0.0.1:8085", "127.0.0.1:9101"},
        "Owned Caddy must expose only its two candidate listeners",
    )
    return sorted(endpoints)


class RoutingPilot(Pilot):
    def __init__(self, inputs):
        super().__init__(inputs)
        self.deadline = time.monotonic() + 120
        self.result.update(
            kind="cross-user-frontend-routing",
            frontendCrossUserRoutingVerified=False,
            externalEgressBlocked=False,
        )

    def start_route(self, service):
        require(
            service in ("llunde-frontend", "caddy"), "Only frontend and Caddy may start"
        )
        user, image = SERVICE_USERS[service], self.manifest["images"][service]
        name = f"infra-rehearsal-{self.token}-{'frontend' if service == 'llunde-frontend' else 'caddy'}"
        extra, command = [], []
        if service == "llunde-frontend":
            network = "pasta"
            extra = ["--publish=127.0.0.1:8081:8080"]
        else:
            network = "host"
            config = self.workspaces[user] / "Caddyfile"
            config.write_bytes((self.inputs / "Caddyfile").read_bytes())
            os.chown(config, USERS[user], USERS[user])
            config.chmod(0o400)
            extra = [
                "--cap-drop=ALL",
                "--cap-add=NET_BIND_SERVICE",
                "--volume",
                f"{config}:/rehearsal/Caddyfile:ro",
                "--tmpfs=/data:size=16m",
                "--tmpfs=/config:size=4m",
                "--entrypoint=caddy",
            ]
            command = [
                "run",
                "--config",
                "/rehearsal/Caddyfile",
                "--adapter",
                "caddyfile",
            ]
        arguments = container_arguments(name, image, 256, extra)
        arguments[arguments.index("--network=none")] = "--network=" + network
        arguments[arguments.index("--timeout=300")] = "--timeout=120"
        self.containers.append((user, name))
        self.podman(user, arguments + command)
        document = json.loads(self.podman(user, ["inspect", name]).stdout)[0]
        if service == "caddy":
            self.result["caddyCapabilities"] = capability_projection(document)
        validate_container(document, service, image, self.workspaces[user])
        self.validated_containers.add((user, name))
        return document["State"]["Pid"]

    def request(self, port):
        query = {
            "port": port,
            "path": "/",
            "headers": {"Host": "llunde.no", "CF-Connecting-IP": "192.0.2.10"},
            "html": True,
        }
        response = self.call(
            ["/usr/bin/python3", "-I", "-c", HTTP_PROBE, json.dumps(query)], timeout=10
        )
        receipt = json.loads(response.stdout)
        require(receipt["status"] == 200, "Frontend HTTP status must be 200")
        return receipt

    def routing(self):
        require_free_ports()
        self.result["stage"] = "frontend-loopback-start"
        self.start_route("llunde-frontend")
        self.result["stage"] = "caddy-cross-user-start"
        pid = self.start_route("caddy")
        for _ in range(30):
            try:
                direct, proxied = self.request(8081), self.request(8085)
                break
            except RehearsalError:
                time.sleep(0.25)
        else:
            raise RehearsalError("Cross-user frontend route unavailable")
        require(
            direct["sha256"] == proxied["sha256"]
            and direct["bytes"] == proxied["bytes"],
            "Direct and proxied frontend HTML differ",
        )
        listeners = validate_listeners(
            self.call(["ss", "-H", "-ltnp"], timeout=5).stdout.decode(), pid
        )
        self.result.update(
            frontendCrossUserRoutingVerified=True,
            directFrontend=direct,
            proxiedFrontend=proxied,
            loopbackListeners=listeners,
            frontendUser=2002,
            caddyUser=2000,
        )


def run_routing(inputs):
    pilot = RoutingPilot(inputs)
    started = time.monotonic()

    def interrupted(signum, frame):
        raise RehearsalError("Routing rehearsal interrupted")

    signal.signal(signal.SIGTERM, interrupted)
    try:
        pilot.preflight()
        pilot.routing()
        pilot.result["passed"] = True
    except (
        OSError,
        ValueError,
        KeyError,
        TypeError,
        subprocess.SubprocessError,
    ) as error:
        pilot.result["reason"] = (
            str(error) if isinstance(error, RehearsalError) else type(error).__name__
        )
        pilot.record_failure()
        pilot.capture_caddy_logs()
    finally:
        if not pilot.cleanup():
            pilot.result["passed"] = False
        try:
            for user, name in pilot.containers:
                remaining = (
                    pilot.podman(
                        user, ["ps", "--all", "--format", "{{.Names}}"], cleanup=True
                    )
                    .stdout.decode()
                    .splitlines()
                )
                require(name not in remaining, "Owned routing container remains")
            require_free_ports()
            pilot.result["ownedContainersAbsent"] = True
            pilot.result["loopbackPortsReleased"] = True
        except (OSError, RehearsalError):
            pilot.result["passed"] = False
            pilot.result["cleanupVerificationFailed"] = True
        pilot.result["elapsedSeconds"] = round(time.monotonic() - started, 3)
        if pilot.report:
            (pilot.report / "routing-result.json").write_text(
                json.dumps(pilot.result, indent=2) + "\n"
            )
    return pilot.result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("inputs", type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    result = run_routing(args.inputs)
    print(json.dumps(result, indent=2))
    return 0 if result["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
