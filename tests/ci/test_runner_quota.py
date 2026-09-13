import subprocess
import unittest
from decimal import Decimal
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]


def millicpu(value):
    value = str(value)
    return Decimal(value[:-1]) if value.endswith("m") else Decimal(value) * 1000


class RunnerQuotaTests(unittest.TestCase):
    def test_infra_quota_admits_both_runner_pools_with_runtime_overhead(self):
        rendered = subprocess.check_output(
            ["kustomize", "build", str(ROOT / "platform/components/runners/infra")],
            text=True,
        )
        resources = list(yaml.safe_load_all(rendered))
        quota = next(r["spec"]["hard"] for r in resources if r["kind"] == "ResourceQuota")
        runtimes = {
            r["metadata"]["name"]: r["overhead"]["podFixed"]["cpu"]
            for r in yaml.safe_load_all((ROOT / "platform/components/policy/runtime.yaml").read_text())
        }
        releases = [r["spec"]["values"] for r in resources if r["kind"] == "HelmRelease"]
        self.assertEqual(len(releases), 2)
        for field in ["requests", "limits"]:
            required = Decimal(0)
            for release in releases:
                pod = release["template"]["spec"]
                per_runner = millicpu(runtimes[pod["runtimeClassName"]])
                per_runner += sum(millicpu(c["resources"][field]["cpu"]) for c in pod["containers"])
                required += per_runner * release["maxRunners"]
            self.assertGreaterEqual(millicpu(quota[f"{field}.cpu"]), required)


if __name__ == "__main__":
    unittest.main()
