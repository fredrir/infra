package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/cli"
	"github.com/fredrir/infra/internal/reconcile"
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
	for _, args := range [][]string{{"unknown"}, {"ci"}, {"ci", "plan-images", "extra"}, {"ci", "plan-images", "--unknown"}, {"ci", "plan-images", "--timeout=0s"}, {"dev"}, {"dev", "doctor", "extra"}, {"dev", "setup", "--timeout=0s"}, {"dev", "engine"}, {"dev", "engine", "status", "--profile=other"}, {"dev", "qualify"}, {"dev", "cluster"}, {"dev", "hosts"}, {"dev", "hosts", "play"}, {"dev", "bench"}, {"dev", "bench", "compare", "one"}, {"reconcile", "apply", "--deep"}, {"reconcile", "verify", "--wait=1m"}, {"reconcile", "provenance", "--full"}, {"reconcile", "provenance", "--wait=1m"}, {"reconcile", "request-verification", "--full"}, {"reconcile", "request-verification", "extra"}, {"reconcile", "run"}, {"reconcile", "run", "verify", "extra"}, {"reconcile", "run", "verify", "--config", "/nonexistent/verify.json"}} {
		var output bytes.Buffer
		if err := cli.Run(context.Background(), args, &output, &output); err == nil {
			t.Errorf("accepted invalid command %q", args)
		}
	}
	for _, args := range [][]string{nil, {"--help"}, {"version"}, {"ci", "plan-images", "--help"}, {"dev", "--help"}, {"dev", "doctor", "--help"}, {"reconcile", "verify", "--scope=cloud", "--help"}, {"reconcile", "apply", "--wait=30m", "--help"}, {"reconcile", "provenance", "--provenance-base=" + strings.Repeat("a", 40), "--help"}, {"reconcile", "request-verification", "--help"}, {"reconcile", "run", "verify", "--help"}} {
		var output bytes.Buffer
		if err := cli.Run(context.Background(), args, &output, &output); err != nil || output.Len() == 0 {
			t.Errorf("command %q: %v, output=%q", args, err, &output)
		}
	}
}

func TestExitCodeSeparatesRetryFromFailure(t *testing.T) {
	locked := reconcile.ErrLocked{Owner: "laptop", Expires: time.Now().Add(time.Minute)}
	superseded := fmt.Errorf("publish: %w", reconcile.ErrSuperseded)
	for _, test := range []struct {
		err  error
		want int
	}{
		{err: nil, want: 0},
		{err: locked, want: 75},
		{err: fmt.Errorf("comparisons skipped: %w", locked), want: 75},
		{err: superseded, want: 75},
		{err: errors.Join(superseded, locked), want: 75},
		{err: errors.Join(superseded, errors.New("state unavailable")), want: 1},
		{err: errors.New("reconciliation lease lost"), want: 1},
	} {
		if got := cli.ExitCode(test.err); got != test.want {
			t.Errorf("%v exits %d, want %d", test.err, got, test.want)
		}
	}
}

func TestProvenanceReportsItsFailureAndDropsItsToken(t *testing.T) {
	t.Setenv("PROVENANCE_TOKEN", "attestation-secret")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	report := filepath.Join(t.TempDir(), "provenance.json")
	var output bytes.Buffer
	if err := cli.Run(context.Background(), []string{"reconcile", "provenance", "--root", t.TempDir(), "--report", report}, &output, &output); err == nil {
		t.Fatal("provenance without reconciliation state succeeded")
	}
	if token := os.Getenv("PROVENANCE_TOKEN"); token != "" {
		t.Fatal("provenance token left in the environment of child processes")
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var outcome struct{ Base, Error string }
	if err := json.Unmarshal(data, &outcome); err != nil || outcome.Base != "" || !strings.Contains(outcome.Error, "aws") {
		t.Fatalf("provenance failure reported as %s: %v", data, err)
	}
}

func TestReconcileConsumesThePublisherKeyFileAndRunnerToken(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("PUBLISHER_APP_PRIVATE_KEY_FILE", "")
	var output bytes.Buffer
	if err := cli.Run(context.Background(), []string{"reconcile", "apply", "--root", t.TempDir()}, &output, &output); err == nil || err.Error() != "PUBLISHER_APP_PRIVATE_KEY_FILE required" {
		t.Fatalf("apply without the publisher key returned %v", err)
	}
	for _, action := range []string{"plan", "apply", "verify", "status", "requirements", "provenance"} {
		key := filepath.Join(t.TempDir(), "key.pem")
		if err := os.WriteFile(key, []byte("publisher-secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PUBLISHER_APP_PRIVATE_KEY_FILE", key)
		t.Setenv("GH_TOKEN", "runner-secret")
		output.Reset()
		args := []string{"reconcile", action, "--root", t.TempDir()}
		if action == "verify" {
			args = append(args, "--scope=cloud")
		}
		cli.Run(context.Background(), args, &output, &output)
		if _, err := os.Stat(key); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left the publisher key file: %v", action, err)
		}
		for _, name := range []string{"PUBLISHER_APP_PRIVATE_KEY_FILE", "GH_TOKEN"} {
			if value, set := os.LookupEnv(name); set {
				t.Errorf("%s left %s=%q in the environment of child processes", action, name, value)
			}
		}
		if strings.Contains(output.String(), "publisher-secret") || strings.Contains(output.String(), "runner-secret") {
			t.Errorf("%s printed a credential:\n%s", action, output.String())
		}
	}
	key := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(key, []byte("publisher-secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUBLISHER_APP_PRIVATE_KEY_FILE", key)
	if err := cli.Run(context.Background(), []string{"reconcile", "apply", "--root", t.TempDir()}, &output, &output); err == nil || !strings.Contains(err.Error(), "readable only by its owner") {
		t.Fatalf("apply with a world-readable key returned %v", err)
	}
	if _, err := os.Stat(key); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("rejected publisher key file left behind: %v", err)
	}
}

func TestVerificationWritesItsOutcomeReport(t *testing.T) {
	for _, scope := range []string{"cloud", "full"} {
		t.Run(scope, func(t *testing.T) {
			report := filepath.Join(t.TempDir(), "reports", "verification.json")
			var output bytes.Buffer
			if err := cli.Run(context.Background(), []string{"reconcile", "verify", "--scope=" + scope, "--root", t.TempDir(), "--report", report}, &output, &output); err == nil {
				t.Fatal("verification without declarations succeeded")
			}
			data, err := os.ReadFile(report)
			if err != nil {
				t.Fatal(err)
			}
			var outcome struct {
				Scope       string
				Outcome     string
				Differences []any
				Errors      []string
			}
			if err := json.Unmarshal(data, &outcome); err != nil {
				t.Fatal(err)
			}
			if outcome.Scope != scope || outcome.Outcome != "failed" || outcome.Differences == nil || len(outcome.Differences) != 0 || len(outcome.Errors) != 1 || !strings.Contains(outcome.Errors[0], "settings.yaml") {
				t.Fatalf("verification error reported as %s", data)
			}
		})
	}
}

func TestReconciliationStateHonorsTheS3EndpointOverride(t *testing.T) {
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.Method+" "+r.URL.Path)
		w.Header().Set("ETag", `"status"`)
		fmt.Fprint(w, `{"desired_revision":"a","applied_revision":"a","stage":"complete"}`)
	}))
	defer server.Close()
	t.Setenv("AWS_REGION", "eu-north-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIALOCAL")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "local-secret")
	t.Setenv("AWS_ENDPOINT_URL_S3", server.URL)
	var output bytes.Buffer
	if err := cli.Run(context.Background(), []string{"reconcile", "status", "--root", t.TempDir()}, &output, &output); err != nil {
		t.Fatalf("status returned %v\n%s", err, &output)
	}
	if !slices.Equal(requested, []string{"GET /llunde-pyparser-bucket/reconciliation/production/status.json"}) || !strings.Contains(output.String(), `"applied_revision":"a"`) {
		t.Fatalf("requested %q and printed %s", requested, &output)
	}
}

func TestWorkflowVerificationFlagsParseAtThisRevision(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "reconcile-job.yml"))
	if err != nil {
		t.Fatal(err)
	}
	invocations := regexp.MustCompile(`-- infra (reconcile verify [^\n]*)`).FindAllSubmatch(data, -1)
	if len(invocations) != 1 {
		t.Fatalf("workflow invokes verification %d times, want once", len(invocations))
	}
	args := strings.Fields(string(invocations[0][1]))
	if !slices.Contains(args, "--scope=full") {
		t.Fatalf("workflow verification runs %q, want the full scope", args)
	}
	var output bytes.Buffer
	if err := cli.Run(context.Background(), append(args, "--help"), &output, &output); err != nil {
		t.Fatalf("workflow verification flags %q: %v", args, err)
	}
}

func TestVerificationRequiresAKnownScope(t *testing.T) {
	for _, args := range [][]string{{"reconcile", "verify"}, {"reconcile", "verify", "--scope=deep"}, {"reconcile", "verify", "--deep"}} {
		report := filepath.Join(t.TempDir(), "verification.json")
		var output bytes.Buffer
		if err := cli.Run(context.Background(), append(args, "--root", t.TempDir(), "--report", report), &output, &output); err == nil || !strings.Contains(err.Error(), "scope") && !strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("%q returned %v", args, err)
		}
		if _, err := os.Stat(report); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%q wrote a report for an unknown scope", args)
		}
	}
}

func TestVerificationRequestReadsItsCredentialFiles(t *testing.T) {
	credentials := t.TempDir()
	request := []string{"reconcile", "request-verification", "--app-id=1", "--installation-id=2", "--private-key=" + filepath.Join(credentials, "github-app.pem"), "--heartbeat-token=" + filepath.Join(credentials, "gatus-token")}
	for _, credential := range []string{"github-app.pem", "gatus-token"} {
		var output bytes.Buffer
		if err := cli.Run(context.Background(), request, &output, &output); err == nil || !strings.Contains(err.Error(), filepath.Join(credentials, credential)) {
			t.Fatalf("request without %s returned %v", credential, err)
		}
		if err := os.WriteFile(filepath.Join(credentials, credential), []byte("invalid\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := cli.Run(context.Background(), request, &output, &output); err == nil || !strings.Contains(err.Error(), "invalid verification heartbeat token") {
		t.Fatalf("request with an invalid heartbeat token returned %v", err)
	}
	if err := cli.Run(context.Background(), request[:4], &output, &output); err == nil || !strings.Contains(err.Error(), `required flag(s) "heartbeat-token", "private-key" not set`) {
		t.Fatalf("request without credential files returned %v", err)
	}
}
