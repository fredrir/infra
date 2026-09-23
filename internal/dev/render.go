package dev

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
	"go.yaml.in/yaml/v3"
)

const (
	rootKustomizationFile = "platform/clusters/production/flux-system/gotk-sync.yaml"
	clusterKustomizations = "platform/clusters/production/root.yaml"
	settingsFile          = "platform/clusters/production/settings.yaml"
	localSources          = "GitRepository/flux-system/flux-system=."
)

type RenderOptions struct {
	State   State
	Runner  ci.Runner
	Project string
	Output  string
	Stdout  io.Writer
}

type RenderReport struct {
	Output        string   `json:"output"`
	Documents     int      `json:"documents"`
	Unsubstituted []string `json:"unsubstituted,omitempty"`
}

var projectNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
var variablePattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)`)

type target struct{ name, path, kustomizationFile, output string }

func selectTarget(project string) (target, error) {
	if project == "" {
		return target{"flux-system", "./platform/clusters/production", rootKustomizationFile, "platform.yaml"}, nil
	}
	if !projectNamePattern.MatchString(project) {
		return target{}, fmt.Errorf("invalid project name %q", project)
	}
	return target{"platform-projects", "./platform/projects/" + project, clusterKustomizations, "project-" + project + ".yaml"}, nil
}

func (t target) arguments() []string {
	return []string{"kustomization", t.name, "--path=" + t.path, "--kustomization-file=" + t.kustomizationFile, "--recursive", "--local-sources=" + localSources}
}

func Render(ctx context.Context, opts RenderOptions) (RenderReport, error) {
	var report RenderReport
	t, err := selectTarget(opts.Project)
	if err != nil {
		return report, err
	}
	if opts.Runner.Dir == "" {
		opts.Runner.Dir = opts.State.Root
	}
	settings, err := readSettings(filepath.Join(opts.State.Root, settingsFile))
	if err != nil {
		return report, err
	}
	if err := os.MkdirAll(opts.State.Render(), 0o755); err != nil {
		return report, err
	}
	built := filepath.Join(opts.State.Render(), "build-"+t.output)
	arguments := append([]string{"build"}, append(t.arguments(), "--dry-run")...)
	if err := runToFile(ctx, opts.Runner, built, process.Options{Name: "flux", Args: arguments}); err != nil {
		return report, fmt.Errorf("flux build: %w", err)
	}
	output := opts.Output
	if output == "" || output == "-" {
		output = filepath.Join(opts.State.Render(), t.output)
	}
	input, err := os.Open(built)
	if err != nil {
		return report, err
	}
	defer input.Close()
	if err := runToFile(ctx, opts.Runner, output, process.Options{Name: "flux", Args: []string{"envsubst"}, Env: settings, Stdin: input}); err != nil {
		return report, fmt.Errorf("flux envsubst: %w", err)
	}
	report.Output = output
	if report.Documents, report.Unsubstituted, err = inspectRender(output); err != nil {
		return report, err
	}
	if opts.Output == "-" {
		rendered, err := os.Open(output)
		if err != nil {
			return report, err
		}
		defer rendered.Close()
		if _, err := io.Copy(opts.Stdout, rendered); err != nil {
			return report, err
		}
	}
	return report, nil
}

func readSettings(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var settings struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(settings.Data) == 0 {
		return nil, fmt.Errorf("%s declares no settings", path)
	}
	env := make([]string, 0, len(settings.Data))
	for key, value := range settings.Data {
		env = append(env, key+"="+value)
	}
	sort.Strings(env)
	return env, nil
}

func inspectRender(path string) (int, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, nil, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	documents := 0
	for {
		var document any
		err := decoder.Decode(&document)
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, nil, fmt.Errorf("rendered output: %w", err)
		}
		if document != nil {
			documents++
		}
	}
	seen := make(map[string]bool)
	var names []string
	for _, match := range variablePattern.FindAllSubmatch(data, -1) {
		name := string(match[1])
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return documents, names, nil
}
