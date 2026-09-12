import copy
import unittest
from pathlib import Path

import celpy
import yaml
from celpy.adapter import json_to_cel

ROOT = Path(__file__).resolve().parents[2]
POLICIES = {
    item["metadata"]["name"]: item
    for item in yaml.safe_load_all(
        (ROOT / "platform/components/policy/admission.yaml").read_text()
    )
    if item["kind"] == "ValidatingAdmissionPolicy"
}
IMAGE = "ghcr.io/fredrir/infra-ci@sha256:" + "a" * 64


def admitted(
    name,
    obj,
    user="system:serviceaccount:ci-portfolio-amd64:runner",
    operation="CREATE",
    old=None,
):
    environment = celpy.Environment()
    context = json_to_cel(
        {
            "object": obj,
            "oldObject": old or {},
            "request": {
                "operation": operation,
                "namespace": "portfolio",
                "userInfo": {"username": user},
            },
        }
    )
    for validation in POLICIES[name]["spec"]["validations"]:
        expression = validation["expression"].replace("${CI_IMAGE}", IMAGE)
        try:
            value = environment.program(environment.compile(expression)).evaluate(
                context
            )
            if not bool(value):
                return False
        except celpy.CELEvalError:
            return False
    return True


def pod():
    return {
        "metadata": {"name": "job"},
        "spec": {
            "runtimeClassName": "gvisor",
            "priorityClassName": "ci",
            "serviceAccountName": "ci-job",
            "automountServiceAccountToken": False,
            "nodeSelector": {"node-restriction.kubernetes.io/ci": "true"},
            "containers": [
                {
                    "name": "job",
                    "image": IMAGE,
                    "securityContext": {
                        "allowPrivilegeEscalation": False,
                        "capabilities": {"drop": ["ALL"]},
                    },
                    "resources": {
                        "requests": {"cpu": "100m", "memory": "128Mi"},
                        "limits": {"cpu": "1", "memory": "512Mi"},
                    },
                }
            ],
        },
    }


class AdmissionTests(unittest.TestCase):
    def test_isolated_workload_is_admitted(self):
        self.assertTrue(admitted("workload-isolation", pod()))

    def test_privileged_init_container_is_denied(self):
        item = pod()
        item["spec"]["initContainers"] = [copy.deepcopy(item["spec"]["containers"][0])]
        item["spec"]["initContainers"][0]["securityContext"]["privileged"] = True
        self.assertFalse(admitted("workload-isolation", item))

    def test_init_container_requires_resources_and_no_escalation(self):
        item = pod()
        item["spec"]["initContainers"] = [copy.deepcopy(item["spec"]["containers"][0])]
        self.assertTrue(admitted("workload-isolation", item))
        item["spec"]["initContainers"][0]["resources"] = {}
        self.assertFalse(admitted("workload-isolation", item))

    def test_ephemeral_container_cannot_escalate(self):
        item = pod()
        container = copy.deepcopy(item["spec"]["containers"][0])
        del container["resources"]
        item["spec"]["ephemeralContainers"] = [container]
        self.assertTrue(admitted("workload-isolation", item))
        container["securityContext"]["allowPrivilegeEscalation"] = True
        self.assertFalse(admitted("workload-isolation", item))

    def test_host_path_and_control_plane_assignment_are_denied(self):
        for key, value in [
            ("volumes", [{"name": "root", "hostPath": {"path": "/"}}]),
            ("nodeName", "control-1"),
            ("tolerations", [{"operator": "Exists"}]),
        ]:
            item = pod()
            item["spec"][key] = value
            self.assertFalse(admitted("workload-isolation", item), key)

    def test_unchanged_scheduled_node_on_update_is_permitted(self):
        item = pod()
        item["spec"]["nodeName"] = "verified-worker"
        self.assertTrue(
            admitted("workload-isolation", item, operation="UPDATE", old=item)
        )

    def test_cross_namespace_and_private_network_egress_are_denied(self):
        for peer in [
            {"namespaceSelector": {}},
            {"ipBlock": {"cidr": "100.64.0.0/10"}},
            {"ipBlock": {"cidr": "0.0.0.0/0"}},
        ]:
            item = {"spec": {"egress": [{"to": [peer], "ports": [{"port": 443}]}]}}
            self.assertFalse(admitted("project-network-boundary", item))

    def test_public_https_and_same_project_database_are_allowed(self):
        public = {
            "ipBlock": {
                "cidr": "0.0.0.0/0",
                "except": [
                    "10.0.0.0/8",
                    "172.16.0.0/12",
                    "192.168.0.0/16",
                    "100.64.0.0/10",
                    "169.254.0.0/16",
                    "127.0.0.0/8",
                ],
            }
        }
        item = {
            "spec": {
                "egress": [
                    {"to": [public], "ports": [{"port": 443, "protocol": "TCP"}]},
                    {
                        "to": [{"podSelector": {"matchLabels": {"app": "database"}}}],
                        "ports": [{"port": 5432}],
                    },
                ]
            }
        }
        self.assertTrue(admitted("project-network-boundary", item))
        item["spec"]["egress"][0]["ports"][0]["endPort"] = 65535
        self.assertFalse(admitted("project-network-boundary", item))

    def test_tunnel_ports_preserve_private_network_boundary(self):
        public = {
            "ipBlock": {
                "cidr": "0.0.0.0/0",
                "except": [
                    "10.0.0.0/8",
                    "172.16.0.0/12",
                    "192.168.0.0/16",
                    "100.64.0.0/10",
                    "169.254.0.0/16",
                    "127.0.0.0/8",
                ],
            }
        }
        for protocol in ["TCP", "UDP"]:
            item = {
                "spec": {
                    "egress": [
                        {
                            "to": [copy.deepcopy(public)],
                            "ports": [{"port": 7844, "protocol": protocol}],
                        }
                    ]
                }
            }
            self.assertTrue(admitted("project-network-boundary", item))
            item["spec"]["egress"][0]["to"][0]["ipBlock"]["except"].remove(
                "100.64.0.0/10"
            )
            self.assertFalse(admitted("project-network-boundary", item))

    def test_baseline_delete_requires_platform_identity(self):
        old = {"metadata": {"name": "default-deny"}}
        self.assertFalse(
            admitted("project-baseline-owner", {}, operation="DELETE", old=old)
        )
        self.assertTrue(
            admitted(
                "project-baseline-owner",
                {},
                user="system:serviceaccount:flux-system:platform-reconciler",
                operation="DELETE",
                old=old,
            )
        )

    def test_external_secret_cannot_import_another_store_or_all_keys(self):
        item = {
            "spec": {
                "secretStoreRef": {"kind": "SecretStore", "name": "runtime"},
                "target": {"name": "project-runtime"},
            }
        }
        self.assertTrue(admitted("project-secret-boundary", item))
        item["spec"]["dataFrom"] = [{"extract": {"key": "all"}}]
        self.assertFalse(admitted("project-secret-boundary", item))
        del item["spec"]["dataFrom"]
        item["spec"]["secretStoreRef"] = {
            "kind": "ClusterSecretStore",
            "name": "shared",
        }
        self.assertFalse(admitted("project-secret-boundary", item))

    def test_registry_secret_is_owned_by_the_platform(self):
        item = {
            "spec": {
                "secretStoreRef": {"kind": "SecretStore", "name": "runtime"},
                "target": {"name": "project-registry"},
                "data": [
                    {
                        "secretKey": ".dockerconfigjson",
                        "remoteRef": {"key": "GHCR_DOCKER_CONFIG_JSON"},
                    }
                ],
            }
        }
        self.assertFalse(admitted("project-secret-boundary", item))
        self.assertTrue(
            admitted(
                "project-secret-boundary",
                item,
                user="system:serviceaccount:flux-system:platform-reconciler",
            )
        )


if __name__ == "__main__":
    unittest.main()
