package ci

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/process"
)

func writeDeclarations(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		filename := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func changedFiles(t *testing.T, changed string) func(context.Context, process.Options) (process.Result, error) {
	return func(_ context.Context, options process.Options) (process.Result, error) {
		if options.Name != "git" || !reflect.DeepEqual(options.Args, []string{"ls-files"}) {
			t.Errorf("unexpected validation command: %s %v", options.Name, options.Args)
		}
		return process.Result{Stdout: []byte(changed + "\n")}, nil
	}
}

func TestKustomizationsCoverAggregatesAndChildren(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{"platform/components/policy.yaml": "", "platform/projects/settings.yaml": ""}
	for _, directory := range []string{
		"platform/clusters/production",
		"platform/components/common",
		"platform/projects",
		"platform/projects/example",
		"platform/projects/llunde-pyparser/migration",
		"platform/projects/llunde-pyparser/application",
		"build/rollout/flux-artifacts/cutover",
	} {
		files[directory+"/kustomization.yaml"] = "resources: []\n"
	}
	writeDeclarations(t, root, files)
	for _, directory := range []string{"platform/components/policy", "platform/projects/llunde", "platform/projects/y"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0755); err != nil {
			t.Fatal(err)
		}
	}
	directories, err := kustomizations(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"platform/clusters/production", "platform/projects", "build/rollout/flux-artifacts/cutover", "platform/components/common", "platform/projects/example", "platform/projects/llunde-pyparser/application", "platform/projects/llunde-pyparser/migration"}
	if !reflect.DeepEqual(directories, want) {
		t.Fatalf("kustomizations %v, want %v", directories, want)
	}
}

func TestValidateRendersEveryChangedKustomizationInProcess(t *testing.T) {
	root := t.TempDir()
	configMap := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\n"
	writeDeclarations(t, root, map[string]string{
		"platform/components/kustomization.yaml":         "resources:\n- common\n",
		"platform/components/common/kustomization.yaml":  "resources:\n- settings.yaml\n",
		"platform/components/common/settings.yaml":       configMap,
		"platform/components/runners/kustomization.yaml": "resources:\n- settings.yaml\n",
		"platform/components/runners/settings.yaml":      configMap,
	})
	runner := Runner{Dir: root, Execute: changedFiles(t, "platform/components/common/settings.yaml")}
	if err := Validate(context.Background(), runner, ""); err != nil {
		t.Fatal(err)
	}
	writeDeclarations(t, root, map[string]string{
		"platform/components/common/kustomization.yaml":  "resources:\n- missing.yaml\n",
		"platform/components/runners/kustomization.yaml": "resources:\n- ../../outside.yaml\n",
	})
	err := Validate(context.Background(), runner, "")
	if err == nil {
		t.Fatal("broken kustomizations accepted")
	}
	failed := func(directory string) int {
		return strings.Index(err.Error(), "kustomize "+filepath.Join(root, directory)+":")
	}
	aggregate, common, runners := failed("platform/components"), failed("platform/components/common"), failed("platform/components/runners")
	if aggregate < 0 || common <= aggregate || runners <= common {
		t.Fatalf("rendering failures missing or out of directory order: %v", err)
	}
}

func TestDeclarationChecksReportOutputAndErrorsInDeclarationOrder(t *testing.T) {
	var output bytes.Buffer
	runner := Runner{Stdout: &output, Stderr: &output}
	laterFinished := make(chan struct{})
	first, second := errors.New("first failure"), errors.New("second failure")
	checks := []declarationCheck{
		func(_ context.Context, runner Runner) error {
			select {
			case <-laterFinished:
			case <-time.After(5 * time.Second):
			}
			runner.Stdout.Write([]byte("first stdout\n"))
			runner.Stderr.Write([]byte("first stderr\n"))
			runner.Stdout.Write([]byte("first stdout again\n"))
			return first
		},
		func(_ context.Context, runner Runner) error {
			defer close(laterFinished)
			runner.Stderr.Write([]byte("second stderr\n"))
			return second
		},
		func(context.Context, Runner) error { return nil },
	}
	err := runChecks(context.Background(), runner, checks)
	if !errors.Is(err, first) || !errors.Is(err, second) || err.Error() != "first failure\nsecond failure" {
		t.Fatalf("errors not reported in declaration order: %v", err)
	}
	if output.String() != "first stdout\nfirst stderr\nfirst stdout again\nsecond stderr\n" {
		t.Fatalf("output not reported in declaration order:\n%s", output.String())
	}
}

type streamRecorder struct {
	lock    sync.Mutex
	writes  []string
	written chan struct{}
}

func (r *streamRecorder) Write(data []byte) (int, error) {
	r.lock.Lock()
	r.writes = append(r.writes, string(data))
	r.lock.Unlock()
	select {
	case r.written <- struct{}{}:
	default:
	}
	return len(data), nil
}

func TestDeclarationChecksStreamFinishedOutputWhileLaterChecksRun(t *testing.T) {
	recorder := &streamRecorder{written: make(chan struct{}, 1)}
	failure := errors.New("first failure")
	checks := []declarationCheck{
		func(_ context.Context, runner Runner) error {
			runner.Stderr.Write([]byte("first stderr\n"))
			return failure
		},
		func(context.Context, Runner) error {
			select {
			case <-recorder.written:
				return nil
			case <-time.After(5 * time.Second):
				return errors.New("finished check output held back")
			}
		},
	}
	if err := runChecks(context.Background(), Runner{Stdout: recorder, Stderr: recorder}, checks); err == nil || err.Error() != failure.Error() {
		t.Fatalf("unexpected result: %v", err)
	}
	if !reflect.DeepEqual(recorder.writes, []string{"first stderr\n"}) {
		t.Fatalf("streamed output: %q", recorder.writes)
	}
}

func TestDeclarationCheckPanicBecomesItsErrorWhileSiblingsFinish(t *testing.T) {
	var output bytes.Buffer
	checks := []declarationCheck{
		func(context.Context, Runner) error { panic("broken check") },
		func(_ context.Context, runner Runner) error {
			runner.Stdout.Write([]byte("sibling finished\n"))
			return nil
		},
	}
	err := runChecks(context.Background(), Runner{Stdout: &output, Stderr: &output}, checks)
	if err == nil || !strings.HasPrefix(err.Error(), "declaration check panicked: broken check\n") || !strings.Contains(err.Error(), "runCheck") {
		t.Fatalf("panic not reported with its stack: %v", err)
	}
	if output.String() != "sibling finished\n" {
		t.Fatalf("sibling output lost: %q", output.String())
	}
}

func TestTofuPreparationIsSeparateFromDeclarationChecks(t *testing.T) {
	var lock sync.Mutex
	var calls [][]string
	changed := "tofu/main.tf\n"
	failure := errors.New("invalid tofu declaration")
	var validationFailure error
	runner := Runner{Dir: t.TempDir(), Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		switch options.Name {
		case "git":
			return process.Result{Stdout: []byte(changed)}, nil
		case "tofu":
			lock.Lock()
			defer lock.Unlock()
			calls = append(calls, options.Args)
			if options.Args[1] == "validate" {
				return process.Result{}, validationFailure
			}
			return process.Result{}, nil
		default:
			t.Errorf("unexpected validator: %s", options.Name)
			return process.Result{}, nil
		}
	}}
	if err := PrepareValidation(context.Background(), runner, ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, [][]string{{"-chdir=tofu", "init", "-backend=false", "-lockfile=readonly", "-input=false"}}) {
		t.Fatalf("preparation commands: %v", calls)
	}
	calls = nil
	if err := Validate(context.Background(), runner, ""); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(calls, func(a, b []string) int { return strings.Compare(strings.Join(a, " "), strings.Join(b, " ")) })
	want := [][]string{{"-chdir=tofu", "fmt", "-check", "-recursive"}, {"-chdir=tofu", "validate", "-no-tests"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("validation commands: %v, want %v", calls, want)
	}
	validationFailure = failure
	if err := Validate(context.Background(), runner, ""); !errors.Is(err, failure) {
		t.Fatalf("validation failure was lost: %v", err)
	}
	changed, calls = "docs/platform.md\n", nil
	if err := PrepareValidation(context.Background(), runner, ""); err != nil || len(calls) != 0 {
		t.Fatalf("unaffected declarations prepared: %v %v", calls, err)
	}
}

func TestGeneratedOverlaysValidateOnEveryGeneratorInput(t *testing.T) {
	for _, path := range []string{
		"platform/components/policy/reconciliation.yaml",
		"platform/components/backup-job/job.yaml",
		"platform/projects/portfolio/deployment.yaml",
		"platform/clusters/production/root.yaml",
		"platform/clusters/production/settings.yaml",
		"build/rollout/flux-artifacts/cutover/kustomization.yaml",
		"build/rollout/flux-artifacts/generate/main.go",
		"internal/fluxartifacts/generate.go",
		"internal/ci/validate.go",
	} {
		t.Run(path, func(t *testing.T) {
			runner := Runner{Dir: t.TempDir(), Execute: changedFiles(t, path)}
			if err := Validate(context.Background(), runner, ""); err == nil {
				t.Fatal("missing generator inputs accepted")
			}
		})
	}
}
