package packages

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

func TestChannelsRejectIncompleteOrUnsafePayloadsBeforePublishing(t *testing.T) {
	for _, damage := range []string{"", "missing", "empty", "symlink", "directory"} {
		t.Run(damage, func(t *testing.T) {
			root := t.TempDir()
			directory := filepath.Join(root, "tool")
			if err := os.Mkdir(directory, 0755); err != nil {
				t.Fatal(err)
			}
			for _, file := range []string{"tool.rb", "tool.nix", "tool.pkgbuild", "tool.srcinfo", "tool-bin.pkgbuild", "tool-bin.srcinfo"} {
				if err := os.WriteFile(filepath.Join(directory, file), []byte("payload"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(directory, "tool-bin.srcinfo")
			if damage != "" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				switch damage {
				case "empty":
					if err := os.WriteFile(path, nil, 0644); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Symlink("tool.srcinfo", path); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(path, 0755); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := ValidateChannels(root, Tools{"tool": {Repository: "fredrir/tool", Version: "1.2.3", Binary: "tool"}})
			if (err != nil) != (damage != "") {
				t.Fatalf("validation error: %v", err)
			}
		})
	}
}

type failingTokenTransport struct{}

func (failingTokenTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("transport leaked secret-token")
}
func TestCredentialExchangeDoesNotExposeTransportSecrets(t *testing.T) {
	_, err := tokenRequest(context.Background(), &http.Client{Transport: failingTokenTransport{}}, "https://example.test", "secret-token", "value")
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("error: %v", err)
	}
}

func TestChannelPublicationDoesNotCommitUnchangedArtifacts(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	checkout := filepath.Join(root, "checkout")
	runner := ci.Runner{Dir: root, Env: []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}, Stdout: io.Discard, Stderr: io.Discard}
	git := func(arguments ...string) {
		t.Helper()
		if err := runner.Run(context.Background(), "git", arguments...); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "--quiet", "--bare", "--initial-branch=main", remote)
	git("init", "--quiet", "--initial-branch=main", checkout)
	runner.Dir = checkout
	git("remote", "add", "origin", remote)
	if err := os.Mkdir(filepath.Join(checkout, "Casks"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "Casks/tool.rb"), []byte("cask \"tool\" do\nend\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := commitAndPush(context.Background(), runner, "Update tool 1.2.3", "main"); err != nil {
		t.Fatal(err)
	}
	before, err := runner.Output(context.Background(), "git", "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := commitAndPush(context.Background(), runner, "Update tool 1.2.3", "main"); err != nil {
		t.Fatal(err)
	}
	after, err := runner.Output(context.Background(), "git", "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("identical channel content created another commit")
	}
	runner.Dir = remote
	published, err := runner.Output(context.Background(), "git", "rev-parse", "refs/heads/main")
	if err != nil || string(published) != string(after) {
		t.Fatalf("remote revision %s: %v", published, err)
	}
}

func TestGeneratedInstallerRejectsInvalidRequestsBeforeDownloading(t *testing.T) {
	directory := t.TempDir()
	script, err := Installer(Tools{"tool": {Repository: "fredrir/tool", Version: "1.2.3", Binary: "tool"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "install.sh")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Run(context.Background(), process.Options{Name: "sh", Args: []string{"-n", path}}); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{}, {"other"}, {"tool", "1.0;rm"}} {
		result, err := process.Run(context.Background(), process.Options{Name: "sh", Args: append([]string{path}, arguments...), Env: []string{"PATH=/usr/bin:/bin", "HOME=" + directory}})
		if err == nil || !strings.Contains(string(result.Stderr), "install.sh:") {
			t.Fatalf("request %v: %v %s", arguments, err, result.Stderr)
		}
	}
}
