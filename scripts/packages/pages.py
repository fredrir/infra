import argparse
import html
import json
import re
import sys
from pathlib import Path

DOMAIN = "pkgs.fredrir.com"
HERE = Path(__file__).resolve().parent


def installer(tools):
    for name, tool in tools.items():
        if not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", name) or not re.fullmatch(r"[0-9.]+", tool["version"]) \
                or not re.fullmatch(r"fredrir/[A-Za-z0-9_.-]+", tool["repository"]) \
                or not re.fullmatch(r"[A-Za-z0-9_-]+", tool["binary"]):
            raise SystemExit(f"Refusing unsafe installer entry for {name}")
    cases = "\n".join(f"  {name}) repository={tool['repository']} version={tool['version']} binary={tool['binary']} ;;"
                      for name, tool in sorted(tools.items()))
    template = (HERE / "install.sh.in").read_text()
    return template.replace("@TOOLS@", cases).replace("@NAMES@", " ".join(sorted(tools)) or "none")


def page(tools):
    rows = "\n".join(f"<tr><td>{html.escape(name)}</td><td>{html.escape(tool['version'])}</td>"
                     f"<td><a href=\"https://github.com/{html.escape(tool['repository'])}\">{html.escape(tool['repository'])}</a></td></tr>"
                     for name, tool in sorted(tools.items()))
    return f"""<!doctype html>
<html lang="en">
<meta charset="utf-8">
<title>{DOMAIN}</title>
<h1>{DOMAIN}</h1>
<table><tr><th>Tool</th><th>Version</th><th>Source</th></tr>
{rows}
</table>
<h2>Debian and Ubuntu</h2>
<pre>curl -fsSL https://{DOMAIN}/keys/fredrir.asc | sudo tee /etc/apt/keyrings/fredrir.asc >/dev/null
echo "deb [signed-by=/etc/apt/keyrings/fredrir.asc] https://{DOMAIN}/deb stable main" | sudo tee /etc/apt/sources.list.d/fredrir.list
sudo apt update && sudo apt install TOOL</pre>
<h2>Fedora, RHEL and openSUSE</h2>
<pre>sudo curl -fsSLo /etc/yum.repos.d/fredrir.repo https://{DOMAIN}/rpm/fredrir.repo && sudo dnf install TOOL
sudo zypper addrepo https://{DOMAIN}/rpm/fredrir.repo && sudo zypper install TOOL</pre>
<h2>Alpine</h2>
<pre>wget -qO /etc/apk/keys/fredrir.rsa.pub https://{DOMAIN}/keys/fredrir.rsa.pub
echo https://{DOMAIN}/apk >> /etc/apk/repositories && apk add TOOL</pre>
<h2>Homebrew, Arch and Nix</h2>
<pre>brew install fredrir/tap/TOOL
yay -S TOOL   # or TOOL-bin
nix run github:fredrir/nur-packages#TOOL</pre>
<h2>Any Linux or macOS</h2>
<pre>curl -fsSL https://{DOMAIN}/install.sh | sh -s -- TOOL</pre>
</html>
"""


def build(site, tools, public_gpg, public_apk):
    (site / "keys").mkdir(parents=True, exist_ok=True)
    (site / "keys/fredrir.asc").write_text(public_gpg)
    (site / "keys/fredrir.rsa.pub").write_text(public_apk)
    (site / "CNAME").write_text(DOMAIN + "\n")
    (site / ".nojekyll").write_text("")
    (site / "install.sh").write_text(installer(tools))
    (site / "index.html").write_text(page(tools))
    (site / "deb").mkdir(exist_ok=True)
    (site / "deb/fredrir.list").write_text(
        f"deb [signed-by=/etc/apt/keyrings/fredrir.asc] https://{DOMAIN}/deb stable main\n")
    (site / "rpm").mkdir(exist_ok=True)
    (site / "rpm/fredrir.repo").write_text(
        f"[fredrir]\nname=fredrir\nbaseurl=https://{DOMAIN}/rpm/$basearch\nenabled=1\n"
        f"gpgcheck=1\nrepo_gpgcheck=1\ngpgkey=https://{DOMAIN}/keys/fredrir.asc\n")


def main(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("--site", type=Path, required=True)
    parser.add_argument("--tools", type=Path, required=True)
    parser.add_argument("--public-gpg", type=Path, required=True)
    parser.add_argument("--public-apk", type=Path, required=True)
    args = parser.parse_args(argv)
    build(args.site, json.loads(args.tools.read_text()), args.public_gpg.read_text(), args.public_apk.read_text())
    return 0


if __name__ == "__main__":
    sys.exit(main())
