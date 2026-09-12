import hashlib
import json
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/operations"))
import evacuation_transfer as transfer


class TransferTests(unittest.TestCase):
    def test_streams_binary_payload_without_local_files(self):
        producer = [
            sys.executable,
            "-c",
            "import sys;sys.stdout.buffer.write(bytes(range(256))*4096)",
        ]
        consumer = [
            sys.executable,
            "-c",
            "import sys,hashlib;print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())",
        ]
        result = transfer.pipeline(producer, consumer, {}, seconds=3)
        self.assertEqual(
            result.strip().decode(),
            hashlib.sha256(bytes(range(256)) * 4096).hexdigest(),
        )

    def test_pipeline_refuses_producer_failure_and_stops_owned_clients_on_deadline(
        self,
    ):
        consumer = [
            sys.executable,
            "-c",
            'import sys;sys.stdin.buffer.read();print("{}")',
        ]
        with self.assertRaisesRegex(ValueError, "Source pair export"):
            transfer.pipeline(
                [sys.executable, "-c", "import sys;sys.exit(2)"],
                consumer,
                {},
                seconds=3,
            )
        started = time.monotonic()
        with self.assertRaisesRegex(ValueError, "deadline"):
            transfer.pipeline(
                [sys.executable, "-c", "import time;time.sleep(10)"],
                consumer,
                {},
                seconds=0.1,
            )
        self.assertLess(time.monotonic() - started, 3)

    def test_remote_contract_refuses_path_escape_and_keeps_verified_ssh_identity(self):
        python = "/nix/store/" + "a" * 32 + "-python3-3.13.12/bin/python3"
        args = transfer.remote(
            "fredrir-09",
            "/var/lib/platform-evacuation/runs/fresh/pair",
            "forward",
            python,
            True,
        )
        self.assertIn("StrictHostKeyChecking=yes", args)
        self.assertIn("HostName=85.190.100.72", args)
        self.assertEqual(args[args.index("-S") + 1], "none")
        self.assertEqual(args[args.index("-l") + 1], "administrator")
        self.assertIn("receive-pair", args[-1])
        self.assertIn("175s", args[-1])
        for path in [
            "/etc/passwd",
            "/var/lib/platform-evacuation/runs/../pair",
            "/var/lib/platform-evacuation/runs/a;command/pair",
        ]:
            with self.assertRaises(ValueError):
                transfer.remote("fredrir-09", path, "forward", python, True)

    def test_consumer_cleanup_failure_cannot_skip_producer_cleanup(self):
        producer, consumer = Mock(), Mock()
        producer.poll.side_effect = [None, -15]
        consumer.poll.return_value = None
        consumer.terminate.side_effect = OSError("fixture failure")
        status = {}
        with (
            patch.object(
                transfer.subprocess, "Popen", side_effect=[producer, consumer]
            ),
            self.assertRaisesRegex(ValueError, "deadline"),
        ):
            transfer.pipeline(["source"], ["target"], {}, seconds=0, status=status)
        producer.terminate.assert_called_once()
        producer.wait.assert_called_once_with(timeout=2)
        self.assertEqual(status["clients"][0]["errors"], ["OSError"])
        self.assertFalse(status["clients"][0]["stopped"])
        self.assertTrue(status["clients"][1]["stopped"])
        self.assertFalse(status["remoteProcessesImmediatelyCancelled"])

    def test_wrong_destination_manifest_retains_failed_receipt(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "receipt.json"
            Path(directory).chmod(0o700)
            manifest = {
                "schemaVersion": 1,
                "kind": "evacuation-paired-state",
                "direction": "forward",
                "source": "fredrir-05",
                "destination": "fredrir-09",
            }
            with (
                patch.object(
                    transfer, "pipeline", return_value=json.dumps(manifest).encode()
                ),
                self.assertRaisesRegex(ValueError, "sealed source"),
            ):
                transfer.transfer(
                    "/var/lib/platform-evacuation/runs/fresh/pair",
                    "/var/lib/platform-evacuation/runs/fresh/pair",
                    "forward",
                    "b" * 64,
                    "/nix/store/" + "a" * 32 + "-python3-3.13.12/bin/python3",
                    output,
                )
            receipt = json.loads(output.read_text())
            self.assertFalse(receipt["verified"])
            self.assertFalse(receipt["localPlaintextPayloadPersisted"])


if __name__ == "__main__":
    unittest.main()
