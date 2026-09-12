import ipaddress
import json
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
ROLES = ("tag:platform-control", "tag:platform-worker")
ENROLLMENT = "tag:platform-enrollment"
BACKENDS = ("10.60.0.5", "10.60.0.7", "10.60.0.8")
FIXTURES = {
    "100.92.219.50": {"tag:server"},
    "100.109.80.121": {"tag:server"},
    "100.127.255.1": {"tag:platform-control"},
    "100.127.255.2": {"tag:platform-worker"},
    "100.127.255.3": {"tag:ci"},
    "100.127.255.4": {"tag:server"},
    "100.127.255.5": {ENROLLMENT},
}


def identity(policy, value):
    value = policy["hosts"].get(value, value)
    if value.startswith("tag:"):
        address = next(address for address, tags in FIXTURES.items() if value in tags)
        return address, {value}, None
    try:
        address = str(ipaddress.ip_address(value))
    except ValueError:
        if "@" not in value:
            raise AssertionError(f"Unsupported policy fixture: {value}")
        return None, set(), value
    owner = "fredrir@github" if address in ("100.75.71.79", "100.126.231.24") else None
    return address, FIXTURES.get(address, set()), owner


def matches(policy, selector, entity, source):
    address, tags, owner = entity
    if selector == "*":
        return True
    if selector == "autogroup:member":
        return owner is not None
    if selector == "autogroup:self":
        return owner is not None and owner == source[2]
    if selector.startswith("tag:"):
        return selector in tags
    selector = policy["hosts"].get(selector, selector)
    if "@" in selector:
        return owner == selector
    try:
        network = ipaddress.ip_network(selector, strict=False)
    except ValueError as error:
        raise AssertionError(f"Unsupported policy selector: {selector}") from error
    return address is not None and ipaddress.ip_address(address) in network


def allowed(policy, source, destination, protocol="tcp"):
    src = identity(policy, source)
    host, port = destination.rsplit(":", 1)
    dst = identity(policy, host)
    for rule in policy["acls"]:
        if set(rule) - {"action", "src", "dst", "proto"} or rule["action"] != "accept":
            raise AssertionError("Unsupported ACL fixture syntax")
        if rule.get("proto", protocol) != protocol:
            continue
        if not any(matches(policy, selector, src, src) for selector in rule["src"]):
            continue
        for target in rule["dst"]:
            selector, ports = target.rsplit(":", 1)
            if not matches(policy, selector, dst, src):
                continue
            for value in ports.split(","):
                if value == "*":
                    return True
                bounds = value.split("-")
                if int(bounds[0]) <= int(port) <= int(bounds[-1]):
                    return True
    return False


class TailnetBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.policy = json.loads((ROOT / "tailscale/policy.hujson").read_text())

    def test_native_policy_cases_match_explicit_host_and_tag_fixtures(self):
        for case in self.policy["tests"]:
            protocols = [case["proto"]] if "proto" in case else ["tcp", "udp"]
            for result in ("accept", "deny"):
                for destination in case.get(result, []):
                    with self.subTest(
                        source=case["src"], destination=destination, result=result
                    ):
                        access = any(
                            allowed(self.policy, case["src"], destination, protocol)
                            for protocol in protocols
                        )
                        self.assertEqual(access, result == "accept")

    def test_admin_ssh_and_private_api_access_do_not_grant_etcd(self):
        for source in ("macie", "archie"):
            for role in ROLES:
                self.assertTrue(allowed(self.policy, source, f"{role}:22"))
            for address in BACKENDS:
                self.assertTrue(allowed(self.policy, source, f"{address}:6443"))
                for port in (22, 2379, 2380, 9100, 10250):
                    self.assertFalse(allowed(self.policy, source, f"{address}:{port}"))

    def test_node_transport_is_protocol_specific(self):
        for source in ROLES:
            for target in ROLES:
                self.assertTrue(allowed(self.policy, source, f"{target}:8472", "udp"))
                self.assertFalse(allowed(self.policy, source, f"{target}:8472", "tcp"))
                for port in (9100, 10250):
                    self.assertTrue(
                        allowed(self.policy, source, f"{target}:{port}", "tcp")
                    )
                    self.assertFalse(
                        allowed(self.policy, source, f"{target}:{port}", "udp")
                    )

    def test_ci_legacy_nodes_and_pods_do_not_inherit_node_transport(self):
        for source in ("tag:ci", "tag:server", "fredrir@github", "10.42.0.10"):
            for target in (*ROLES, *BACKENDS):
                for port in (22, 2379, 2380, 6443, 8472, 9100, 10250):
                    for protocol in ("tcp", "udp"):
                        with self.subTest(
                            source=source, target=target, port=port, protocol=protocol
                        ):
                            self.assertFalse(
                                allowed(
                                    self.policy, source, f"{target}:{port}", protocol
                                )
                            )

    def test_private_access_does_not_expand_to_other_hosts_or_ports(self):
        for source in (*ROLES, "macie", "archie"):
            for address in (
                "10.60.0.1",
                "10.60.0.6",
                "10.60.0.9",
                "10.60.1.7",
                "192.0.2.1",
            ):
                for port in (22, 2379, 2380, 6443, 9100, 10250):
                    self.assertFalse(allowed(self.policy, source, f"{address}:{port}"))
            for target in (*ROLES, *BACKENDS):
                for port in (2379, 2380, 5432, 6379, 7443):
                    self.assertFalse(allowed(self.policy, source, f"{target}:{port}"))

    def test_new_roles_cannot_administer_peers_or_admin_computers(self):
        for source in ROLES:
            for target in (*ROLES, "macie", "archie"):
                self.assertFalse(allowed(self.policy, source, f"{target}:22"))

    def test_role_ownership_and_routes_require_admin_review(self):
        self.assertEqual(self.policy["tagOwners"][ENROLLMENT], ["autogroup:admin"])
        for role in ROLES:
            self.assertEqual(
                self.policy["tagOwners"][role], ["autogroup:admin", ENROLLMENT]
            )
        self.assertEqual(
            {
                tag
                for tag, owners in self.policy["tagOwners"].items()
                if ENROLLMENT in owners
            },
            set(ROLES),
        )
        self.assertNotIn("autoApprovers", self.policy)
        self.assertNotIn("grants", self.policy)
        self.assertEqual(
            set(self.policy), {"tagOwners", "hosts", "acls", "ssh", "tests"}
        )

    def test_enrollment_authority_has_no_network_or_ssh_grants(self):
        self.assertNotIn(ENROLLMENT, json.dumps(self.policy["acls"]))
        self.assertNotIn(ENROLLMENT, json.dumps(self.policy["ssh"]))
        for peer in (*ROLES, *BACKENDS, "tag:server", "tag:ci", "macie", "archie"):
            for port in (22, 2379, 2380, 3100, 6443, 8472, 9100, 10250):
                for protocol in ("tcp", "udp"):
                    with self.subTest(peer=peer, port=port, protocol=protocol):
                        self.assertFalse(
                            allowed(self.policy, ENROLLMENT, f"{peer}:{port}", protocol)
                        )
                        self.assertFalse(
                            allowed(self.policy, peer, f"{ENROLLMENT}:{port}", protocol)
                        )

    def test_native_regressions_cover_every_new_role_and_untrusted_source(self):
        cases = self.policy["tests"]
        for source in (
            *ROLES,
            ENROLLMENT,
            "macie",
            "archie",
            "tag:ci",
            "tag:server",
            "fredrir@github",
            "10.42.0.10",
        ):
            self.assertTrue(
                any(
                    case["src"] == source
                    and case.get("proto") == "tcp"
                    and case.get("deny")
                    for case in cases
                )
            )
        for source in ROLES:
            self.assertTrue(
                any(
                    case["src"] == source
                    and case.get("proto") == "udp"
                    and case.get("accept")
                    and case.get("deny")
                    for case in cases
                )
            )
        self.assertTrue(
            any(
                case["src"] == ENROLLMENT
                and case.get("proto") == "udp"
                and case.get("deny")
                and not case.get("accept")
                for case in cases
            )
        )


if __name__ == "__main__":
    unittest.main()
