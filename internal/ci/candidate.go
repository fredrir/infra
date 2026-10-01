package ci

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/fredrir/infra/internal/process"
	"go.yaml.in/yaml/v3"
)

const projectProfiles = "build/projects"

func AffectedCandidates(ctx context.Context, runner process.Runner, workflow, base string) ([]string, error) {
	profiles, err := filepath.Glob(filepath.Join(runner.Dir, projectProfiles, "*.json"))
	if err != nil {
		return nil, err
	}
	var projects []string
	for _, profile := range profiles {
		projects = append(projects, strings.TrimSuffix(filepath.Base(profile), ".json"))
	}
	if len(projects) == 0 {
		return nil, errors.New("no candidate project profiles")
	}
	if base == "" {
		return projects, nil
	}
	triggers, err := candidateTriggers(filepath.Join(runner.Dir, workflow))
	if err != nil {
		return nil, err
	}
	revision, err := runner.Output(ctx, "git", "rev-parse", "--verify", "--end-of-options", base+"^{commit}")
	if err != nil {
		return nil, err
	}
	changed, err := runner.Output(ctx, "git", "diff", "--name-only", "--no-renames", "-z", strings.TrimSpace(string(revision)), "HEAD", "--")
	if err != nil {
		return nil, err
	}
	affected := map[string]bool{}
	for path := range strings.SplitSeq(strings.TrimSuffix(string(changed), "\x00"), "\x00") {
		if path == "" || !slices.ContainsFunc(triggers, func(trigger string) bool { return doublestar.MatchUnvalidated(trigger, path) }) {
			continue
		}
		project := profileProject(path)
		if !slices.Contains(projects, project) {
			return projects, nil
		}
		affected[project] = true
	}
	return slices.DeleteFunc(projects, func(project string) bool { return !affected[project] }), nil
}

func candidateTriggers(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var workflow struct {
		On struct {
			Push struct{ Paths []string }
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return nil, err
	}
	if len(workflow.On.Push.Paths) == 0 {
		return nil, fmt.Errorf("%s has no push path triggers", path)
	}
	return workflow.On.Push.Paths, nil
}

func profileProject(path string) string {
	rest, ok := strings.CutPrefix(path, projectProfiles+"/")
	if !ok {
		return ""
	}
	if project, _, nested := strings.Cut(rest, "/"); nested {
		return project
	}
	if project, ok := strings.CutSuffix(rest, ".json"); ok {
		return project
	}
	return ""
}
