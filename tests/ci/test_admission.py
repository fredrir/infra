import copy
import subprocess
import unittest
from pathlib import Path

import celpy
import yaml
from celpy.adapter import json_to_cel

ROOT = Path(__file__).resolve().parents[2]


class AdmissionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        policies = list(yaml.safe_load_all((ROOT / "platform/components/policy/admission.yaml").read_text()))
        env = celpy.Environment()
        cls.programs = [env.program(env.compile(v["expression"]))
                        for p in policies if p and p["kind"] == "ValidatingAdmissionPolicy"
                        and p["metadata"]["name"] in ["ci-sandbox", "ci-job-credentials", "workload-isolation"]
                        for v in p["spec"]["validations"]]
        release = yaml.safe_load((ROOT / "platform/components/runners/base/buildkit.yaml").read_text())
        cls.pod = copy.deepcopy(release["spec"]["values"]["template"])
        cls.pod.setdefault("metadata", {})["name"] = "runner-1"

    def allowed(self, pod, namespace="ci-y", controller=True):
        request = {"namespace": namespace, "operation": "CREATE", "userInfo": {
            "username": "system:serviceaccount:arc-system:arc-controller" if controller else "untrusted"}}
        try:
            return all(program.evaluate({"object": json_to_cel(pod), "oldObject": json_to_cel(None),
                                         "request": json_to_cel(request)}) for program in self.programs)
        except celpy.CELEvalError:
            return False

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
        release = yaml.safe_load((ROOT / "platform/components/runners/infra/nix.yaml").read_text())
        pod = copy.deepcopy(release["spec"]["values"]["template"])
        pod.setdefault("metadata", {})["name"] = "nix-1"
        self.assertTrue(self.allowed(pod, namespace="ci-infra"))
        self.assertFalse(self.allowed(pod, namespace="ci-y"))
        self.assertFalse(self.allowed(pod, namespace="ci-infra", controller=False))

    def test_sidecars_and_secret_volumes_are_rejected(self):
        pod = copy.deepcopy(self.pod)
        pod["spec"]["containers"].append(copy.deepcopy(pod["spec"]["containers"][0]))
        self.assertFalse(self.allowed(pod))
        pod = copy.deepcopy(self.pod)
        pod["spec"]["volumes"] = [{"name": "credentials", "secret": {"secretName": "github-app"}}]
        self.assertFalse(self.allowed(pod))

    def legacy_pod(self):
        pod = copy.deepcopy(self.pod)
        for field in ["requests", "limits"]:
            del pod["spec"]["containers"][0]["resources"][field]["infra.fredrir.com/ci-slot"]
        pod["metadata"]["labels"] = {"infra.fredrir.com/ci-slot": "build"}
        pod["spec"]["affinity"] = {"podAntiAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": [{
            "labelSelector": {"matchLabels": {"infra.fredrir.com/ci-slot": "build"}},
            "namespaceSelector": {"matchLabels": {"infra.fredrir.com/tier": "ci"}},
            "topologyKey": "kubernetes.io/hostname"}]}}
        return pod

    def test_runner_pools_hold_exactly_one_worker_slot(self):
        resources = self.pod["spec"]["containers"][0]["resources"]
        self.assertEqual(resources["limits"]["infra.fredrir.com/ci-slot"], "1")
        self.assertNotIn("affinity", self.pod["spec"].get("affinity", {}).get("podAntiAffinity", {}))
        for value, allowed in [("1", True), (1, True), ("2", False), ("0", False), ("1000m", False)]:
            with self.subTest(value=value):
                pod = copy.deepcopy(self.pod)
                pod["spec"]["containers"][0]["resources"]["limits"]["infra.fredrir.com/ci-slot"] = value
                self.assertEqual(self.allowed(pod), allowed)
        pod = copy.deepcopy(self.pod)
        del pod["spec"]["containers"][0]["resources"]["limits"]["infra.fredrir.com/ci-slot"]
        self.assertFalse(self.allowed(pod))

    def test_only_the_bounded_infra_deploy_pool_runs_without_a_slot(self):
        release = yaml.safe_load((ROOT / "platform/components/runners/infra/deploy.yaml").read_text())
        pod = copy.deepcopy(release["spec"]["values"]["template"])
        pod["metadata"] = {"name": "deploy-1", "labels": {"actions.github.com/scale-set-name": "deploy-amd64"}}
        self.assertTrue(self.allowed(pod, namespace="ci-infra"))
        for namespace, mutate in [
            ("ci-y", lambda p: None),
            ("ci-infra", lambda p: p["metadata"]["labels"].update({"actions.github.com/scale-set-name": "buildkit-amd64"})),
            ("ci-infra", lambda p: p["spec"]["containers"][0]["resources"]["limits"].update({"cpu": "4"})),
            ("ci-infra", lambda p: p["spec"]["containers"][0]["resources"]["limits"].update({"memory": "8Gi"})),
        ]:
            candidate = copy.deepcopy(pod)
            mutate(candidate)
            self.assertFalse(self.allowed(candidate, namespace=namespace))

    def test_shared_anti_affinity_no_longer_replaces_a_slot(self):
        self.assertFalse(self.allowed(self.legacy_pod()))

    def rust_pod(self, variant):
        rendered = subprocess.run(["kubectl", "kustomize", str(ROOT / "platform/components/runners/rust" / variant)],
                                  check=True, capture_output=True, text=True).stdout
        values = yaml.safe_load(rendered)["spec"]["values"]
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
        pod = self.rust_pod("main")
        for mutate in [
            lambda p: p["metadata"]["labels"].clear(),
            lambda p: p["metadata"]["labels"].update({"actions.github.com/scale-set-name": "buildkit-amd64"}),
            lambda p: p["spec"]["containers"][0].update({"image": self.pod["spec"]["containers"][0]["image"]}),
            lambda p: p["spec"]["containers"][0]["env"][4]["valueFrom"]["secretKeyRef"].update({"key": "AWS_SECRET_ACCESS_KEY"}),
            lambda p: p["spec"]["containers"][0]["env"].append(
                {"name": "GITHUB_TOKEN", "valueFrom": {"secretKeyRef": {"name": "sccache-rw", "key": "AWS_ACCESS_KEY_ID"}}}),
        ]:
            candidate = copy.deepcopy(pod)
            mutate(candidate)
            self.assertFalse(self.allowed(candidate, namespace="ci-example"))

    def cached_buildkit_pod(self):
        component = yaml.safe_load((ROOT / "platform/components/runners/buildkit-cache/kustomization.yaml").read_text())
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
        for mutate in [
            lambda p: p["metadata"]["labels"].clear(),
            lambda p: p["metadata"]["labels"].update({"actions.github.com/scale-set-name": "publish-amd64"}),
            lambda p: p["spec"]["containers"][0].update({"image": self.rust_pod("main")["spec"]["containers"][0]["image"]}),
            lambda p: p["spec"]["containers"][0]["env"][-1]["valueFrom"]["secretKeyRef"].update({"name": "sccache-rw"}),
            lambda p: p["spec"]["containers"][0]["env"][-1]["valueFrom"]["secretKeyRef"].update({"key": "AWS_ACCESS_KEY_ID"}),
            lambda p: p["spec"]["containers"][0]["env"].append(
                {"name": "GITHUB_TOKEN", "valueFrom": {"secretKeyRef": {"name": "buildkit-cache", "key": "AWS_ACCESS_KEY_ID"}}}),
        ]:
            candidate = copy.deepcopy(pod)
            mutate(candidate)
            self.assertFalse(self.allowed(candidate))

    def test_rust_image_is_not_approved_for_kata(self):
        pod = self.rust_pod("pr")
        pod["spec"]["runtimeClassName"] = "kata"
        pod["spec"]["nodeSelector"] = {"node-restriction.kubernetes.io/kata": "true", "kubernetes.io/arch": "amd64"}
        pod["spec"]["securityContext"]["seccompProfile"] = {"type": "Localhost", "localhostProfile": "kata-nix.json"}
        self.assertFalse(self.allowed(pod, namespace="ci-example"))


if __name__ == "__main__":
    unittest.main()
