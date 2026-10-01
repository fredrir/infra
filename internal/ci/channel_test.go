package ci

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/deployment"
	"go.yaml.in/yaml/v3"
)

func TestChannelPromotionRequiresImmutableQualifiedReleaseAndPreparedTrust(t *testing.T) {
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
	immutable, qualified := true, true
	promotions := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch:
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
		case strings.Contains(r.URL.Path, "check-runs"):
			conclusion := "failure"
			if qualified {
				conclusion = "success"
			}
			json.NewEncoder(w).Encode(map[string]any{"check_runs": []any{map[string]any{"name": "CI candidate", "head_sha": revision, "status": "completed", "conclusion": conclusion, "app": map[string]string{"slug": "github-actions"}}}})
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
	qualified = false
	if err := api.Promote(context.Background(), root); err == nil || promotions != 0 {
		t.Fatal("failed candidate promoted")
	}
	qualified = true
	if err := api.Promote(context.Background(), root); err != nil || promotions != 1 {
		t.Fatalf("qualified release not promoted: %v", err)
	}
	channel.Revision = old
	encoded, _ := json.Marshal(channel)
	os.WriteFile(filepath.Join(root, "build/ci-channel.json"), encoded, 0644)
	if err := api.Promote(context.Background(), root); err == nil || promotions != 1 {
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

func TestCIReleasePublicationRefusesUnqualifiedOrMutableSettingsBeforeWrites(t *testing.T) {
	revision := strings.Repeat("a", 40)
	qualified, immutable := false, false
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
			w.WriteHeader(500)
			return
		}
		if strings.Contains(r.URL.Path, "check-runs") {
			conclusion := "failure"
			if qualified {
				conclusion = "success"
			}
			json.NewEncoder(w).Encode(map[string]any{"check_runs": []any{map[string]any{"name": "CI candidate", "head_sha": revision, "status": "completed", "conclusion": conclusion, "app": map[string]string{"slug": "github-actions"}}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"enabled": immutable})
	}))
	defer server.Close()
	api := ChannelAPI{Client: server.Client(), Base: server.URL, Token: "test"}
	if err := api.Publish(context.Background(), "ci-v1.0.1", revision); err == nil || writes != 0 {
		t.Fatal("failed candidate created release objects")
	}
	qualified = true
	if err := api.Publish(context.Background(), "ci-v1.0.1", revision); err == nil || writes != 0 {
		t.Fatal("mutable release settings created release objects")
	}
}
