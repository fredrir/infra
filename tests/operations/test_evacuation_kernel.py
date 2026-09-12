import io
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
import evacuation_target as target

STATUS = (
    b"".join(
        key.encode() + b":\t0000000000000000\n"
        for key in ("CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb")
    )
    + b"NoNewPrivs:\t1\n"
)
STAT = b"123 (owned fixture) " + b" ".join([b"S"] + [b"0"] * 18 + [b"777"])


class KernelTests(unittest.TestCase):
    def test_all_five_zero_sets_nnp_and_stable_identity_are_required(self):
        document = {"State": {"Running": True, "Pid": 123}}
        with patch.object(
            Path,
            "open",
            side_effect=[io.BytesIO(STAT), io.BytesIO(STATUS), io.BytesIO(STAT)],
        ):
            proof = target.kernel_profile(document)
        self.assertEqual(
            set(proof["capabilities"]),
            {"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"},
        )
        self.assertTrue(proof["noNewPrivileges"])
        self.assertEqual(proof["processStartIdentity"], "777")
        invalid = [
            STATUS.replace(
                key.encode() + b":\t0000000000000000",
                key.encode() + b":\t0000000000000001",
            )
            for key in proof["capabilities"]
        ]
        invalid += [
            STATUS.replace(b"NoNewPrivs:\t1", b"NoNewPrivs:\t0"),
            STATUS.replace(b"CapAmb:\t0000000000000000\n", b""),
            STATUS + b"CapEff:\t0000000000000000\n",
        ]
        for status in invalid:
            with (
                patch.object(
                    Path, "open", side_effect=[io.BytesIO(STAT), io.BytesIO(status)]
                ),
                self.assertRaisesRegex(ValueError, "Kernel capabilities"),
            ):
                target.kernel_profile(document)
        with (
            patch.object(
                Path,
                "open",
                side_effect=[
                    io.BytesIO(STAT),
                    io.BytesIO(STATUS),
                    io.BytesIO(STAT.replace(b"777", b"778")),
                ],
            ),
            self.assertRaisesRegex(ValueError, "changed"),
        ):
            target.kernel_profile(document)

    def test_exited_or_missing_process_cannot_supply_live_kernel_proof(self):
        for state in (
            {"Running": False, "Pid": 123},
            {"Running": True, "Pid": 0},
            {"Running": True, "Pid": True},
            {},
        ):
            with self.assertRaisesRegex(ValueError, "Running owned"):
                target.kernel_profile({"State": state})

    def test_exact_shell_guard_executes_checker_only_after_own_status_proof(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            checker = root / "valkey-check-aof"
            checker.write_text('#!/bin/sh\nprintf "checker-executed\\n"\n')
            checker.chmod(0o700)
            status = root / "status"
            script = target.CHECKER_GUARD.replace(
                "done < /proc/$$/status", 'done < "$1"'
            )
            for data, expected in (
                (STATUS, 0),
                (
                    STATUS.replace(
                        b"CapBnd:\t0000000000000000", b"CapBnd:\t0000000000000001"
                    ),
                    73,
                ),
                (STATUS.replace(b"NoNewPrivs:\t1", b"NoNewPrivs:\t0"), 73),
                (STATUS.replace(b"CapAmb:\t0000000000000000\n", b""), 73),
            ):
                status.write_bytes(data)
                result = subprocess.run(
                    ["/bin/sh", "-eu", "-c", script, "fixture", str(status)],
                    capture_output=True,
                    timeout=3,
                    env={"PATH": str(root) + ":/usr/bin:/bin"},
                )
                self.assertEqual(result.returncode, expected)
                self.assertEqual(b"checker-executed" in result.stdout, expected == 0)
                self.assertEqual(
                    b"infra-kernel-zero-caps-nnp" in result.stdout, expected == 0
                )


if __name__ == "__main__":
    unittest.main()
