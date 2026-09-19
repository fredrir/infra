import copy
import functools
import subprocess
import unittest
from pathlib import Path

import celpy
import yaml
from celpy.adapter import json_to_cel

ROOT = Path(__file__).resolve().parents[2]
RUNNERS = ROOT / "platform/components/runners"
ENVIRONMENT = celpy.Environment()
POLICIES = {
    policy["metadata"]["name"]: [ENVIRONMENT.program(ENVIRONMENT.compile(validation["expression"]))
                                 for validation in policy["spec"]["validations"]]
    for policy in yaml.safe_load_all((ROOT / "platform/components/policy/admission.yaml").read_text())
    if policy and policy["kind"] == "ValidatingAdmissionPolicy"
}
CONTROLLER = "system:serviceaccount:arc-system:arc-controller"
RECONCILER = "system:serviceaccount:flux-system:platform-reconciler"
PROJECT_RUNNER = "system:serviceaccount:ci-portfolio-amd64:runner"
IMAGE = "ghcr.io/fredrir/infra-ci@sha256:" + "a" * 64
PUBLIC = {"ipBlock": {"cidr": "0.0.0.0/0", "except": [
    "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "127.0.0.0/8"]}}


def admitted(policies, obj, *, namespace, user, operation="CREATE", old=None):
    context = {"object": json_to_cel(obj), "oldObject": json_to_cel(old), "request": json_to_cel(
        {"namespace": namespace, "operation": operation, "userInfo": {"username": user}})}
    try:
        return all(bool(program.evaluate(context)) for name in policies for program in POLICIES[name])
    except celpy.CELEvalError:
        return False


def runner_template(path):
    release = yaml.safe_load((RUNNERS / path).read_text())
    return copy.deepcopy(release["spec"]["values"]["template"])


@functools.cache
def rendered_rust_pool(variant):
    rendered = subprocess.run(["kubectl", "kustomize", str(RUNNERS / "rust" / variant)],
                              check=True, capture_output=True, text=True).stdout
    return yaml.safe_load(rendered)["spec"]["values"]


class RunnerAdmissionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.pod = runner_template("base/buildkit.yaml")
        cls.pod.setdefault("metadata", {})["name"] = "runner-1"

    def allowed(self, pod, namespace="ci-y", controller=True):
        return admitted(["ci-sandbox", "ci-job-credentials", "workload-isolation"], pod, namespace=namespace,
                        user=CONTROLLER if controller else "untrusted")

    def assert_every_mutation_is_rejected(self, pod, mutations, **request):
        for index, mutate in enumerate(mutations):
            with self.subTest(mutation=index):
                candidate = copy.deepcopy(pod)
                mutate(candidate)
                self.assertFalse(self.allowed(candidate, **request))

    def test_runner_without_cluster_credentials_is_allowed(self):
        self.assertTrue(self.allowed(copy.deepcopy(self.pod)))

    def test_host_access_api_token_and_unapproved_runtime_are_rejected(self):
        for name, value in [("hostNetwork", True), ("automountServiceAccountToken", True),
                            ("runtimeClassName", "runc"), ("activeDeadlineSeconds", 86400)]:
            with self.subTest(name=name):
                pod = copy.deepcopy(self.pod)
                pod["spec"][name] = value
                self.assertFalse(self.allowed(pod))

    def test_only_controller_can_attach_the_runners_own_jit_token(self):
        pod = copy.deepcopy(self.pod)
        secret = {"name": "ACTIONS_RUNNER_INPUT_JITCONFIG", "valueFrom": {"secretKeyRef": {"name": "runner-1", "key": "jitToken"}}}
        pod["spec"]["containers"][0]["env"].append(secret)
        self.assertTrue(self.allowed(pod))
        self.assertFalse(self.allowed(pod, controller=False))
        secret["valueFrom"]["secretKeyRef"]["name"] = "github-app"
        self.assertFalse(self.allowed(pod))

    def test_attic_token_is_limited_to_infra_kata(self):
        pod = runner_template("infra/nix.yaml")
        pod.setdefault("metadata", {})["name"] = "nix-1"
        self.assertTrue(self.allowed(pod, namespace="ci-infra"))
        self.assertFalse(self.allowed(pod, namespace="ci-y"))
        self.assertFalse(self.allowed(pod, namespace="ci-infra", controller=False))

    def test_sidecars_and_secret_volumes_are_rejected(self):
        self.assert_every_mutation_is_rejected(self.pod, [
            lambda p: p["spec"]["containers"].append(copy.deepcopy(p["spec"]["containers"][0])),
            lambda p: p["spec"].update({"volumes": [{"name": "credentials", "secret": {"secretName": "github-app"}}]}),
        ])

    def test_runner_pools_hold_exactly_one_worker_slot(self):
        limits = self.pod["spec"]["containers"][0]["resources"]["limits"]
        self.assertEqual(limits["infra.fredrir.com/ci-slot"], "1")
        for value, allowed in [("1", True), (1, True), ("2", False), ("0", False), ("1000m", False)]:
            with self.subTest(value=value):
                pod = copy.deepcopy(self.pod)
                pod["spec"]["containers"][0]["resources"]["limits"]["infra.fredrir.com/ci-slot"] = value
                self.assertEqual(self.allowed(pod), allowed)
        pod = copy.deepcopy(self.pod)
        del pod["spec"]["containers"][0]["resources"]["limits"]["infra.fredrir.com/ci-slot"]
        self.assertFalse(self.allowed(pod))

    def test_check_pool_runs_the_approved_check_image_with_a_slot(self):
        release = yaml.safe_load((ROOT / "platform/components/runners/infra/check.yaml").read_text())
        pod = copy.deepcopy(release["spec"]["values"]["template"])
        pod["metadata"] = {"name": "check-1", "labels": {"actions.github.com/scale-set-name": "check-amd64"}}
        self.assertTrue(self.allowed(pod, namespace="ci-infra"))
        pod["spec"]["containers"][0]["image"] = "ghcr.io/fredrir/infra-runner-check@sha256:" + "f" * 64
        self.assertFalse(self.allowed(pod, namespace="ci-infra"))

    def test_only_the_bounded_infra_deploy_pool_runs_without_a_slot(self):
        pod = runner_template("infra/deploy.yaml")
        pod["metadata"] = {"name": "deploy-1", "labels": {"actions.github.com/scale-set-name": "deploy-amd64"}}
        self.assertTrue(self.allowed(pod, namespace="ci-infra"))
        self.assertFalse(self.allowed(pod, namespace="ci-y"))
        self.assert_every_mutation_is_rejected(pod, [
            lambda p: p["metadata"]["labels"].update({"actions.github.com/scale-set-name": "buildkit-amd64"}),
            lambda p: p["spec"]["containers"][0]["resources"]["limits"].update({"cpu": "4"}),
            lambda p: p["spec"]["containers"][0]["resources"]["limits"].update({"memory": "8Gi"}),
        ], namespace="ci-infra")

    def rust_pod(self, variant):
        values = rendered_rust_pool(variant)
        pod = copy.deepcopy(values["template"])
        pod["metadata"] = {"name": "rust-1", "labels": {"actions.github.com/scale-set-name": values["runnerScaleSetName"]}}
        return pod

    def test_each_rust_pool_only_receives_its_own_cache_credentials(self):
        pools = {"pr": "sccache-ro", "main": "sccache-rw", "release": "sccache-release"}
        for variant, own in pools.items():
            pod = self.rust_pod(variant)
            self.assertTrue(self.allowed(pod, namespace="ci-example"), variant)
            self.assertFalse(self.allowed(pod, namespace="ci-example", controller=False), variant)
            for other in set(pools.values()) - {own}:
                with self.subTest(variant=variant, secret=other):
                    stolen = copy.deepcopy(pod)
                    for variable in stolen["spec"]["containers"][0]["env"]:
                        if variable["name"].startswith("AWS_"):
                            variable["valueFrom"]["secretKeyRef"]["name"] = other
                    self.assertFalse(self.allowed(stolen, namespace="ci-example"))

    def test_rust_cache_credentials_require_the_rust_image_pool_label_and_matching_key(self):
        self.assert_every_mutation_is_rejected(self.rust_pod("main"), [
            lambda p: p["metadata"]["labels"].clear(),
            lambda p: p["metadata"]["labels"].update({"actions.github.com/scale-set-name": "buildkit-amd64"}),
            lambda p: p["spec"]["containers"][0].update({"image": self.pod["spec"]["containers"][0]["image"]}),
            lambda p: p["spec"]["containers"][0]["env"][4]["valueFrom"]["secretKeyRef"].update({"key": "AWS_SECRET_ACCESS_KEY"}),
            lambda p: p["spec"]["containers"][0]["env"].append(
                {"name": "GITHUB_TOKEN", "valueFrom": {"secretKeyRef": {"name": "sccache-rw", "key": "AWS_ACCESS_KEY_ID"}}}),
        ], namespace="ci-example")

    def cached_buildkit_pod(self):
        component = yaml.safe_load((RUNNERS / "buildkit-cache/kustomization.yaml").read_text())
        pod = copy.deepcopy(self.pod)
        pod["metadata"]["labels"] = {"actions.github.com/scale-set-name": "buildkit-amd64"}
        pod["spec"]["containers"][0]["env"] += [o["value"] for o in yaml.safe_load(component["patches"][0]["patch"])]
        return pod

    def test_layer_cache_credentials_require_the_buildkit_image_pool_label_and_own_secret(self):
        pod = self.cached_buildkit_pod()
        self.assertTrue(self.allowed(pod))
        self.assertFalse(self.allowed(pod, controller=False))
        credentials = [e for e in pod["spec"]["containers"][0]["env"] if e["name"].startswith("AWS_")]
        self.assertEqual([e["valueFrom"]["secretKeyRef"]["name"] for e in credentials], ["buildkit-cache"] * 2)
        self.assert_every_mutation_is_rejected(pod, [
            lambda p: p["metadata"]["labels"].clear(),
            lambda p: p["metadata"]["labels"].update({"actions.github.com/scale-set-name": "publish-amd64"}),
            lambda p: p["spec"]["containers"][0].update({"image": self.rust_pod("main")["spec"]["containers"][0]["image"]}),
            lambda p: p["spec"]["containers"][0]["env"][-1]["valueFrom"]["secretKeyRef"].update({"name": "sccache-rw"}),
            lambda p: p["spec"]["containers"][0]["env"][-1]["valueFrom"]["secretKeyRef"].update({"key": "AWS_ACCESS_KEY_ID"}),
            lambda p: p["spec"]["containers"][0]["env"].append(
                {"name": "GITHUB_TOKEN", "valueFrom": {"secretKeyRef": {"name": "buildkit-cache", "key": "AWS_ACCESS_KEY_ID"}}}),
        ])

    def test_rust_image_is_not_approved_for_kata(self):
        pod = self.rust_pod("pr")
        pod["spec"]["runtimeClassName"] = "kata"
        pod["spec"]["nodeSelector"] = {"node-restriction.kubernetes.io/kata": "true", "kubernetes.io/arch": "amd64"}
        pod["spec"]["securityContext"]["seccompProfile"] = {"type": "Localhost", "localhostProfile": "kata-nix.json"}
        self.assertFalse(self.allowed(pod, namespace="ci-example"))


def project_pod():
    return {
        "metadata": {"name": "job"},
        "spec": {
            "runtimeClassName": "gvisor", "priorityClassName": "ci", "serviceAccountName": "ci-job",
            "automountServiceAccountToken": False, "nodeSelector": {"node-restriction.kubernetes.io/ci": "true"},
            "containers": [{
                "name": "job", "image": IMAGE,
                "securityContext": {"allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]}},
                "resources": {"requests": {"cpu": "100m", "memory": "128Mi"}, "limits": {"cpu": "1", "memory": "512Mi"}},
            }],
        },
    }


def egress(peer, **port):
    return {"spec": {"egress": [{"to": [copy.deepcopy(peer)], "ports": [port]}]}}


class ProjectAdmissionTests(unittest.TestCase):
    def allowed(self, policy, obj, user=PROJECT_RUNNER, **request):
        return admitted([policy], obj, namespace="portfolio", user=user, **request)

    def test_isolated_workload_is_admitted(self):
        self.assertTrue(self.allowed("workload-isolation", project_pod()))

    def test_init_containers_need_resources_and_no_privilege(self):
        pod = project_pod()
        pod["spec"]["initContainers"] = [copy.deepcopy(pod["spec"]["containers"][0])]
        self.assertTrue(self.allowed("workload-isolation", pod))
        for mutate in [lambda c: c.update({"resources": {}}), lambda c: c["securityContext"].update({"privileged": True})]:
            candidate = copy.deepcopy(pod)
            mutate(candidate["spec"]["initContainers"][0])
            self.assertFalse(self.allowed("workload-isolation", candidate))

    def test_ephemeral_container_cannot_escalate(self):
        pod = project_pod()
        container = copy.deepcopy(pod["spec"]["containers"][0])
        del container["resources"]
        pod["spec"]["ephemeralContainers"] = [container]
        self.assertTrue(self.allowed("workload-isolation", pod))
        container["securityContext"]["allowPrivilegeEscalation"] = True
        self.assertFalse(self.allowed("workload-isolation", pod))

    def test_host_path_and_control_plane_assignment_are_denied(self):
        for key, value in [("volumes", [{"name": "root", "hostPath": {"path": "/"}}]),
                           ("nodeName", "control-1"), ("tolerations", [{"operator": "Exists"}])]:
            with self.subTest(key=key):
                pod = project_pod()
                pod["spec"][key] = value
                self.assertFalse(self.allowed("workload-isolation", pod))

    def test_unchanged_scheduled_node_on_update_is_permitted(self):
        pod = project_pod()
        pod["spec"]["nodeName"] = "verified-worker"
        self.assertTrue(self.allowed("workload-isolation", pod, operation="UPDATE", old=pod))

    def test_cross_namespace_and_private_network_egress_are_denied(self):
        for peer in [{"namespaceSelector": {}}, {"ipBlock": {"cidr": "100.64.0.0/10"}}, {"ipBlock": {"cidr": "0.0.0.0/0"}}]:
            with self.subTest(peer=peer):
                self.assertFalse(self.allowed("project-network-boundary", egress(peer, port=443)))

    def test_public_https_and_same_project_database_are_allowed(self):
        policy = egress(PUBLIC, port=443, protocol="TCP")
        policy["spec"]["egress"].append({"to": [{"podSelector": {"matchLabels": {"app": "database"}}}], "ports": [{"port": 5432}]})
        self.assertTrue(self.allowed("project-network-boundary", policy))
        policy["spec"]["egress"][0]["ports"][0]["endPort"] = 65535
        self.assertFalse(self.allowed("project-network-boundary", policy))

    def test_tunnel_ports_preserve_private_network_boundary(self):
        for protocol in ["TCP", "UDP"]:
            with self.subTest(protocol=protocol):
                policy = egress(PUBLIC, port=7844, protocol=protocol)
                self.assertTrue(self.allowed("project-network-boundary", policy))
                policy["spec"]["egress"][0]["to"][0]["ipBlock"]["except"].remove("100.64.0.0/10")
                self.assertFalse(self.allowed("project-network-boundary", policy))

    def test_baseline_delete_requires_platform_identity(self):
        request = {"operation": "DELETE", "old": {"metadata": {"name": "default-deny"}}}
        self.assertFalse(self.allowed("project-baseline-owner", {}, **request))
        self.assertTrue(self.allowed("project-baseline-owner", {}, user=RECONCILER, **request))

    def test_external_secret_cannot_import_another_store_or_all_keys(self):
        secret = {"spec": {"secretStoreRef": {"kind": "SecretStore", "name": "runtime"}, "target": {"name": "project-runtime"}}}
        self.assertTrue(self.allowed("project-secret-boundary", secret))
        secret["spec"]["dataFrom"] = [{"extract": {"key": "all"}}]
        self.assertFalse(self.allowed("project-secret-boundary", secret))
        del secret["spec"]["dataFrom"]
        secret["spec"]["secretStoreRef"] = {"kind": "ClusterSecretStore", "name": "shared"}
        self.assertFalse(self.allowed("project-secret-boundary", secret))

    def test_registry_secret_is_owned_by_the_platform(self):
        secret = {"spec": {
            "secretStoreRef": {"kind": "SecretStore", "name": "runtime"}, "target": {"name": "project-registry"},
            "data": [{"secretKey": ".dockerconfigjson", "remoteRef": {"key": "GHCR_DOCKER_CONFIG_JSON"}}],
        }}
        self.assertFalse(self.allowed("project-secret-boundary", secret))
        self.assertTrue(self.allowed("project-secret-boundary", secret, user=RECONCILER))


if __name__ == "__main__":
    unittest.main()
