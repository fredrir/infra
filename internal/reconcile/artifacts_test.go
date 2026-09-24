package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

func artifactFixture(t *testing.T, body string) resource {
	t.Helper()
	var item resource
	if err := json.Unmarshal([]byte(body), &item); err != nil {
		t.Fatal(err)
	}
	return item
}

func TestWorkloadSnapshotBatchesReadsWithoutReusingStalePolls(t *testing.T) {
	calls := 0
	commands := &Commands{Runner: ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		calls++
		if strings.Join(options.Args, " ") != "get deployments.apps -n=llunde -o=json --request-timeout=30s" {
			t.Fatalf("unexpected resource scope: %v", options.Args)
		}
		return process.Result{Stdout: []byte(fmt.Sprintf(`{"items":[{"metadata":{"name":"web","namespace":"llunde","generation":%d}},{"metadata":{"name":"api","namespace":"llunde"}}]}`, calls))}, nil
	}}}
	snapshot := resourceSnapshot{}
	for _, name := range []string{"web", "api"} {
		if _, err := commands.snapshotResource(context.Background(), snapshot, "deployments.apps", "llunde", name); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("same namespace read %d times", calls)
	}
	if _, err := commands.snapshotResource(context.Background(), snapshot, "deployments.apps", "llunde", "missing"); err == nil {
		t.Fatal("missing workload accepted")
	}
	item, err := commands.snapshotResource(context.Background(), resourceSnapshot{}, "deployments.apps", "llunde", "web")
	if err != nil || item.Metadata.Generation != 2 || calls != 2 {
		t.Fatalf("new poll reused stale state: %+v, %v", item, err)
	}
}

func TestArtifactOriginRequiresCurrentSourceEvenWhenContentIsUnchanged(t *testing.T) {
	generator := artifactFixture(t, `{"spec":{"sources":[{"alias":"repo","kind":"GitRepository","name":"flux-system"}],"artifacts":[{"name":"project-y","originRevision":"@repo","copy":[{"from":"@repo/platform/projects/y/**","to":"@artifact/platform/projects/y/"},{"from":"@repo/platform/components/backup-job/**","to":"@artifact/platform/components/backup-job/"},{"from":"@repo/platform/clusters/production/settings.yaml","to":"@artifact/platform/clusters/production/settings.yaml"}]}]}}`)
	old := strings.Repeat("a", 40)
	target := strings.Repeat("b", 40)
	artifact := artifactFixture(t, fmt.Sprintf(`{"metadata":{"name":"project-y"},"status":{"artifact":{"metadata":{"org.opencontainers.image.revision":"production@sha1:%s"}}}}`, old))
	commands := &Commands{}
	if err := commands.verifyArtifactOrigin(context.Background(), generator, artifact, target); err == nil {
		t.Fatal("old origin accepted before generator observes source")
	}
	artifact.Status.Artifact.Metadata["org.opencontainers.image.revision"] = "production@sha1:" + target
	if err := commands.verifyArtifactOrigin(context.Background(), generator, artifact, target); err != nil {
		t.Fatal(err)
	}
	artifact.Status.Artifact.Metadata["org.opencontainers.image.revision"] = "latest"
	if err := commands.verifyArtifactOrigin(context.Background(), generator, artifact, target); err == nil {
		t.Fatal("unverifiable provenance accepted")
	}
}

func TestArtifactInputsRejectUnknownTransforms(t *testing.T) {
	for _, from := range []string{"@other/foo/**", "@repo/foo/*.yaml", "@repo/../foo/**"} {
		generator := artifactFixture(t, fmt.Sprintf(`{"spec":{"sources":[{"alias":"repo","kind":"GitRepository","name":"flux-system"}],"artifacts":[{"name":"project-y","originRevision":"@repo","copy":[{"from":%q,"to":"@artifact/foo/"}]}]}}`, from))
		if _, err := artifactPaths(generator, "project-y"); err == nil {
			t.Fatalf("unsupported input accepted: %s", from)
		}
	}
}

func TestArtifactConditionsRequireObservedGeneration(t *testing.T) {
	item := artifactFixture(t, `{"metadata":{"name":"project-y","generation":2},"status":{"conditions":[{"type":"Ready","status":"True","observedGeneration":1}]}}`)
	if err := conditionReady(item); err == nil {
		t.Fatal("stale Ready accepted")
	}
	item.Status.Conditions[0].ObservedGeneration = 2
	if err := conditionReady(item); err != nil {
		t.Fatal(err)
	}
	item.Spec.Suspend = true
	if err := conditionReady(item); err == nil {
		t.Fatal("suspended resource accepted")
	}
}

func TestVerifyArtifactsRejectsStaleAndUnownedContent(t *testing.T) {
	revision := strings.Repeat("a", 40)
	digest := "sha256:" + strings.Repeat("b", 64)
	generator := fmt.Sprintf(`{"metadata":{"name":"platform-artifacts","namespace":"flux-system","uid":"generator-id","generation":1},"spec":{"sources":[{"alias":"repo","kind":"GitRepository","name":"flux-system"}],"artifacts":[{"name":"project-y","originRevision":"@repo","copy":[{"from":"@repo/platform/projects/y/**","to":"@artifact/platform/projects/y/"}]}]},"status":{"conditions":[{"type":"Ready","status":"True","observedGeneration":1}],"inventory":[{"name":"project-y","namespace":"flux-system","digest":%q}]}}`, digest)
	artifact := fmt.Sprintf(`{"metadata":{"name":"project-y","namespace":"flux-system","generation":1,"labels":{"source.extensions.fluxcd.io/generator":"generator-id"}},"spec":{"sourceRef":{"kind":"ArtifactGenerator","name":"platform-artifacts","namespace":"flux-system"}},"status":{"conditions":[{"type":"Ready","status":"True","observedGeneration":1}],"artifact":{"digest":%q,"revision":%q,"metadata":{"org.opencontainers.image.revision":%q}}}}`, digest, "latest@"+digest, "production@sha1:"+revision)
	for _, test := range []struct {
		name      string
		mutate    func(*resource, *resource, *resource)
		wantError bool
		baseline  map[string]string
		projects  []string
	}{
		{name: "scoped selected changes", projects: []string{"llunde", "y"}, baseline: map[string]string{"project-y": "old"}, mutate: func(*resource, *resource, *resource) {}},
		{name: "scoped unselected artifact missing", projects: []string{"llunde", "y"}, baseline: map[string]string{"project-portfolio": digest}, wantError: true, mutate: func(*resource, *resource, *resource) {}},
		{name: "current", mutate: func(*resource, *resource, *resource) {}},
		{name: "stale consumer", wantError: true, mutate: func(_, _ *resource, owner *resource) { owner.Status.LastAppliedRevision = "latest@sha256:old" }},
		{name: "unowned artifact", wantError: true, mutate: func(_ *resource, artifact, _ *resource) {
			artifact.Metadata.Labels["source.extensions.fluxcd.io/generator"] = "other"
		}},
		{name: "wrong digest", wantError: true, mutate: func(_ *resource, artifact, _ *resource) {
			artifact.Status.Artifact.Digest = "sha256:" + strings.Repeat("c", 64)
		}},
		{name: "missing inventory", wantError: true, mutate: func(generator, _, _ *resource) { generator.Status.Inventory = json.RawMessage(`[]`) }},
		{name: "stale generator", wantError: true, mutate: func(generator, _, _ *resource) { generator.Metadata.Generation = 2 }},
		{name: "wrong provenance", wantError: true, mutate: func(_ *resource, artifact, _ *resource) {
			artifact.Status.Artifact.Metadata["org.opencontainers.image.revision"] = "unverified"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			gen := artifactFixture(t, generator)
			art := artifactFixture(t, artifact)
			owner := artifactFixture(t, fmt.Sprintf(`{"metadata":{"name":"project-y"},"spec":{"sourceRef":{"kind":"ExternalArtifact","name":"project-y"}},"status":{"lastAppliedRevision":%q}}`, "latest@"+digest))
			test.mutate(&gen, &art, &owner)
			commands := &Commands{kubernetes: &kubernetesState{generator: gen, baseline: test.baseline}, Runner: ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				var value any = gen
				if strings.Contains(strings.Join(options.Args, " "), "externalartifacts") {
					value = struct{ Items []resource }{[]resource{art}}
				}
				output, err := json.Marshal(value)
				return process.Result{Stdout: output}, err
			}}}
			err := commands.verifyArtifacts(context.Background(), Plan{Revision: revision, Affected: Selection{Projects: test.projects}}, []resource{owner})
			if (err != nil) != test.wantError {
				t.Fatalf("error %v, want error %v", err, test.wantError)
			}
		})
	}
}

func TestPinnedControllerArtifactEvidence(t *testing.T) {
	path := os.Getenv("INFRA_TEST_FLUX_EVIDENCE")
	if path == "" {
		t.Skip("requires isolated pinned-controller evidence")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		Passed    bool
		Snapshots map[string][]json.RawMessage
	}
	if err = json.Unmarshal(data, &evidence); err != nil {
		t.Fatal(err)
	}
	if !evidence.Passed {
		t.Fatal("controller qualification did not pass")
	}
	decode := func(name string, index int) resource {
		t.Helper()
		items := evidence.Snapshots[name]
		if len(items) <= index {
			t.Fatalf("missing %s snapshot %d", name, index)
		}
		return artifactFixture(t, string(items[index]))
	}
	initialArtifact := decode("initial-origin-and-owner", 0)
	initialGenerator := decode("initial-origin-and-owner", 1)
	owner := decode("initial-origin-and-owner", 2)
	unchangedArtifact := decode("unchanged-content-provenance", 0)
	unchangedGenerator := decode("unchanged-content-provenance", 1)
	if initialArtifact.Status.Artifact.Digest != unchangedArtifact.Status.Artifact.Digest {
		t.Fatal("fixture did not preserve unchanged content")
	}
	lagArtifact := decode("ready-owner-stale-artifact-during-generator-lag", 0)
	lagOwner := decode("ready-owner-stale-artifact-during-generator-lag", 1)
	lagSource := decode("ready-owner-stale-artifact-during-generator-lag", 2)
	for _, test := range []struct {
		name                       string
		artifact, generator, owner resource
		revision                   string
		wantError                  bool
	}{
		{"initial", initialArtifact, initialGenerator, owner, strings.Split(initialArtifact.Status.Artifact.Metadata["org.opencontainers.image.revision"], ":")[1], false},
		{"unchanged content with current provenance", unchangedArtifact, unchangedGenerator, owner, strings.Split(unchangedArtifact.Status.Artifact.Metadata["org.opencontainers.image.revision"], ":")[1], false},
		{"ready owner while generator lags", lagArtifact, unchangedGenerator, lagOwner, strings.Split(lagSource.Status.Artifact.Revision, ":")[1], true},
	} {
		t.Run(test.name, func(t *testing.T) {
			commands := &Commands{kubernetes: &kubernetesState{generator: test.generator}, Runner: ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				var value any = test.generator
				if strings.Contains(strings.Join(options.Args, " "), "externalartifacts") {
					value = struct{ Items []resource }{[]resource{test.artifact}}
				}
				data, err := json.Marshal(value)
				return process.Result{Stdout: data}, err
			}}}
			err := commands.verifyArtifacts(context.Background(), Plan{Revision: test.revision}, []resource{test.owner})
			if (err != nil) != test.wantError {
				t.Fatalf("error %v, want error %v", err, test.wantError)
			}
		})
	}
}
