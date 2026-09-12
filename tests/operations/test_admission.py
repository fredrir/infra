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
    def test_sandboxed_job_is_admitted(self):
        for name in ["workload-isolation", "ci-sandbox", "ci-job-credentials"]:
            self.assertTrue(admitted(name, pod()), name)

    def test_privileged_init_container_is_denied(self):
        item = pod()
        item["spec"]["initContainers"] = [copy.deepcopy(item["spec"]["containers"][0])]
        item["spec"]["initContainers"][0]["securityContext"]["privileged"] = True
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

    def test_runtime_and_approved_image_cannot_be_replaced(self):
        item = pod()
        item["spec"]["runtimeClassName"] = "runc"
        self.assertFalse(admitted("ci-sandbox", item))
        item = pod()
        item["spec"]["containers"][0]["image"] = "alpine:latest"
        self.assertFalse(admitted("ci-job-credentials", item))

    def test_job_cannot_project_api_token_or_production_secret(self):
        item = pod()
        item["spec"]["volumes"] = [
            {
                "name": "token",
                "projected": {"sources": [{"serviceAccountToken": {"path": "token"}}]},
            }
        ]
        self.assertFalse(admitted("ci-job-credentials", item))
        item = pod()
        item["spec"]["containers"][0]["envFrom"] = [
            {"secretRef": {"name": "production"}}
        ]
        self.assertFalse(admitted("ci-sandbox", item))
        for volume in [
            {"name": "credentials", "secret": {"secretName": "github-app"}},
            {
                "name": "credentials",
                "projected": {"sources": [{"secret": {"name": "github-app"}}]},
            },
        ]:
            item = pod()
            item["spec"]["volumes"] = [volume]
            self.assertFalse(admitted("ci-sandbox", item))

    def test_arc_registration_token_is_scoped_to_controller_and_runner(self):
        item = pod()
        item["spec"]["containers"][0]["name"] = "runner"
        reference = {"name": "job", "key": "jitToken"}
        item["spec"]["containers"][0]["env"] = [
            {
                "name": "ACTIONS_RUNNER_INPUT_JITCONFIG",
                "valueFrom": {"secretKeyRef": reference},
            }
        ]
        controller = "system:serviceaccount:arc-system:arc-controller"
        self.assertTrue(admitted("ci-sandbox", item, user=controller))
        self.assertFalse(admitted("ci-sandbox", item))
        reference["name"] = "github-app"
        self.assertFalse(admitted("ci-sandbox", item, user=controller))

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
