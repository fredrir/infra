import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[2] / "platform/components/controllers/ci-slots.sh"


def node(name, wanted, advertised=None):
    capacity = {"cpu": "4"} | ({"infra.fredrir.com/ci-slot": advertised} if advertised is not None else {})
    return {"metadata": {"name": name, "labels": {"infra.fredrir.com/ci-slots": wanted}}, "status": {"capacity": capacity}}


class CiSlotReconcilerTests(unittest.TestCase):
    def reconcile(self, nodes):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            (base / "nodes.json").write_text(json.dumps({"items": nodes}))
            kubectl = base / "kubectl"
            kubectl.write_text('#!/bin/sh\nif [ "$1" = get ]; then cat "$FAKE/nodes.json"; else echo "$@" >> "$FAKE/patches"; fi\n')
            kubectl.chmod(0o755)
            result = subprocess.run(["bash", str(SCRIPT)], capture_output=True, text=True, check=False,
                                    env={**os.environ, "PATH": f"{base}:{os.environ['PATH']}", "FAKE": directory, "RECONCILE_ONCE": "1"})
            patches = (base / "patches").read_text().splitlines() if (base / "patches").exists() else []
            return result, patches

    def test_only_drifted_nodes_with_valid_labels_are_patched(self):
        result, patches = self.reconcile([node("fredrir-04", "1", "1"), node("fredrir-09", "3", "0"), node("new", "2"),
                                          node("odd", '3"}}'), node("empty", "")])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(patches), 2)
        self.assertIn('patch node fredrir-09 --subresource=status --type=merge --patch {"status":{"capacity":{"infra.fredrir.com/ci-slot":"3"}}}', patches)
        self.assertTrue(any(p.startswith("patch node new ") and '"2"' in p for p in patches))
        self.assertIn("Advertised 3 CI slots on fredrir-09 (was 0)", result.stdout)
        self.assertIn("Ignoring odd", result.stdout)


if __name__ == "__main__":
    unittest.main()
