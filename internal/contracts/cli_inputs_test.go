package contracts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestCLIInputDigestIgnoresOnlyTestSources(t *testing.T) {
	for _, tool := range []string{"bash", "git", "jq", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("requires %s", tool)
		}
	}
	var action struct {
		Runs struct {
			Steps []struct {
				ID  string `yaml:"id"`
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(root(t), ".github/actions/cli-inputs/action.yml")), &action); err != nil {
		t.Fatal(err)
	}
	if len(action.Runs.Steps) != 1 || action.Runs.Steps[0].ID != "inputs" {
		t.Fatalf("unexpected CLI input steps: %+v", action.Runs.Steps)
	}
	repository := t.TempDir()
	environment := append(os.Environ(), "HOME="+t.TempDir(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repository, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repository, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(name string, arguments ...string) string {
		t.Helper()
		command := exec.Command(name, arguments...)
		command.Dir, command.Env = repository, environment
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, arguments, err, output)
		}
		return string(output)
	}
	commit := func() {
		t.Helper()
		run("git", "add", "--all")
		run("git", "-c", "user.name=check", "-c", "user.email=check@example.com", "-c", "commit.gpgsign=false", "commit", "--quiet", "--allow-empty", "--message", "change")
	}
	digest := func() string {
		t.Helper()
		output := filepath.Join(t.TempDir(), "output")
		command := exec.Command("bash", "-eo", "pipefail", "-c", action.Runs.Steps[0].Run)
		command.Dir, command.Env = repository, append(environment, "GITHUB_OUTPUT="+output)
		if result, err := command.CombinedOutput(); err != nil {
			t.Fatalf("CLI input digest: %v\n%s", err, result)
		}
		for _, line := range strings.Split(string(read(t, output)), "\n") {
			if value, found := strings.CutPrefix(line, "digest="); found {
				return value
			}
		}
		t.Fatal("CLI input digest missing")
		return ""
	}
	run("git", "init", "--quiet")
	write("go.mod", "module example.com/infra\n")
	write("build/cli-release.json", "{}\n")
	write("cmd/infra/main.go", "package main\n")
	write("internal/check/check.go", "package check\n")
	write("internal/check/check_test.go", "package check\n")
	commit()
	base := digest()
	write("internal/check/check_test.go", "package check\n\nconst covered = true\n")
	write("internal/check/new_test.go", "package check\n")
	commit()
	if got := digest(); got != base {
		t.Fatalf("test-only change altered the CLI input digest: %s != %s", got, base)
	}
	for _, change := range []string{"internal/check/check.go", "internal/check/testing.go", "go.mod"} {
		write(change, "changed\n")
		commit()
		if got := digest(); got == base {
			t.Fatalf("%s did not alter the CLI input digest", change)
		}
		base = digest()
	}
}
