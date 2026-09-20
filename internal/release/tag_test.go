package release

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/ci"
)

func init() {
	if os.Getenv("INFRA_RELEASE_TEST_TOOLS") != "1" {
		return
	}
	switch filepath.Base(os.Args[0]) {
	case "cargo":
		err := os.WriteFile(filepath.Join(os.Getenv("RUNNER_TEMP"), "cargo.log"), []byte(strings.Join(os.Args[1:], " ")), 0600)
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case "git-cliff":
		tag, output := "", ""
		for index := 1; index+1 < len(os.Args); index++ {
			switch os.Args[index] {
			case "--tag":
				tag = os.Args[index+1]
			case "--output":
				output = os.Args[index+1]
			}
		}
		if tag == "" || output == "" {
			os.Exit(1)
		}
		if err := os.WriteFile(output, []byte("## "+tag+"\n"), 0644); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func TestReleaseCommitAndAnnotatedTagReachRemote(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	area := t.TempDir()
	binaries := filepath.Join(area, "bin")
	if err := os.Mkdir(binaries, 0755); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cargo", "git-cliff"} {
		if err := os.Symlink(executable, filepath.Join(binaries, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binaries+string(os.PathListSeparator)+os.Getenv("PATH"))
	runner := ci.Runner{Dir: area, Env: []string{"INFRA_RELEASE_TEST_TOOLS=1", "RUNNER_TEMP=" + area}, Stdout: io.Discard, Stderr: io.Discard}
	git := func(directory string, args ...string) string {
		t.Helper()
		local := runner
		local.Dir = directory
		data, err := local.Output(context.Background(), "git", args...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(data))
	}
	remote, work := filepath.Join(area, "remote.git"), filepath.Join(area, "work")
	git(area, "init", "--quiet", "--bare", "--initial-branch=main", remote)
	git(area, "init", "--quiet", "--initial-branch=main", work)
	git(work, "config", "user.name", "test")
	git(work, "config", "user.email", "test@example.invalid")
	for name, content := range map[string]string{"Cargo.toml": "[package]\nname = \"nsql\"\nversion = \"0.1.13\"\n", "Cargo.lock": "lock\n", "cliff.toml": "[changelog]\n"} {
		if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git(work, "add", ".")
	git(work, "commit", "--quiet", "-m", "initial")
	git(work, "tag", "v0.1.13")
	git(work, "commit", "--quiet", "--allow-empty", "-m", "fix: things")
	git(work, "remote", "add", "origin", remote)
	git(work, "push", "--quiet", "origin", "main", "v0.1.13")
	runner.Dir = work
	if err := Tag(context.Background(), runner, ""); err != nil {
		t.Fatal(err)
	}
	if got := git(remote, "log", "-1", "--format=%s", "main"); got != "release: v0.1.14" {
		t.Fatal(got)
	}
	if git(remote, "rev-parse", "v0.1.14^{commit}") != git(remote, "rev-parse", "main") {
		t.Fatal("remote tag does not reference release commit")
	}
	if got := git(remote, "cat-file", "-t", "v0.1.14"); got != "tag" {
		t.Fatalf("expected annotated tag, got %s", got)
	}
	if !strings.Contains(git(remote, "show", "main:Cargo.toml"), `version = "0.1.14"`) {
		t.Fatal("release version not published")
	}
	if git(remote, "show", "main:CHANGELOG.md") != "## v0.1.14" {
		t.Fatal("changelog not published")
	}
	log, err := os.ReadFile(filepath.Join(area, "cargo.log"))
	if err != nil || !strings.Contains(string(log), "update --workspace") {
		t.Fatal(fmt.Sprintf("lockfile refresh missing: %q %v", log, err))
	}
}
