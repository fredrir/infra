package contracts

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const repositorySubject = "repo:fredrir@114402558/infra@1328085692:"

type federatedIdentity struct {
	Name             string            `json:"name"`
	ClientIDVariable string            `json:"clientIdVariable"`
	Environment      string            `json:"environment"`
	Subject          string            `json:"subject"`
	Audience         string            `json:"audience"`
	Claims           map[string]string `json:"claims"`
	Scopes           []string          `json:"scopes"`
	Tags             []string          `json:"tags"`
}

type oidcToken struct {
	subject  string
	audience string
	claims   map[string]string
}

func federatedIdentities(t *testing.T) []federatedIdentity {
	t.Helper()
	var declared struct {
		Issuer     string              `json:"issuer"`
		Identities []federatedIdentity `json:"identities"`
	}
	if err := json.Unmarshal(read(t, filepath.Join(root(t), "tailscale/federated-identities.json")), &declared); err != nil {
		t.Fatal(err)
	}
	if declared.Issuer != "https://token.actions.githubusercontent.com" || len(declared.Identities) == 0 {
		t.Fatalf("federated identities trust %q with %d identities", declared.Issuer, len(declared.Identities))
	}
	return declared.Identities
}

func globMatches(pattern, value string) bool {
	parts := strings.Split(pattern, "*")
	for index, part := range parts {
		parts[index] = regexp.QuoteMeta(part)
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$").MatchString(value)
}

func (i federatedIdentity) accepts(token oidcToken) bool {
	if token.audience != i.Audience || !globMatches(i.Subject, token.subject) {
		return false
	}
	for claim, pattern := range i.Claims {
		if value, present := token.claims[claim]; !present || !globMatches(pattern, value) {
			return false
		}
	}
	return true
}

func TestFederatedIdentitiesGrantOnlyAdminOwnedTagsToThisRepository(t *testing.T) {
	policy := parseTailnetPolicy(t, read(t, filepath.Join(root(t), "tailscale/policy.hujson")))
	audiences := map[string]bool{}
	variables := map[string]bool{}
	for _, identity := range federatedIdentities(t) {
		if !strings.HasPrefix(identity.Subject, repositorySubject) {
			t.Errorf("%s trusts subject %q outside the immutable repository identity", identity.Name, identity.Subject)
		}
		if !slices.Equal(identity.Scopes, []string{"auth_keys"}) {
			t.Errorf("%s holds scopes %v", identity.Name, identity.Scopes)
		}
		for _, tag := range identity.Tags {
			if !slices.Equal(policy.TagOwners[tag], []string{"autogroup:admin"}) {
				t.Errorf("%s grants %s, owned by %v", identity.Name, tag, policy.TagOwners[tag])
			}
		}
		if audiences[identity.Audience] {
			t.Errorf("audience %s is shared", identity.Audience)
		}
		audiences[identity.Audience] = true
		variable := identity.Environment + "/" + identity.ClientIDVariable
		if variables[variable] {
			t.Errorf("client ID variable %s is shared", variable)
		}
		variables[variable] = true
	}
}

func TestEvaluatorMatchesTailscaleClaimPatterns(t *testing.T) {
	identity := federatedIdentity{Subject: repositorySubject + "ref:refs/tags/infra-v*", Audience: "audience", Claims: map[string]string{"job_workflow_ref": "fredrir/infra/.github/workflows/*@refs/heads/main"}}
	for _, test := range []struct {
		token oidcToken
		want  bool
	}{
		{oidcToken{repositorySubject + "ref:refs/tags/infra-v1.2.3", "audience", map[string]string{"job_workflow_ref": "fredrir/infra/.github/workflows/check.yml@refs/heads/main"}}, true},
		{oidcToken{repositorySubject + "ref:refs/tags/infra-v1.2.3", "other", map[string]string{"job_workflow_ref": "fredrir/infra/.github/workflows/check.yml@refs/heads/main"}}, false},
		{oidcToken{repositorySubject + "ref:refs/tags/v1", "audience", map[string]string{"job_workflow_ref": "fredrir/infra/.github/workflows/check.yml@refs/heads/main"}}, false},
		{oidcToken{repositorySubject + "ref:refs/tags/infra-v1", "audience", map[string]string{"job_workflow_ref": "fredrir/infra/.github/workflows/check.yml@refs/heads/mainline"}}, false},
		{oidcToken{repositorySubject + "ref:refs/tags/infra-v1", "audience", map[string]string{}}, false},
	} {
		if got := identity.accepts(test.token); got != test.want {
			t.Errorf("%+v accepted = %v, want %v", test.token, got, test.want)
		}
	}
}
