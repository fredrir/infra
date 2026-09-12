import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parents[2]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--provider-directory", type=Path, required=True)
    args = parser.parse_args()
    provider_directory = args.provider_directory.resolve(strict=True)
    tofu = shutil.which("tofu")
    if tofu is None:
        parser.error("tofu not found")
    environment = {
        key: value for key, value in os.environ.items()
        if key in {"PATH", "HOME", "TMPDIR", "SSL_CERT_FILE", "NIX_SSL_CERT_FILE", "SYSTEMROOT"}
    }
    with tempfile.TemporaryDirectory(prefix="infra-mail-test-") as temporary:
        destination = Path(temporary)
        source = ROOT / "tofu"
        for path in [*source.glob("*.tf"), *source.glob("modules/**/*.tf"), source / ".terraform.lock.hcl"]:
            target = destination / path.relative_to(source)
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(path, target)
        tests = destination / "tests"
        tests.mkdir()
        test_source = source / "tests/platform-mail.tftest.hcl"
        text = test_source.read_text()
        marker = 'run "scoped_smtp_identity" {\n  command = plan'
        if text.count(marker) != 1:
            raise RuntimeError("mail test entrypoint changed")
        (tests / test_source.name).write_text(text.replace(marker, marker.replace("plan", "apply"), 1))
        overrides = {
            "resource": {
                kind: {name: {"lifecycle": {"prevent_destroy": False}}}
                for kind, name in (
                    ("aws_sesv2_email_identity", "sender"),
                    ("cloudflare_dns_record", "dkim"),
                    ("aws_iam_policy", "sender"),
                    ("aws_iam_user", "sender"),
                )
            }
        }
        (destination / "modules/platform-mail/test_override.tf.json").write_text(json.dumps(overrides))
        commands = [
            ["init", "-backend=false", "-lockfile=readonly", "-input=false", f"-plugin-dir={provider_directory}"],
            ["test", "-filter=tests/platform-mail.tftest.hcl"],
        ]
        for command in commands:
            subprocess.run([tofu, f"-chdir={destination}", *command], env=environment, check=True, timeout=180)


if __name__ == "__main__":
    main()
