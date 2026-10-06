package ci

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/deployment"
	"github.com/fredrir/infra/internal/process"
	"go.yaml.in/yaml/v3"
)

func TestChannelPromotionRequiresImmutableCheckedReleaseAndPreparedTrust(t *testing.T) {
	old, revision := strings.Repeat("a", 40), strings.Repeat("b", 40)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "build"), 0755)
	os.MkdirAll(filepath.Join(root, ".github/chainguard"), 0755)
	policy := filepath.Join(root, ".github/chainguard/deploy-123.sts.yaml")
	os.WriteFile(policy, []byte("claim_pattern:\n  job_workflow_sha: '^"+old+"$'\n  job_workflow_ref: '^fredrir/infra/\\.github/workflows/build-image\\.yml@"+old+"$'\npermissions:\n  actions: write\n"), 0644)
	channel := CIChannel{Schema: 1, Tag: "ci-v1", Release: "ci-v1.0.0", Revision: revision}
	if err := PrepareChannel(root, channel); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(policy)
	if err != nil {
		t.Fatal(err)
	}
	var trust struct {
		Claims struct {
			SHA string `yaml:"job_workflow_sha"`
		} `yaml:"claim_pattern"`
	}
	if err := yaml.Unmarshal(data, &trust); err != nil {
		t.Fatal(err)
	}
	revisions, err := deployment.WorkflowRevisions(trust.Claims.SHA)
	if err != nil || !slices.Equal(revisions, []string{old, revision}) {
		t.Fatal("promotion lost accepted deployment revision")
	}
	immutable, checked := true, true
	channelExists := true
	promotions := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/git/ref/tags/ci-v1") && !channelExists:
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/refs"):
			var payload struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			}
			json.NewDecoder(r.Body).Decode(&payload)
			if payload.Ref != "refs/tags/ci-v1" || payload.SHA != revision {
				t.Error("incorrect initial channel revision")
			}
			channelExists = true
			promotions++
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPatch:
			if !channelExists {
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
			promotions++
			var payload struct {
				SHA   string `json:"sha"`
				Force bool   `json:"force"`
			}
			json.NewDecoder(r.Body).Decode(&payload)
			if payload.SHA != revision || !payload.Force {
				t.Error("incorrect promoted revision")
			}
			io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "releases/tags/"):
			json.NewEncoder(w).Encode(map[string]any{"immutable": immutable, "tag_name": channel.Release})
		case strings.Contains(r.URL.Path, "git/ref/"):
			json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": revision, "type": "commit"}})
		case strings.Contains(r.URL.Path, "actions/workflows/reconcile.yml/runs"):
			conclusion := "failure"
			if checked {
				conclusion = "success"
			}
			json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []any{map[string]any{"head_branch": "main", "event": "push", "head_sha": revision, "status": "completed", "conclusion": conclusion}}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	api := ChannelAPI{Client: server.Client(), Base: server.URL, Token: "test"}
	immutable = false
	if err := api.Promote(context.Background(), root); err == nil || promotions != 0 {
		t.Fatal("mutable release promoted")
	}
	immutable = true
	checked = false
	if err := api.Promote(context.Background(), root); err == nil || promotions != 0 {
		t.Fatal("failed infrastructure checks promoted")
	}
	checked = true
	if err := api.Promote(context.Background(), root); err != nil || promotions != 1 {
		t.Fatalf("checked release not promoted: %v", err)
	}
	channelExists = false
	if err := api.Promote(context.Background(), root); err != nil || promotions != 2 || !channelExists {
		t.Fatalf("checked initial channel not created: %v", err)
	}
	channel.Revision = old
	encoded, _ := json.Marshal(channel)
	os.WriteFile(filepath.Join(root, "build/ci-channel.json"), encoded, 0644)
	if err := api.Promote(context.Background(), root); err == nil || promotions != 2 {
		t.Fatal("mismatched release revision promoted")
	}
}

func TestChannelPreparationRejectsBroadTrustBeforeWritingFiles(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "build"), 0755)
	os.MkdirAll(filepath.Join(root, ".github/chainguard"), 0755)
	file := filepath.Join(root, ".github/chainguard/deploy-123.sts.yaml")
	before := []byte("claim_pattern:\n  job_workflow_sha: '^.*$'\n")
	os.WriteFile(file, before, 0644)
	if err := PrepareChannel(root, CIChannel{Schema: 1, Tag: "ci-v1", Release: "ci-v1.0.0", Revision: strings.Repeat("a", 40)}); err == nil {
		t.Fatal("broad trust promoted")
	}
	if _, err := os.Stat(filepath.Join(root, "build/ci-channel.json")); !os.IsNotExist(err) {
		t.Fatal("partial channel preparation")
	}
	data, _ := os.ReadFile(file)
	if string(data) != string(before) {
		t.Fatal("failed preparation modified trust")
	}
}

func TestCIReleasePublicationRefusesUncheckedOrMutableSettingsBeforeWrites(t *testing.T) {
	revision := strings.Repeat("a", 40)
	checked, immutable := false, false
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
			w.WriteHeader(500)
			return
		}
		if strings.Contains(r.URL.Path, "actions/workflows/reconcile.yml/runs") {
			conclusion := "failure"
			if checked {
				conclusion = "success"
			}
			json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []any{map[string]any{"head_branch": "main", "event": "push", "head_sha": revision, "status": "completed", "conclusion": conclusion}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"enabled": immutable})
	}))
	defer server.Close()
	api := ChannelAPI{Client: server.Client(), Base: server.URL, Token: "test"}
	if err := api.Publish(context.Background(), "ci-v1.0.1", revision); err == nil || writes != 0 {
		t.Fatal("failed infrastructure checks created release objects")
	}
	checked = true
	if err := api.Publish(context.Background(), "ci-v1.0.1", revision); err == nil || writes != 0 {
		t.Fatal("mutable release settings created release objects")
	}
}

func TestCIReleaseChecksRequireLatestSuccessfulMainPush(t *testing.T) {
	revision := strings.Repeat("a", 40)
	success := map[string]string{"head_branch": "main", "event": "push", "head_sha": revision, "status": "completed", "conclusion": "success"}
	for _, test := range []struct {
		name   string
		fields map[string]string
	}{
		{"failed", map[string]string{"conclusion": "failure"}},
		{"cancelled", map[string]string{"conclusion": "cancelled"}},
		{"pending", map[string]string{"status": "in_progress"}},
		{"pull request", map[string]string{"event": "pull_request"}},
		{"another branch", map[string]string{"head_branch": "feature"}},
		{"another revision", map[string]string{"head_sha": strings.Repeat("b", 40)}},
		{"missing", nil},
		{"successful", map[string]string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var runs []map[string]string
			if test.fields != nil {
				run := make(map[string]string)
				for name, value := range success {
					run[name] = value
				}
				for name, value := range test.fields {
					run[name] = value
				}
				runs = append(runs, run)
			}
			if test.name == "failed" || test.name == "cancelled" || test.name == "pending" {
				runs = append(runs, success)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/fredrir/infra/actions/workflows/reconcile.yml/runs" || r.URL.Query().Get("head_sha") != revision || r.URL.Query().Get("branch") != "main" || r.URL.Query().Get("event") != "push" {
					t.Errorf("incorrect infrastructure check request: %s", r.URL)
				}
				json.NewEncoder(w).Encode(map[string]any{"workflow_runs": runs})
			}))
			defer server.Close()
			api := ChannelAPI{Client: server.Client(), Base: server.URL, Token: "test"}
			if err := api.Checked(context.Background(), revision); (err == nil) != (test.name == "successful") {
				t.Fatalf("infrastructure check result: %v", err)
			}
		})
	}
}

func TestChannelPromotionWaitsForCheckAndPlanBeforeMerging(t *testing.T) {
	for _, test := range []struct {
		name, checks string
		merge        bool
	}{
		{"checks not registered", `[{"name":"cli / build","state":"SUCCESS"}]`, false},
		{"check running", `[{"name":"check / check","state":"IN_PROGRESS"},{"name":"reconcile / plan","state":"SUCCESS"}]`, false},
		{"check failed", `[{"name":"check / check","state":"FAILURE"},{"name":"reconcile / plan","state":"SUCCESS"}]`, false},
		{"plan running", `[{"name":"check / check","state":"SKIPPED"},{"name":"reconcile / plan","state":"IN_PROGRESS"}]`, false},
		{"plan skipped", `[{"name":"check / check","state":"SUCCESS"},{"name":"reconcile / plan","state":"SKIPPED"}]`, false},
		{"another check queued", `[{"name":"check / check","state":"SKIPPED"},{"name":"reconcile / plan","state":"SUCCESS"},{"name":"check / host-access","state":"QUEUED"}]`, false},
		{"checks passed", `[{"name":"check / check","state":"SUCCESS"},{"name":"reconcile / plan","state":"SUCCESS"}]`, true},
		{"bot check skipped", `[{"name":"check / check","state":"SKIPPED"},{"name":"reconcile / plan","state":"SUCCESS"}]`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			merged := false
			runner := process.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				if slices.Contains(options.Args, "--json") {
					if !test.merge {
						cancel()
					}
					if test.name == "another check queued" {
						return process.Result{Stdout: []byte(test.checks), ExitCode: 8}, errors.New("checks pending")
					}
					return process.Result{Stdout: []byte(test.checks)}, nil
				}
				if slices.Contains(options.Args, "merge") {
					merged = true
				}
				return process.Result{}, nil
			}}
			err := mergeProposal(ctx, runner, "ci-promotion-test")
			if merged != test.merge || (err == nil) != test.merge {
				t.Fatalf("merged=%t, error=%v", merged, err)
			}
			if !test.merge && !errors.Is(err, context.Canceled) {
				t.Fatalf("pending checks did not wait: %v", err)
			}
		})
	}
}
