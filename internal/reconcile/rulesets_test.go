package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"github.com/google/go-github/v88/github"
)

const repositoryRoot = "../.."

func TestProductionRulesetsAdmitOnlyThePublisherFastForward(t *testing.T) {
	publisher, err := ReadPublisher(repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	if publisher.Repository != "fredrir/infra" || publisher.AppID != 5079532 || publisher.InstallationID != 164968284 || publisher.Remote != "https://github.com/fredrir/infra.git" {
		t.Fatalf("publisher %+v", publisher)
	}
	declared, err := declaredRulesets(repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	production := &github.RepositoryRulesetConditions{RefName: &github.RepositoryRulesetRefConditionParameters{Include: []string{"refs/heads/" + publishedBranch}, Exclude: []string{}}}
	for name, want := range map[string]github.RepositoryRuleset{
		"production": {
			Target:       github.Ptr(github.RulesetTargetBranch),
			Enforcement:  github.RulesetEnforcementActive,
			Conditions:   production,
			BypassActors: []*github.BypassActor{{ActorID: github.Ptr(publisher.AppID), ActorType: github.Ptr(github.BypassActorTypeIntegration), BypassMode: github.Ptr(github.BypassModeAlways)}},
			Rules:        &github.RepositoryRulesetRules{Creation: &github.EmptyRuleParameters{}, Update: &github.UpdateRuleParameters{}},
		},
		"production-history": {
			Target:       github.Ptr(github.RulesetTargetBranch),
			Enforcement:  github.RulesetEnforcementActive,
			Conditions:   production,
			BypassActors: []*github.BypassActor{},
			Rules:        &github.RepositoryRulesetRules{Deletion: &github.EmptyRuleParameters{}, NonFastForward: &github.EmptyRuleParameters{}},
		},
	} {
		want.Name = name
		got := declared[name]
		if got == nil || !sameJSON(want, got) || got.BypassActors == nil {
			t.Errorf("ruleset %s declares %s, want %s", name, encoded(t, got), encoded(t, want))
		}
	}
	for name, ruleset := range declared {
		if name == "production" || name == "production-history" {
			continue
		}
		if ruleset.Conditions != nil && ruleset.Conditions.RefName != nil && slices.ContainsFunc(ruleset.Conditions.RefName.Include, func(ref string) bool { return strings.Contains(ref, publishedBranch) || ref == "~ALL" }) {
			t.Errorf("ruleset %s also governs %s", name, publishedBranch)
		}
		if slices.ContainsFunc(ruleset.BypassActors, func(actor *github.BypassActor) bool { return actor.GetActorID() == publisher.AppID }) {
			t.Errorf("ruleset %s lets the publisher App bypass it", name)
		}
	}
	if main := declared["main"]; main == nil || !slices.Equal(main.Conditions.RefName.Include, []string{"refs/heads/main"}) {
		t.Fatal("main ruleset is not declared")
	}
}

func encoded(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type rulesetAPI struct {
	rulesets map[int64]map[string]any
	failure  int
}

func (a *rulesetAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a.failure != 0 {
		w.WriteHeader(a.failure)
		json.NewEncoder(w).Encode(map[string]string{"message": "unavailable"})
		return
	}
	if r.URL.Query().Get("includes_parents") != "false" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if r.URL.Path == "/repos/fredrir/infra/rulesets" {
		var summaries []map[string]any
		for _, id := range slices.Sorted(maps.Keys(a.rulesets)) {
			summaries = append(summaries, map[string]any{"id": id, "name": a.rulesets[id]["name"], "enforcement": a.rulesets[id]["enforcement"]})
		}
		json.NewEncoder(w).Encode(summaries)
		return
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/repos/fredrir/infra/rulesets/"), 10, 64)
	if ruleset, ok := a.rulesets[id]; err == nil && ok {
		json.NewEncoder(w).Encode(ruleset)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func declaredDocuments(t *testing.T, bypassVisible bool) map[int64]map[string]any {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(repositoryRoot, ".github", "*-ruleset.json"))
	if err != nil {
		t.Fatal(err)
	}
	documents := map[int64]map[string]any{}
	for index, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		id := int64(100 + index)
		document["id"], document["source_type"], document["source"] = id, "Repository", "fredrir/infra"
		if !bypassVisible {
			delete(document, "bypass_actors")
		}
		for _, entry := range document["rules"].([]any) {
			rule := entry.(map[string]any)
			switch rule["type"] {
			case "pull_request":
				parameters := rule["parameters"].(map[string]any)
				parameters["required_reviewers"], parameters["require_extra_approval_for_unattributed_changes"] = []any{}, true
			case "update":
				delete(rule, "parameters")
			}
		}
		documents[id] = document
	}
	return documents
}

func named(documents map[int64]map[string]any, name string) map[string]any {
	for _, document := range documents {
		if document["name"] == name {
			return document
		}
	}
	return nil
}

func TestLiveRulesetsMatchTheirDeclaration(t *testing.T) {
	for _, test := range []struct {
		name    string
		visible bool
		change  func(map[int64]map[string]any)
		want    Differences
		failure int
	}{
		{name: "repository token view"},
		{name: "administrator view", visible: true},
		{name: "administrator bypass", visible: true, change: func(documents map[int64]map[string]any) {
			production := named(documents, "production")
			production["bypass_actors"] = append(production["bypass_actors"].([]any), map[string]any{"actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "always"})
		}, want: Differences{{System: "rulesets", Item: "production differs in bypass actors"}}},
		{name: "bypass on the history ruleset", visible: true, change: func(documents map[int64]map[string]any) {
			named(documents, "production-history")["bypass_actors"] = []any{map[string]any{"actor_id": 5079532, "actor_type": "Integration", "bypass_mode": "always"}}
		}, want: Differences{{System: "rulesets", Item: "production-history differs in bypass actors"}}},
		{name: "canary conditions", change: func(documents map[int64]map[string]any) {
			for _, name := range []string{"production", "production-history"} {
				named(documents, name)["conditions"] = map[string]any{"ref_name": map[string]any{"include": []any{"refs/heads/production", "refs/heads/production-canary"}, "exclude": []any{}}}
			}
		}, want: Differences{{System: "rulesets", Item: "production differs in conditions"}, {System: "rulesets", Item: "production-history differs in conditions"}}},
		{name: "deletion allowed", change: func(documents map[int64]map[string]any) {
			named(documents, "production-history")["rules"] = []any{map[string]any{"type": "non_fast_forward"}}
		}, want: Differences{{System: "rulesets", Item: "production-history differs in rules"}}},
		{name: "evaluate only", change: func(documents map[int64]map[string]any) {
			named(documents, "production")["enforcement"] = "evaluate"
		}, want: Differences{{System: "rulesets", Item: "production differs in enforcement"}}},
		{name: "deleted and added", change: func(documents map[int64]map[string]any) {
			for id, document := range documents {
				if document["name"] == "production-history" {
					delete(documents, id)
				}
			}
			documents[1] = map[string]any{"id": 1, "name": "legacy", "target": "branch", "enforcement": "active", "rules": []any{}}
		}, want: Differences{{System: "rulesets", Item: "production-history missing"}, {System: "rulesets", Item: "legacy undeclared"}}},
		{name: "API unavailable", failure: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &rulesetAPI{rulesets: declaredDocuments(t, test.visible), failure: test.failure}
			if test.change != nil {
				test.change(api.rulesets)
			}
			server := httptest.NewServer(api)
			defer server.Close()
			client, err := GitHubClient(server.URL, "repository-token")
			if err != nil {
				t.Fatal(err)
			}
			err = (&Commands{Runner: ci.Runner{Dir: repositoryRoot}, GitHub: client}).VerifyRulesets(context.Background())
			var differences Differences
			switch {
			case test.failure != 0:
				if err == nil || errors.As(err, &differences) || !strings.Contains(err.Error(), "list rulesets") {
					t.Fatalf("unavailable API reported %v", err)
				}
			case test.want == nil && err != nil:
				t.Fatalf("matching rulesets reported %v", err)
			case test.want != nil && (!errors.As(err, &differences) || !reflect.DeepEqual(differences, test.want)):
				t.Fatalf("differences %v, want %v", err, test.want)
			}
		})
	}
}
