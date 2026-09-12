import copy
import importlib.util
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location(
    "tailscale_routes", ROOT / "scripts/operations/tailscale_routes.py"
)
routes = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(routes)


class FakeRouter(routes.Router):
    def __init__(self, root):
        super().__init__("fredrir-07")
        self.directory = root / "receipt"
        self.sysctl_file = root / "sysctl.conf"
        self.forward_file = root / "forward"
        self.forward_file.write_text("0\n")
        self.k3s_paths = [root / "k3s"]
        self.preferences = {
            "accept-routes": False,
            "advertise-routes": "",
            "advertise-exit-node": False,
            "ssh": False,
            "snat-subnet-routes": True,
        }
        self.state = {
            "BackendState": "Running",
            "Self": {
                "ID": self.identity["deviceId"],
                "HostName": self.node,
                "Tags": ["tag:platform-control"],
                "TailscaleIPs": [self.identity["tailnetIP"]],
            },
        }
        self.calls = []
        self.fail_advertisement = False
        self.fail_rollback = False
        self.route_device = "enp7s0"

    def call(self, arguments):
        self.calls.append(arguments)
        if arguments[:2] == ["sysctl", "-w"]:
            self.forward_file.write_text(arguments[2].split("=")[1] + "\n")
            return b""
        if arguments[0].endswith("/tailscale"):
            if arguments[1] == "status":
                return json.dumps(self.state).encode()
            if arguments[1] == "get":
                return json.dumps(self.preferences).encode()
            value = arguments[2].split("=", 1)[1]
            if value and self.fail_advertisement:
                self.fail_advertisement = False
                raise routes.RouteError("fixture failure")
            if not value and self.fail_rollback:
                raise routes.RouteError("fixture rollback failure")
            self.preferences["advertise-routes"] = value
            return b""
        if arguments[:3] == ["ip", "-j", "address"]:
            return json.dumps(
                [
                    {
                        "addr_info": [
                            {"local": self.identity["privateIP"], "prefixlen": 32}
                        ]
                    }
                ]
            ).encode()
        if arguments[:3] == ["ip", "-j", "route"]:
            return json.dumps(
                [{"dev": self.route_device, "prefsrc": self.identity["privateIP"]}]
            ).encode()
        if arguments[0] == "nft":
            return b"chain ts-forward {} chain ts-postrouting { masquerade }"
        raise AssertionError(arguments)


class RouteStagingTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.router = FakeRouter(self.root)
        self.hostname = patch.object(
            routes.socket, "gethostname", return_value=self.router.identity["hostname"]
        )
        self.hostname.start()
        self.addCleanup(self.hostname.stop)
        self.policy = "a" * 64

    def mutations(self):
        return [
            arguments
            for arguments in self.router.calls
            if arguments[0] == "sysctl" or "set" in arguments
        ]

    def test_inspection_is_read_only_and_does_not_claim_backend_health(self):
        result = self.router.snapshot()
        self.assertEqual(self.mutations(), [])
        self.assertFalse(self.router.directory.exists())
        self.assertFalse(result["privateReachabilityVerified"])
        self.assertEqual(result["unattachedBackend"], "10.60.0.5")

    def test_stage_is_idempotent_and_rollback_restores_the_exact_baseline(self):
        before = self.router.snapshot()
        old_umask = os.umask(0o077)
        try:
            result = self.router.stage(self.policy)
        finally:
            os.umask(old_umask)
        self.assertEqual(result["status"], "staged")
        self.assertEqual(result["routeApproval"], "not-performed")
        self.assertEqual(result["after"]["routes"].split(","), routes.ROUTES)
        self.assertEqual(self.router.sysctl_file.stat().st_mode & 0o777, 0o644)
        first_mutations = copy.deepcopy(self.mutations())
        self.assertEqual(self.router.stage(self.policy), result)
        self.assertEqual(self.mutations(), first_mutations)
        self.assertEqual(
            self.router.restore(self.router.receipt())["status"], "rolled-back"
        )
        self.assertEqual(self.router.snapshot(), before)
        self.assertFalse(self.router.sysctl_file.exists())

    def test_failed_advertisement_restores_forwarding_and_owned_file(self):
        self.router.fail_advertisement = True
        with self.assertRaises(routes.RouteError):
            self.router.stage(self.policy)
        self.assertEqual(self.router.receipt()["status"], "rolled-back")
        self.assertEqual(self.router.forward_file.read_text().strip(), "0")
        self.assertFalse(self.router.sysctl_file.exists())
        self.assertEqual(self.router.preferences["advertise-routes"], "")

    def test_unavailable_rollback_remains_explicitly_recoverable(self):
        self.router.fail_advertisement = True
        self.router.fail_rollback = True
        with self.assertRaises(routes.RouteError):
            self.router.stage(self.policy)
        self.assertEqual(self.router.receipt()["status"], "recovery-required")
        self.router.fail_rollback = False
        self.assertEqual(
            self.router.restore(self.router.receipt())["status"], "rolled-back"
        )

    def test_existing_or_symlinked_sysctl_is_never_overwritten(self):
        for kind in ["different", "same", "symlink"]:
            with self.subTest(kind=kind):
                if kind == "symlink":
                    self.router.sysctl_file.symlink_to(self.router.forward_file)
                else:
                    self.router.sysctl_file.write_bytes(
                        routes.SYSCTL_TEXT if kind == "same" else b"private-fixture"
                    )
                    self.router.sysctl_file.chmod(0o644)
                with self.assertRaises((routes.RouteError, OSError)):
                    self.router.stage(self.policy)
                self.assertEqual(self.mutations(), [])
                self.router.sysctl_file.unlink()

    def test_identity_tags_route_drift_and_existing_k3s_refuse_before_changes(self):
        original = copy.deepcopy(self.router.state)
        for key, value in [
            ("ID", "wrong"),
            ("HostName", "fredrir-09"),
            ("Tags", ["tag:platform-worker"]),
        ]:
            self.router.state = copy.deepcopy(original)
            self.router.state["Self"][key] = value
            with self.assertRaises(routes.RouteError):
                self.router.stage(self.policy)
        self.router.state = original
        self.router.route_device = "tailscale0"
        with self.assertRaises(routes.RouteError):
            self.router.stage(self.policy)
        self.router.route_device = "enp7s0"
        self.router.k3s_paths[0].mkdir()
        with self.assertRaises(routes.RouteError):
            self.router.stage(self.policy)
        self.assertEqual(self.mutations(), [])

    def test_unsafe_preferences_and_unexpected_advertisements_are_rejected(self):
        original = copy.deepcopy(self.router.preferences)
        for key, value in [
            ("accept-routes", True),
            ("ssh", True),
            ("snat-subnet-routes", False),
            ("advertise-exit-node", True),
            ("advertise-routes", "0.0.0.0/0"),
            ("advertise-routes", "10.60.0.0/16"),
        ]:
            self.router.preferences = original | {key: value}
            with self.assertRaises(routes.RouteError):
                self.router.stage(self.policy)
        self.assertEqual(self.mutations(), [])

    def test_receipt_and_file_drift_refuse_automatic_reconciliation(self):
        self.router.stage(self.policy)
        with self.assertRaises(routes.RouteError):
            self.router.stage("b" * 64)
        self.router.sysctl_file.write_bytes(b"changed\n")
        mutations = copy.deepcopy(self.mutations())
        with self.assertRaises(routes.RouteError):
            self.router.restore(self.router.receipt())
        self.assertEqual(self.mutations(), mutations)

    def test_recovery_preserves_preexisting_forwarding(self):
        self.router.forward_file.write_text("1\n")
        self.router.stage(self.policy)
        self.router.restore(self.router.receipt())
        self.assertEqual(self.router.forward_file.read_text().strip(), "1")

    def test_only_known_control_plane_targets_are_supported(self):
        self.assertEqual(set(routes.NODES), {"fredrir-07", "fredrir-08"})
        for node in ["fredrir-05", "fredrir-09", "archie", "fredrir-07;false"]:
            with self.assertRaises(routes.RouteError):
                routes.Router(node)


if __name__ == "__main__":
    unittest.main()
