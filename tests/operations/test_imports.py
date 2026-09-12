import os
import subprocess
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


class ImportTests(unittest.TestCase):
    def test_script_directory_preserves_standard_library_platform_and_schema_imports(
        self,
    ):
        imports = [
            "import platform; import jsonschema",
            "import evacuation_cutover; import jsonschema; import platform",
        ]
        for sequence in imports:
            with self.subTest(imports=sequence):
                code = (
                    sequence
                    + "\nfrom pathlib import Path\nimport sysconfig\nassert platform.python_implementation()\nassert Path(platform.__file__).resolve() == (Path(sysconfig.get_path('stdlib')) / 'platform.py').resolve()\njsonschema.validate({'enabled': False}, {'type': 'object', 'properties': {'enabled': {'type': 'boolean'}}})\n"
                )
                result = subprocess.run(
                    [sys.executable, "-c", code],
                    cwd=ROOT / "scripts/operations",
                    env={
                        "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
                        "PYTHONDONTWRITEBYTECODE": "1",
                    },
                    capture_output=True,
                    text=True,
                    timeout=20,
                )
                self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
