import argparse
import datetime
import hashlib
import json
import os
import pathlib
import struct
import subprocess
import tempfile
import uuid


def varint(value):
    encoded = bytearray()
    while value > 127:
        encoded.append((value & 127) | 128)
        value >>= 7
    encoded.append(value)
    return bytes(encoded)


def field(number, data):
    return varint(number * 8 + 2) + varint(len(data)) + data


def read_varint(data, index):
    value, shift = 0, 0
    while index < len(data) and shift < 64:
        byte = data[index]
        index += 1
        value |= (byte & 127) << shift
        if not byte & 128:
            return value, index
        shift += 7
    raise ValueError("Invalid response varint")


def read_payload(body):
    result = bytearray()
    while body:
        if len(body) < 5 or body[0] != 0:
            raise ValueError("Invalid gRPC frame")
        size = struct.unpack(">I", body[1:5])[0]
        if len(body) < size + 5:
            raise ValueError("Incomplete gRPC frame")
        message, body = body[5:size + 5], body[size + 5:]
        key, offset = read_varint(message, 0)
        size, offset = read_varint(message, offset)
        if key != 82 or len(message) != offset + size:
            raise ValueError("Invalid ByteStream response")
        result.extend(message[offset:])
    return bytes(result)


def rpc(receipt, address, path, message, expected_http, expected_grpc=None):
    with tempfile.TemporaryDirectory() as directory:
        headers = pathlib.Path(directory) / "headers"
        output = pathlib.Path(directory) / "body"
        command = ["curl", "--silent", "--show-error", "--http2-prior-knowledge",
                   "--connect-timeout", "5", "--max-time", "10", "--output", str(output),
                   "--dump-header", str(headers), "--write-out", "%{http_code}",
                   "--header", "Content-Type: application/grpc", "--header", "TE: trailers",
                   "--data-binary", "@-", "http://" + address + path]
        result = subprocess.run(command, input=b"\0" + struct.pack(">I", len(message)) + message,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=15)
        status = result.stdout.decode().strip()
        grpc = [line.partition(":")[2].strip() for line in headers.read_text().splitlines()
                if line.lower().startswith("grpc-status:")]
        passed = (status == str(expected_http)
                  and (expected_grpc is None or str(expected_grpc) in grpc)
                  and (result.returncode == 0 or expected_http == "000" and result.returncode in (7, 28)))
        receipt["rpc"].append({"address": address, "path": path, "http": status,
                               "grpc": grpc, "curl_exit": result.returncode, "passed": passed})
        if not passed:
            raise ValueError("Unexpected cache response")
        return output.read_bytes() if output.exists() else b""


def qualify(args, receipt):
    request_names = ("ACTIONS_ID_TOKEN_REQUEST_URL", "ACTIONS_ID_TOKEN_REQUEST_TOKEN")
    receipt["oidc_request_environment_absent"] = all(name not in os.environ for name in request_names)
    if not receipt["oidc_request_environment_absent"]:
        raise ValueError("OIDC request environment exposed")
    payload = ("bazel-cache-qualification:" + args.nonce).encode()
    digest = hashlib.sha256(payload).hexdigest()
    resource = f"blobs/{digest}/{len(payload)}".encode()
    receipt["blob_sha256"] = digest
    receipt["blob_bytes"] = len(payload)
    capability = "/build.bazel.remote.execution.v2.Capabilities/GetCapabilities"
    address = args.writer if args.role == "writer" else args.reader
    rpc(receipt, address, capability, b"", 200, 0)
    if args.role == "writer":
        name = b"uploads/" + uuid.uuid4().hex.encode() + b"/" + resource
        message = field(1, name) + b"\x18\x01" + field(10, payload)
        rpc(receipt, args.writer, "/google.bytestream.ByteStream/Write", message, 200, 0)
    else:
        for path in ["/google.bytestream.ByteStream/Write", "/google.bytestream.ByteStream/QueryWriteStatus",
                     "/build.bazel.remote.execution.v2.ContentAddressableStorage/BatchUpdateBlobs",
                     "/build.bazel.remote.execution.v2.ActionCache/UpdateActionResult"]:
            rpc(receipt, args.reader, path, b"", 403)
        rpc(receipt, args.writer, capability, b"", "000")
    body = rpc(receipt, args.reader, "/google.bytestream.ByteStream/Read", field(1, resource), 200, 0)
    receipt["read_matches"] = read_payload(body) == payload
    if not receipt["read_matches"]:
        raise ValueError("Cache read differs from written payload")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("role", choices=("writer", "reader"))
    parser.add_argument("--nonce", required=True)
    parser.add_argument("--reader", default="100.87.168.66:9092")
    parser.add_argument("--writer", default="100.87.168.66:9093")
    parser.add_argument("--report", type=pathlib.Path, required=True)
    args = parser.parse_args()
    receipt = {"schema": 1, "role": args.role, "nonce": args.nonce, "rpc": [],
               "identity": os.environ.get("BAZEL_CACHE_IDENTITY"), "endpoint": os.environ.get("ENDPOINT"),
               "run_id": os.environ.get("GITHUB_RUN_ID"), "attempt": os.environ.get("GITHUB_RUN_ATTEMPT"),
               "revision": os.environ.get("GITHUB_SHA"), "at": datetime.datetime.now(datetime.timezone.utc).isoformat()}
    try:
        receipt["tailnet_ip"] = subprocess.check_output(["tailscale", "ip", "-4"], stderr=subprocess.PIPE).decode().strip()
    except (FileNotFoundError, subprocess.CalledProcessError):
        pass
    try:
        qualify(args, receipt)
        receipt["passed"] = True
    except (ValueError, OSError, subprocess.SubprocessError):
        receipt["passed"] = False
    args.report.write_text(json.dumps(receipt, indent=2) + "\n")
    print(json.dumps(receipt))
    return 0 if receipt["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
