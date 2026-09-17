import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
AUTHOR = ["-c", "user.name=fredrir-packages[bot]", "-c", "user.email=packages@fredrir.com"]


def git(*args, cwd=None, env=None):
    return subprocess.run(["git", *args], cwd=cwd, env=env, check=True, capture_output=True, text=True).stdout


def exchange(scope, identity):
    request = urllib.request.Request(
        os.environ["ACTIONS_ID_TOKEN_REQUEST_URL"] + "&audience=octo-sts.dev",
        headers={"Authorization": "Bearer " + os.environ["ACTIONS_ID_TOKEN_REQUEST_TOKEN"]})
    oidc = json.load(urllib.request.urlopen(request, timeout=30))["value"]
    request = urllib.request.Request(f"https://octo-sts.dev/sts/exchange?scope={scope}&identity={identity}",
                                     headers={"Authorization": "Bearer " + oidc})
    return json.load(urllib.request.urlopen(request, timeout=30))["token"]


def nur_index(text, name):
    line = f"  {name} = pkgs.callPackage ./pkgs/{name} {{ }};"
    if re.search(rf"^\s*{re.escape(name)}\s*=", text, re.M):
        return text
    closing = text.rstrip().rfind("}")
    if closing < 0:
        raise SystemExit("NUR default.nix has no attribute set")
    return text[:closing] + line + "\n" + text[closing:]


def commit_and_push(checkout, message, branch, env=None):
    git("add", "-A", cwd=checkout)
    if not git("status", "--porcelain", cwd=checkout).strip():
        return False
    git(*AUTHOR, "commit", "--quiet", "--message", message, cwd=checkout)
    git("push", "--quiet", "origin", f"HEAD:{branch}", cwd=checkout, env=env)
    return True


def github_checkout(repository, token, destination):
    credential = f"!f() {{ echo username=x-access-token; echo password={token}; }}; f"
    git("clone", "--quiet", "--depth", "1", "-c", f"credential.helper={credential}",
        f"https://github.com/{repository}.git", str(destination))
    git("config", "credential.helper", credential, cwd=destination)
    return git("rev-parse", "--abbrev-ref", "HEAD", cwd=destination).strip()


def publish_taps(tools, channels, work, token_for):
    taps = {}
    for name, tool in tools.items():
        for tap in ["homebrew-tap", *tool.get("taps", [])]:
            taps.setdefault(tap, []).append(name)
    for tap, names in sorted(taps.items()):
        checkout = work / tap
        branch = github_checkout(f"fredrir/{tap}", token_for(f"fredrir/{tap}"), checkout)
        (checkout / "Casks").mkdir(exist_ok=True)
        for name in names:
            shutil.copy2(channels / name / f"{name}.rb", checkout / "Casks" / f"{name}.rb")
        versions = ", ".join(f"{name} {tools[name]['version']}" for name in names)
        if commit_and_push(checkout, f"Update {versions}", branch):
            print(f"Updated {tap}: {versions}")


def publish_nur(tools, channels, work, token_for):
    checkout = work / "nur-packages"
    branch = github_checkout("fredrir/nur-packages", token_for("fredrir/nur-packages"), checkout)
    index = checkout / "default.nix"
    for name in sorted(tools):
        package = checkout / "pkgs" / name
        package.mkdir(parents=True, exist_ok=True)
        shutil.copy2(channels / name / f"{name}.nix", package / "default.nix")
        index.write_text(nur_index(index.read_text(), name))
    versions = ", ".join(f"{name} {tool['version']}" for name, tool in sorted(tools.items()))
    if commit_and_push(checkout, f"Update {versions}", branch):
        print(f"Updated nur-packages: {versions}")


def publish_aur(tools, channels, work, key):
    ssh = f"ssh -i {key} -o IdentitiesOnly=yes -o UserKnownHostsFile={HERE / 'aur_known_hosts'} -o StrictHostKeyChecking=yes"
    env = {**os.environ, "GIT_SSH_COMMAND": ssh}
    for name, tool in sorted(tools.items()):
        for package in [f"{name}-bin", name]:
            checkout = work / "aur" / package
            git("clone", "--quiet", f"ssh://aur@aur.archlinux.org/{package}.git", str(checkout), env=env)
            shutil.copy2(channels / name / f"{package}.pkgbuild", checkout / "PKGBUILD")
            shutil.copy2(channels / name / f"{package}.srcinfo", checkout / ".SRCINFO")
            if commit_and_push(checkout, f"Update to {tool['version']}", "master", env=env):
                print(f"Updated AUR {package} to {tool['version']}")


def main(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("--channels", type=Path, required=True)
    args = parser.parse_args(argv)
    tools = json.loads((args.channels / "tools.json").read_text())
    if not tools:
        print("No releases to publish")
        return 0
    with tempfile.TemporaryDirectory() as directory:
        work = Path(directory)
        key = work / "aur.key"
        key.touch(mode=0o600)
        key.write_text(os.environ["AUR_SSH_KEY"].strip() + "\n")
        tokens = {}
        token_for = lambda scope: tokens.setdefault(scope, exchange(scope, "publisher"))
        publish_taps(tools, args.channels, work, token_for)
        publish_nur(tools, args.channels, work, token_for)
        publish_aur(tools, args.channels, work, key)
    return 0


if __name__ == "__main__":
    sys.exit(main())
