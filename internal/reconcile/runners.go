package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

type RunnerFleet struct {
	Schema       int      `json:"schema"`
	Owner        string   `json:"owner"`
	Host         string   `json:"host"`
	Version      string   `json:"version"`
	SHA256       string   `json:"sha256"`
	Labels       []string `json:"labels"`
	Repositories []string `json:"repositories"`
}

type registeredRunner struct {
	Name    string        `json:"name"`
	Status  string        `json:"status"`
	Version *string       `json:"version"`
	Labels  []runnerLabel `json:"labels"`
}

type runnerLabel struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

var (
	runnerOwnerPattern      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)
	runnerHostPattern       = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	runnerRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	runnerVersionPattern    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	runnerSHA256Pattern     = regexp.MustCompile(`^[a-f0-9]{64}$`)
	runnerLabelPattern      = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
)

func LoadRunnerFleet(root string) (RunnerFleet, error) {
	data, err := os.ReadFile(filepath.Join(root, "build/runners.json"))
	if err != nil {
		return RunnerFleet{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var fleet RunnerFleet
	if err := decoder.Decode(&fleet); err != nil {
		return RunnerFleet{}, fmt.Errorf("runner fleet: %w", err)
	}
	switch {
	case fleet.Schema != 1:
		return RunnerFleet{}, fmt.Errorf("runner fleet: unsupported schema %d", fleet.Schema)
	case !runnerOwnerPattern.MatchString(fleet.Owner):
		return RunnerFleet{}, fmt.Errorf("runner fleet: invalid owner %q", fleet.Owner)
	case !runnerHostPattern.MatchString(fleet.Host):
		return RunnerFleet{}, fmt.Errorf("runner fleet: invalid host %q", fleet.Host)
	case !runnerVersionPattern.MatchString(fleet.Version):
		return RunnerFleet{}, fmt.Errorf("runner fleet: invalid version %q", fleet.Version)
	case !runnerSHA256Pattern.MatchString(fleet.SHA256):
		return RunnerFleet{}, fmt.Errorf("runner fleet: invalid sha256 %q", fleet.SHA256)
	}
	if err := uniqueNames("label", fleet.Labels, runnerLabelPattern); err != nil {
		return RunnerFleet{}, err
	}
	if err := uniqueNames("repository", fleet.Repositories, runnerRepositoryPattern); err != nil {
		return RunnerFleet{}, err
	}
	return fleet, nil
}

func uniqueNames(kind string, names []string, pattern *regexp.Regexp) error {
	if len(names) == 0 {
		return fmt.Errorf("runner fleet: no %s declared", kind)
	}
	seen := map[string]bool{}
	for _, name := range names {
		if !pattern.MatchString(name) {
			return fmt.Errorf("runner fleet: invalid %s %q", kind, name)
		}
		if seen[name] {
			return fmt.Errorf("runner fleet: duplicate %s %q", kind, name)
		}
		seen[name] = true
	}
	return nil
}

func (c *Commands) runnerStates(ctx context.Context, fleet RunnerFleet) (map[string][]registeredRunner, error) {
	states := make(map[string][]registeredRunner, len(fleet.Repositories))
	var mu sync.Mutex
	runner := c.Runner
	if runner.Stderr != nil {
		runner.Stderr = &lockedWriter{mu: &mu, writer: runner.Stderr}
	}
	group, groupContext := errgroup.WithContext(ctx)
	group.SetLimit(8)
	for _, repository := range fleet.Repositories {
		group.Go(func() error {
			data, err := runner.Output(groupContext, "gh", "api", "repos/"+fleet.Owner+"/"+repository+"/actions/runners?name="+fleet.Host+"-"+repository)
			if err != nil {
				return fmt.Errorf("read runners of %s/%s: %w", fleet.Owner, repository, err)
			}
			var response struct {
				Runners []registeredRunner `json:"runners"`
			}
			if err := json.Unmarshal(data, &response); err != nil {
				return fmt.Errorf("decode runners of %s/%s: %w", fleet.Owner, repository, err)
			}
			mu.Lock()
			defer mu.Unlock()
			states[repository] = response.Runners
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return states, nil
}

func runnerDrift(fleet RunnerFleet, states map[string][]registeredRunner) []string {
	labels := slices.Sorted(slices.Values(fleet.Labels))
	var problems []string
	for _, repository := range fleet.Repositories {
		name := fleet.Host + "-" + repository
		var matches []registeredRunner
		for _, runner := range states[repository] {
			if runner.Name == name {
				matches = append(matches, runner)
			}
		}
		if len(matches) != 1 {
			problems = append(problems, fmt.Sprintf("%s/%s has %d runners named %s, want 1", fleet.Owner, repository, len(matches), name))
			continue
		}
		runner := matches[0]
		if runner.Status != "online" {
			problems = append(problems, fmt.Sprintf("%s is %s, want online", name, runner.Status))
		}
		if runner.Version == nil {
			problems = append(problems, fmt.Sprintf("%s reports no version, want %s", name, fleet.Version))
		} else if *runner.Version != fleet.Version {
			problems = append(problems, fmt.Sprintf("%s runs %s, want %s", name, *runner.Version, fleet.Version))
		}
		var custom []string
		for _, label := range runner.Labels {
			if label.Type != "read-only" {
				custom = append(custom, label.Name)
			}
		}
		slices.Sort(custom)
		if !slices.Equal(custom, labels) {
			problems = append(problems, fmt.Sprintf("%s has labels [%s], want [%s]", name, strings.Join(custom, " "), strings.Join(labels, " ")))
		}
	}
	return problems
}

func (c *Commands) verifyRunnerFleet(ctx context.Context, fleet RunnerFleet) error {
	wait, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return poll(wait, func() error {
		states, err := c.runnerStates(wait, fleet)
		if err != nil {
			return err
		}
		if problems := runnerDrift(fleet, states); len(problems) > 0 {
			return fmt.Errorf("runner fleet drift: %s", strings.Join(problems, "; "))
		}
		return nil
	})
}
