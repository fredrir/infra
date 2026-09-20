package packages

import (
	"context"
	_ "embed"
	"fmt"
	"html"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/fredrir/infra/internal/ci"
)

//go:embed assets/install.sh.in
var installerTemplate string

const domain = "pkgs.fredrir.com"

func Installer(tools Tools) (string, error) {
	names := make([]string, 0, len(tools))
	for name, tool := range tools {
		if !toolName.MatchString(name) || !regexp.MustCompile(`^[0-9.]+$`).MatchString(tool.Version) || !repositoryName.MatchString(tool.Repository) || !binaryName.MatchString(tool.Binary) {
			return "", fmt.Errorf("unsafe installer entry for %s", name)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	var cases []string
	for _, name := range names {
		tool := tools[name]
		cases = append(cases, fmt.Sprintf("  %s) repository=%s version=%s binary=%s ;;", name, tool.Repository, tool.Version, tool.Binary))
	}
	available := strings.Join(names, " ")
	if available == "" {
		available = "none"
	}
	return strings.NewReplacer("@TOOLS@", strings.Join(cases, "\n"), "@NAMES@", available).Replace(installerTemplate), nil
}

func Page(tools Tools) string {
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	slices.Sort(names)
	var rows []string
	for _, name := range names {
		tool := tools[name]
		rows = append(rows, fmt.Sprintf("<tr><td>%s</td><td>%s</td><td><a href=\"https://github.com/%s\">%s</a></td></tr>", html.EscapeString(name), html.EscapeString(tool.Version), html.EscapeString(tool.Repository), html.EscapeString(tool.Repository)))
	}
	return `<!doctype html>
<html lang="en">
<meta charset="utf-8">
<title>pkgs.fredrir.com</title>
<h1>pkgs.fredrir.com</h1>
<table><tr><th>Tool</th><th>Version</th><th>Source</th></tr>
` + strings.Join(rows, "\n") + `
</table>
<h2>Debian and Ubuntu</h2>
<pre>curl -fsSL https://pkgs.fredrir.com/keys/fredrir.asc | sudo tee /etc/apt/keyrings/fredrir.asc >/dev/null
echo "deb [signed-by=/etc/apt/keyrings/fredrir.asc] https://pkgs.fredrir.com/deb stable main" | sudo tee /etc/apt/sources.list.d/fredrir.list
sudo apt update && sudo apt install TOOL</pre>
<h2>Fedora, RHEL and openSUSE</h2>
<pre>sudo curl -fsSLo /etc/yum.repos.d/fredrir.repo https://pkgs.fredrir.com/rpm/fredrir.repo && sudo dnf install TOOL
sudo zypper addrepo https://pkgs.fredrir.com/rpm/fredrir.repo && sudo zypper install TOOL</pre>
<h2>Alpine</h2>
<pre>wget -qO /etc/apk/keys/fredrir.rsa.pub https://pkgs.fredrir.com/keys/fredrir.rsa.pub
echo https://pkgs.fredrir.com/apk >> /etc/apk/repositories && apk add TOOL</pre>
<h2>Homebrew, Arch and Nix</h2>
<pre>brew install fredrir/tap/TOOL
yay -S TOOL   # or TOOL-bin
nix run github:fredrir/nur-packages#TOOL</pre>
<h2>Any Linux or macOS</h2>
<pre>curl -fsSL https://pkgs.fredrir.com/install.sh | sh -s -- TOOL</pre>
</html>
`
}

func BuildSite(site string, tools Tools, publicGPG, publicAPK []byte) error {
	installer, err := Installer(tools)
	if err != nil {
		return err
	}
	files := map[string][]byte{"keys/fredrir.asc": publicGPG, "keys/fredrir.rsa.pub": publicAPK, "CNAME": []byte(domain + "\n"), ".nojekyll": {}, "install.sh": []byte(installer), "index.html": []byte(Page(tools)), "deb/fredrir.list": []byte("deb [signed-by=/etc/apt/keyrings/fredrir.asc] https://" + domain + "/deb stable main\n"), "rpm/fredrir.repo": []byte("[fredrir]\nname=fredrir\nbaseurl=https://" + domain + "/rpm/$basearch\nenabled=1\ngpgcheck=1\nrepo_gpgcheck=1\ngpgkey=https://" + domain + "/keys/fredrir.asc\n")}
	for name, data := range files {
		path := filepath.Join(site, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			return err
		}
	}
	return nil
}

func PublishPages(ctx context.Context, runner ci.Runner, site, repository string) error {
	if !repositoryName.MatchString(repository) || os.Getenv("GH_TOKEN") == "" {
		return fmt.Errorf("package publication requires repository and GitHub credentials")
	}
	site, err := filepath.Abs(site)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(site, ".git")); err == nil {
		return fmt.Errorf("package site already contains a Git checkout")
	} else if !os.IsNotExist(err) {
		return err
	}
	var size int64
	err = filepath.WalkDir(site, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("package site contains a symbolic link")
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if size > 900<<20 {
		return fmt.Errorf("package site exceeds the 900 MB Pages budget")
	}
	for _, name := range []string{"CNAME", ".nojekyll", "deb/dists/stable/InRelease"} {
		info, err := os.Stat(filepath.Join(site, name))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("package site missing %s", name)
		}
	}
	runner.Dir = site
	for _, arguments := range [][]string{{"init", "--quiet", "--initial-branch=gh-pages"}, {"add", "-A"}, {"-c", "user.name=fredrir-packages[bot]", "-c", "user.email=packages@fredrir.com", "commit", "--quiet", "--message", "Publish packages"}, {"-c", "credential.helper=!gh auth git-credential", "push", "--quiet", "--force", "https://github.com/" + repository + ".git", "HEAD:gh-pages"}} {
		if err := runner.Run(ctx, "git", arguments...); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(runner.Stdout, "Published %d MB to GitHub Pages\n", (size+(1<<20)-1)>>20)
	return err
}
