package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/cli"
)

func TestPlanWritesJSONAndAppendsGitHubOutput(t *testing.T) {
	root := t.TempDir()
	for path, content := range map[string]string{
		"images/catalog.yaml":          "- image: ghcr.io/fredrir/example\n  dockerfile: Containerfile\n  check: example --version\n  inputs: [Containerfile]\n",
		".github/workflows/images.yml": "name: Images\n", ".github/workflows/build-image.yml": "name: Build\n", ".github/workflows/infra-cli.yml": "name: CLI\n", ".dockerignore": ".git\n", "Containerfile": "FROM scratch\n",
	} {
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "--quiet"}, {"add", "--all"}, {"commit", "--quiet", "--message", "fixture"}} {
		command := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git: %v\n%s", err, output)
		}
	}
	output := filepath.Join(t.TempDir(), "output")
	if err := os.WriteFile(output, []byte("existing=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_OUTPUT", output)
	t.Setenv("REFRESH", "true")
	t.Setenv("REGISTRY_URL", "http://127.0.0.1:1")
	var stdout, stderr bytes.Buffer
	if err := cli.Run(context.Background(), []string{"ci", "plan-images", "--root", root}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil || len(entries) != 1 {
		t.Fatalf("stdout is not a valid matrix: %s (%v)", &stdout, err)
	}
	if !strings.Contains(stderr.String(), "Building: ghcr.io/fredrir/example") {
		t.Fatal("missing progress on stderr:", stderr.String())
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing=value\nimages="+stdout.String() {
		t.Fatalf("unexpected Actions output: %s", data)
	}
	stdout.Reset()
	if err := cli.Run(context.Background(), []string{"ci", "plan-images", "--root", root, "--github-output", root}, &stdout, &stderr); err == nil || stdout.Len() != 0 {
		t.Fatal("output write failure must fail without printing a successful matrix")
	}
}

func TestCommandValidation(t *testing.T) {
	for _, args := range [][]string{{"unknown"}, {"ci"}, {"ci", "plan-images", "extra"}, {"ci", "plan-images", "--unknown"}, {"ci", "plan-images", "--timeout=0s"}, {"dev"}, {"dev", "doctor", "extra"}, {"dev", "setup", "--timeout=0s"}, {"dev", "engine"}, {"dev", "engine", "status", "--profile=other"}, {"dev", "qualify"}, {"dev", "cluster"}, {"dev", "hosts"}, {"dev", "hosts", "play"}, {"dev", "bench"}, {"dev", "bench", "compare", "one"}, {"reconcile", "apply", "--deep"}} {
		var output bytes.Buffer
		if err := cli.Run(context.Background(), args, &output, &output); err == nil {
			t.Errorf("accepted invalid command %q", args)
		}
	}
	for _, args := range [][]string{nil, {"--help"}, {"version"}, {"ci", "plan-images", "--help"}, {"dev", "--help"}, {"dev", "doctor", "--help"}, {"reconcile", "verify", "--deep", "--help"}} {
		var output bytes.Buffer
		if err := cli.Run(context.Background(), args, &output, &output); err != nil || output.Len() == 0 {
			t.Errorf("command %q: %v, output=%q", args, err, &output)
		}
	}
}

func TestVerificationWritesItsOutcomeReport(t *testing.T) {
	report := filepath.Join(t.TempDir(), "reports", "verification.json")
	var output bytes.Buffer
	if err := cli.Run(context.Background(), []string{"reconcile", "verify", "--deep", "--root", t.TempDir(), "--report", report}, &output, &output); err == nil {
		t.Fatal("verification without declarations succeeded")
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var outcome struct {
		Deep        bool
		Outcome     string
		Differences []any
		Errors      []string
	}
	if err := json.Unmarshal(data, &outcome); err != nil {
		t.Fatal(err)
	}
	if !outcome.Deep || outcome.Outcome != "failed" || outcome.Differences == nil || len(outcome.Differences) != 0 || len(outcome.Errors) != 1 || !strings.Contains(outcome.Errors[0], "settings.yaml") {
		t.Fatalf("verification error reported as %s", data)
	}
}
