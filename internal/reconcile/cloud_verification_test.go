package reconcile

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

func TestCloudVerificationComparesOpenTofuAndRunnerRegistrationsWithoutHostPlays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fleet := testRunnerFleet()
	var mu sync.Mutex
	queried := map[string]bool{}
	var stdout bytes.Buffer
	commands := Commands{Work: t.TempDir(), RunnerToken: "observer-token", Runner: ci.Runner{Dir: writeRunnerFleet(t, fleet), Stdout: &stdout, Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		token := slices.Contains(options.Env, "GH_TOKEN=observer-token")
		switch options.Name {
		case "kubectl":
			return process.Result{ExitCode: 1}, errors.New("kubectl failed: connection refused")
		case "tofu":
			if token {
				t.Errorf("tofu received the runner token")
			}
			if slices.Contains(options.Args, "plan") {
				return process.Result{Stdout: []byte(`{"@level":"info","@message":"cloudflare_dns_record.grafana: Plan to update","type":"planned_change","change":{"resource":{"addr":"cloudflare_dns_record.grafana"},"action":"update"}}` + "\n"), ExitCode: 2}, errors.New("exit status 2")
			}
			return process.Result{}, nil
		case "gh":
			if !token {
				t.Errorf("runner registrations were read without the runner token")
			}
			queried[queriedRepository(options)] = true
			return runnerResponse(t, healthyRunners(fleet, queriedRepository(options))...), nil
		}
		t.Errorf("cloud verification ran %s %q", options.Name, options.Args)
		return process.Result{}, errors.New("unexpected command")
	}}}
	plan := Plan{Revision: strings.Repeat("a", 40), Affected: Selection{Kubernetes: true, Ansible: true, HostScope: HostScopeFull, Projects: []string{"portfolio"}}}
	got := VerificationOutcome("", ScopeCloud, commands.VerifyCloud(ctx, plan))
	if want := []Difference{{System: "opentofu", Item: "cloudflare_dns_record.grafana update"}}; !reflect.DeepEqual(got.Differences, want) {
		t.Fatalf("cloud verification reported differences %+v, want %+v", got.Differences, want)
	}
	if len(got.Errors) == 0 || !strings.Contains(got.Errors[0], "kubectl failed: connection refused") {
		t.Fatalf("cloud verification reported errors %q", got.Errors)
	}
	if !reflect.DeepEqual(queried, map[string]bool{"infra": true, "Y": true}) {
		t.Fatalf("runner registrations queried %v", queried)
	}
	if !strings.Contains(stdout.String(), "cloudflare_dns_record.grafana: Plan to update\n") {
		t.Fatalf("OpenTofu comparison output lost:\n%s", &stdout)
	}
}
