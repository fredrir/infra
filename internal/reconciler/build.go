package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var (
	goVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)
	cloudTools       = []string{"flux", "gh", "kubectl", "tofu"}
)

func goToolchain(source string) (string, error) {
	data, err := os.ReadFile(filepath.Join(source, "build", "toolchain.json"))
	if err != nil {
		return "", err
	}
	var toolchain struct{ Go string }
	if err := json.Unmarshal(data, &toolchain); err != nil {
		return "", fmt.Errorf("build/toolchain.json: %w", err)
	}
	if !goVersionPattern.MatchString(toolchain.Go) {
		return "", fmt.Errorf("build/toolchain.json: invalid Go version %q", toolchain.Go)
	}
	return "go" + toolchain.Go, nil
}

func goEnvironment(work, cache, toolchain string) []string {
	return []string{
		"GOTOOLCHAIN=" + toolchain,
		"GOPROXY=" + moduleProxy(cache) + ",https://proxy.golang.org,direct",
		"GOCACHE=" + filepath.Join(work, "go", "cache"),
		"GOMODCACHE=" + filepath.Join(work, "go", "mod"),
		"GOPATH=" + filepath.Join(work, "go", "path"),
		"GOFLAGS=-mod=readonly -modcacherw",
		"CGO_ENABLED=0",
	}
}

func (e executor) buildEngine(ctx context.Context, work, cache, source, output string) error {
	toolchain, err := goToolchain(source)
	if err != nil {
		return err
	}
	if _, err := e.run(ctx, source, goEnvironment(work, cache, toolchain), "go", "build", "-trimpath", "-o", output, "./cmd/infra"); err != nil {
		return fmt.Errorf("build engine with %s: %w", toolchain, err)
	}
	if err := mirrorModuleDownloads(filepath.Join(work, "go", "mod", "cache", "download"), cache); err != nil {
		return fmt.Errorf("cache verified module downloads: %w", err)
	}
	return nil
}

func (e executor) installTools(ctx context.Context, engine, work, cache string, tools []string) error {
	environment := []string{"INFRA_TOOL_CACHE=" + filepath.Join(work, "tools"), "INFRA_TOOL_DOWNLOADS=" + filepath.Join(cache, "tools")}
	if _, err := e.run(ctx, work, environment, engine, append([]string{"ci", "install-tools", "--temporary", work}, tools...)...); err != nil {
		return fmt.Errorf("install tools: %w", err)
	}
	return nil
}
