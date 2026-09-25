package reconcile

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/go-github/v88/github"
)

func declaredRulesets(root string) (map[string]*github.RepositoryRuleset, error) {
	paths, err := filepath.Glob(filepath.Join(root, ".github", "*-ruleset.json"))
	if err != nil || len(paths) == 0 {
		return nil, cmp.Or(err, fmt.Errorf("no rulesets declared in %s", filepath.Join(root, ".github")))
	}
	declared := map[string]*github.RepositoryRuleset{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var ruleset github.RepositoryRuleset
		if err := json.Unmarshal(data, &ruleset); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		if _, duplicate := declared[ruleset.Name]; duplicate || ruleset.Name == "" {
			return nil, fmt.Errorf("%s: ruleset name %q is empty or declared twice", filepath.Base(path), ruleset.Name)
		}
		declared[ruleset.Name] = &ruleset
	}
	return declared, nil
}

func liveRulesets(ctx context.Context, client *github.Client, owner, name string) (map[string]*github.RepositoryRuleset, error) {
	listed, _, err := client.Repositories.GetAllRulesets(ctx, owner, name, &github.RepositoryListRulesetsOptions{IncludesParents: github.Ptr(false), ListOptions: github.ListOptions{PerPage: 100}})
	if err != nil {
		return nil, fmt.Errorf("list rulesets: %w", err)
	}
	live := map[string]*github.RepositoryRuleset{}
	for _, summary := range listed {
		ruleset, _, err := client.Repositories.GetRuleset(ctx, owner, name, summary.GetID(), false)
		if err != nil {
			return nil, fmt.Errorf("read ruleset %s: %w", summary.Name, err)
		}
		live[ruleset.Name] = ruleset
	}
	return live, nil
}

func (c *Commands) VerifyRulesets(ctx context.Context) error {
	publisher, err := ReadPublisher(c.Runner.Dir)
	if err != nil {
		return err
	}
	owner, name, err := publisher.repository()
	if err != nil {
		return err
	}
	declared, err := declaredRulesets(c.Runner.Dir)
	if err != nil {
		return err
	}
	client := c.GitHub
	if client == nil {
		if client, err = GitHubClient(GitHubAPI, ""); err != nil {
			return err
		}
	}
	live, err := liveRulesets(ctx, client, owner, name)
	if err != nil {
		return err
	}
	if differences := rulesetDifferences(declared, live); len(differences) > 0 {
		return differences
	}
	return nil
}

func rulesetDifferences(declared, live map[string]*github.RepositoryRuleset) Differences {
	var differences Differences
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		want, got := declared[name], live[name]
		if got == nil {
			differences = append(differences, Difference{System: "rulesets", Item: name + " missing"})
			continue
		}
		compared := []rulesetField{
			{"target", want.Target, got.Target},
			{"enforcement", want.Enforcement, got.Enforcement},
			{"conditions", want.Conditions, got.Conditions},
			{"rules", want.Rules, got.Rules},
		}
		if got.BypassActors != nil {
			compared = append(compared, rulesetField{"bypass actors", sortedActors(want.BypassActors), sortedActors(got.BypassActors)})
		}
		var fields []string
		for _, field := range compared {
			if !sameJSON(field.want, field.got) {
				fields = append(fields, field.name)
			}
		}
		if len(fields) > 0 {
			differences = append(differences, Difference{System: "rulesets", Item: name + " differs in " + strings.Join(fields, ", ")})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(live)) {
		if declared[name] == nil {
			differences = append(differences, Difference{System: "rulesets", Item: name + " undeclared"})
		}
	}
	return differences
}

type rulesetField struct {
	name      string
	want, got any
}

func sortedActors(actors []*github.BypassActor) []string {
	encoded := make([]string, 0, len(actors))
	for _, actor := range actors {
		data, err := json.Marshal(actor)
		if err != nil {
			return nil
		}
		encoded = append(encoded, string(data))
	}
	slices.Sort(encoded)
	return encoded
}

func sameJSON(a, b any) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}
