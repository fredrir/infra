import importlib.util
import json
import socket
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
SPEC = importlib.util.spec_from_file_location(
    "evacuation_routing", ROOT / "scripts/operations/evacuation_routing.py"
)
routing = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(routing)
IMAGE = "sha256:" + "a" * 64


def container(service, workspace):
    value = {
        "Image": IMAGE,
        "State": {"Running": True, "Pid": 3456},
        "Config": {"Env": []},
        "HostConfig": {
            "Memory": 256 * 1024**2,
            "MemorySwap": 256 * 1024**2,
            "PidsLimit": 128,
            "NanoCpus": 1000000000,
            "Privileged": False,
            "SecurityOpt": ["no-new-privileges"],
        },
        "Mounts": [],
    }
    if service == "caddy":
        value["HostConfig"].update(NetworkMode="host", PortBindings={})
        value.update(
            EffectiveCaps=["CAP_NET_BIND_SERVICE"],
            BoundingCaps=["CAP_NET_BIND_SERVICE"],
        )
        value["Mounts"] = [
            {
                "Type": "bind",
                "Source": str(workspace / "Caddyfile"),
                "Destination": "/rehearsal/Caddyfile",
                "RW": False,
            }
        ]
    else:
        value["HostConfig"].update(
            NetworkMode="pasta",
            PortBindings={"8080/tcp": [{"HostIp": "127.0.0.1", "HostPort": "8081"}]},
        )
    return value


class RoutingTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.root.chmod(0o700)

    def pilot(self):
        manifest = {"images": {"llunde-frontend": IMAGE, "caddy": IMAGE}, "sources": {}}
        with patch("evacuation_rehearsal.validate_inputs", return_value=manifest):
            pilot = routing.RoutingPilot(self.root)
        pilot.workspaces = {"edge": self.root, "llunde-frontend": self.root}
        return pilot

    def test_busy_port_is_rejected_without_disturbing_existing_listener(self):
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            listener.listen()
            port = listener.getsockname()[1]
            with self.assertRaisesRegex(ValueError, "occupied"):
                routing.require_free_ports((port,))
            self.assertEqual(listener.getsockname()[1], port)
        routing.require_free_ports((port,))

    def test_exact_private_container_contract_accepts_both_users(self):
        for service in ("llunde-frontend", "caddy"):
            routing.validate_container(
                container(service, self.root), service, IMAGE, self.root
            )

    def test_public_ports_credentials_extra_caps_and_writable_config_are_rejected(self):
        changes = [
            (
                "llunde-frontend",
                lambda value: value["HostConfig"]["PortBindings"]["8080/tcp"][0].update(
                    HostIp="0.0.0.0"
                ),
            ),
            (
                "llunde-frontend",
                lambda value: value["Config"].update(
                    Env=["DOPPLER_TOKEN=private-fixture"]
                ),
            ),
            ("caddy", lambda value: value["BoundingCaps"].append("CAP_SYS_ADMIN")),
            ("caddy", lambda value: value["Mounts"][0].update(RW=True)),
            ("caddy", lambda value: value["HostConfig"].update(SecurityOpt=[])),
            ("caddy", lambda value: value["HostConfig"].update(MemorySwap=-1)),
        ]
        for service, change in changes:
            value = container(service, self.root)
            change(value)
            with self.assertRaises(ValueError):
                routing.validate_container(value, service, IMAGE, self.root)

    def test_listener_validation_binds_caddy_pid_and_rejects_extra_public_listeners(
        self,
    ):
        lines = [
            'LISTEN 0 128 127.0.0.1:8081 0.0.0.0:* users:(("pasta",pid=3333,fd=3))',
            'LISTEN 0 128 127.0.0.1:8085 0.0.0.0:* users:(("caddy",pid=3456,fd=4))',
            'LISTEN 0 128 127.0.0.1:9101 0.0.0.0:* users:(("caddy",pid=3456,fd=5))',
        ]
        self.assertEqual(len(routing.validate_listeners("\n".join(lines), 3456)), 3)
        for value in [
            "\n".join(lines).replace("127.0.0.1:8085", "0.0.0.0:8085"),
            "\n".join(lines).replace("pid=3456", "pid=34567"),
            "\n".join(
                lines
                + [
                    'LISTEN 0 128 127.0.0.1:2019 0.0.0.0:* users:(("caddy",pid=3456,fd=6))'
                ]
            ),
        ]:
            with self.assertRaises(ValueError):
                routing.validate_listeners(value, 3456)

    def test_only_explicit_frontend_path_uses_pasta_and_existing_default_stays_isolated(
        self,
    ):
        pilot = self.pilot()
        document = container("llunde-frontend", self.root)
        pilot.podman = Mock(
            side_effect=[
                subprocess.CompletedProcess([], 0, b"id", b""),
                subprocess.CompletedProcess(
                    [], 0, json.dumps([document]).encode(), b""
                ),
            ]
        )
        pilot.start_route("llunde-frontend")
        arguments = pilot.podman.call_args_list[0].args[1]
        self.assertIn("--network=pasta", arguments)
        self.assertIn("--publish=127.0.0.1:8081:8080", arguments)
        self.assertIn("--timeout=120", arguments)
        self.assertIn(
            "--network=none",
            routing.container_arguments(
                "infra-rehearsal-0123456789ab-test", IMAGE, 256, []
            ),
        )
        with self.assertRaisesRegex(ValueError, "Only frontend"):
            pilot.start_route("llunde-backend")

    def test_caddy_uses_only_private_config_and_required_capability(self):
        pilot = self.pilot()
        (self.root / "Caddyfile").write_text("fixture")
        document = container("caddy", self.root)
        pilot.podman = Mock(
            side_effect=[
                subprocess.CompletedProcess([], 0, b"id", b""),
                subprocess.CompletedProcess(
                    [], 0, json.dumps([document]).encode(), b""
                ),
            ]
        )
        with patch.object(routing.os, "chown"):
            pilot.start_route("caddy")
        arguments = pilot.podman.call_args_list[0].args[1]
        self.assertIn("--network=host", arguments)
        self.assertIn("--cap-drop=ALL", arguments)
        self.assertIn("--cap-add=NET_BIND_SERVICE", arguments)
        self.assertNotIn("--privileged", arguments)
        self.assertFalse(
            any("secret" in value or "cloudflared" in value for value in arguments)
        )

    def test_cross_user_proof_requires_matching_body_hash_and_size(self):
        pilot = self.pilot()
        pilot.start_route = Mock(return_value=3456)
        pilot.request = Mock(
            side_effect=[
                {"sha256": "a" * 64, "bytes": 100},
                {"sha256": "b" * 64, "bytes": 100},
            ]
        )
        with (
            patch.object(routing, "require_free_ports"),
            self.assertRaisesRegex(ValueError, "HTML differ"),
        ):
            pilot.routing()
        self.assertFalse(pilot.result["frontendCrossUserRoutingVerified"])

    def test_failure_always_cleans_owned_resources_and_never_claims_success(self):
        pilot = self.pilot()
        pilot.preflight = Mock()
        pilot.routing = Mock(side_effect=routing.RehearsalError("fixture failure"))
        pilot.cleanup = Mock(return_value=False)
        with (
            patch.object(routing, "RoutingPilot", return_value=pilot),
            patch.object(routing, "require_free_ports"),
        ):
            result = routing.run_routing(self.root)
        pilot.cleanup.assert_called_once()
        self.assertFalse(result["passed"])
        self.assertFalse(result["fullCrossUserRoutingVerified"])
        self.assertFalse(result["backendStarted"])
        self.assertFalse(result["cloudflaredStarted"])


if __name__ == "__main__":
    unittest.main()
