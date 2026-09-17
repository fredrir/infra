import subprocess
import unittest
from decimal import Decimal
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
RUNNERS = ROOT / "platform/components/runners"
UNITS = {"Ki": 2**10, "Mi": 2**20, "Gi": 2**30, "m": Decimal("0.001")}


def quantity(value):
    value = str(value)
    for suffix, factor in UNITS.items():
        if value.endswith(suffix):
            return Decimal(value[:-len(suffix)]) * factor
    return Decimal(value)


class RunnerQuotaTests(unittest.TestCase):
    def test_every_overlay_quota_admits_all_of_its_runners_with_runtime_overhead(self):
        overhead = {r["metadata"]["name"]: r["overhead"]["podFixed"]
                    for r in yaml.safe_load_all((ROOT / "platform/components/policy/runtime.yaml").read_text())}
        overlays = yaml.safe_load((RUNNERS / "kustomization.yaml").read_text())["resources"]
        self.assertIn("infra", overlays)
        for overlay in overlays:
            with self.subTest(overlay=overlay):
                rendered = subprocess.check_output(["kustomize", "build", str(RUNNERS / overlay)], text=True)
                resources = list(yaml.safe_load_all(rendered))
                quota = next(r["spec"]["hard"] for r in resources if r["kind"] == "ResourceQuota")
                releases = [r["spec"]["values"] for r in resources if r["kind"] == "HelmRelease"]
                self.assertGreaterEqual(quantity(quota["pods"]), sum(r["maxRunners"] for r in releases))
                for field in ["requests", "limits"]:
                    for resource in ["cpu", "memory"]:
                        required = sum((quantity(overhead[r["template"]["spec"]["runtimeClassName"]][resource])
                                        + sum(quantity(c["resources"][field][resource]) for c in r["template"]["spec"]["containers"]))
                                       * r["maxRunners"] for r in releases)
                        self.assertGreaterEqual(quantity(quota[f"{field}.{resource}"]), required, f"{field}.{resource}")
                slots = sum(quantity(c["resources"]["requests"].get("infra.fredrir.com/ci-slot", 0)) * r["maxRunners"]
                            for r in releases for c in r["template"]["spec"]["containers"])
                if slots:
                    self.assertGreaterEqual(quantity(quota["requests.infra.fredrir.com/ci-slot"]), slots)


if __name__ == "__main__":
    unittest.main()
