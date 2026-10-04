package deployment

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/fredrir/infra/internal/process"
	"go.yaml.in/yaml/v3"
)

func TestProvenanceRequiresExactSourceAndWorkflow(t *testing.T) {
	workflow, revision := strings.Repeat("d", 40), strings.Repeat("b", 40)
	image := "ghcr.io/fredrir/example@sha256:" + strings.Repeat("a", 64)
	for _, visibility := range []string{"public", "private"} {
		name, args, err := ProvenanceCommand(visibility, "fredrir/example", workflow, revision, image)
		if err != nil {
			t.Fatal(err)
		}
		if visibility == "public" {
			if name != "gh" || !slices.Contains(args, workflow) || !slices.Contains(args, revision) || !slices.Contains(args, "oci://"+image) {
				t.Fatalf("wrong verification command: %s %v", name, args)
			}
		} else if name != "cosign" || !slices.Contains(args, "workflow-revision="+workflow) || !slices.Contains(args, "--certificate-identity-regexp") || !slices.Contains(args, "source-revision="+revision) || args[len(args)-1] != image {
			t.Fatalf("wrong verification command: %s %v", name, args)
		}
	}
	for _, visibility := range []string{"internal", ""} {
		if _, _, err := ProvenanceCommand(visibility, "fredrir/example", workflow, revision, image); err == nil {
			t.Fatal("unclassified image accepted")
		}
	}
}

func TestPinWorkloadPreservesScaleAndOtherWorkloads(t *testing.T) {
	input := []byte("kind: HelmRelease\nspec:\n  values:\n    workloads:\n      web:\n        image: old\n        replicas: 0\n      worker:\n        image: worker\n        replicas: 2\n")
	if _, err := pinWorkload(input, "missing", "image", "revision"); err == nil {
		t.Fatal("accepted missing workload")
	}
	data, err := pinWorkload(input, "web", "image", "revision")
	if err != nil {
		t.Fatal(err)
	}
	var resource struct {
		Spec struct {
			Values struct {
				Workloads map[string]struct {
					Image    string
					Replicas int
					Revision string `yaml:"sourceRevision"`
				}
			}
		}
	}
	if err := yaml.Unmarshal(data, &resource); err != nil {
		t.Fatal(err)
	}
	web := resource.Spec.Values.Workloads["web"]
	worker := resource.Spec.Values.Workloads["worker"]
	if web.Image != "image" || web.Revision != "revision" || web.Replicas != 0 || worker.Image != "worker" || worker.Replicas != 2 {
		t.Fatalf("unexpected workload mutation: %+v", resource)
	}
	if repeated, err := pinWorkload(data, "web", "image", "revision"); err != nil || string(repeated) != string(data) {
		t.Fatalf("repeated update changed output: %v", err)
	}
}

func TestPinImageUpdatesOnlyTheNamedPin(t *testing.T) {
	image, digest := "ghcr.io/fredrir/example", "sha256:"+strings.Repeat("a", 64)
	for _, test := range []struct {
		name, input, want string
	}{
		{name: "pinned", input: "resources:\n- application.yaml\nimages:\n- name: ghcr.io/fredrir/other\n  digest: sha256:old\n- name: ghcr.io/fredrir/example\n  newTag: latest\n", want: "resources:\n    - application.yaml\nimages:\n    - name: ghcr.io/fredrir/other\n      digest: sha256:old\n    - name: ghcr.io/fredrir/example\n      newName: ghcr.io/fredrir/example\n      digest: " + digest + "\n"},
		{name: "other image", input: "images:\n- name: ghcr.io/fredrir/other\n"},
		{name: "no images", input: "resources:\n- application.yaml\n"},
		{name: "empty", input: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			pinned, err := pinImage([]byte(test.input), image, digest)
			if err != nil || string(pinned) != test.want {
				t.Fatalf("pinned %q, %v; want %q", pinned, err, test.want)
			}
		})
	}
	if _, err := pinImage([]byte("images: ghcr.io/fredrir/example\n"), image, digest); err == nil {
		t.Fatal("accepted image pins that are not a list")
	}
}

func TestProvenanceChecksApprovedRevisionsConcurrentlyAndRetriesOnlyTriggerMismatches(t *testing.T) {
	var revisions []string
	for digit := 1; digit <= 6; digit++ {
		revisions = append(revisions, strings.Repeat(strconv.Itoa(digit), 40))
	}
	trust := "claim_pattern:\n  job_workflow_sha: '^(" + strings.Join(revisions, "|") + ")$'\n  event_name: '^(push|workflow_dispatch)$'\n"
	root := fstest.MapFS{".github/chainguard/deploy-1328252868.sts.yaml": {Data: []byte(trust)}}
	mapping := Mapping{Repository: "fredrir/example", Visibility: "private"}
	image, digest, revision := "ghcr.io/fredrir/example", "sha256:"+strings.Repeat("a", 64), strings.Repeat("b", 40)
	attested := `[{"optional":{"source-run-id":"7","source-run-attempt":"1","workflow-revision":"` + revisions[4] + `"}}]`
	var calls, retries atomic.Int32
	runner := process.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		trigger := options.Args[slices.Index(options.Args, "--certificate-github-workflow-trigger")+1]
		if trigger == "workflow_dispatch" {
			retries.Add(1)
		}
		if slices.Contains(options.Args, "workflow-revision="+revisions[4]) && trigger == "push" {
			return process.Result{Stdout: []byte(attested)}, nil
		}
		stderr := "Error: no matching attestations: missing or incorrect annotation\n"
		if slices.Contains(options.Args, "workflow-revision="+revisions[1]) && trigger == "push" {
			stderr = "Error: no matching attestations: failed to verify certificate identity: expected GithubWorkflowTrigger to be \"push\", got \"workflow_dispatch\"\n"
		}
		if options.Stderr != nil {
			_, _ = options.Stderr.Write([]byte(stderr))
		}
		return process.Result{Stderr: []byte(stderr), ExitCode: 1}, errors.New("exit status 1")
	}}
	started := time.Now()
	orders, err := VerifyProvenance(context.Background(), runner, root, "1328252868", mapping, image, digest, revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 || orders[0].RunID != 7 || orders[0].Attempt != 1 {
		t.Fatalf("attested orders: %+v", orders)
	}
	if got := calls.Load(); got != int32(len(revisions))+1 || retries.Load() != 1 {
		t.Fatalf("%d verification calls with %d trigger retries; one retry for the single trigger mismatch expected", got, retries.Load())
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("approved revisions verified sequentially: %s", elapsed)
	}
	runner.Execute = func(_ context.Context, options process.Options) (process.Result, error) {
		return process.Result{Stderr: []byte("Error: no matching attestations: missing or incorrect annotation\n"), ExitCode: 1}, errors.New("exit status 1")
	}
	if _, err := VerifyProvenance(context.Background(), runner, root, "1328252868", mapping, image, digest, revision); err == nil || !strings.Contains(err.Error(), "approved workflow revision") {
		t.Fatalf("unmatched provenance accepted: %v", err)
	}
}
