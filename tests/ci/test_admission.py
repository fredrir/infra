import copy
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
        cls.pod["metadata"]["name"] = "runner-1"

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
        pod["metadata"]["name"] = "nix-1"
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

    def test_shared_worker_budget_cannot_be_removed_or_narrowed(self):
        pod = copy.deepcopy(self.pod)
        del pod["spec"]["affinity"]
        self.assertFalse(self.allowed(pod))
        pod = copy.deepcopy(self.pod)
        pod["metadata"]["labels"] = {}
        self.assertFalse(self.allowed(pod))
        pod = copy.deepcopy(self.pod)
        term = pod["spec"]["affinity"]["podAntiAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"][0]
        term["namespaceSelector"] = {"matchLabels": {"kubernetes.io/metadata.name": "ci-y"}}
        self.assertFalse(self.allowed(pod))

    def test_a_single_worker_slot_replaces_the_shared_build_affinity(self):
        pod = copy.deepcopy(self.pod)
        del pod["spec"]["affinity"]
        del pod["metadata"]["labels"]
        resources = pod["spec"]["containers"][0]["resources"]
        for value, allowed in [("1", True), (1, True), ("2", False), ("0", False), ("1000m", False)]:
            with self.subTest(value=value):
                resources["limits"]["infra.fredrir.com/ci-slot"] = value
                self.assertEqual(self.allowed(pod), allowed)
        del resources["limits"]["infra.fredrir.com/ci-slot"]
        resources["requests"]["infra.fredrir.com/ci-slot"] = "1"
        self.assertFalse(self.allowed(pod))


if __name__ == "__main__":
    unittest.main()
