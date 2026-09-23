package dev

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fluxRunner(t *testing.T, root string, calls *[]process.Options) ci.Runner {
	t.Helper()
	return ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		*calls = append(*calls, options)
		if options.Name != "flux" || options.Dir != root {
			t.Errorf("unexpected command %s in %s", options.Name, options.Dir)
		}
		switch options.Args[0] {
		case "build":
			fmt.Fprint(options.Stdout, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: ${STORAGE_CLASS}-ns\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: dashboard\ndata:\n  query: ${__value}\n---\n")
		case "envsubst":
			data, err := io.ReadAll(options.Stdin)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			for _, entry := range options.Env {
				key, value, _ := strings.Cut(entry, "=")
				text = strings.ReplaceAll(text, "${"+key+"}", value)
			}
			fmt.Fprint(options.Stdout, text)
		default:
			t.Errorf("unexpected flux command %v", options.Args)
		}
		return process.Result{}, nil
	}}
}

func TestRenderSubstitutesSettingsAndReportsLeftovers(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, settingsFile), "apiVersion: v1\nkind: ConfigMap\ndata:\n  STORAGE_CLASS: local-path\n  GRAFANA_HOST: grafana.example.test\n")
	var calls []process.Options
	runner := fluxRunner(t, root, &calls)
	var stdout strings.Builder
	report, err := Render(context.Background(), RenderOptions{State: NewState(root), Runner: runner, Output: "-", Stdout: &stdout})
	if err != nil {
		t.Fatal(err)
	}
	expected := RenderReport{Output: filepath.Join(root, ".cache/dev/render/platform.yaml"), Documents: 2, Unsubstituted: []string{"__value"}}
	if !reflect.DeepEqual(report, expected) {
		t.Fatalf("report %+v, expected %+v", report, expected)
	}
	if !strings.Contains(stdout.String(), "name: local-path-ns") || strings.Contains(stdout.String(), "${STORAGE_CLASS}") {
		t.Fatalf("settings not substituted into standard output:\n%s", stdout.String())
	}
	if len(calls) != 2 {
		t.Fatalf("expected build and envsubst, got %d commands", len(calls))
	}
	build := []string{"build", "kustomization", "flux-system", "--path=./platform/clusters/production", "--kustomization-file=platform/clusters/production/flux-system/gotk-sync.yaml", "--recursive", "--local-sources=GitRepository/flux-system/flux-system=.", "--dry-run"}
	if !reflect.DeepEqual(calls[0].Args, build) {
		t.Fatalf("unexpected build arguments %v", calls[0].Args)
	}
	for _, entry := range []string{"STORAGE_CLASS=local-path", "GRAFANA_HOST=grafana.example.test"} {
		if !slices.Contains(calls[1].Env, entry) {
			t.Errorf("substitution environment lacks %s", entry)
		}
	}
	if data, err := os.ReadFile(report.Output); err != nil || string(data) != stdout.String() {
		t.Fatalf("rendered file differs from standard output: %v", err)
	}
}

func TestRenderTargetsOneProject(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, settingsFile), "data:\n  STORAGE_CLASS: local-path\n")
	var calls []process.Options
	runner := fluxRunner(t, root, &calls)
	report, err := Render(context.Background(), RenderOptions{State: NewState(root), Runner: runner, Project: "llunde"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Output != filepath.Join(root, ".cache/dev/render/project-llunde.yaml") || report.Documents != 2 {
		t.Fatalf("unexpected report %+v", report)
	}
	output := filepath.Join(root, "llunde.yaml")
	if report, err := Render(context.Background(), RenderOptions{State: NewState(root), Runner: runner, Project: "llunde", Output: output}); err != nil || report.Output != output {
		t.Fatalf("explicit output not honored: %+v, %v", report, err)
	}
	for _, argument := range []string{"platform-projects", "--path=./platform/projects/llunde", "--kustomization-file=platform/clusters/production/root.yaml"} {
		if !slices.Contains(calls[0].Args, argument) {
			t.Errorf("project build lacks %s: %v", argument, calls[0].Args)
		}
	}
	for _, project := range []string{"Llunde", "../etc", "a b", "-x"} {
		if _, err := Render(context.Background(), RenderOptions{State: NewState(root), Runner: runner, Project: project}); err == nil {
			t.Errorf("accepted project name %q", project)
		}
	}
}

func TestRenderRequiresSettingsAndReportsBuildFailures(t *testing.T) {
	root := t.TempDir()
	runner := ci.Runner{Execute: func(context.Context, process.Options) (process.Result, error) {
		t.Fatal("flux executed without settings")
		return process.Result{}, nil
	}}
	if _, err := Render(context.Background(), RenderOptions{State: NewState(root), Runner: runner}); err == nil || !strings.Contains(err.Error(), settingsFile) {
		t.Fatalf("missing settings accepted: %v", err)
	}
	writeFile(t, filepath.Join(root, settingsFile), "data:\n  STORAGE_CLASS: local-path\n")
	failing := ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		return process.Result{ExitCode: 1}, errors.New("flux failed: exit status 1")
	}}
	if _, err := Render(context.Background(), RenderOptions{State: NewState(root), Runner: failing}); err == nil || !strings.HasPrefix(err.Error(), "flux build: ") {
		t.Fatalf("build failure not reported: %v", err)
	}
}

func TestDiffReportsDifferencesByExitCode(t *testing.T) {
	root := t.TempDir()
	expected := []string{"diff", "kustomization", "flux-system", "--path=./platform/clusters/production", "--kustomization-file=platform/clusters/production/flux-system/gotk-sync.yaml", "--recursive", "--local-sources=GitRepository/flux-system/flux-system=.", "--ignore-paths=**/*.sops.yaml", "--progress-bar=false"}
	for _, test := range []struct {
		code   int
		failed bool
		want   error
	}{{0, false, nil}, {1, true, ErrDifferences}, {2, true, nil}} {
		var arguments []string
		runner := ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
			arguments = options.Args
			fmt.Fprintln(options.Stdout, "► Kustomization/flux-system/platform-policy drifted")
			if test.failed {
				return process.Result{ExitCode: test.code}, fmt.Errorf("flux failed: exit status %d", test.code)
			}
			return process.Result{}, nil
		}}
		var stdout strings.Builder
		err := Diff(context.Background(), DiffOptions{State: NewState(root), Runner: runner, Stdout: &stdout, Stderr: io.Discard})
		switch {
		case test.code == 2 && (err == nil || errors.Is(err, ErrDifferences)):
			t.Errorf("exit %d: failure reported as %v", test.code, err)
		case test.code != 2 && !errors.Is(err, test.want):
			t.Errorf("exit %d: got %v, want %v", test.code, err, test.want)
		}
		if !reflect.DeepEqual(arguments, expected) {
			t.Fatalf("unexpected diff arguments %v", arguments)
		}
		if !strings.Contains(stdout.String(), "drifted") {
			t.Fatal("diff output not streamed")
		}
	}
}
