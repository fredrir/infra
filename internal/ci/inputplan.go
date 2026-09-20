package ci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type ProjectInputs struct {
	Schema  int                 `json:"schema"`
	Shared  []string            `json:"shared"`
	Ignored []string            `json:"ignored"`
	Targets map[string][]string `json:"targets"`
}

type ProjectInputPlan struct {
	Changed         bool     `json:"changed"`
	Reason          string   `json:"reason"`
	AffectedTargets []string `json:"affected_targets"`
}

func PlanProjectInputs(ctx context.Context, root, configPath, base, target string) (ProjectInputPlan, error) {
	if !validInputPath(configPath, false) {
		return ProjectInputPlan{}, errors.New("input configuration must be a clean repository-relative path")
	}
	data, err := os.ReadFile(filepath.Join(root, configPath))
	if err != nil {
		return ProjectInputPlan{}, err
	}
	var config ProjectInputs
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return ProjectInputPlan{}, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return ProjectInputPlan{}, errors.New("input configuration must contain one JSON object")
	}
	if err := validateProjectInputs(config, target); err != nil {
		return ProjectInputPlan{}, err
	}
	if err := ctx.Err(); err != nil {
		return ProjectInputPlan{}, err
	}
	if base == "" {
		return allProjectInputs(config, "missing-base"), nil
	}
	runner := Runner{Dir: root}
	revision, err := runner.Output(ctx, "git", "rev-parse", "--verify", "--end-of-options", base+"^{commit}")
	if err != nil {
		if ctx.Err() != nil {
			return ProjectInputPlan{}, ctx.Err()
		}
		return allProjectInputs(config, "unavailable-base"), nil
	}
	changed, err := runner.Output(ctx, "git", "diff", "--name-only", "--no-renames", "-z", strings.TrimSpace(string(revision)), "--")
	if err != nil {
		return ProjectInputPlan{}, err
	}
	untracked, err := runner.Output(ctx, "git", "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return ProjectInputPlan{}, err
	}
	return projectInputsForPaths(config, configPath, target, strings.Split(string(append(changed, untracked...)), "\x00")), nil
}

func validInputPath(value string, prefix bool) bool {
	if prefix {
		value = strings.TrimSuffix(value, "/")
	}
	return value != "." && filepath.IsLocal(value) && filepath.ToSlash(filepath.Clean(value)) == value && !strings.ContainsAny(value, "*?[]\\\x00\r\n")
}

func validateProjectInputs(config ProjectInputs, target string) error {
	if config.Schema != 1 || len(config.Targets) == 0 {
		return errors.New("input configuration requires schema 1 and at least one target")
	}
	if _, ok := config.Targets[target]; !ok {
		return fmt.Errorf("unknown project target %q", target)
	}
	patterns := append(slices.Clone(config.Shared), config.Ignored...)
	for name, inputs := range config.Targets {
		if name == "" || strings.ContainsAny(name, "\x00\r\n") || len(inputs) == 0 {
			return errors.New("project targets require names and input paths")
		}
		patterns = append(patterns, inputs...)
	}
	for _, pattern := range patterns {
		if !validInputPath(pattern, true) {
			return fmt.Errorf("invalid project input path %q", pattern)
		}
	}
	return nil
}

func matchesProjectInput(changed string, patterns []string) bool {
	for _, pattern := range patterns {
		if changed == pattern || (strings.HasSuffix(pattern, "/") && strings.HasPrefix(changed, pattern)) {
			return true
		}
	}
	return false
}

func allProjectInputs(config ProjectInputs, reason string) ProjectInputPlan {
	plan := ProjectInputPlan{Changed: true, Reason: reason, AffectedTargets: make([]string, 0, len(config.Targets))}
	for name := range config.Targets {
		plan.AffectedTargets = append(plan.AffectedTargets, name)
	}
	slices.Sort(plan.AffectedTargets)
	return plan
}

func projectInputsForPaths(config ProjectInputs, configPath, target string, paths []string) ProjectInputPlan {
	affected := map[string]bool{}
	for _, changed := range paths {
		if changed == "" {
			continue
		}
		if changed == configPath {
			return allProjectInputs(config, "configuration-changed")
		}
		if matchesProjectInput(changed, config.Shared) {
			return allProjectInputs(config, "shared-input")
		}
		matched := false
		for name, inputs := range config.Targets {
			if matchesProjectInput(changed, inputs) {
				affected[name], matched = true, true
			}
		}
		if !matched && !matchesProjectInput(changed, config.Ignored) {
			return allProjectInputs(config, "unmapped-input")
		}
	}
	plan := ProjectInputPlan{Changed: affected[target], Reason: "unaffected", AffectedTargets: make([]string, 0, len(affected))}
	for name := range affected {
		plan.AffectedTargets = append(plan.AffectedTargets, name)
	}
	slices.Sort(plan.AffectedTargets)
	if plan.Changed {
		plan.Reason = "target-input"
	}
	return plan
}
