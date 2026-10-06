package ci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/fredrir/infra/internal/process"
)

type Project struct {
	Schema           int             `json:"schema"`
	Repository       string          `json:"repository"`
	RepositoryID     string          `json:"repository_id"`
	Inputs           ProjectInputs   `json:"inputs"`
	ImageInputs      *ProjectInputs  `json:"image_inputs,omitempty"`
	Images           []ProjectImage  `json:"images"`
	Checks           []ProjectCheck  `json:"checks"`
	Rust             *ProjectRust    `json:"rust,omitempty"`
	Doppler          *ProjectDoppler `json:"doppler,omitempty"`
	DependencyRecipe string          `json:"dependency_recipe,omitempty"`
}

type ProjectImage struct {
	Image       string            `json:"image"`
	Dockerfile  string            `json:"dockerfile"`
	Target      string            `json:"target"`
	CheckTarget string            `json:"check_target"`
	Timeout     int               `json:"timeout"`
	ScannerJava bool              `json:"scanner_java"`
	Recipe      string            `json:"recipe"`
	Smoke       string            `json:"smoke"`
	Precheck    string            `json:"precheck"`
	Arguments   map[string]string `json:"arguments,omitempty"`
	Variables   []string          `json:"variables,omitempty"`
	BuildArgs   string            `json:"build_args"`
}

type ProjectCheck struct {
	Name      string            `json:"name"`
	Recipe    string            `json:"recipe"`
	Target    string            `json:"target"`
	Timeout   int               `json:"timeout"`
	Scheduled bool              `json:"scheduled,omitempty"`
	InImage   bool              `json:"in_image,omitempty"`
	Arguments map[string]string `json:"arguments,omitempty"`
}

type ProjectRust struct {
	Clippy           string `json:"clippy"`
	Test             string `json:"test"`
	FastTest         string `json:"fast_test"`
	PostgresEnv      string `json:"postgres_env"`
	BuildOutputCache bool   `json:"build_output_cache"`
}

type ProjectDoppler struct {
	Project string   `json:"project"`
	Config  string   `json:"config"`
	Public  []string `json:"public"`
}

type ProjectPlan struct {
	Images     []ProjectImage `json:"images"`
	Checks     []ProjectCheck `json:"checks"`
	Rust       *ProjectRust   `json:"rust,omitempty"`
	Dependency bool           `json:"dependency"`
}

var projectNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]*$`)
var projectRepositoryPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func ReadProject(root, repository, repositoryID string) (Project, error) {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner != "fredrir" || !projectRepositoryPattern.MatchString(name) {
		return Project{}, errors.New("unknown project repository")
	}
	file := filepath.Join(root, "build/projects", name+".json")
	info, err := os.Lstat(file)
	if err != nil {
		return Project{}, err
	}
	if !info.Mode().IsRegular() {
		return Project{}, errors.New("project profile must be a regular file")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return Project{}, err
	}
	var project Project
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&project); err != nil {
		return Project{}, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Project{}, errors.New("one project profile required")
	}
	if project.Schema != 1 || project.Inputs.Schema != 1 || project.Repository != repository || !regexp.MustCompile(`^[0-9]+$`).MatchString(project.RepositoryID) || (repositoryID != "" && project.RepositoryID != repositoryID) {
		return Project{}, errors.New("project repository identity mismatch")
	}
	for _, image := range project.Images {
		if !strings.HasPrefix(image.Image, "ghcr.io/fredrir/") || !validInputPath(image.Dockerfile, false) || image.Timeout < 1 || image.Timeout > 180 {
			return Project{}, errors.New("invalid project image")
		}
		if _, err := BuildArguments(strings.Repeat("a", 40), image.BuildArgs); err != nil {
			return Project{}, err
		}
	}
	for _, check := range project.Checks {
		if !projectNamePattern.MatchString(check.Name) || !validInputPath(check.Recipe, false) || check.Timeout < 1 || check.Timeout > 180 {
			return Project{}, errors.New("invalid project check")
		}
	}
	for target := range project.Inputs.Targets {
		if err := validateProjectInputs(project.Inputs, target); err != nil {
			return Project{}, err
		}
	}
	if project.ImageInputs != nil {
		for target := range project.ImageInputs.Targets {
			if err := validateProjectInputs(*project.ImageInputs, target); err != nil {
				return Project{}, err
			}
		}
	}
	return project, nil
}

func PlanProject(ctx context.Context, runner process.Runner, project Project, base string, scheduled bool, variables map[string]string) (ProjectPlan, error) {
	plan := ProjectPlan{Images: []ProjectImage{}, Checks: []ProjectCheck{}, Rust: project.Rust, Dependency: project.DependencyRecipe != ""}
	var sourceRevision string
	affected := allProjectInputs(project.Inputs, "missing-base")
	imageAffected := affected
	if project.ImageInputs != nil {
		imageAffected = allProjectInputs(*project.ImageInputs, "missing-base")
	}
	if base != "" && len(project.Inputs.Targets) > 0 {
		revision, err := runner.Output(ctx, "git", "rev-parse", "--verify", "--end-of-options", base+"^{commit}")
		if err == nil {
			paths, err := runner.Output(ctx, "git", "diff", "--name-only", "--no-renames", "-z", strings.TrimSpace(string(revision)), "--")
			if err != nil {
				return plan, err
			}
			affected = projectInputsForPaths(project.Inputs, "", "", strings.Split(string(paths), "\x00"))
			imageAffected = affected
			if project.ImageInputs != nil {
				imageAffected = projectInputsForPaths(*project.ImageInputs, "", "", strings.Split(string(paths), "\x00"))
			}
		} else if ctx.Err() != nil {
			return plan, ctx.Err()
		}
	}
	if base != "" && len(project.Inputs.Targets) > 0 && len(affected.AffectedTargets) == 0 {
		plan.Rust = nil
	}
	for _, image := range project.Images {
		if base != "" && !slices.Contains(imageAffected.AffectedTargets, strings.TrimPrefix(image.Image, "ghcr.io/fredrir/")) {
			continue
		}
		arguments := make(map[string]string)
		for name, value := range image.Arguments {
			if value == "$revision" {
				if sourceRevision == "" {
					revision, err := runner.Output(ctx, "git", "rev-parse", "HEAD")
					if err != nil {
						return plan, err
					}
					sourceRevision = strings.TrimSpace(string(revision))
					if !revisionPattern.MatchString(sourceRevision) {
						return plan, errors.New("invalid source revision")
					}
				}
				value = sourceRevision
			}
			arguments[name] = value
		}
		for _, name := range image.Variables {
			value := variables[name]
			if value == "" {
				return plan, fmt.Errorf("public build variable %s missing", name)
			}
			arguments[name] = value
		}
		keys := make([]string, 0, len(arguments))
		for key := range arguments {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		var lines []string
		for _, key := range keys {
			lines = append(lines, key+"="+arguments[key])
		}
		image.BuildArgs = strings.Join(lines, "\n")
		if _, err := BuildArguments(strings.Repeat("a", 40), image.BuildArgs); err != nil {
			return plan, err
		}
		plan.Images = append(plan.Images, image)
	}
	for _, check := range project.Checks {
		if base != "" && len(affected.AffectedTargets) == 0 {
			continue
		}
		if check.Scheduled && !scheduled {
			continue
		}
		plan.Checks = append(plan.Checks, check)
	}
	return plan, ctx.Err()
}

func PublicProjectVariables(ctx context.Context, client *http.Client, base, token string, doppler ProjectDoppler) (map[string]string, error) {
	if token == "" || len(doppler.Public) == 0 {
		return nil, errors.New("Doppler token and public variable names required")
	}
	query := url.Values{"project": {doppler.Project}, "config": {doppler.Config}, "format": {"json"}, "secrets": {strings.Join(doppler.Public, ",")}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v3/configs/config/secrets/download?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("Doppler request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Doppler returned HTTP %d", response.StatusCode)
	}
	var values map[string]string
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&values); err != nil {
		return nil, errors.New("invalid Doppler public variables")
	}
	result := make(map[string]string)
	for _, name := range doppler.Public {
		value := values[name]
		if value == "" || strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("invalid public variable %s", name)
		}
		result[name] = value
	}
	return result, nil
}

func WriteProjectOutputs(file string, plan ProjectPlan) error {
	images, err := json.Marshal(plan.Images)
	if err != nil {
		return err
	}
	checks, err := json.Marshal(plan.Checks)
	if err != nil {
		return err
	}
	values := map[string]string{"images": string(images), "checks": string(checks), "has-images": fmt.Sprint(len(plan.Images) > 0), "has-checks": fmt.Sprint(len(plan.Checks) > 0), "dependency": fmt.Sprint(plan.Dependency), "rust": fmt.Sprint(plan.Rust != nil)}
	if plan.Rust != nil {
		values["clippy"] = plan.Rust.Clippy
		values["test"] = plan.Rust.Test
		values["fast-test"] = plan.Rust.FastTest
		values["postgres-env"] = plan.Rust.PostgresEnv
		values["build-output-cache"] = fmt.Sprint(plan.Rust.BuildOutputCache)
	}
	return WriteOutputs(file, values)
}

func WriteOutputs(file string, values map[string]string) error {
	if file == "" {
		return nil
	}
	var output strings.Builder
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if strings.ContainsAny(key+values[key], "\r\n\x00") {
			return errors.New("multiline workflow output")
		}
		fmt.Fprintf(&output, "%s=%s\n", key, values[key])
	}
	handle, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = io.WriteString(handle, output.String())
	return errors.Join(err, handle.Close())
}
