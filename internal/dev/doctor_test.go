package dev

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/pipeline"
	"github.com/fredrir/infra/internal/process"
)

func pinnedAssets(name string) (ci.ToolAsset, bool) {
	return ci.ToolAsset{URL: "https://example.invalid/" + name + "-v1.2.3-linux-amd64.tar.gz", Digest: strings.Repeat("a", 64)}, true
}

func executable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func diagnostic(t *testing.T, checks []pipeline.Diagnostic, name string) pipeline.Diagnostic {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("missing diagnostic %s in %+v", name, checks)
	return pipeline.Diagnostic{}
}

func TestDoctorComparesToolsWithPinsAndReportsEnvironment(t *testing.T) {
	root := t.TempDir()
	state := NewState(root)
	t.Setenv("PATH", t.TempDir())
	for _, tool := range Tools {
		if tool.Name != "kubectl" {
			executable(t, filepath.Join(state.Tools(), tool.Name))
		}
	}
	executable(t, filepath.Join(state.Venv(), "bin", "ansible-playbook"))
	kvm := filepath.Join(root, "kvm")
	if err := os.WriteFile(kvm, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(root, "cluster.yaml")
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	runner := ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		calls = append(calls, filepath.Base(options.Name)+" "+strings.Join(options.Args, " "))
		if options.Dir != root {
			t.Errorf("command ran outside the repository: %s", options.Dir)
		}
		switch filepath.Base(options.Name) {
		case "flux":
			return process.Result{Stdout: []byte("flux version 9.9.9\n")}, nil
		case "docker":
			fmt.Fprintln(options.Stderr, "Cannot connect to the Docker daemon")
			return process.Result{ExitCode: 1}, errors.New("docker failed: exit status 1")
		case "ansible-playbook":
			return process.Result{Stdout: []byte("ansible-playbook [core 2.21.4]\n  config file = None\n")}, nil
		case "kubectl":
			if len(options.Args) > 1 && options.Args[1] == "--request-timeout=5s" {
				fmt.Fprintln(options.Stderr, "Unable to connect to the server: dial tcp: i/o timeout")
				return process.Result{Stdout: []byte("Client Version: v1.2.3\n"), ExitCode: 1}, errors.New("kubectl failed: exit status 1")
			}
		}
		return process.Result{Stdout: []byte(filepath.Base(options.Name) + " v1.2.3\nextra\n")}, nil
	}}
	checks := Doctor(context.Background(), DoctorOptions{State: state, Runner: runner, Kubeconfig: kubeconfig, KVMDevice: kvm, Platform: "linux", Assets: pinnedAssets})
	if check := diagnostic(t, checks, "tofu"); !check.OK || !strings.HasSuffix(check.Detail, "tofu: tofu v1.2.3") {
		t.Errorf("pinned tool not accepted: %+v", check)
	}
	if check := diagnostic(t, checks, "flux"); check.OK || !strings.Contains(check.Detail, "flux version 9.9.9; pinned 1.2.3") {
		t.Errorf("version drift not reported: %+v", check)
	}
	if check := diagnostic(t, checks, "kubectl"); check.OK || check.Detail != "missing; pinned 1.2.3" {
		t.Errorf("missing tool not reported: %+v", check)
	}
	if check := diagnostic(t, checks, "docker"); check.OK || !strings.Contains(check.Detail, "Cannot connect to the Docker daemon") {
		t.Errorf("docker failure lost its stderr detail: %+v", check)
	}
	if check := diagnostic(t, checks, "kvm"); !check.OK || check.Detail != kvm {
		t.Errorf("kvm device not accepted: %+v", check)
	}
	if check := diagnostic(t, checks, "kubeconfig"); !check.OK || check.Detail != kubeconfig {
		t.Errorf("kubeconfig not accepted: %+v", check)
	}
	if check := diagnostic(t, checks, "ansible"); !check.OK || check.Detail != "ansible-playbook [core 2.21.4]" {
		t.Errorf("ansible environment not accepted: %+v", check)
	}
	if check := diagnostic(t, checks, "cluster"); check.OK || check.Detail != "kubectl missing" {
		t.Errorf("cluster check ran without kubectl: %+v", check)
	}
	for _, call := range []string{"tofu version", "kubectl version --client", "docker info --format {{.OSType}}", "ansible-playbook --version"} {
		if strings.Contains(strings.Join(calls, "\n"), call) == (call == "kubectl version --client") {
			t.Errorf("unexpected command set %q: %v", call, calls)
		}
	}
}

func TestDoctorRejectsDirectoriesAndMissingKubeconfigs(t *testing.T) {
	root := t.TempDir()
	runner := ci.Runner{Execute: func(context.Context, process.Options) (process.Result, error) {
		return process.Result{Stdout: []byte("linux\n")}, nil
	}}
	opts := DoctorOptions{State: NewState(root), Runner: runner, Kubeconfig: root + string(os.PathListSeparator) + filepath.Join(root, "missing.yaml"), KVMDevice: filepath.Join(root, "no-kvm"), Platform: "linux", Assets: pinnedAssets}
	checks := Doctor(context.Background(), opts)
	if check := diagnostic(t, checks, "kubeconfig"); check.OK || !strings.Contains(check.Detail, "missing.yaml") || !strings.Contains(check.Detail, root) {
		t.Errorf("invalid kubeconfig entries not reported: %+v", check)
	}
	if check := diagnostic(t, checks, "kvm"); check.OK {
		t.Errorf("missing KVM device accepted: %+v", check)
	}
	if check := diagnostic(t, checks, "docker"); !check.OK || check.Detail != "OSType linux" {
		t.Errorf("docker not accepted: %+v", check)
	}
	executable(t, filepath.Join(opts.State.Tools(), "kubectl"))
	reachable := ci.Runner{Execute: func(context.Context, process.Options) (process.Result, error) {
		return process.Result{Stdout: []byte("Client Version: v1.36.3\nKustomize Version: v5.7.1\nServer Version: v1.36.3+k3s1\n")}, nil
	}}
	opts.Runner = reachable
	if check := diagnostic(t, Doctor(context.Background(), opts), "cluster"); !check.OK || check.Detail != "v1.36.3+k3s1" {
		t.Errorf("reachable cluster not reported: %+v", check)
	}
	unreachable := ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		fmt.Fprintln(options.Stderr, "Unable to connect to the server: dial tcp: i/o timeout")
		return process.Result{ExitCode: 1}, errors.New("kubectl failed: exit status 1")
	}}
	opts.Runner = unreachable
	if check := diagnostic(t, Doctor(context.Background(), opts), "cluster"); check.OK || !strings.Contains(check.Detail, "Unable to connect") {
		t.Errorf("unreachable cluster not reported: %+v", check)
	}
	opts.Runner = runner
	opts.Platform = "darwin"
	if check := diagnostic(t, Doctor(context.Background(), opts), "kvm"); !check.OK || check.Detail != "not required on darwin" {
		t.Errorf("kvm required on darwin: %+v", check)
	}
}

func TestPinnedVersionsFollowAssetURLs(t *testing.T) {
	for name, expected := range map[string]string{"kubectl": "1.36.3", "helm": "4.3.0", "sops": "3.13.3", "uv": "0.12.17", "kustomize": "5.8.1", "cosign": "3.1.3", "k3d": "5.9.0", "hyperfine": "1.20.0", "age-keygen": "1.3.2"} {
		asset, ok := ci.Tool(name)
		if !ok {
			t.Fatalf("%s is not pinned", name)
		}
		if got := PinnedVersion(asset); got != expected {
			t.Errorf("%s: pinned %q, expected %q", name, got, expected)
		}
	}
	for _, tool := range Tools {
		if _, ok := ci.Tool(tool.Name); !ok {
			t.Errorf("%s has no pinned asset", tool.Name)
		}
	}
	if PinnedVersion(ci.ToolAsset{URL: "https://example.invalid/tool"}) != "" {
		t.Error("unversioned asset produced a version")
	}
}
