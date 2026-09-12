import argparse
import json
import shutil
import subprocess
import tempfile
from pathlib import Path


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--gitleaks", default="gitleaks")
    parser.add_argument("--output", default=".infra/audit")
    args = parser.parse_args()
    root = Path(subprocess.check_output(["git", "rev-parse", "--show-toplevel"], text=True).strip())
    output = Path(args.output).resolve()
    if not output.is_relative_to(root / ".infra"):
        parser.error("audit output must remain in the ignored .infra directory")
    output.mkdir(parents=True, exist_ok=True, mode=0o700)
    output.chmod(0o700)
    scanner = str(Path(shutil.which(args.gitleaks) or args.gitleaks).resolve())
    flags = ["--redact", "--max-decode-depth", "2", "--report-format", "json", "--config", str(root / ".gitleaks.toml"), "--gitleaks-ignore-path", str(root / ".gitleaksignore")]
    results = {}
    symlinks = {}
    for mode in ["history", "working-tree"]:
        with tempfile.TemporaryDirectory(prefix="infra-publication-") as directory:
            source = root
            command = "git"
            if mode == "working-tree":
                command = "dir"
                source = Path(directory)
                files = subprocess.check_output(["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"], cwd=root).decode().split("\0")
                for name in set(files) - {""}:
                    original = root / name
                    if original.is_symlink():
                        symlinks[name] = str(original.readlink())
                        destination = source / name
                        destination.parent.mkdir(parents=True, exist_ok=True)
                        destination.write_text(symlinks[name])
                    elif original.is_file():
                        destination = source / name
                        destination.parent.mkdir(parents=True, exist_ok=True)
                        shutil.copyfile(original, destination)
            report = output / f"{mode}.json"
            invocation = [scanner, command, str(source), *flags, "--report-path", str(report)]
            if mode == "history":
                invocation += ["--log-opts=--all"]
            log = output / f"{mode}.log"
            with log.open("w") as stream:
                result = subprocess.run(invocation, cwd=root, stdout=stream, stderr=subprocess.STDOUT)
            log.chmod(0o600)
            if result.returncode not in [0, 1] or not report.exists():
                raise RuntimeError(f"scanner failed; inspect {log}")
            report.chmod(0o600)
            findings = json.loads(report.read_text())
            results[mode] = [{key: finding.get(key) for key in ["RuleID", "File", "StartLine", "Commit", "Fingerprint"]} for finding in findings]
    summary = {
        "revision": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True).strip(),
        "reachableCommits": int(subprocess.check_output(["git", "rev-list", "--all", "--count"], cwd=root, text=True)),
        "scannerVersion": subprocess.check_output([scanner, "version"], text=True).strip(),
        "findings": results,
        "visibilityChanged": False,
        "symlinks": symlinks,
        "manualReviewRequired": ["encrypted secret payloads and recipients", "SSH public keys and owner identities", "host addresses and topology", "repository settings, artifacts and logs"],
    }
    (output / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    (output / "summary.json").chmod(0o600)
    print(json.dumps({"findings": {key: len(value) for key, value in results.items()}, "summary": str(output / "summary.json")}))
    return int(any(results.values()))


if __name__ == "__main__":
    raise SystemExit(main())
