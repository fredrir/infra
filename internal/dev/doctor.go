package dev

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/pipeline"
)

type DoctorOptions struct {
	State      State
	Runner     ci.Runner
	Bazel      string
	Kubeconfig string
	KVMDevice  string
	Platform   string
	Assets     func(string) (ci.ToolAsset, bool)
}

func (opts DoctorOptions) defaults() DoctorOptions {
	if opts.Runner.Dir == "" {
		opts.Runner.Dir = opts.State.Root
	}
	if opts.Kubeconfig == "" {
		opts.Kubeconfig = os.Getenv("KUBECONFIG")
	}
	if opts.KVMDevice == "" {
		opts.KVMDevice = "/dev/kvm"
	}
	if opts.Platform == "" {
		opts.Platform = runtime.GOOS
	}
	if opts.Assets == nil {
		opts.Assets = ci.Tool
	}
	return opts
}

func Doctor(ctx context.Context, opts DoctorOptions) []pipeline.Diagnostic {
	opts = opts.defaults()
	checks := pipeline.Doctor(ctx, opts.State.Root, opts.Bazel, false)
	for _, tool := range Tools {
		checks = append(checks, checkTool(ctx, opts, tool))
	}
	return append(checks, checkDocker(ctx, opts), checkKVM(opts), checkKubeconfig(opts), checkAnsible(ctx, opts))
}

func (opts DoctorOptions) output(ctx context.Context, name string, args ...string) (string, error) {
	runner := opts.Runner
	var stderr bytes.Buffer
	runner.Stderr = &stderr
	output, err := runner.Output(ctx, name, args...)
	if err != nil {
		if detail := firstLine(stderr.String()); detail != "" {
			return "", fmt.Errorf("%w: %s", err, detail)
		}
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func checkTool(ctx context.Context, opts DoctorOptions, tool Tool) pipeline.Diagnostic {
	asset, ok := opts.Assets(tool.Name)
	if !ok {
		return pipeline.Diagnostic{Name: tool.Name, Detail: "no pinned asset"}
	}
	pinned := PinnedVersion(asset)
	path, err := opts.State.toolPath(tool.Name)
	if err != nil {
		return pipeline.Diagnostic{Name: tool.Name, Detail: "missing; pinned " + pinned}
	}
	output, err := opts.output(ctx, path, tool.VersionArgs...)
	if err != nil {
		return pipeline.Diagnostic{Name: tool.Name, Detail: path + ": " + err.Error()}
	}
	detail := path + ": " + firstLine(output)
	if !strings.Contains(output, pinned) {
		return pipeline.Diagnostic{Name: tool.Name, Detail: detail + "; pinned " + pinned}
	}
	return pipeline.Diagnostic{Name: tool.Name, OK: true, Detail: detail}
}

func checkDocker(ctx context.Context, opts DoctorOptions) pipeline.Diagnostic {
	output, err := opts.output(ctx, "docker", "info", "--format", "{{.OSType}}")
	if err != nil {
		return pipeline.Diagnostic{Name: "docker", Detail: err.Error()}
	}
	return pipeline.Diagnostic{Name: "docker", OK: output == "linux", Detail: "OSType " + output}
}

func checkKVM(opts DoctorOptions) pipeline.Diagnostic {
	if opts.Platform != "linux" {
		return pipeline.Diagnostic{Name: "kvm", OK: true, Detail: "not required on " + opts.Platform}
	}
	device, err := os.OpenFile(opts.KVMDevice, os.O_RDWR, 0)
	if err != nil {
		return pipeline.Diagnostic{Name: "kvm", Detail: err.Error()}
	}
	device.Close()
	return pipeline.Diagnostic{Name: "kvm", OK: true, Detail: opts.KVMDevice}
}

func checkKubeconfig(opts DoctorOptions) pipeline.Diagnostic {
	value := opts.Kubeconfig
	if value == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return pipeline.Diagnostic{Name: "kubeconfig", Detail: err.Error()}
		}
		value = filepath.Join(home, ".kube", "config")
	}
	var missing []string
	for _, path := range filepath.SplitList(value) {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			missing = append(missing, path)
		}
	}
	if len(missing) > 0 {
		return pipeline.Diagnostic{Name: "kubeconfig", Detail: "not a file: " + strings.Join(missing, ", ")}
	}
	return pipeline.Diagnostic{Name: "kubeconfig", OK: true, Detail: value}
}

func checkAnsible(ctx context.Context, opts DoctorOptions) pipeline.Diagnostic {
	path := filepath.Join(opts.State.Venv(), "bin", "ansible-playbook")
	output, err := opts.output(ctx, path, "--version")
	if err != nil {
		return pipeline.Diagnostic{Name: "ansible", Detail: path + ": " + err.Error()}
	}
	return pipeline.Diagnostic{Name: "ansible", OK: true, Detail: firstLine(output)}
}
