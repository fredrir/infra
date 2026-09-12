import argparse
import hashlib
import json
import os
import re
import resource
import select
import shlex
import signal
import subprocess
import sys
import time
from pathlib import Path

from evacuation_execution import canonical, durable_json, require
from evacuation_images import private_directory

HELPERS = "/root/infra-evacuation-cutover/helpers"
PAIR_PATH = r"/var/lib/platform-evacuation/runs/[a-z0-9][a-z0-9-]{0,63}/pair"


def remote(host, directory, direction, python05, receive=False):
    require(
        host in ("fredrir-05", "fredrir-09")
        and direction in ("forward", "reverse")
        and re.fullmatch(PAIR_PATH, str(directory)),
        "Fixed host and private pair path required",
    )
    require(
        re.fullmatch(r"/nix/store/[a-z0-9]{32}-python3-[0-9.]+/bin/python3", python05),
        "Reviewed existing source Python path required",
    )
    python = "/usr/bin/python3" if host == "fredrir-09" else python05
    operation = (
        ["receive-pair", str(directory), "--direction", direction]
        if receive
        else ["pack-pair", str(directory)]
    )
    command = [
        "timeout",
        "--signal=TERM",
        "--kill-after=5s",
        "175s",
        "env",
        "-i",
        "PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/run/current-system/sw/bin",
        "HOME=/root",
        "LC_ALL=C",
        python,
        "-B",
        HELPERS + "/evacuation_execution.py",
        *operation,
    ]
    quoted = shlex.join(command)
    shell = (
        'if [ "$(id -u)" -eq 0 ]; then exec '
        + quoted
        + "; else exec sudo -n "
        + quoted
        + "; fi"
    )
    options = (
        [
            "-l",
            "administrator",
            "-o",
            "HostName=85.190.100.72",
            "-o",
            "HostKeyAlias=85.190.100.72",
        ]
        if host == "fredrir-09"
        else []
    )
    return [
        "ssh",
        "-T",
        "-S",
        "none",
        "-o",
        "BatchMode=yes",
        "-o",
        "StrictHostKeyChecking=yes",
        "-o",
        "ForwardAgent=no",
        "-o",
        "ClearAllForwardings=yes",
        "-o",
        "ConnectTimeout=10",
        "-o",
        "ServerAliveInterval=5",
        "-o",
        "ServerAliveCountMax=2",
        *options,
        host,
        shell,
    ]


def pipeline(
    producer_arguments, consumer_arguments, environment, seconds=180, status=None
):
    deadline = time.monotonic() + seconds
    producer = subprocess.Popen(
        producer_arguments,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        env=environment,
        close_fds=True,
    )
    consumer = None
    try:
        consumer = subprocess.Popen(
            consumer_arguments,
            stdin=producer.stdout,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            env=environment,
            close_fds=True,
        )
        producer.stdout.close()
        chunks, size = [], 0
        while True:
            require(time.monotonic() < deadline, "Pair transport deadline reached")
            if not select.select(
                [consumer.stdout], [], [], min(0.2, max(0, deadline - time.monotonic()))
            )[0]:
                continue
            data = os.read(consumer.stdout.fileno(), 65536)
            if not data:
                break
            size += len(data)
            require(size <= 2097152, "Transfer receipt exceeds budget")
            chunks.append(data)
        require(
            consumer.wait(timeout=max(0.1, deadline - time.monotonic())) == 0,
            "Destination pair verification failed",
        )
        require(
            producer.wait(timeout=max(0.1, deadline - time.monotonic())) == 0,
            "Source pair export failed",
        )
        return b"".join(chunks)
    finally:
        failing = sys.exc_info()[0] is not None
        observations = []
        for name, process in (("consumer", consumer), ("producer", producer)):
            if process is None:
                continue
            observation = {"client": name, "stopped": False, "errors": []}
            try:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=2)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=2)
                observation["returnCode"] = process.poll()
                observation["stopped"] = observation["returnCode"] is not None
            except (OSError, ValueError, subprocess.SubprocessError) as error:
                observation["errors"].append(type(error).__name__)
            try:
                if process.stdout is not None and not process.stdout.closed:
                    process.stdout.close()
            except (OSError, ValueError) as error:
                observation["errors"].append(type(error).__name__)
            observations.append(observation)
        if status is not None:
            status["clients"] = observations
            status["remoteProcessesImmediatelyCancelled"] = False
        if not failing:
            require(
                all(item["stopped"] and not item["errors"] for item in observations),
                "Local transport cleanup incomplete",
            )


def transfer(source, destination, direction, manifest_sha, python05, output):
    require(
        re.fullmatch(r"[a-f0-9]{64}", manifest_sha),
        "Exact sealed pair manifest required",
    )
    source_host, destination_host = (
        ("fredrir-05", "fredrir-09")
        if direction == "forward"
        else ("fredrir-09", "fredrir-05")
    )
    producer = remote(source_host, source, direction, python05)
    consumer = remote(destination_host, destination, direction, python05, receive=True)
    private_directory(Path(output).parent, os.geteuid())
    record = {
        "schemaVersion": 1,
        "kind": "evacuation-paired-transfer",
        "direction": direction,
        "source": source_host,
        "destination": destination_host,
        "sourceDirectory": str(source),
        "destinationDirectory": str(destination),
        "pairManifestSHA256": manifest_sha,
        "maximumSeconds": 180,
        "localPlaintextPayloadPersisted": False,
        "verified": False,
        "applicationOrFenceChanges": False,
    }
    durable_json(output, record)
    environment = {
        key: os.environ[key]
        for key in ("PATH", "HOME", "SSH_AUTH_SOCK", "TMPDIR")
        if key in os.environ
    }
    try:
        record["transport"] = {}
        manifest = json.loads(
            pipeline(producer, consumer, environment, status=record["transport"])
        )
        require(
            manifest["schemaVersion"] == 1
            and manifest["kind"] == "evacuation-paired-state"
            and manifest["direction"] == direction
            and manifest["source"] == source_host
            and manifest["destination"] == destination_host
            and hashlib.sha256(canonical(manifest)).hexdigest() == manifest_sha,
            "Transferred pair does not match the sealed source execution",
        )
        record["verified"] = True
        durable_json(output, record, replace=True)
        return record
    except BaseException:
        record["failure"] = (
            "Transfer incomplete; inspect destination before retry and retain source fence"
        )
        durable_json(output, record, replace=True)
        raise


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("direction", choices=("forward", "reverse"))
    parser.add_argument("source", type=Path)
    parser.add_argument("destination", type=Path)
    parser.add_argument("--manifest-sha256", required=True)
    parser.add_argument("--python-05", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    os.umask(0o077)
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    signal.signal(
        signal.SIGTERM,
        lambda *_: (_ for _ in ()).throw(ValueError("Pair transport interrupted")),
    )
    try:
        print(
            json.dumps(
                transfer(
                    args.source,
                    args.destination,
                    args.direction,
                    args.manifest_sha256,
                    args.python_05,
                    args.output,
                )
            )
        )
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
        print(
            json.dumps(
                {
                    "error": "Pair transport failed; retained receipt and remote state require inspection"
                }
            )
        )
        raise SystemExit(1)


if __name__ == "__main__":
    main()
