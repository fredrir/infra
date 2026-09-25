package ci

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifiedDeploymentOrder(t *testing.T) {
	options := DeployOptions{Image: "ghcr.io/fredrir/example", Revision: strings.Repeat("b", 40), Digest: "sha256:" + strings.Repeat("a", 64)}
	cases := []struct {
		name, visibility, data string
		valid                  bool
	}{
		{"public", "public", `[{"verificationResult":{"signature":{"certificate":{"runInvocationURI":"https://github.com/fredrir/example/actions/runs/35525556507/attempts/2"}}}}]`, true},
		{"private", "private", `[{"optional":{"source-run-id":"35525556507","source-run-attempt":"2"}}]`, true},
		{"wrong-repository", "public", `[{"verificationResult":{"signature":{"certificate":{"runInvocationURI":"https://github.com/other/example/actions/runs/35525556507/attempts/2"}}}}]`, false},
		{"mutable-statement", "public", `[{"verificationResult":{"statement":{"predicate":{"runDetails":{"metadata":{"invocationId":"https://github.com/fredrir/example/actions/runs/35525556507/attempts/2"}}}}}}]`, false},
		{"unsigned-root-field", "public", `[{"run_id":35525556507}]`, false},
		{"legacy-private", "private", `[{"optional":{"source-revision":"abc"}}]`, false},
		{"numeric-annotation", "private", `[{"optional":{"source-run-id":35525556507,"source-run-attempt":"2"}}]`, false},
		{"overflow", "private", `[{"optional":{"source-run-id":"18446744073709551616","source-run-attempt":"2"}}]`, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			order, err := verifiedDeploymentOrder([]byte(test.data), test.visibility, "fredrir/example", options)
			if (err == nil) != test.valid {
				t.Fatalf("order=%+v err=%v", order, err)
			}
			if test.valid && (order.RunID != 35525556507 || order.Attempt != 2 || order.Image != options.Image) {
				t.Fatal(order)
			}
		})
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
