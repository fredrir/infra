package reconcile

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

func TestPublishedRevisionRequiresReadyProductionSource(t *testing.T) {
	revision := strings.Repeat("a", 40)
	for _, test := range []struct {
		name, branch, artifact, ready string
		observed                      int
		wantError                     bool
	}{
		{"production", "production", "production@sha1:" + revision, "True", 2, false},
		{"main branch", "main", "main@sha1:" + revision, "True", 2, true},
		{"wrong artifact branch", "production", "main@sha1:" + revision, "True", 2, true},
		{"short revision", "production", "production@sha1:abc", "True", 2, true},
		{"not ready", "production", "production@sha1:" + revision, "False", 2, true},
		{"stale generation", "production", "production@sha1:" + revision, "True", 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := map[string]any{"metadata": map[string]any{"name": "flux-system", "generation": 2}, "spec": map[string]any{"ref": map[string]any{"branch": test.branch}}, "status": map[string]any{"artifact": map[string]any{"revision": test.artifact}, "conditions": []any{map[string]any{"type": "Ready", "status": test.ready, "observedGeneration": test.observed}}}}
			payload, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			commands := &Commands{Runner: ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				if options.Name != "kubectl" || strings.Join(options.Args, " ") != "get gitrepositories.source.toolkit.fluxcd.io flux-system -n=flux-system -o=json --request-timeout=30s" {
					t.Fatalf("unexpected read: %s %v", options.Name, options.Args)
				}
				return process.Result{Stdout: payload}, nil
			}}}
			actual, err := commands.PublishedRevision(context.Background())
			if (err != nil) != test.wantError {
				t.Fatalf("error %v, want error %v", err, test.wantError)
			}
			if !test.wantError && actual != revision {
				t.Fatalf("revision %q, want %q", actual, revision)
			}
		})
	}
}
