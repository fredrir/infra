package ci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/process"
	"go.yaml.in/yaml/v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

type deployFixture struct {
	t                     *testing.T
	root, remote, project string
	runner                Runner
	options               DeployOptions
	calls                 []process.Options
	verify                func(process.Options) error
}

func newDeployFixture(t *testing.T, mode, visibility string, nested bool) *deployFixture {
	t.Helper()
	area := t.TempDir()
	f := &deployFixture{t: t, root: filepath.Join(area, "infra"), remote: filepath.Join(area, "origin.git")}
	f.project = filepath.Join(f.root, "platform/projects/example")
	if err := os.MkdirAll(f.project, 0700); err != nil {
		t.Fatal(err)
	}
	f.runner = Runner{Dir: f.root, Env: []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}, Stdout: io.Discard, Stderr: io.Discard}
	f.git("init", "--quiet", "--bare", "--initial-branch=main", f.remote)
	f.git("init", "--quiet", "--initial-branch=main")
	f.git("config", "user.name", "Test")
	f.git("config", "user.email", "test@example.invalid")
	f.git("remote", "add", "origin", "https://github.com/fredrir/infra")
	f.git("config", "url."+f.remote+".insteadOf", "https://github.com/fredrir/infra")
	f.options = DeployOptions{Root: f.root, RepositoryID: "123", Revision: strings.Repeat("b", 40), Image: "ghcr.io/fredrir/example", Digest: "sha256:" + strings.Repeat("a", 64), Token: "deployment-secret"}
	f.write(".github/chainguard/deploy-123.sts.yaml", "claim_pattern:\n  job_workflow_sha: '^"+strings.Repeat("d", 40)+"$'\n")
	f.writeMapping(mode, visibility, "platform/projects/example")
	if mode == "helmrelease" {
		f.write("platform/projects/example/release.yaml", "apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: example\nspec:\n  values:\n    workloads:\n      web:\n        image: old\n        replicas: 0\n      worker:\n        image: untouched\n        replicas: 2\n")
		f.kustomization("", []string{"release.yaml"}, false)
	} else if nested {
		for _, stage := range []string{"migration", "application"} {
			f.application(stage+"/application.yaml", stage)
			f.kustomization(stage, []string{"application.yaml"}, true)
		}
		f.write("platform/projects/example/namespace.yaml", "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: example\n")
		f.kustomization("", []string{"namespace.yaml"}, false)
	} else {
		f.application("application.yaml", "example")
		f.kustomization("", []string{"application.yaml"}, true)
	}
	f.commit("Initial deployment")
	f.git("push", "--quiet", "origin", "HEAD:main")
	f.runner.Execute = func(ctx context.Context, p process.Options) (process.Result, error) {
		if p.Name == "gh" || p.Name == "cosign" {
			f.calls = append(f.calls, p)
			if f.git("status", "--porcelain") != "" {
				t.Error("mutation occurred before provenance verification")
			}
			if strings.Contains(strings.Join(p.Args, " "), f.options.Token) {
				t.Error("deployment token exposed in arguments")
			}
			if f.verify != nil {
				if err := f.verify(p); err != nil {
					return process.Result{}, err
				}
			}
			data := `[ {"optional":{"source-run-id":"100","source-run-attempt":"1"}} ]`
			if p.Name == "gh" {
				data = `[ {"verificationResult":{"signature":{"certificate":{"runInvocationURI":"https://github.com/fredrir/example/actions/runs/100/attempts/1"}}}} ]`
			}
			return process.Result{Stdout: []byte(data)}, nil
		}
		return process.Run(ctx, p)
	}
	return f
}
func (f *deployFixture) git(args ...string) string {
	f.t.Helper()
	r := f.runner
	r.Execute = nil
	b, err := r.Output(context.Background(), "git", args...)
	if err != nil {
		f.t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(b))
}
func (f *deployFixture) write(path, text string) {
	f.t.Helper()
	path = filepath.Join(f.root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		f.t.Fatal(err)
	}
}
func (f *deployFixture) commit(message string) {
	f.git("add", "-A")
	f.git("commit", "--quiet", "-m", message)
}
func (f *deployFixture) deployed() string {
	return f.git("--git-dir="+f.remote, "rev-parse", "refs/heads/main")
}
func (f *deployFixture) run() error { return Deploy(context.Background(), f.runner, f.options) }
func (f *deployFixture) writeMapping(mode, visibility, path string) {
	f.write(".github/deployments/123.yaml", fmt.Sprintf("repository: fredrir/example\nvisibility: %s\nimages:\n  ghcr.io/fredrir/example:\n    path: %s\n    mode: %s\n    workload: web\n", visibility, path, mode))
}
func (f *deployFixture) application(path, name string) {
	f.write("platform/projects/example/"+path, fmt.Sprintf("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: %s\nspec:\n  replicas: 1\n  template:\n    spec:\n      containers:\n      - name: web\n        image: ghcr.io/fredrir/example:latest\n", name))
}
func (f *deployFixture) kustomization(directory string, resources []string, pinned bool) {
	f.t.Helper()
	doc := map[string]any{"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "resources": resources}
	if pinned {
		doc["images"] = []map[string]string{{"name": f.options.Image, "newName": f.options.Image, "newTag": "old", "digest": "sha256:" + strings.Repeat("c", 64)}}
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(filepath.Join("platform/projects/example", directory, "kustomization.yaml"), string(b))
}
func (f *deployFixture) rendered(directory string) string {
	f.t.Helper()
	r, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(filesys.MakeFsOnDisk(), filepath.Join(f.project, directory))
	if err != nil {
		f.t.Fatal(err)
	}
	b, err := r.AsYaml()
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func TestDeployVerifiesExactProvenanceAndPushesOnce(t *testing.T) {
	for _, visibility := range []string{"public", "private"} {
		t.Run(visibility, func(t *testing.T) {
			f := newDeployFixture(t, "kustomize", visibility, false)
			before := f.deployed()
			if err := f.run(); err != nil {
				t.Fatal(err)
			}
			if f.git("rev-parse", "HEAD") != f.deployed() || before == f.deployed() {
				t.Fatal("deployment not published")
			}
			if f.git("diff", "--name-only", "HEAD^") != "platform/projects/example/.deployments/example.json\nplatform/projects/example/kustomization.yaml" {
				t.Fatal("unexpected changed paths")
			}
			text := f.rendered("")
			if !strings.Contains(text, f.options.Image+"@"+f.options.Digest) || !strings.Contains(text, "replicas: 1") {
				t.Fatal(text)
			}
			if len(f.calls) != 1 {
				t.Fatal(f.calls)
			}
			command := f.calls[0]
			var args []string
			if visibility == "public" {
				args = []string{"attestation", "verify", "oci://" + f.options.Image + "@" + f.options.Digest, "--repo", "fredrir/example", "--signer-workflow", "fredrir/infra/.github/workflows/build-image.yml", "--signer-digest", strings.Repeat("d", 40), "--source-ref", "refs/heads/main", "--source-digest", f.options.Revision, "--format", "json"}
				if command.Name != "gh" {
					t.Fatal(command.Name)
				}
			} else {
				args = []string{"verify", "--certificate-oidc-issuer", "https://token.actions.githubusercontent.com", "--certificate-identity", "https://github.com/fredrir/infra/.github/workflows/build-image.yml@" + strings.Repeat("d", 40), "--certificate-github-workflow-repository", "fredrir/example", "--certificate-github-workflow-sha", f.options.Revision, "--certificate-github-workflow-ref", "refs/heads/main", "--certificate-github-workflow-trigger", "push", "--annotations", "source-repository=fredrir/example", "--annotations", "source-revision=" + f.options.Revision, "--annotations", "workflow-revision=" + strings.Repeat("d", 40), f.options.Image + "@" + f.options.Digest}
				if command.Name != "cosign" {
					t.Fatal(command.Name)
				}
			}
			if !reflect.DeepEqual(command.Args, args) {
				t.Fatalf("provenance flags differ:\n%v\n%v", command.Args, args)
			}
			deployed := f.deployed()
			if err := f.run(); err != nil {
				t.Fatal(err)
			}
			if f.deployed() != deployed {
				t.Fatal("no-op deployment made a commit")
			}
		})
	}
}

func TestDeployProvenanceFailuresNeverMutate(t *testing.T) {
	for _, visibility := range []string{"public", "private", "internal"} {
		t.Run(visibility, func(t *testing.T) {
			f := newDeployFixture(t, "kustomize", visibility, false)
			before := f.deployed()
			f.verify = func(process.Options) error { return errors.New("verification failed") }
			if err := f.run(); err == nil {
				t.Fatal("unverified deployment accepted")
			}
			if f.deployed() != before || f.git("status", "--porcelain") != "" {
				t.Fatal("unverified deployment mutated repository")
			}
		})
	}
}

func TestDeployRebasesOntoMovedMain(t *testing.T) {
	f := newDeployFixture(t, "kustomize", "public", false)
	other := filepath.Join(t.TempDir(), "other")
	f.git("clone", "--quiet", "--branch", "main", f.remote, other)
	if err := os.WriteFile(filepath.Join(other, "README"), []byte("moved\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README"}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "Move main"}, {"push", "--quiet", "origin", "HEAD:main"}} {
		f.git(append([]string{"-C", other}, args...)...)
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if f.git("rev-parse", "HEAD") != f.deployed() || f.git("log", "--format=%s", "-2") != "Deploy example "+f.options.Revision[:12]+"\nMove main" {
		t.Fatal("deployment did not preserve moved main")
	}
}

func TestDeployUpdatesEveryNestedPin(t *testing.T) {
	f := newDeployFixture(t, "kustomize", "public", true)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"application", "migration"} {
		if !strings.Contains(f.rendered(stage), f.options.Image+"@"+f.options.Digest) {
			t.Fatal("nested pin unchanged", stage)
		}
	}
	if f.git("diff", "--name-only", "HEAD^") != "platform/projects/example/.deployments/example.json\nplatform/projects/example/application/kustomization.yaml\nplatform/projects/example/migration/kustomization.yaml" {
		t.Fatal("unexpected nested mutations")
	}
}

func TestDeployHelmReleasePreservesZeroScale(t *testing.T) {
	f := newDeployFixture(t, "helmrelease", "public", false)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	var resource struct {
		Spec struct {
			Values struct {
				Workloads map[string]struct {
					Image          string
					Replicas       int
					SourceRevision string `yaml:"sourceRevision"`
				}
			}
		}
	}
	if err := readYAML(filepath.Join(f.project, "release.yaml"), &resource); err != nil {
		t.Fatal(err)
	}
	web, worker := resource.Spec.Values.Workloads["web"], resource.Spec.Values.Workloads["worker"]
	if web.Replicas != 0 || web.Image != f.options.Image+"@"+f.options.Digest || web.SourceRevision != f.options.Revision || worker.Image != "untouched" || worker.Replicas != 2 {
		t.Fatalf("unsafe workload update: %+v", resource)
	}
}

func TestDeployRejectsInvalidMappingsAndPinsWithoutMutation(t *testing.T) {
	for _, scenario := range []string{"no-pin", "unknown-image", "traversal", "project-symlink", "helm-file-symlink", "invalid-workflow"} {
		t.Run(scenario, func(t *testing.T) {
			mode := "kustomize"
			if scenario == "helm-file-symlink" {
				mode = "helmrelease"
			}
			f := newDeployFixture(t, mode, "public", false)
			outside := filepath.Join(t.TempDir(), "outside.yaml")
			switch scenario {
			case "no-pin":
				f.kustomization("", []string{"application.yaml"}, false)
				f.commit("Remove pin")
			case "unknown-image":
				f.options.Image = "ghcr.io/fredrir/other"
			case "traversal":
				f.writeMapping(mode, "public", "platform/projects/example/../../../.github")
				f.commit("Invalid mapping")
			case "project-symlink":
				moved := f.project + "-original"
				if err := os.Rename(f.project, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, f.project); err != nil {
					t.Fatal(err)
				}
				f.commit("Symlink project")
			case "helm-file-symlink":
				path := filepath.Join(f.project, "release.yaml")
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(outside, b, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
				f.commit("Symlink release")
			case "invalid-workflow":
				f.write(".github/chainguard/deploy-123.sts.yaml", "claim_pattern:\n  job_workflow_sha: '^.*$'\n")
				f.commit("Invalid trust")
			}
			before := f.deployed()
			external, _ := os.ReadFile(outside)
			if err := f.run(); err == nil {
				t.Fatal("accepted invalid deployment", scenario)
			}
			if f.deployed() != before || f.git("status", "--porcelain") != "" {
				t.Fatal("invalid deployment changed repository")
			}
			after, _ := os.ReadFile(outside)
			if string(after) != string(external) {
				t.Fatal("symlink target changed")
			}
		})
	}
}

func TestDeployBoundedWorkflowOverlapVerifiesBeforeMutation(t *testing.T) {
	old, newRevision := strings.Repeat("d", 40), strings.Repeat("e", 40)
	for _, visibility := range []string{"public", "private"} {
		for _, succeed := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/%t", visibility, succeed), func(t *testing.T) {
				f := newDeployFixture(t, "kustomize", visibility, false)
				f.write(".github/chainguard/deploy-123.sts.yaml", "claim_pattern:\n  job_workflow_sha: '^("+old+"|"+newRevision+")$'\n")
				f.commit("Bounded trust overlap")
				f.verify = func(p process.Options) error {
					if succeed && strings.Contains(strings.Join(p.Args, " "), newRevision) {
						return nil
					}
					return errors.New("wrong signer")
				}
				before := f.deployed()
				err := f.run()
				if (err == nil) != succeed {
					t.Fatalf("unexpected verification outcome: %v", err)
				}
				if len(f.calls) != 2 {
					t.Fatalf("expected two exact candidate checks, got%d", len(f.calls))
				}
				for i, want := range []string{old, newRevision} {
					if !strings.Contains(strings.Join(f.calls[i].Args, " "), want) || strings.Contains(strings.Join(f.calls[i].Args, " "), "|") {
						t.Fatal("candidate not exact", f.calls[i].Args)
					}
				}
				if !succeed && (f.deployed() != before || f.git("status", "--porcelain") != "") {
					t.Fatal("failed overlap verification mutated")
				}
			})
		}
	}
}

func TestWorkflowTrustAcceptsOnlyAnchoredExactCandidates(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, valid := range []string{"^" + a + "$", "^(" + a + "|" + b + ")$"} {
		if _, err := WorkflowRevisions(valid); err != nil {
			t.Fatal(err)
		}
	}
	for _, invalid := range []string{a, "^" + a, "" + a + "$", "^.*$", "^(" + a + "|" + b + "|" + strings.Repeat("c", 40) + ")$", "^(" + a + "|" + a + ")$", "^" + a + "|" + b + "$", "^(?:" + a + "|" + b + ")$", "^" + strings.Repeat("A", 40) + "$", "^" + a + "$\n"} {
		if _, err := WorkflowRevisions(invalid); err == nil {
			t.Fatal("unconstrained trust accepted", invalid)
		}
	}
}
