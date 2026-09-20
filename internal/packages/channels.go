package packages

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/ci"
)

func NURIndex(text, name string) (string, error) {
	if !toolName.MatchString(name) {
		return "", fmt.Errorf("invalid NUR package name")
	}
	if regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(name) + `\s*=`).MatchString(text) {
		return text, nil
	}
	closing := strings.LastIndex(strings.TrimRight(text, " \t\r\n"), "}")
	if closing < 0 {
		return "", fmt.Errorf("NUR default.nix has no attribute set")
	}
	return text[:closing] + "  " + name + " = pkgs.callPackage ./pkgs/" + name + " { };\n" + text[closing:], nil
}

func Exchange(ctx context.Context, client *http.Client, requestURL, requestToken, scope, identity string) (string, error) {
	if !repositoryName.MatchString(scope) || identity == "" || requestToken == "" {
		return "", fmt.Errorf("invalid credential exchange identity")
	}
	address, err := url.Parse(requestURL)
	if err != nil || address.Host == "" || address.Scheme != "https" || address.User != nil {
		return "", fmt.Errorf("invalid OIDC request URL")
	}
	query := address.Query()
	query.Set("audience", "octo-sts.dev")
	address.RawQuery = query.Encode()
	value, err := tokenRequest(ctx, client, address.String(), requestToken, "value")
	if err != nil {
		return "", err
	}
	query = url.Values{"scope": {scope}, "identity": {identity}}
	return tokenRequest(ctx, client, "https://octo-sts.dev/sts/exchange?"+query.Encode(), value, "token")
}

func tokenRequest(ctx context.Context, client *http.Client, address, token, field string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("credential exchange request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return "", fmt.Errorf("credential exchange returned HTTP %d", response.StatusCode)
	}
	var result map[string]string
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if result[field] == "" {
		return "", fmt.Errorf("credential exchange returned no token")
	}
	return result[field], nil
}

func PublishChannels(ctx context.Context, runner ci.Runner, channels, knownHosts string) error {
	data, err := os.ReadFile(filepath.Join(channels, "tools.json"))
	if err != nil {
		return err
	}
	var tools Tools
	if err := json.Unmarshal(data, &tools); err != nil {
		return err
	}
	if _, err := Installer(tools); err != nil {
		return err
	}
	if len(tools) == 0 {
		_, err := fmt.Fprintln(runner.Stdout, "No releases to publish")
		return err
	}
	if err := ValidateChannels(channels, tools); err != nil {
		return err
	}
	if os.Getenv("AUR_SSH_KEY") == "" {
		return fmt.Errorf("AUR SSH key is required")
	}
	work, err := os.MkdirTemp("", "infra-channels-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	key := filepath.Join(work, "aur.key")
	if err := os.WriteFile(key, []byte(strings.TrimSpace(os.Getenv("AUR_SSH_KEY"))+"\n"), 0600); err != nil {
		return err
	}
	if knownHosts == "" {
		knownHosts = filepath.Join(work, "known_hosts")
		if err := os.WriteFile(knownHosts, AURKnownHosts, 0600); err != nil {
			return err
		}
	}
	knownHosts, err = filepath.Abs(knownHosts)
	if err != nil {
		return err
	}
	if _, err := os.Stat(knownHosts); err != nil {
		return err
	}
	if _, err := runner.Output(ctx, "ssh-keygen", "-y", "-P", "", "-f", key); err != nil {
		return fmt.Errorf("invalid AUR SSH key")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	tokenFor := func(scope string) (string, error) {
		return Exchange(ctx, client, os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"), scope, "publisher")
	}
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	slices.Sort(names)
	taps := map[string][]string{}
	for _, name := range names {
		for _, tap := range append([]string{"homebrew-tap"}, tools[name].Taps...) {
			if !toolName.MatchString(tap) {
				return fmt.Errorf("invalid Homebrew tap")
			}
			if !slices.Contains(taps[tap], name) {
				taps[tap] = append(taps[tap], name)
			}
		}
	}
	tapNames := make([]string, 0, len(taps))
	for tap := range taps {
		tapNames = append(tapNames, tap)
	}
	slices.Sort(tapNames)
	tokens := make(map[string]string, len(tapNames)+1)
	for _, tap := range append(slices.Clone(tapNames), "nur-packages") {
		token, err := tokenFor("fredrir/" + tap)
		if err != nil {
			return err
		}
		tokens[tap] = token
	}
	for _, tap := range tapNames {
		local, branch, err := githubCheckout(ctx, runner, "fredrir/"+tap, tokens[tap], filepath.Join(work, tap))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(local.Dir, "Casks"), 0755); err != nil {
			return err
		}
		var versions []string
		for _, name := range taps[tap] {
			if err := copyFile(filepath.Join(channels, name, name+".rb"), filepath.Join(local.Dir, "Casks", name+".rb"), 0644); err != nil {
				return err
			}
			versions = append(versions, name+" "+tools[name].Version)
		}
		if err := commitAndPush(ctx, local, "Update "+strings.Join(versions, ", "), branch); err != nil {
			return err
		}
	}
	local, branch, err := githubCheckout(ctx, runner, "fredrir/nur-packages", tokens["nur-packages"], filepath.Join(work, "nur-packages"))
	if err != nil {
		return err
	}
	indexPath := filepath.Join(local.Dir, "default.nix")
	index, err := os.ReadFile(indexPath)
	if err != nil {
		return err
	}
	var versions []string
	for _, name := range names {
		directory := filepath.Join(local.Dir, "pkgs", name)
		if err := os.MkdirAll(directory, 0755); err != nil {
			return err
		}
		if err := copyFile(filepath.Join(channels, name, name+".nix"), filepath.Join(directory, "default.nix"), 0644); err != nil {
			return err
		}
		updated, err := NURIndex(string(index), name)
		if err != nil {
			return err
		}
		index = []byte(updated)
		versions = append(versions, name+" "+tools[name].Version)
	}
	if err := os.WriteFile(indexPath, index, 0644); err != nil {
		return err
	}
	if err := commitAndPush(ctx, local, "Update "+strings.Join(versions, ", "), branch); err != nil {
		return err
	}
	ssh := "ssh -i " + shellQuote(key) + " -o IdentitiesOnly=yes -o UserKnownHostsFile=" + shellQuote(knownHosts) + " -o StrictHostKeyChecking=yes"
	for _, name := range names {
		for _, pkg := range []string{name + "-bin", name} {
			checkout := filepath.Join(work, "aur", pkg)
			if err := os.MkdirAll(filepath.Dir(checkout), 0755); err != nil {
				return err
			}
			aur := runner
			aur.Env = append(slices.Clone(runner.Env), "GIT_SSH_COMMAND="+ssh)
			if err := aur.Run(ctx, "git", "clone", "--quiet", "ssh://aur@aur.archlinux.org/"+pkg+".git", checkout); err != nil {
				return err
			}
			aur.Dir = checkout
			for source, destination := range map[string]string{pkg + ".pkgbuild": "PKGBUILD", pkg + ".srcinfo": ".SRCINFO"} {
				if err := copyFile(filepath.Join(channels, name, source), filepath.Join(checkout, destination), 0644); err != nil {
					return err
				}
			}
			if err := commitAndPush(ctx, aur, "Update to "+tools[name].Version, "master"); err != nil {
				return err
			}
		}
	}
	return nil
}

func ValidateChannels(channels string, tools Tools) error {
	if _, err := Installer(tools); err != nil {
		return err
	}
	for name, tool := range tools {
		for _, tap := range tool.Taps {
			if !toolName.MatchString(tap) {
				return fmt.Errorf("invalid Homebrew tap")
			}
		}
		directory := filepath.Join(channels, name)
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("channel directory %s is missing or unsafe", name)
		}
		for _, file := range []string{name + ".rb", name + ".nix", name + ".pkgbuild", name + ".srcinfo", name + "-bin.pkgbuild", name + "-bin.srcinfo"} {
			info, err := os.Lstat(filepath.Join(directory, file))
			if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
				return fmt.Errorf("channel file %s/%s is missing, empty, or unsafe", name, file)
			}
		}
	}
	return nil
}

func githubCheckout(ctx context.Context, runner ci.Runner, repository, token, destination string) (ci.Runner, string, error) {
	runner.Env = append(slices.Clone(runner.Env), "GH_TOKEN="+token)
	if err := runner.Run(ctx, "git", "-c", "credential.helper=!gh auth git-credential", "clone", "--quiet", "--depth", "1", "https://github.com/"+repository+".git", destination); err != nil {
		return runner, "", err
	}
	runner.Dir = destination
	if err := runner.Run(ctx, "git", "config", "credential.helper", "!gh auth git-credential"); err != nil {
		return runner, "", err
	}
	branch, err := runner.Output(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	return runner, strings.TrimSpace(string(branch)), err
}
func commitAndPush(ctx context.Context, runner ci.Runner, message, branch string) error {
	if err := runner.Run(ctx, "git", "add", "-A"); err != nil {
		return err
	}
	status, err := runner.Output(ctx, "git", "status", "--porcelain")
	if err != nil {
		return err
	}
	if len(status) == 0 {
		return nil
	}
	if err := runner.Run(ctx, "git", "-c", "user.name=fredrir-packages[bot]", "-c", "user.email=packages@fredrir.com", "commit", "--quiet", "--message", message); err != nil {
		return err
	}
	return runner.Run(ctx, "git", "push", "--quiet", "origin", "HEAD:"+branch)
}
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
