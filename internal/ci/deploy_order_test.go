package ci

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/fredrir/infra/internal/process"
)

func TestProvenanceVerifierFailuresNameTheirCause(t *testing.T) {
	trust := fstest.MapFS{".github/chainguard/deploy-1.sts.yaml": {Data: []byte("claim_pattern:\n  job_workflow_sha: '^" + strings.Repeat("d", 40) + "$'\n")}}
	mapping := DeploymentMapping{Repository: "fredrir/example", Visibility: "public"}
	for _, test := range []struct {
		name   string
		result process.Result
		err    error
		want   string
	}{
		{name: "rejected credentials", result: process.Result{ExitCode: 1, Stderr: []byte("verifying\nHTTP 401: Bad credentials for ghs_attestationsecret\n")}, err: errors.New("gh failed: exit status 1"), want: "gh at workflow dddddddddddd: HTTP 401: Bad credentials for [redacted]"},
		{name: "missing verifier", result: process.Result{ExitCode: -1}, err: errors.New(`start gh: exec: "gh": executable file not found in $PATH`), want: `executable file not found`},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := Runner{Env: []string{"GH_TOKEN=ghs_attestationsecret"}, Execute: func(context.Context, process.Options) (process.Result, error) { return test.result, test.err }}
			_, err := VerifyDeploymentProvenance(context.Background(), runner, trust, "1", mapping, "ghcr.io/fredrir/example", "sha256:"+strings.Repeat("a", 64), strings.Repeat("b", 40))
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "ghs_attestationsecret") {
				t.Fatalf("verifier failure reported as %v", err)
			}
		})
	}
}

func TestAttestedDeploymentOrders(t *testing.T) {
	image, revision, digest := "ghcr.io/fredrir/example", strings.Repeat("b", 40), "sha256:"+strings.Repeat("a", 64)
	cases := []struct {
		name, visibility, data string
		runs                   [][2]uint64
	}{
		{"public", "public", `[{"verificationResult":{"signature":{"certificate":{"runInvocationURI":"https://github.com/fredrir/example/actions/runs/35525556507/attempts/2"}}}}]`, [][2]uint64{{35525556507, 2}}},
		{"private", "private", `[{"optional":{"source-run-id":"35525556507","source-run-attempt":"2"}}]`, [][2]uint64{{35525556507, 2}}},
		{"re-run", "public", `[{"verificationResult":{"signature":{"certificate":{"runInvocationURI":"https://github.com/fredrir/example/actions/runs/7/attempts/2"}}}},{"verificationResult":{"signature":{"certificate":{"runInvocationURI":"https://github.com/fredrir/example/actions/runs/7/attempts/1"}}}}]`, [][2]uint64{{7, 2}, {7, 1}}},
		{"wrong-repository", "public", `[{"verificationResult":{"signature":{"certificate":{"runInvocationURI":"https://github.com/other/example/actions/runs/35525556507/attempts/2"}}}}]`, nil},
		{"mutable-statement", "public", `[{"verificationResult":{"statement":{"predicate":{"runDetails":{"metadata":{"invocationId":"https://github.com/fredrir/example/actions/runs/35525556507/attempts/2"}}}}}}]`, nil},
		{"unsigned-root-field", "public", `[{"run_id":35525556507}]`, nil},
		{"legacy-private", "private", `[{"optional":{"source-revision":"abc"}}]`, nil},
		{"numeric-annotation", "private", `[{"optional":{"source-run-id":35525556507,"source-run-attempt":"2"}}]`, nil},
		{"overflow", "private", `[{"optional":{"source-run-id":"18446744073709551616","source-run-attempt":"2"}}]`, nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			orders, err := attestedDeploymentOrders([]byte(test.data), test.visibility, "fredrir/example", image, digest, revision)
			if err != nil || len(orders) != len(test.runs) {
				t.Fatalf("orders=%+v err=%v", orders, err)
			}
			for index, order := range orders {
				if order != (DeploymentOrder{Schema: 1, Image: image, Revision: revision, Digest: digest, RunID: test.runs[index][0], Attempt: test.runs[index][1]}) {
					t.Fatalf("order %d = %+v", index, order)
				}
			}
		})
	}
	if _, err := attestedDeploymentOrders([]byte(`[]`), "internal", "fredrir/example", image, digest, revision); err == nil {
		t.Fatal("unclassified visibility accepted")
	}
}

func TestDeploymentOrderRejectsRollbackAndConflictingIdentity(t *testing.T) {
	accepted := DeploymentOrder{Schema: 1, Image: "ghcr.io/fredrir/example", Revision: strings.Repeat("a", 40), Digest: "sha256:" + strings.Repeat("b", 64), RunID: 20, Attempt: 2}
	for _, scenario := range []string{"older-run", "older-attempt", "different-revision", "different-digest"} {
		t.Run(scenario, func(t *testing.T) {
			candidate := accepted
			switch scenario {
			case "older-run":
				candidate.RunID = 19
				candidate.Attempt = 100
			case "older-attempt":
				candidate.Attempt = 1
			case "different-revision":
				candidate.Revision = strings.Repeat("c", 40)
			case "different-digest":
				candidate.Digest = "sha256:" + strings.Repeat("d", 64)
			}
			if err := checkDeploymentOrder(candidate, accepted); err == nil {
				t.Fatal("rollback accepted")
			}
		})
	}
	if err := checkDeploymentOrder(accepted, accepted); err != nil {
		t.Fatal(err)
	}
	newer := accepted
	newer.RunID++
	newer.Attempt = 1
	newer.Revision = strings.Repeat("c", 40)
	if err := checkDeploymentOrder(newer, accepted); err != nil {
		t.Fatal(err)
	}
}

func TestDeployRejectsOlderQueuedRunWithoutMutation(t *testing.T) {
	f := newDeployFixture(t, "kustomize", "private", false)
	f.receipt(f.root, DeploymentOrder{Schema: 1, Image: f.options.Image, Revision: f.options.Revision, Digest: f.options.Digest, RunID: 101, Attempt: 1})
	f.commit("Accept newer run")
	f.git("push", "--quiet", "origin", "HEAD:main")
	before := f.deployed()
	if err := f.run(); err == nil || !strings.Contains(err.Error(), "stale deployment") {
		t.Fatalf("expected stale error, got %v", err)
	}
	if f.deployed() != before || f.git("status", "--porcelain") != "" {
		t.Fatal("stale deployment mutated checkout")
	}
}

func TestDeployRejectsNewerRunDuringPushRace(t *testing.T) {
	f := newDeployFixture(t, "kustomize", "public", false)
	other := filepath.Join(t.TempDir(), "other")
	f.git("clone", "--quiet", "--branch", "main", f.remote, other)
	f.receipt(other, DeploymentOrder{Schema: 1, Image: f.options.Image, Revision: f.options.Revision, Digest: f.options.Digest, RunID: 101, Attempt: 1})
	for _, args := range [][]string{{"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "Accept newer deployment"}, {"push", "--quiet", "origin", "HEAD:main"}} {
		f.git(append([]string{"-C", other}, args...)...)
	}
	before := f.deployed()
	if err := f.run(); err == nil || !strings.Contains(err.Error(), "stale deployment") {
		t.Fatalf("expected stale race rejection, got %v", err)
	}
	if f.deployed() != before {
		t.Fatal("older deployment overwrote newer remote")
	}
}

func TestDeploymentReceiptRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "platform/projects/example"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "platform/projects/example/.deployments")); err != nil {
		t.Fatal(err)
	}
	if err := checkLocalDeploymentOrder(os.DirFS(root), deploymentReceiptPath("platform/projects/example", "ghcr.io/fredrir/example"), DeploymentOrder{}); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestFetchedOrderAllowsUnrelatedRemoteChanges(t *testing.T) {
	f := newDeployFixture(t, "kustomize", "public", false)
	f.git("fetch", "--quiet", "origin", "main")
	if err := checkFetchedDeploymentOrder(context.Background(), f.runner, "platform/projects/example/.deployments/example.json", DeploymentOrder{}); err != nil {
		t.Fatal(err)
	}
}
