package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
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
	Schema       int            `json:"schema"`
	Owner        string         `json:"owner"`
	Host         string         `json:"host"`
	Version      string         `json:"version"`
	SHA256       string         `json:"sha256"`
	Labels       []string       `json:"labels"`
	Repositories map[string]int `json:"repositories"`
}

type fleetRunner struct {
	Repository string
	Name       string
}

func (f RunnerFleet) RepositoryNames() []string {
	return slices.Sorted(maps.Keys(f.Repositories))
}

func (f RunnerFleet) Runners() []fleetRunner {
	var runners []fleetRunner
	for _, repository := range f.RepositoryNames() {
		for index := 1; index <= f.Repositories[repository]; index++ {
			runners = append(runners, fleetRunner{Repository: repository, Name: fmt.Sprintf("%s-%d", repository, index)})
		}
	}
	return runners
}

func (f RunnerFleet) registeredName(runner fleetRunner) string {
	return f.Host + "-" + runner.Name
}

type registeredRunner struct {
	ID      int64         `json:"id"`
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
	if _, err := decoder.Token(); err != io.EOF {
		return RunnerFleet{}, fmt.Errorf("runner fleet: trailing data after declaration")
	}
	switch {
	case fleet.Schema != 2:
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
	if len(fleet.Repositories) == 0 {
		return RunnerFleet{}, fmt.Errorf("runner fleet: no repository declared")
	}
	for repository, count := range fleet.Repositories {
		if !runnerRepositoryPattern.MatchString(repository) {
			return RunnerFleet{}, fmt.Errorf("runner fleet: invalid repository %q", repository)
		}
		if count < 1 || count > maxRepositoryRunners {
			return RunnerFleet{}, fmt.Errorf("runner fleet: %s declares %d runners, want 1 to %d", repository, count, maxRepositoryRunners)
		}
	}
	return fleet, nil
}

const maxRepositoryRunners = 8

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
	runner := c.runnerAPI()
	if runner.Stderr != nil {
		runner.Stderr = &lockedWriter{mu: &mu, writer: runner.Stderr}
	}
	group, groupContext := errgroup.WithContext(ctx)
	group.SetLimit(8)
	for _, repository := range fleet.RepositoryNames() {
		group.Go(func() error {
			data, err := runner.Output(groupContext, "gh", "api", "repos/"+fleet.Owner+"/"+repository+"/actions/runners?per_page=100")
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

func namedRunners(fleet RunnerFleet, states map[string][]registeredRunner, declared fleetRunner) []registeredRunner {
	var matches []registeredRunner
	for _, runner := range states[declared.Repository] {
		if runner.Name == fleet.registeredName(declared) {
			matches = append(matches, runner)
		}
	}
	return matches
}

func customLabels(runner registeredRunner) []string {
	var custom []string
	for _, label := range runner.Labels {
		if label.Type != "read-only" {
			custom = append(custom, label.Name)
		}
	}
	slices.Sort(custom)
	return custom
}

func runnerDrift(fleet RunnerFleet, states map[string][]registeredRunner) []string {
	labels := slices.Sorted(slices.Values(fleet.Labels))
	var problems []string
	declared := map[string]bool{}
	for _, runner := range fleet.Runners() {
		name := fleet.registeredName(runner)
		declared[name] = true
		matches := namedRunners(fleet, states, runner)
		if len(matches) != 1 {
			problems = append(problems, fmt.Sprintf("%s/%s has %d runners named %s, want 1", fleet.Owner, runner.Repository, len(matches), name))
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
		if custom := customLabels(runner); !slices.Equal(custom, labels) {
			problems = append(problems, fmt.Sprintf("%s has labels [%s], want [%s]", name, strings.Join(custom, " "), strings.Join(labels, " ")))
		}
	}
	for _, repository := range fleet.RepositoryNames() {
		for _, runner := range states[repository] {
			if strings.HasPrefix(runner.Name, fleet.Host+"-") && !declared[runner.Name] {
				problems = append(problems, fmt.Sprintf("%s/%s registers undeclared runner %s", fleet.Owner, repository, runner.Name))
			}
		}
	}
	return problems
}

var (
	runnerStateWindow   = 10 * time.Second
	runnerStateInterval = 2 * time.Second
	runnerMissingPause  = 5 * time.Second
)

func pause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Commands) readRunnerStates(ctx context.Context, fleet RunnerFleet) (map[string][]registeredRunner, error) {
	read, cancel := context.WithTimeout(ctx, runnerStateWindow)
	defer cancel()
	for {
		states, err := c.runnerStates(read, fleet)
		if err == nil || pause(read, runnerStateInterval) != nil {
			return states, err
		}
	}
}

func offlineRunners(fleet RunnerFleet, states map[string][]registeredRunner) []string {
	var offline []string
	for _, runner := range fleet.Runners() {
		if slices.ContainsFunc(namedRunners(fleet, states, runner), func(registered registeredRunner) bool { return registered.Status == "offline" }) {
			offline = append(offline, runner.Name)
		}
	}
	return offline
}

func missingRunners(fleet RunnerFleet, states map[string][]registeredRunner) []string {
	var missing []string
	for _, runner := range fleet.Runners() {
		if len(namedRunners(fleet, states, runner)) == 0 {
			missing = append(missing, runner.Name)
		}
	}
	return missing
}

func (c *Commands) unregisteredRunners(ctx context.Context, fleet RunnerFleet, states map[string][]registeredRunner) []string {
	missing := missingRunners(fleet, states)
	if len(missing) == 0 || pause(ctx, runnerMissingPause) != nil {
		return nil
	}
	affected := fleet
	affected.Repositories = map[string]int{}
	for _, runner := range fleet.Runners() {
		if slices.Contains(missing, runner.Name) {
			affected.Repositories[runner.Repository] = fleet.Repositories[runner.Repository]
		}
	}
	confirmed, err := c.runnerStates(ctx, affected)
	if err != nil {
		c.warn("confirm missing runner registrations: %v", err)
		return nil
	}
	return slices.DeleteFunc(missingRunners(affected, confirmed), func(name string) bool { return !slices.Contains(missing, name) })
}

func (c *Commands) convergeRunnerLabels(ctx context.Context, fleet RunnerFleet, states map[string][]registeredRunner) {
	labels := slices.Sorted(slices.Values(fleet.Labels))
	for _, runner := range fleet.Runners() {
		matches := namedRunners(fleet, states, runner)
		if len(matches) != 1 || slices.Equal(customLabels(matches[0]), labels) {
			continue
		}
		args := []string{"api", "--method", "PUT", "--silent", fmt.Sprintf("repos/%s/%s/actions/runners/%d/labels", fleet.Owner, runner.Repository, matches[0].ID)}
		for _, label := range fleet.Labels {
			args = append(args, "-f", "labels[]="+label)
		}
		if err := c.runnerAPI().Run(ctx, "gh", args...); err != nil {
			c.warn("set labels of %s: %v", matches[0].Name, err)
		}
	}
}

func (c *Commands) convergeRunners(ctx context.Context, plan Plan, playbooks []string) error {
	fleet, err := LoadRunnerFleet(c.Runner.Dir)
	if err != nil {
		return err
	}
	converged := slices.Contains(playbooks, convergencePlaybook) || slices.Contains(playbooks, runnerPlaybook)
	states, err := c.readRunnerStates(ctx, fleet)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		c.warn("runner fleet state unavailable; converging runners without GitHub repairs: %v", err)
		if !converged {
			playbooks, c.repairedRunners = append(playbooks, runnerPlaybook), true
		}
		return ansiblePlaybooks(ctx, c.runnerAPI(), playbooks)
	}
	c.convergeRunnerLabels(ctx, fleet, states)
	repairs := map[string][]string{}
	if restart := offlineRunners(fleet, states); len(restart) > 0 {
		repairs["build_runner_restart"] = restart
	}
	if missing := c.unregisteredRunners(ctx, fleet, states); len(missing) > 0 {
		repairs["build_runner_reregister"] = missing
	}
	drift := runnerDrift(fleet, states)
	if !converged && (len(repairs) > 0 || len(drift) > 0) {
		playbooks, c.repairedRunners, converged = append(playbooks, runnerPlaybook), true, true
	}
	if !converged {
		if slices.Equal(playbooks, []string{factsPlaybook}) {
			return nil
		}
		return ansiblePlaybooks(ctx, c.Runner, playbooks)
	}
	var args []string
	if len(repairs) > 0 {
		variables, err := json.Marshal(repairs)
		if err != nil {
			return err
		}
		args = append(args, "--extra-vars", string(variables))
	}
	if len(drift) == 0 && effectiveHostScope(plan.Affected) == HostScopeRunners && slices.Equal(plan.Affected.RunnerInputs, []string{"build/cli-release.json"}) {
		args = append(args, "--tags=infra_binary")
	}
	return ansiblePlaybooks(ctx, c.runnerAPI(), playbooks, args...)
}

func (c *Commands) warn(format string, args ...any) {
	if c.Runner.Stderr != nil {
		fmt.Fprintf(c.Runner.Stderr, "warning: "+format+"\n", args...)
	}
}

func (c *Commands) verifyRunnerFleet(ctx context.Context, fleet RunnerFleet) error {
	wait, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var drift Differences
	err := poll(wait, func() error {
		states, err := c.runnerStates(wait, fleet)
		if err != nil {
			return err
		}
		drift = nil
		for _, problem := range runnerDrift(fleet, states) {
			drift = append(drift, Difference{System: "runners", Item: problem})
		}
		if len(drift) > 0 {
			return drift
		}
		return nil
	})
	if err != nil && len(drift) > 0 && ctx.Err() == nil {
		return drift
	}
	return err
}
