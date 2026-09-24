package reconcile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

func TestProjectDependencyChain(t *testing.T) {
	if got := projectOwners("llunde-pyparser"); len(got) != 3 || got[1] != "llunde-pyparser-migration" || got[2] != "llunde-pyparser-application" {
		t.Fatalf("incomplete migration chain: %v", got)
	}
	if got := projectOwners("unknown"); got != nil {
		t.Fatalf("unknown project selected: %v", got)
	}
}

func TestSelectedProjectsIncludeEveryMigrationDependency(t *testing.T) {
	commands := &Commands{}
	plan := Plan{Affected: Selection{Projects: []string{"llunde", "llunde-pyparser", "y"}}}
	want := []string{"project-llunde", "project-llunde-pyparser", "llunde-pyparser-migration", "llunde-pyparser-application", "project-y"}
	if got := commands.selectedOwners(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("selected dependencies = %v, want %v", got, want)
	}
	if supportedProjects([]string{"llunde", "unknown"}) {
		t.Fatal("mixed known and unknown projects accepted")
	}
}

func TestRenderActiveOverlayLocally(t *testing.T) {
	if os.Getenv("INFRA_TEST_FLUX_RENDER") != "true" {
		t.Skip("requires pinned kubectl and flux executables")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	commands := &Commands{Runner: ci.Runner{Dir: root}, Work: t.TempDir()}
	plan := Plan{Affected: All()}
	if err := commands.RenderKubernetes(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"project-llunde", "project-portfolio", "project-y", "project-llunde-pyparser", "llunde-pyparser-migration", "llunde-pyparser-application"} {
		if len(commands.kubernetes.workloads[name]) == 0 {
			t.Errorf("no expected workloads for %s", name)
		}
	}
}

func TestReadOnlyProductionWorkloadContracts(t *testing.T) {
	if os.Getenv("INFRA_TEST_LIVE_READS") != "true" {
		t.Skip("requires explicit read-only production validation")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	commands := &Commands{Runner: ci.Runner{Dir: root}, Work: t.TempDir()}
	plan := Plan{Affected: All()}
	if err = commands.RenderKubernetes(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	for _, name := range commands.selectedOwners(plan) {
		owner, err := commands.getResource(context.Background(), "kustomizations.kustomize.toolkit.fluxcd.io", "flux-system", name)
		if err != nil {
			t.Error(err)
			continue
		}
		if err = commands.verifyOwnedWorkloads(context.Background(), owner, resourceSnapshot{}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestPlanRendersOnlyCompleteSelectedProjectLocally(t *testing.T) {
	if os.Getenv("INFRA_TEST_FLUX_RENDER") != "true" {
		t.Skip("requires pinned kubectl and flux executables")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	commands := &Commands{Runner: ci.Runner{Dir: root}, Work: t.TempDir()}
	plan := Plan{Affected: Selection{Kubernetes: true, Projects: []string{"llunde-pyparser"}}}
	if err = commands.Plan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(commands.kubernetes.workloads) != 3 {
		t.Fatalf("unexpected rendered owners: %v", commands.kubernetes.workloads)
	}
	for _, name := range projectOwners("llunde-pyparser") {
		if len(commands.kubernetes.workloads[name]) == 0 {
			t.Errorf("missing parser workload %s", name)
		}
	}
}

func TestReadOnlyProductionScopedVerification(t *testing.T) {
	if os.Getenv("INFRA_TEST_LIVE_READS") != "true" {
		t.Skip("requires explicit read-only production validation")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, projects := range [][]string{{"portfolio"}, {"llunde"}, {"llunde-pyparser"}, {"llunde", "llunde-pyparser", "y"}} {
		t.Run(strings.Join(projects, "+"), func(t *testing.T) {
			commands := &Commands{Runner: ci.Runner{Dir: root}, Work: t.TempDir(), VerifyArtifacts: true, ScopeProjects: true}
			source, err := commands.getResource(context.Background(), "gitrepositories.source.toolkit.fluxcd.io", "flux-system", "flux-system")
			if err != nil {
				t.Fatal(err)
			}
			revision := strings.TrimPrefix(source.Status.Artifact.Revision, "production@sha1:")
			plan := Plan{Revision: revision, Affected: Selection{Kubernetes: true, Projects: projects}}
			plan, err = commands.Preflight(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(plan.Affected.Projects, projects) {
				t.Fatalf("project unexpectedly widened: %v", plan.Affected)
			}
			if err = commands.RenderKubernetes(context.Background(), plan); err != nil {
				t.Fatal(err)
			}
			if err = commands.Verify(context.Background(), plan); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPreflightFallsBackBeforeWritesForUnknownProject(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	commands := &Commands{VerifyArtifacts: true, ScopeProjects: true, kubernetes: &kubernetesState{}, Runner: ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		command := options.Name + " " + strings.Join(options.Args, " ")
		mu.Lock()
		calls = append(calls, command)
		mu.Unlock()
		if strings.Contains(command, "auth can-i") {
			return process.Result{Stdout: []byte("yes\n")}, nil
		}
		return process.Result{Stdout: []byte(`{"items":[]}`)}, nil
	}}}
	plan, err := commands.Preflight(context.Background(), Plan{Affected: Selection{Kubernetes: true, Projects: []string{"unexpected-project"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Affected.Projects) != 0 || !plan.Affected.Kubernetes {
		t.Fatalf("unknown project did not select full Kubernetes: %v", plan.Affected)
	}
	for _, call := range calls {
		if strings.Contains(call, "annotate") || strings.Contains(call, "push") {
			t.Fatalf("preflight mutated production: %s", call)
		}
	}
}

func TestPreflightRejectsMissingArtifactPermission(t *testing.T) {
	commands := &Commands{VerifyArtifacts: true, kubernetes: &kubernetesState{}, Runner: ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		return process.Result{}, fmt.Errorf("forbidden: externalartifacts")
	}}}
	if _, err := commands.Preflight(context.Background(), Plan{Affected: All()}); err == nil {
		t.Fatal("missing permissions were accepted")
	}
}
