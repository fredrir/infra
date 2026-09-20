package images_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/images"
)

const catalog = `- image: ghcr.io/fredrir/one
  dockerfile: images/one/Containerfile
  inputs: [images/one, pins.lock]
  check: one --version
  scan-skip-dirs: /nix
  shell: '["/bin/sh", "-euc"]'
- image: ghcr.io/fredrir/two
  dockerfile: images/two/Containerfile
  inputs: [images/two]
  check: two --version
`

func repository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for path, content := range map[string]string{
		"images/catalog.yaml": catalog, ".github/workflows/images.yml": "name: Images\n",
		".dockerignore": ".git\n", "pins.lock": "one\n",
		"images/one/Containerfile": "FROM scratch\n", "images/two/Containerfile": "FROM scratch\n",
	} {
		write(t, root, path, content)
	}
	git(t, root, "init", "--quiet")
	commit(t, root)
	return root
}

func write(t *testing.T, root, path, content string) {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func commit(t *testing.T, root string) {
	t.Helper()
	git(t, root, "add", "--all")
	git(t, root, "commit", "--quiet", "--message", "change")
}

func planner(t *testing.T, handler http.HandlerFunc) images.Planner {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return images.Planner{Root: repository(t), Registry: server.URL, Client: server.Client(), Log: io.Discard}
}

func plan(t *testing.T, p images.Planner) []images.Image {
	t.Helper()
	result, err := p.Plan(context.Background(), "images/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestOnlyUnpublishedImagesArePlanned(t *testing.T) {
	published := make(map[string]bool)
	requests := 0
	p := planner(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path == "/token" {
			if r.Method != http.MethodGet || r.URL.Query().Get("service") != "ghcr.io" || !strings.HasSuffix(r.URL.Query().Get("scope"), ":pull") {
				t.Error("invalid token request:", r.URL)
			}
			fmt.Fprint(w, `{"token":"registry-token"}`)
			return
		}
		if r.Method != http.MethodHead || r.Header.Get("Authorization") != "Bearer registry-token" || !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
			t.Error("invalid manifest request")
		}
		if !published[r.URL.Path] {
			w.WriteHeader(http.StatusNotFound)
		}
	})
	before := plan(t, p)
	if len(before) != 2 || before[0].Image != "ghcr.io/fredrir/one" || before[1].Image != "ghcr.io/fredrir/two" {
		t.Fatalf("unexpected matrix: %+v", before)
	}
	if before[0].Inputs != nil || before[0].ScanSkipDirs != "/nix" || before[0].Shell != `["/bin/sh", "-euc"]` || before[0].Check != "one --version" || before[0].Dockerfile != "images/one/Containerfile" {
		t.Fatalf("matrix lost catalog metadata or exposed inputs: %+v", before[0])
	}
	for _, entry := range before {
		if !strings.HasPrefix(entry.Tag, "inputs-") || len(entry.Tag) != 71 {
			t.Fatal("invalid tag:", entry.Tag)
		}
	}
	published["/v2/fredrir/one/manifests/"+before[0].Tag] = true
	if after := plan(t, p); len(after) != 1 || after[0].Image != before[1].Image {
		t.Fatalf("published image was planned: %+v", after)
	}
	published["/v2/fredrir/two/manifests/"+before[1].Tag] = true
	if after := plan(t, p); after == nil || len(after) != 0 {
		t.Fatalf("expected empty array, got %+v", after)
	}
	requests = 0
	p.Refresh = true
	if after := plan(t, p); !reflect.DeepEqual(before, after) || requests != 0 {
		t.Fatalf("refresh must plan all images without registry requests: %+v, requests=%d", after, requests)
	}
}

func TestTagsTrackOnlyDeclaredInputsAndSharedBuildConfiguration(t *testing.T) {
	p := planner(t, http.NotFound)
	p.Refresh = true
	before := plan(t, p)
	write(t, p.Root, "unrelated", "change\n")
	commit(t, p.Root)
	if after := plan(t, p); !reflect.DeepEqual(before, after) {
		t.Fatal("unrelated commit changed tags")
	}
	write(t, p.Root, "pins.lock", "two\n")
	if after := plan(t, p); !reflect.DeepEqual(before, after) {
		t.Fatal("uncommitted build input changed tags")
	}
	commit(t, p.Root)
	after := plan(t, p)
	if after[0].Tag == before[0].Tag || after[1].Tag != before[1].Tag {
		t.Fatal("input change did not selectively invalidate its image")
	}
	for _, path := range []string{".github/workflows/images.yml", ".dockerignore"} {
		before = after
		write(t, p.Root, path, "changed\n")
		commit(t, p.Root)
		after = plan(t, p)
		if after[0].Tag == before[0].Tag || after[1].Tag == before[1].Tag {
			t.Fatal("shared input did not invalidate every image:", path)
		}
	}
	before = after
	write(t, p.Root, "images/catalog.yaml", strings.Replace(catalog, "one --version", "one --check", 1))
	commit(t, p.Root)
	after = plan(t, p)
	if after[0].Tag == before[0].Tag || after[1].Tag != before[1].Tag {
		t.Fatal("catalog entry change did not selectively invalidate its image")
	}
}

func TestRegistryFailuresPlanImages(t *testing.T) {
	for _, test := range []struct {
		name   string
		token  string
		status int
	}{
		{"unavailable", "", http.StatusInternalServerError},
		{"invalid-token-json", "{", http.StatusOK},
		{"empty-token", `{"token":""}`, http.StatusOK},
		{"manifest-forbidden", `{"token":"token"}`, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := planner(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					w.WriteHeader(test.status)
					fmt.Fprint(w, test.token)
					return
				}
				w.WriteHeader(http.StatusForbidden)
			})
			if got := len(plan(t, p)); got != 2 {
				t.Fatalf("expected both images, got %d", got)
			}
		})
	}
	t.Run("connection-refused", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		server.Close()
		p := images.Planner{Root: repository(t), Registry: server.URL, Client: server.Client(), Log: io.Discard}
		if len(plan(t, p)) != 2 {
			t.Fatal("connection failure did not plan all images")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		p := planner(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		p.Client.Timeout = 10 * time.Millisecond
		if len(plan(t, p)) != 2 {
			t.Fatal("request timeout did not plan all images")
		}
	})
}

func TestInvalidCatalogFailsBeforeRegistryRequests(t *testing.T) {
	for name, content := range map[string]string{
		"missing-inputs":     strings.Replace(catalog, "[images/one, pins.lock]", "[]", 1),
		"missing-object":     strings.Replace(catalog, "pins.lock", "missing", 1),
		"foreign-image":      strings.Replace(catalog, "fredrir/one", "other/one", 1),
		"duplicate-image":    strings.Replace(catalog, "fredrir/two", "fredrir/one", 1),
		"unknown-field":      catalog + "  typo: true\n",
		"multiple-documents": catalog + "---\n[]\n",
		"escaping-input":     strings.Replace(catalog, "pins.lock", "../pins.lock", 1),
		"missing-dockerfile": strings.Replace(catalog, "  dockerfile: images/one/Containerfile\n", "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			p := planner(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid catalog contacted registry") })
			write(t, p.Root, "images/catalog.yaml", content)
			if _, err := p.Plan(context.Background(), "images/catalog.yaml"); err == nil {
				t.Fatal("invalid catalog was accepted")
			}
		})
	}
}

func TestCancellationStopsPlanning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := planner(t, func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	})
	defer cancel()
	if _, err := p.Plan(ctx, "images/catalog.yaml"); err != context.Canceled {
		t.Fatalf("expected cancellation, got %v", err)
	}
}
