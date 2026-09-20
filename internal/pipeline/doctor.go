package pipeline

import (
	"context"
	"os/exec"
	"runtime"
	"strings"

	"dagger.io/dagger"
)

type Diagnostic struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func Doctor(ctx context.Context, root, bazel string, engine bool) []Diagnostic {
	checks := []Diagnostic{{Name: "host", OK: true, Detail: runtime.GOOS + "/" + runtime.GOARCH}}
	config, err := ReadToolchain(root)
	if err != nil {
		return append(checks, Diagnostic{Name: "toolchain", Detail: err.Error()})
	}
	checks = append(checks, Diagnostic{Name: "toolchain", OK: true, Detail: "Go " + config.Go + ", Bazel " + config.Bazel + ", Dagger " + config.Dagger})
	tools := []string{"git"}
	if bazel != "" {
		tools = append(tools, bazel)
	}
	for _, tool := range tools {
		command := exec.CommandContext(ctx, tool, "--version")
		data, err := command.CombinedOutput()
		detail := strings.TrimSpace(string(data))
		if err != nil {
			detail = err.Error()
		}
		ok := err == nil
		if tool == bazel && ok {
			ok = detail == "bazel "+config.Bazel
		}
		checks = append(checks, Diagnostic{Name: tool, OK: ok, Detail: detail})
	}
	if engine {
		client, err := dagger.Connect(ctx)
		if err != nil {
			return append(checks, Diagnostic{Name: "dagger", Detail: err.Error()})
		}
		defer client.Close()
		version, err := client.Version(ctx)
		check := Diagnostic{Name: "dagger", OK: err == nil && strings.TrimPrefix(version, "v") == config.Dagger, Detail: version}
		if err != nil {
			check.Detail = err.Error()
		}
		checks = append(checks, check)
	}
	return checks
}
