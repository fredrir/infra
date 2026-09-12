#!/usr/bin/env python3
import argparse
import ipaddress
import json
import sys
from pathlib import Path

from jsonschema import Draft202012Validator, FormatChecker, ValidationError

ROOT = Path(__file__).resolve().parents[2]


def build_inventory(inventory):
    schema = json.loads((ROOT / "platform/inventory/nodes.schema.json").read_text())
    Draft202012Validator(schema, format_checker=FormatChecker()).validate(inventory)
    identifiers = [node["id"] for node in inventory["nodes"]]
    if len(identifiers) != len(set(identifiers)):
        raise ValueError("Node IDs must be unique")
    pod_network = ipaddress.ip_network(inventory["cluster"]["podCIDR"])
    service_network = ipaddress.ip_network(inventory["cluster"]["serviceCIDR"])
    if (
        pod_network.version != 4
        or service_network.version != 4
        or pod_network.overlaps(service_network)
    ):
        raise ValueError("Pod and service IPv4 networks must not overlap")
    active = [node for node in inventory["nodes"] if node["enrollment"] is not None]
    hostnames = [node["enrollment"]["hostname"] for node in active]
    addresses = [node["enrollment"]["nodeIP"] for node in active]
    if len(hostnames) != len(set(hostnames)) or len(addresses) != len(set(addresses)):
        raise ValueError("Enrolled node hostnames and addresses must be unique")
    cluster_nodes = [node for node in active if node["desiredRole"] != "external"]
    servers = [
        node["enrollment"]["privateIP"]
        for node in cluster_nodes
        if node["desiredRole"] == "server"
    ]
    if cluster_nodes and (len(servers) != 3 or len(set(servers)) != 3):
        raise ValueError(
            "Enrollment requires three distinct control-plane private addresses"
        )
    hostvars = {}
    external = []
    for node in active:
        enrollment = node["enrollment"]
        address = ipaddress.ip_address(enrollment["nodeIP"])
        if address in pod_network or address in service_network:
            raise ValueError("Node addresses must not overlap cluster networks")
        if not node["capabilities"]["verified"]:
            raise ValueError("Enrolled hardware requires verified preflight approval")
        if node["desiredRole"] == "server":
            if enrollment["privateIP"] != enrollment["nodeIP"]:
                raise ValueError(
                    "Control planes must use their private address as node IP"
                )
            if any(
                value
                for key, value in node["capabilities"].items()
                if key != "verified"
            ):
                raise ValueError(
                    "Control planes cannot advertise workload capabilities"
                )
        if node["osAdapter"] != "ansible":
            continue
        if node["desiredRole"] == "external":
            hostvars[node["id"]] = {
                "ansible_host": enrollment["sshAddress"],
                "platform_watchdog_enable": False,
            }
            external.append(node["id"])
            continue
        hostvars[node["id"]] = {
            "ansible_host": enrollment["sshAddress"],
            "platform_node_name": enrollment["hostname"],
            "platform_admin_keys": enrollment["adminKeys"],
            "platform_node_ip": enrollment["nodeIP"],
            "platform_private_ip": enrollment.get("privateIP", ""),
            "platform_private_interface": enrollment.get("privateInterface", ""),
            "platform_role": "server" if node["desiredRole"] == "server" else "agent",
            "platform_architecture": node["architecture"],
            "platform_api_backends": servers,
            "platform_api_name": inventory["cluster"]["apiName"],
            "platform_pod_cidr": inventory["cluster"]["podCIDR"],
            "platform_service_cidr": inventory["cluster"]["serviceCIDR"],
            "platform_preflight_approved": True,
            "platform_sandbox_enabled": node["capabilities"]["ci"],
        }
    return {
        "_meta": {"hostvars": hostvars},
        "platform": {"hosts": [name for name in hostvars if name not in external]},
        "platform_external": {"hosts": external},
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--list", action="store_true")
    parser.add_argument("--host")
    args = parser.parse_args()
    try:
        result = build_inventory(
            json.loads((ROOT / "platform/inventory/nodes.json").read_text())
        )
        print(
            json.dumps(
                result["_meta"]["hostvars"].get(args.host, {}) if args.host else result
            )
        )
    except (OSError, ValueError, KeyError, ValidationError) as error:
        print(f"Platform inventory refused: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
