package ci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fredrir/infra/internal/process"
)

func TestRuntimeDependencyKeyIgnoresDevLockChangesButTracksRecipeRuntimeAndDebianPackages(t *testing.T) {
	root := t.TempDir()
	recipe := "ARG PYTHON_IMAGE=docker.io/library/python:3.14.8-slim-bookworm@sha256:" + strings.Repeat("0", 64) + "\n"
	for name, content := range map[string]string{"build/projects/llunde-pyparser/dependencies.Containerfile": recipe, "internal/ci/dependencies.go": "input", "pyproject.toml": "input", "uv.lock": "input"} {
		path := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(path), 0755)
		os.WriteFile(path, []byte(content), 0644)
	}
	lock := "lock-version = '1.0'\n[[packages]]\nname='runtime'\nversion='1.0'\n"
	runner := process.Runner{Dir: root, Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		if options.Name != "uv" || !slices.Contains(options.Args, "--no-dev") || !slices.Contains(options.Env, "UV_NO_CONFIG=1") {
			t.Error("runtime export changed")
		}
		return process.Result{Stdout: []byte(lock)}, nil
	}}
	var security atomic.Value
	security.Store(strings.Repeat("a", 64))
	var published atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		packages := map[string]string{"/debian/dists/bookworm/InRelease": strings.Repeat("b", 64), "/debian/dists/bookworm-updates/InRelease": strings.Repeat("c", 64), "/debian-security/dists/bookworm-security/InRelease": security.Load().(string)}[r.URL.Path]
		if packages == "" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, "Date: %d\nSHA256:\n %s 1 main/binary-amd64/Packages\n %s 1 main/binary-all/Packages\n %s 1 main/i18n/Index\n", published.Add(1), packages, strings.Repeat("d", 64), strings.Repeat("e", 64))
	}))
	defer server.Close()
	archive := DebianArchive{Client: server.Client(), Base: server.URL}
	before, err := PlanDependencies(context.Background(), runner, root, archive)
	if err != nil {
		t.Fatal(err)
	}
	after, err := PlanDependencies(context.Background(), runner, root, archive)
	if err != nil || before.Key != after.Key {
		t.Fatal("republished Debian release metadata rebuilt image")
	}
	os.WriteFile(filepath.Join(root, "uv.lock"), []byte("dev-only change"), 0644)
	after, err = PlanDependencies(context.Background(), runner, root, archive)
	if err != nil || before.Key != after.Key {
		t.Fatal("dev dependency change rebuilt runtime image")
	}
	lock = strings.ReplaceAll(lock, "1.0'\n", "2.0'\n")
	after, err = PlanDependencies(context.Background(), runner, root, archive)
	if err != nil || before.Key == after.Key {
		t.Fatal("runtime dependency change reused image")
	}
	lock = "lock-version='1.0'\n[[packages]]\nversion='1.0'\nname='runtime'\n"
	after, err = PlanDependencies(context.Background(), runner, root, archive)
	if err != nil || before.Key != after.Key {
		t.Fatal("TOML formatting changed dependency key")
	}
	os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte("new entry point"), 0644)
	after, err = PlanDependencies(context.Background(), runner, root, archive)
	if err != nil || before.Key == after.Key {
		t.Fatal("package metadata change reused installed entry points")
	}
	os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte("input"), 0644)
	security.Store(strings.Repeat("f", 64))
	after, err = PlanDependencies(context.Background(), runner, root, archive)
	if err != nil || before.Key == after.Key {
		t.Fatal("Debian security update reused image")
	}
	security.Store(strings.Repeat("a", 64))
	os.WriteFile(filepath.Join(root, "build/projects/llunde-pyparser/dependencies.Containerfile"), []byte(recipe+"new recipe"), 0644)
	after, err = PlanDependencies(context.Background(), runner, root, archive)
	if err != nil || before.Key == after.Key {
		t.Fatal("recipe change reused image")
	}
	os.WriteFile(filepath.Join(root, "build/projects/llunde-pyparser/dependencies.Containerfile"), []byte(strings.Replace(recipe, "bookworm", "trixie", 1)), 0644)
	if _, err := PlanDependencies(context.Background(), runner, root, archive); err == nil {
		t.Fatal("unpublished Debian release produced a key")
	}
	os.WriteFile(filepath.Join(root, "build/projects/llunde-pyparser/dependencies.Containerfile"), []byte(strings.Replace(recipe, "-slim-bookworm", "-slim", 1)), 0644)
	if _, err := PlanDependencies(context.Background(), runner, root, archive); err == nil {
		t.Fatal("recipe without a Debian release produced a key")
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
		if options.Name != "cosign" || !strings.Contains(strings.Join(options.Args, " "), workflow) || !slices.Contains(options.Args, "--certificate-identity-regexp") {
			t.Error("dependency signer unconstrained")
		}
		return process.Result{Stdout: []byte(`[{"optional":{"workflow-revision":"` + workflow + `"}}]`)}, nil
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
	if _, _, err := fetchMetadata(registry.Client, request, false); err != nil {
		t.Fatal(err)
	}
}

func TestDependencySignaturesRequireAnApprovedSignedRevisionWithoutRetryingEveryRevision(t *testing.T) {
	var revisions []string
	for i := 1; i <= 40; i++ {
		revisions = append(revisions, fmt.Sprintf("%040x", i))
	}
	source := strings.Repeat("a", 40)
	image := parserDependencyImage + "@sha256:" + strings.Repeat("b", 64)
	approved := `[{"optional":{"workflow-revision":"` + revisions[39] + `"}}]`
	for _, test := range []struct {
		name, output, trigger string
		invalid, rejected     bool
		calls                 int
	}{
		{name: "last approved revision", output: approved, calls: 1},
		{name: "manual publication", output: approved, trigger: "workflow_dispatch", calls: 2},
		{name: "unapproved signed revision", output: `[{"optional":{"workflow-revision":"` + source + `"}}]`, rejected: true, calls: 2},
		{name: "missing signed revision", output: `[{"optional":{}}]`, rejected: true, calls: 2},
		{name: "empty verified signatures", output: `[]`, rejected: true, calls: 2},
		{name: "malformed verifier output", output: `{`, rejected: true, calls: 1},
		{name: "invalid signature with approved annotation", output: approved, invalid: true, rejected: true, calls: 2},
		{name: "one approved signature", output: `[{"optional":{}},{"optional":{"workflow-revision":"` + revisions[39] + `"}}]`, calls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			runner := process.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				calls++
				value := func(flag string) string {
					i := slices.Index(options.Args, flag)
					if i < 0 || i+1 >= len(options.Args) {
						t.Fatalf("missing %s", flag)
					}
					return options.Args[i+1]
				}
				for flag, want := range map[string]string{
					"--certificate-oidc-issuer":                "https://token.actions.githubusercontent.com",
					"--certificate-github-workflow-repository": "fredrir/llunde-pyparser",
					"--certificate-github-workflow-sha":        source,
					"--certificate-github-workflow-ref":        "refs/heads/main",
				} {
					if got := value(flag); got != want {
						t.Errorf("%s = %s", flag, got)
					}
				}
				for _, annotation := range []string{"source-repository=fredrir/llunde-pyparser", "source-revision=" + source} {
					if !slices.Contains(options.Args, annotation) {
						t.Errorf("missing signed %s", annotation)
					}
				}
				identity := regexp.MustCompile(value("--certificate-identity-regexp"))
				prefix := "https://github.com/fredrir/infra/.github/workflows/build-image.yml@"
				for _, revision := range append(slices.Clone(revisions), "refs/tags/ci-v1") {
					if !identity.MatchString(prefix + revision) {
						t.Errorf("approved identity rejected: %s", revision)
					}
				}
				for _, candidate := range []string{prefix + source, prefix + revisions[0] + "extra", "https://evil.example/" + prefix + revisions[0]} {
					if identity.MatchString(candidate) {
						t.Errorf("unapproved identity accepted: %s", candidate)
					}
				}
				if options.Name != "cosign" || options.Args[len(options.Args)-1] != image {
					t.Fatal("wrong image verifier")
				}
				if test.invalid || (test.trigger != "" && value("--certificate-github-workflow-trigger") != test.trigger) {
					return process.Result{Stdout: []byte(test.output)}, errors.New("signature rejected")
				}
				return process.Result{Stdout: []byte(test.output)}, nil
			}}
			err := verifyDependencySignatures(context.Background(), runner, revisions, source, image)
			if (err != nil) != test.rejected || calls != test.calls {
				t.Fatalf("verification: %v, %d calls; want rejected=%v, %d calls", err, calls, test.rejected, test.calls)
			}
		})
	}
}
