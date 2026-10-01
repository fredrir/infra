package ci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/process"
)

func TestRuntimeDependencyKeyIgnoresDevLockChangesButTracksRecipeAndRuntime(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"build/projects/llunde-pyparser/dependencies.Containerfile", "internal/ci/dependencies.go", "pyproject.toml", "uv.lock"} {
		path := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(path), 0755)
		os.WriteFile(path, []byte("input"), 0644)
	}
	lock := "lock-version = '1.0'\n[[packages]]\nname='runtime'\nversion='1.0'\n"
	runner := process.Runner{Dir: root, Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		if options.Name != "uv" || !slices.Contains(options.Args, "--no-dev") || !slices.Contains(options.Env, "UV_NO_CONFIG=1") {
			t.Error("runtime export changed")
		}
		return process.Result{Stdout: []byte(lock)}, nil
	}}
	before, err := PlanDependencies(context.Background(), runner, root)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "uv.lock"), []byte("dev-only change"), 0644)
	after, err := PlanDependencies(context.Background(), runner, root)
	if err != nil || before.Key != after.Key {
		t.Fatal("dev dependency change rebuilt runtime image")
	}
	lock = strings.ReplaceAll(lock, "1.0'\n", "2.0'\n")
	after, err = PlanDependencies(context.Background(), runner, root)
	if err != nil || before.Key == after.Key {
		t.Fatal("runtime dependency change reused image")
	}
	lock = "lock-version='1.0'\n[[packages]]\nversion='1.0'\nname='runtime'\n"
	after, err = PlanDependencies(context.Background(), runner, root)
	if err != nil || before.Key != after.Key {
		t.Fatal("TOML formatting changed dependency key")
	}
	os.WriteFile(filepath.Join(root, "build/projects/llunde-pyparser/dependencies.Containerfile"), []byte("new recipe"), 0644)
	after, err = PlanDependencies(context.Background(), runner, root)
	if err != nil || before.Key == after.Key {
		t.Fatal("recipe change reused image")
	}
}

func TestDependencyRegistryVerifiesChecksumsLabelsPlatformAndSigner(t *testing.T) {
	key := strings.Repeat("a", 64)
	config, _ := json.Marshal(map[string]any{"os": "linux", "architecture": "amd64", "config": map[string]any{"Labels": map[string]string{"io.llunde.parser.dependencies.key": key, "org.opencontainers.image.revision": strings.Repeat("b", 40)}}})
	checksum := func(data []byte) string { sum := sha256.Sum256(data); return "sha256:" + hex.EncodeToString(sum[:]) }
	manifest, _ := json.Marshal(map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "config": map[string]string{"digest": checksum(config)}})
	corrupt := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			io.WriteString(w, `{"token":"bearer"}`)
		case strings.Contains(r.URL.Path, "/manifests/"):
			if strings.HasSuffix(r.URL.Path, "inputs-"+strings.Repeat("c", 64)) {
				w.WriteHeader(404)
				return
			}
			if corrupt {
				io.WriteString(w, `{}`)
				return
			}
			w.Write(manifest)
		case strings.Contains(r.URL.Path, "/blobs/"):
			w.Write(config)
		default:
			w.WriteHeader(403)
		}
	}))
	defer server.Close()
	registry := DependencyRegistry{Client: server.Client(), Base: server.URL, Actor: "fredrir", Token: "test-token"}
	plan := DependencyPlan{Key: key, Tag: "inputs-" + key, Repository: parserDependencyImage, Build: true}
	plan, err := registry.Lookup(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".github/chainguard"), 0755)
	workflow := strings.Repeat("d", 40)
	os.WriteFile(filepath.Join(root, ".github/chainguard/deploy-1328252868.sts.yaml"), []byte("claim_pattern:\n  job_workflow_sha: '^"+workflow+"$'\n"), 0644)
	verified := false
	runner := process.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		verified = true
		if options.Name != "cosign" || !slices.Contains(options.Args, "workflow-revision="+workflow) || !slices.Contains(options.Args, "--certificate-identity-regexp") {
			t.Error("dependency signer unconstrained")
		}
		return process.Result{}, nil
	}}
	if err := registry.Verify(context.Background(), runner, root, plan, plan.Image); err != nil || !verified {
		t.Fatalf("verified dependency: %v", err)
	}
	verified = false
	plan.Key = strings.Repeat("e", 64)
	if err := registry.Verify(context.Background(), runner, root, plan, plan.Image); err == nil || verified {
		t.Fatal("mismatched dependency inputs reached signature verification")
	}
	corrupt = true
	if err := registry.Verify(context.Background(), runner, root, plan, plan.Image); err == nil {
		t.Fatal("corrupt manifest accepted")
	}
	plan.Tag = "inputs-" + strings.Repeat("c", 64)
	plan.Build = true
	plan.Image = ""
	if result, err := registry.Lookup(context.Background(), plan); err != nil || !result.Build || result.Image != "" {
		t.Fatalf("missing dependency: %+v, %v", result, err)
	}
}

func TestDependencyRegistryStripsAuthorizationOnCrossOriginRedirect(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("registry credentials crossed origin")
		}
		io.WriteString(w, "{}")
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
	defer origin.Close()
	registry := DependencyRegistry{Client: origin.Client()}
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, origin.URL, nil)
	request.Header.Set("Authorization", "Bearer secret")
	if _, _, err := registry.fetch(request, false); err != nil {
		t.Fatal(err)
	}
}
