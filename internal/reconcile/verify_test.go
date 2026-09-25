package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

func readyResource(t *testing.T, kind, namespace, name string) resource {
	t.Helper()
	return artifactFixture(t, fmt.Sprintf(`{"kind":%q,"metadata":{"name":%q,"namespace":%q,"generation":1},"status":{"observedGeneration":1,"conditions":[{"type":"Ready","status":"True","observedGeneration":1}]}}`, kind, name, namespace))
}

func notReady(item *resource) {
	item.Status.Conditions[0].Status = "False"
}

type kubernetesFake map[string]any

func (f kubernetesFake) execute(t *testing.T, options process.Options) (process.Result, error) {
	t.Helper()
	command := strings.Join(options.Args, " ")
	for prefix, value := range f {
		if strings.HasPrefix(command, prefix) {
			if err, ok := value.(error); ok {
				return process.Result{ExitCode: 1}, err
			}
			data, err := json.Marshal(value)
			return process.Result{Stdout: data}, err
		}
	}
	t.Errorf("unexpected kubectl %s", command)
	return process.Result{ExitCode: 1}, errors.New("unexpected command")
}

func items(resources ...resource) any { return struct{ Items []resource }{resources} }

func TestKubernetesDeclarationMismatchesAreDifferences(t *testing.T) {
	revision := strings.Repeat("a", 40)
	root := func(t *testing.T) resource {
		item := readyResource(t, "Kustomization", "flux-system", "flux-system")
		item.Spec.SourceRef.Kind, item.Spec.SourceRef.Name = "GitRepository", "flux-system"
		item.Status.LastAppliedRevision = "production@sha1:" + revision
		return item
	}
	monitoring := func(t *testing.T) resource {
		item := readyResource(t, "HelmRelease", "observability", "monitoring")
		item.Spec.Values.Grafana.INI.Server.RootURL = "https://logs.fredrir.com"
		return item
	}
	for _, test := range []struct {
		name        string
		mutate      func(kustomizations, releases *[]resource)
		differences []Difference
		errors      []string
	}{
		{name: "matching"},
		{name: "unapplied revision", mutate: func(kustomizations, _ *[]resource) {
			(*kustomizations)[0].Status.LastAppliedRevision = "production@sha1:old"
		}, differences: []Difference{{System: "kubernetes", Item: "Kustomization flux-system/flux-system has not applied " + revision}}},
		{name: "suspended root", mutate: func(kustomizations, _ *[]resource) { (*kustomizations)[0].Spec.Suspend = true }, errors: []string{"Kustomization flux-system/flux-system is suspended"}},
		{name: "suspended child is declared elsewhere", mutate: func(kustomizations, _ *[]resource) {
			child := readyResource(t, "Kustomization", "flux-system", "platform-projects")
			child.Spec.Suspend = true
			*kustomizations = append(*kustomizations, child)
		}},
		{name: "unready and unapplied", mutate: func(kustomizations, _ *[]resource) {
			child := readyResource(t, "Kustomization", "flux-system", "platform-cache")
			child.Spec.SourceRef.Kind, child.Spec.SourceRef.Name = "GitRepository", "flux-system"
			notReady(&child)
			*kustomizations = append(*kustomizations, child)
		}, differences: []Difference{{System: "kubernetes", Item: "Kustomization flux-system/platform-cache has not applied " + revision}}, errors: []string{"flux-system/platform-cache is not ready"}},
		{name: "Grafana root_url", mutate: func(_, releases *[]resource) {
			(*releases)[0].Spec.Values.Grafana.INI.Server.RootURL = "https://grafana.fredrir.com"
		}, differences: []Difference{{System: "kubernetes", Item: `HelmRelease observability/monitoring serves Grafana at "https://grafana.fredrir.com", want "https://logs.fredrir.com"`}}},
		{name: "suspended Grafana", mutate: func(_, releases *[]resource) { (*releases)[0].Spec.Suspend = true }, errors: []string{"HelmRelease observability/monitoring is suspended"}},
		{name: "missing Grafana", mutate: func(_, releases *[]resource) {
			*releases = []resource{readyResource(t, "HelmRelease", "cache", "valkey")}
		}, differences: []Difference{{System: "kubernetes", Item: "HelmRelease observability/monitoring is missing"}}},
		{name: "unready release", mutate: func(_, releases *[]resource) {
			release := readyResource(t, "HelmRelease", "cache", "valkey")
			notReady(&release)
			*releases = append(*releases, release)
		}, errors: []string{"cache/valkey is not ready"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			kustomizations, releases := []resource{root(t)}, []resource{monitoring(t)}
			if test.mutate != nil {
				test.mutate(&kustomizations, &releases)
			}
			fake := kubernetesFake{"get kustomizations.kustomize.toolkit.fluxcd.io": items(kustomizations...), "get helmreleases.helm.toolkit.fluxcd.io": items(releases...)}
			commands := Commands{Runner: ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				return fake.execute(t, options)
			}}}
			outcome := VerificationOutcome("", false, commands.verifyCluster(context.Background(), Plan{Revision: revision, Host: "logs.fredrir.com"}))
			if !reflect.DeepEqual(outcome.Differences, append([]Difference{}, test.differences...)) || !reflect.DeepEqual(outcome.Errors, append([]string{}, test.errors...)) {
				t.Fatalf("cluster verification reported %+v, want differences %+v and errors %q", outcome, test.differences, test.errors)
			}
		})
	}
}

func TestDeploymentVerificationClassifiesMismatches(t *testing.T) {
	revision := strings.Repeat("a", 40)
	digest := "sha256:" + strings.Repeat("b", 64)
	generator := artifactFixture(t, fmt.Sprintf(`{"kind":"ArtifactGenerator","metadata":{"name":"platform-artifacts","namespace":"flux-system","uid":"generator-id","generation":1},"spec":{"sources":[{"alias":"repo","kind":"GitRepository","name":"flux-system"}],"artifacts":[{"name":"project-y","originRevision":"@repo","copy":[{"from":"@repo/platform/projects/y/**","to":"@artifact/platform/projects/y/"}]}]},"status":{"conditions":[{"type":"Ready","status":"True","observedGeneration":1}],"inventory":[{"name":"project-y","namespace":"flux-system","digest":%q}]}}`, digest))
	artifact := artifactFixture(t, fmt.Sprintf(`{"kind":"ExternalArtifact","metadata":{"name":"project-y","namespace":"flux-system","generation":1,"labels":{"source.extensions.fluxcd.io/generator":"generator-id"}},"spec":{"sourceRef":{"kind":"ArtifactGenerator","name":"platform-artifacts","namespace":"flux-system"}},"status":{"conditions":[{"type":"Ready","status":"True","observedGeneration":1}],"artifact":{"digest":%q,"revision":%q,"metadata":{"org.opencontainers.image.revision":%q}}}}`, digest, "latest@"+digest, "production@sha1:"+revision))
	source := readyResource(t, "GitRepository", "flux-system", "flux-system")
	source.Spec.Ref.Branch, source.Status.Artifact.Revision = "production", "production@sha1:"+revision
	declared := map[string]resource{}
	for name, sourceRef := range map[string]string{"flux-system": "GitRepository", "project-y": "ExternalArtifact"} {
		owner := readyResource(t, "Kustomization", "flux-system", name)
		owner.Spec.Path = "./" + name
		owner.Spec.SourceRef.Kind, owner.Spec.SourceRef.Name = sourceRef, map[string]string{"GitRepository": "flux-system", "ExternalArtifact": "project-y"}[sourceRef]
		owner.Status.LastAppliedRevision = map[string]string{"GitRepository": "production@sha1:" + revision, "ExternalArtifact": "latest@" + digest}[sourceRef]
		owner.Status.Inventory = json.RawMessage(`{"entries":[{"id":"y_api_apps_Deployment"}]}`)
		declared[name] = owner
	}
	expected := artifactFixture(t, `{"kind":"Deployment","metadata":{"name":"api","namespace":"y"},"spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"api","image":"example@sha256:desired"}]}}}}`)
	deployment := artifactFixture(t, `{"kind":"Deployment","metadata":{"name":"api","namespace":"y","generation":2,"labels":{"kustomize.toolkit.fluxcd.io/name":"project-y","kustomize.toolkit.fluxcd.io/namespace":"flux-system"}},"spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"api","image":"example@sha256:desired"}]}}},"status":{"observedGeneration":2,"replicas":1,"updatedReplicas":1,"availableReplicas":1}}`)
	monitoring := readyResource(t, "HelmRelease", "observability", "monitoring")
	monitoring.Spec.Values.Grafana.INI.Server.RootURL = "https://logs.fredrir.com"
	type state struct {
		source, deployment, monitoring resource
		owners                         map[string]resource
	}
	image := Difference{System: "kubernetes", Item: "Deployment y/api does not run the declared container images"}
	for _, test := range []struct {
		name        string
		mutate      func(*state)
		differences []Difference
		errors      []string
	}{
		{name: "matching", mutate: func(*state) {}},
		{name: "source changed", mutate: func(s *state) { s.source.Status.Artifact.Revision = "production@sha1:old" }, differences: []Difference{{System: "kubernetes", Item: "GitRepository flux-system/flux-system is at production@sha1:old of branch production, want production@sha1:" + revision}}},
		{name: "suspended owner", mutate: func(s *state) {
			owner := s.owners["project-y"]
			owner.Spec.Suspend = true
			s.owners["project-y"] = owner
		}, errors: []string{"Kustomization flux-system/project-y is suspended"}},
		{name: "topology", mutate: func(s *state) {
			owner := s.owners["project-y"]
			owner.Spec.Path = "./elsewhere"
			s.owners["project-y"] = owner
		}, differences: []Difference{{System: "kubernetes", Item: "Kustomization flux-system/project-y source topology differs from its declaration"}}},
		{name: "unapplied revision", mutate: func(s *state) {
			owner := s.owners["flux-system"]
			owner.Status.LastAppliedRevision = "production@sha1:old"
			s.owners["flux-system"] = owner
		}, differences: []Difference{{System: "kubernetes", Item: "Kustomization flux-system/flux-system has not applied " + revision}}},
		{name: "image", mutate: func(s *state) { s.deployment.Spec.Template.Spec.Containers[0].Image = "example@sha256:old" }, differences: []Difference{image}},
		{name: "missing workload", mutate: func(s *state) { s.deployment.Metadata.Name = "renamed" }, differences: []Difference{{System: "kubernetes", Item: "deployments.apps y/api is missing"}}},
		{name: "rollout incomplete", mutate: func(s *state) { s.deployment.Status.AvailableReplicas = 0 }, errors: []string{"y/api deployment rollout is incomplete"}},
		{name: "scaled", mutate: func(s *state) {
			scaled := int64(3)
			s.deployment.Spec.Replicas = &scaled
		}, differences: []Difference{{System: "kubernetes", Item: "Deployment y/api runs 3 replicas, want 1"}}},
		{name: "image and unready Grafana", mutate: func(s *state) {
			s.deployment.Spec.Template.Spec.Containers[0].Image = "example@sha256:old"
			notReady(&s.monitoring)
		}, differences: []Difference{image}, errors: []string{"observability/monitoring is not ready"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := state{source: source, deployment: deployment, monitoring: monitoring, owners: map[string]resource{}}
			for name, owner := range declared {
				current.owners[name] = owner
			}
			current.deployment.Spec.Template.Spec.Containers = append([]struct{ Name, Image string }{}, deployment.Spec.Template.Spec.Containers...)
			current.monitoring.Status.Conditions = append(current.monitoring.Status.Conditions[:0:0], monitoring.Status.Conditions...)
			test.mutate(&current)
			fake := kubernetesFake{
				"get gitrepositories.source.toolkit.fluxcd.io flux-system":              current.source,
				"get kustomizations.kustomize.toolkit.fluxcd.io -n=flux-system":         items(current.owners["flux-system"], current.owners["project-y"]),
				"get artifactgenerators.source.extensions.fluxcd.io platform-artifacts": generator,
				"get externalartifacts.source.toolkit.fluxcd.io":                        items(artifact),
				"get deployments.apps -n=y":                                             items(current.deployment),
				"get helmreleases.helm.toolkit.fluxcd.io --all-namespaces":              items(current.monitoring),
			}
			var mu sync.Mutex
			commands := Commands{VerifyArtifacts: true, kubernetes: &kubernetesState{owners: declared, generator: generator, workloads: map[string][]resource{"flux-system": nil, "project-y": {expected}}}, Runner: ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				mu.Lock()
				defer mu.Unlock()
				return fake.execute(t, options)
			}}}
			outcome := VerificationOutcome("", false, commands.verifyCluster(context.Background(), Plan{Revision: revision, Affected: All(), Host: "logs.fredrir.com"}))
			if !reflect.DeepEqual(outcome.Differences, append([]Difference{}, test.differences...)) || !reflect.DeepEqual(outcome.Errors, append([]string{}, test.errors...)) {
				t.Fatalf("deployment verification reported %+v, want differences %+v and errors %q", outcome, test.differences, test.errors)
			}
		})
	}
}

func TestVerificationCollectsEveryPart(t *testing.T) {
	fleet := testRunnerFleet()
	revision := strings.Repeat("a", 40)
	root := readyResource(t, "Kustomization", "flux-system", "flux-system")
	root.Spec.SourceRef.Kind, root.Spec.SourceRef.Name = "GitRepository", "flux-system"
	root.Status.LastAppliedRevision = "production@sha1:old"
	unready := readyResource(t, "HelmRelease", "observability", "monitoring")
	notReady(&unready)
	drift := junitCase("[infra-build-09] Verify dedicated build runners: Reject declared runner state drift", "verify-runners.yml:92", `<failure message="The build VM differs from its declared runner state"/>`)
	for _, test := range []struct {
		name        string
		kubernetes  kubernetesFake
		live        bool
		differences []Difference
		errors      []string
	}{
		{name: "cluster mismatch", kubernetes: kubernetesFake{"get kustomizations.kustomize.toolkit.fluxcd.io": items(root), "get helmreleases.helm.toolkit.fluxcd.io": items(unready)}, differences: []Difference{
			{System: "kubernetes", Item: "Kustomization flux-system/flux-system has not applied " + revision},
			{System: "runners", Host: "infra-build-09", Item: "Verify dedicated build runners: Reject declared runner state drift"},
		}, errors: []string{"observability/monitoring is not ready"}},
		{name: "cluster unreachable", kubernetes: kubernetesFake{"get kustomizations.kustomize.toolkit.fluxcd.io": errors.New("kubectl failed: connection refused"), "get helmreleases.helm.toolkit.fluxcd.io": errors.New("kubectl failed: connection refused")}, differences: []Difference{
			{System: "runners", Host: "infra-build-09", Item: "Verify dedicated build runners: Reject declared runner state drift"},
		}, errors: []string{"kubectl failed: connection refused"}},
		{name: "desired state unrendered", kubernetes: kubernetesFake{"kustomize": errors.New("kubectl failed: kustomize build failed")}, live: true, differences: []Difference{
			{System: "runners", Host: "infra-build-09", Item: "Verify dedicated build runners: Reject declared runner state drift"},
		}, errors: []string{"kubectl failed: kustomize build failed"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			commands := Commands{Work: t.TempDir(), Runner: ci.Runner{Dir: writeRunnerFleet(t, fleet), Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				switch options.Name {
				case "kubectl":
					mu.Lock()
					defer mu.Unlock()
					return test.kubernetes.execute(t, options)
				case "ansible-playbook":
					return fakePlaybooks{reports: []string{junitReport("verify-runners", drift)}, exit: 2}.execute(t, options)
				case "gh":
					return runnerResponse(t, healthyRunner(fleet, queriedRepository(options))), nil
				}
				t.Errorf("unexpected command %s", options.Name)
				return process.Result{}, errors.New("unexpected command")
			}}}
			plan := Plan{Revision: revision, Affected: Selection{Kubernetes: true, Ansible: true, HostScope: HostScopeRunners, Projects: []string{"portfolio"}}}
			verify := commands.Verify
			if test.live {
				verify = commands.VerifyLive
			}
			outcome := VerificationOutcome("", false, verify(context.Background(), plan))
			if !reflect.DeepEqual(outcome.Differences, test.differences) || !reflect.DeepEqual(outcome.Errors, test.errors) {
				t.Fatalf("verification reported %+v, want differences %+v and errors %q", outcome, test.differences, test.errors)
			}
		})
	}
}
