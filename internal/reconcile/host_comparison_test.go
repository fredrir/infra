package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

func junitReport(playbook string, cases ...string) string {
	return `<?xml version="1.0" ?><testsuites><testsuite name="` + playbook + `" tests="` + fmt.Sprint(len(cases)) + `">` + strings.Join(cases, "") + `</testsuite></testsuites>`
}

func junitCase(name, class, body string) string {
	return `<testcase name="` + name + `" classname="` + class + `">` + body + `</testcase>`
}

func junitResult(changed bool) string {
	return fmt.Sprintf(`<system-out>{&#10;"changed": %t,&#10;"msg": "ok"&#10;}</system-out>`, changed)
}

func environment(options process.Options, name string) string {
	for _, entry := range slices.Backward(options.Env) {
		if value, ok := strings.CutPrefix(entry, name+"="); ok {
			return value
		}
	}
	return ""
}

type fakePlaybooks struct {
	reports []string
	recap   string
	exit    int
}

func (f fakePlaybooks) execute(t *testing.T, options process.Options) (process.Result, error) {
	t.Helper()
	reports := environment(options, "JUNIT_OUTPUT_DIR")
	if environment(options, "ANSIBLE_CALLBACKS_ENABLED") != "ansible.builtin.junit" || environment(options, "JUNIT_HIDE_TASK_ARGUMENTS") != "true" || reports == "" {
		t.Errorf("Ansible results are not recorded: %q", options.Env)
	}
	for index, report := range f.reports {
		if err := os.WriteFile(filepath.Join(reports, fmt.Sprintf("playbook-%d.%d.xml", 1790000000+index, index)), []byte(report), 0600); err != nil {
			t.Error(err)
			return process.Result{ExitCode: 1}, err
		}
	}
	if options.Stdout != nil {
		fmt.Fprint(options.Stdout, "PLAY RECAP *****\n"+f.recap)
	}
	if f.exit != 0 {
		return process.Result{ExitCode: f.exit}, fmt.Errorf("ansible-playbook failed: exit status %d", f.exit)
	}
	return process.Result{}, nil
}

func playbookArgument(options process.Options) string {
	for _, argument := range options.Args {
		if strings.HasSuffix(argument, ".yml") && !strings.Contains(argument, "/") {
			return argument
		}
	}
	return ""
}

func TestHostComparisonClassifiesCheckModeResults(t *testing.T) {
	unchanged := junitCase("[fredrir-07] Configure Ubuntu hosts: ubuntu : Harden SSH authentication", "roles/ubuntu/tasks/main.yml:22", junitResult(false))
	skipped := junitCase("[fredrir-06] Configure K3s workers: k3s : Assign worker capabilities", "roles/k3s/tasks/main.yml:180", `<skipped message="Conditional result was False"/>`)
	changed := junitCase("[fredrir-04] Configure host network access: firewall : Configure host input filtering", "roles/firewall/tasks/main.yml:19", junitResult(true))
	failed := junitCase("[fredrir-09] Configure K3s workers: k3s : Wait for the node to become ready", "roles/k3s/tasks/main.yml:160", `<failure message="non-zero return code"/>`)
	broken := junitCase("[fredrir-05] Configure Ubuntu hosts: ubuntu : Set the inventory hostname", "roles/ubuntu/tasks/main.yml:8", `<error message="Task failed"/>`)
	included := junitCase("[include] Configure K3s workers: tailscale : Install verified transport binaries", "roles/tailscale/tasks/install.yml:9", `<system-out>included</system-out>`)
	monitored := junitCase("[fredrir-06] Configure independent infrastructure monitoring: gatus : Install monitor configuration", "roles/gatus/tasks/main.yml:40", junitResult(true))
	filtering := Difference{System: "hosts", Host: "fredrir-04", Item: "Configure host network access: firewall : Configure host input filtering"}
	monitor := Difference{System: "hosts", Host: "fredrir-06", Item: "Configure independent infrastructure monitoring: gatus : Install monitor configuration"}
	matching := fakePlaybooks{reports: []string{junitReport("external", unchanged)}}
	for _, test := range []struct {
		name        string
		reconcile   fakePlaybooks
		external    fakePlaybooks
		differences []Difference
		errors      []string
	}{
		{name: "matching", reconcile: fakePlaybooks{reports: []string{junitReport("reconcile", unchanged, skipped, included)}}, external: matching},
		{name: "changed", reconcile: fakePlaybooks{reports: []string{junitReport("reconcile", unchanged, changed)}}, external: fakePlaybooks{reports: []string{junitReport("external", unchanged, monitored)}}, differences: []Difference{filtering, monitor}},
		{name: "failed", reconcile: fakePlaybooks{reports: []string{junitReport("reconcile", changed, failed, broken)}, exit: 2}, external: fakePlaybooks{reports: []string{junitReport("external", monitored)}}, differences: []Difference{filtering, monitor}, errors: []string{"reconcile.yml comparison incomplete: host tasks failed: [fredrir-09] Configure K3s workers: k3s : Wait for the node to become ready; [fredrir-05] Configure Ubuntu hosts: ubuntu : Set the inventory hostname"}},
		{name: "unreachable", reconcile: fakePlaybooks{reports: []string{junitReport("reconcile", changed)}, recap: "fredrir-07                 : ok=40   changed=0    unreachable=0    failed=0    skipped=9\n"}, external: fakePlaybooks{reports: []string{junitReport("external")}, recap: "\x1b[0;31mfredrir-06\x1b[0m                 : \x1b[0;32mok=0\x1b[0m    changed=0    \x1b[1;31munreachable=1\x1b[0m    failed=0    skipped=0\n", exit: 4}, differences: []Difference{filtering}, errors: []string{"external.yml comparison incomplete: unreachable hosts: fredrir-06"}},
		{name: "unreported", errors: []string{"reconcile.yml was not compared: no task results recorded", "external.yml was not compared: no task results recorded"}},
		{name: "unreported failure", reconcile: fakePlaybooks{reports: []string{junitReport("reconcile", unchanged)}}, external: fakePlaybooks{exit: 1}, errors: []string{"external.yml was not compared: ansible-playbook failed: exit status 1"}},
		{name: "unreadable", reconcile: fakePlaybooks{reports: []string{"<testsuites"}}, external: matching, errors: []string{"reconcile.yml was not compared: Ansible result report playbook-1790000000.0.xml: XML syntax error on line 1: unexpected EOF"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			var calls [][]string
			work := t.TempDir()
			commands := Commands{Work: work, Runner: ci.Runner{Dir: "/source", Env: []string{"JUNIT_OUTPUT_DIR=/elsewhere"}, Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				mu.Lock()
				calls = append(calls, options.Args)
				mu.Unlock()
				if environment(options, "ANSIBLE_CONFIG") != "/source/ansible/ansible.cfg" || filepath.Dir(environment(options, "JUNIT_OUTPUT_DIR")) != work {
					t.Errorf("results recorded outside the private work directory: %q", options.Env)
				}
				return map[string]fakePlaybooks{"reconcile.yml": test.reconcile, "external.yml": test.external}[playbookArgument(options)].execute(t, options)
			}}}
			outcome := VerificationOutcome("", ScopeFull, commands.compareHosts(context.Background()))
			slices.SortFunc(calls, slices.Compare)
			if want := [][]string{{"-i", "inventory/production.yml", "external.yml", "--check", "--skip-tags=runners"}, {"-i", "inventory/production.yml", "reconcile.yml", "--check", "--skip-tags=runners"}}; !slices.EqualFunc(calls, want, slices.Equal) {
				t.Fatalf("host comparison ran %q, want %q", calls, want)
			}
			if !reflect.DeepEqual(outcome.Differences, append([]Difference{}, test.differences...)) || !reflect.DeepEqual(outcome.Errors, append([]string{}, test.errors...)) {
				t.Fatalf("host comparison reported %+v, want differences %+v and errors %q", outcome, test.differences, test.errors)
			}
			if entries, _ := os.ReadDir(work); len(entries) != 0 {
				t.Fatalf("recorded results left in the work directory: %v", entries)
			}
		})
	}
}

func TestHostComparisonKeepsPlaybookOutputsApart(t *testing.T) {
	var stdout bytes.Buffer
	release := make(chan struct{})
	commands := Commands{Work: t.TempDir(), Runner: ci.Runner{Stdout: &stdout, Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		playbook := playbookArgument(options)
		if playbook == "reconcile.yml" {
			<-release
		}
		for line := range 20 {
			fmt.Fprintf(options.Stdout, "%s %d\n", playbook, line)
			if options.Stderr != nil {
				fmt.Fprintf(options.Stderr, "%s warning %d\n", playbook, line)
			}
		}
		if playbook == "external.yml" {
			close(release)
		}
		return fakePlaybooks{reports: []string{junitReport(strings.TrimSuffix(playbook, ".yml"))}}.execute(t, options)
	}}}
	if err := commands.compareHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	output := stdout.String()
	if !strings.HasPrefix(output, "reconcile.yml 0\n") || strings.LastIndex(output, "reconcile.yml") > strings.Index(output, "external.yml") || !strings.Contains(output, "external.yml warning 19\n") {
		t.Fatalf("playbook outputs interleaved or lost:\n%s", output)
	}
}

func TestRunnerVerificationReportsRunnerPlayDifferences(t *testing.T) {
	fleet := testRunnerFleet()
	runner := junitCase("[infra-build-09] Verify dedicated build runners: build_runner : Install immutable trusted-job hook adapter", "roles/build_runner/tasks/state.yml:57", junitResult(true))
	rejected := junitCase("[infra-build-09] Verify dedicated build runners: Reject declared runner state drift", "verify-runners.yml:92", `<failure message="The build VM differs from its declared runner state"/>`)
	inactive := junitCase("[fredrir-04] Verify cluster services: Read Kubernetes service status", "verify.yml:6", `<failure message="non-zero return code"/>`)
	drift := []Difference{
		{System: "runners", Host: "infra-build-09", Item: "Verify dedicated build runners: build_runner : Install immutable trusted-job hook adapter"},
		{System: "runners", Host: "infra-build-09", Item: "Verify dedicated build runners: Reject declared runner state drift"},
	}
	service := "verify.yml comparison incomplete: host tasks failed: [fredrir-04] Verify cluster services: Read Kubernetes service status"
	for _, test := range []struct {
		name        string
		verify      []string
		runners     []string
		differences []Difference
		errors      []string
	}{
		{name: "matching", verify: []string{junitReport("verify")}, runners: []string{junitReport("verify-runners")}},
		{name: "runner drift", verify: []string{junitReport("verify")}, runners: []string{junitReport("verify-runners", runner, rejected)}, differences: drift},
		{name: "cluster service", verify: []string{junitReport("verify", inactive)}, runners: []string{junitReport("verify-runners")}, errors: []string{service}},
		{name: "cluster service and runner drift", verify: []string{junitReport("verify", inactive)}, runners: []string{junitReport("verify-runners", runner, rejected)}, differences: drift, errors: []string{service}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			queried := 0
			commands := Commands{Work: t.TempDir(), Runner: ci.Runner{Dir: writeRunnerFleet(t, fleet), Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				switch options.Name {
				case "ansible-playbook":
					reports := map[string][]string{"verify.yml": test.verify, "verify-runners.yml": test.runners}[playbookArgument(options)]
					exit := 0
					if strings.Contains(strings.Join(reports, ""), "<failure") {
						exit = 2
					}
					return fakePlaybooks{reports: reports, exit: exit}.execute(t, options)
				case "gh":
					mu.Lock()
					queried++
					mu.Unlock()
					return runnerResponse(t, healthyRunners(fleet, queriedRepository(options))...), nil
				}
				return process.Result{}, fmt.Errorf("unexpected command %s", options.Name)
			}}}
			outcome := VerificationOutcome("", ScopeCloud, commands.VerifyHosts(context.Background(), Plan{Affected: All()}))
			if !reflect.DeepEqual(outcome.Differences, append([]Difference{}, test.differences...)) || !reflect.DeepEqual(outcome.Errors, append([]string{}, test.errors...)) {
				t.Fatalf("runner verification reported %+v, want differences %+v and errors %q", outcome, test.differences, test.errors)
			}
			if queried != len(fleet.Repositories) {
				t.Fatalf("runner fleet queried %d times, want %d", queried, len(fleet.Repositories))
			}
		})
	}
}

func TestVerificationOutcomeSeparatesDifferencesFromErrors(t *testing.T) {
	hosts := Differences{{System: "hosts", Host: "fredrir-04", Item: "Configure host network access: firewall : Enable host input filtering"}}
	infrastructure := Differences{{System: "opentofu", Item: "cloudflare_dns_record.grafana update"}}
	for _, test := range []struct {
		name    string
		err     error
		outcome string
		want    Verification
	}{
		{name: "matching", want: Verification{Outcome: OutcomeMatches}},
		{name: "differences", err: errors.Join(errors.Join(nil, hosts), infrastructure), want: Verification{Outcome: OutcomeDiffers, Differences: slices.Concat(hosts, infrastructure)}},
		{name: "errors", err: errors.Join(errors.New("unreachable hosts: fredrir-06"), fmt.Errorf("verification: %v: %w", "Grafana /login returned HTTP 502", context.DeadlineExceeded)), want: Verification{Outcome: OutcomeFailed, Errors: []string{"unreachable hosts: fredrir-06", "verification: Grafana /login returned HTTP 502: context deadline exceeded"}}},
		{name: "both", err: errors.Join(errors.New("flux-system is not ready"), hosts), want: Verification{Outcome: OutcomeDiffers, Differences: hosts, Errors: []string{"flux-system is not ready"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := VerificationOutcome("abc", ScopeFull, test.err)
			want := test.want
			want.Revision, want.Scope = "abc", ScopeFull
			want.Differences = append([]Difference{}, want.Differences...)
			want.Errors = append([]string{}, want.Errors...)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("outcome %+v, want %+v", got, want)
			}
		})
	}
	if message := hosts.Error(); message != "production differs from its declaration: hosts: fredrir-04: Configure host network access: firewall : Enable host input filtering" {
		t.Fatalf("difference message %q", message)
	}
}

func TestDeclarationComparisonCombinesOpenTofuAndHosts(t *testing.T) {
	changes := `{"@level":"info","@message":"OpenTofu 1.12.6","type":"version"}
{"@level":"info","@message":"cloudflare_dns_record.grafana: Plan to update","change":{"resource":{"addr":"cloudflare_dns_record.grafana"},"action":"update"},"type":"planned_change"}
{"@level":"info","@message":"Plan: 0 to add, 1 to change, 0 to destroy.","changes":{"add":0,"change":1,"remove":0,"operation":"plan"},"type":"change_summary"}
{"@level":"info","@message":"Outputs: 2","outputs":{"grafana":{"sensitive":false,"action":"update"},"zone":{"sensitive":false,"action":"noop"}},"type":"outputs"}
`
	failure := `{"@level":"error","@message":"Error: Invalid provider configuration","diagnostic":{"severity":"error","summary":"Invalid provider configuration"},"type":"diagnostic"}
`
	for _, test := range []struct {
		name      string
		plan      string
		tofuExit  int
		hostDrift bool
		want      Verification
	}{
		{name: "matching", plan: `{"@level":"info","@message":"No changes. Your infrastructure matches the configuration.","type":"change_summary"}` + "\n", want: Verification{Outcome: OutcomeMatches}},
		{name: "OpenTofu drift", plan: changes, tofuExit: 2, want: Verification{Outcome: OutcomeDiffers, Differences: []Difference{{System: "opentofu", Item: "cloudflare_dns_record.grafana update"}, {System: "opentofu", Item: "output grafana update"}}}},
		{name: "OpenTofu summary only", plan: `{"@level":"info","@message":"Plan: 0 to add, 0 to change, 1 to destroy.","type":"change_summary"}` + "\n", tofuExit: 2, want: Verification{Outcome: OutcomeDiffers, Differences: []Difference{{System: "opentofu", Item: "Plan: 0 to add, 0 to change, 1 to destroy."}}}},
		{name: "OpenTofu failure", plan: failure, tofuExit: 1, want: Verification{Outcome: OutcomeFailed, Errors: []string{"OpenTofu comparison: Error: Invalid provider configuration"}}},
		{name: "both", plan: changes, tofuExit: 2, hostDrift: true, want: Verification{Outcome: OutcomeDiffers, Differences: []Difference{
			{System: "hosts", Host: "fredrir-04", Item: "Configure host network access: firewall : Configure host input filtering"},
			{System: "opentofu", Item: "cloudflare_dns_record.grafana update"},
			{System: "opentofu", Item: "output grafana update"},
		}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			var commandsRun []string
			var stdout bytes.Buffer
			commands := Commands{Work: t.TempDir(), Runner: ci.Runner{Stdout: &stdout, Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				mu.Lock()
				defer mu.Unlock()
				command := options.Name + " " + strings.Join(options.Args, " ")
				commandsRun = append(commandsRun, command)
				switch command {
				case "tofu -chdir=tofu init -input=false -lockfile=readonly":
					return process.Result{}, nil
				case "tofu -chdir=tofu plan -input=false -lock=false -json -detailed-exitcode -var-file=production.tfvars.json":
					if test.tofuExit != 0 {
						return process.Result{Stdout: []byte(test.plan), ExitCode: test.tofuExit}, fmt.Errorf("tofu failed: exit status %d", test.tofuExit)
					}
					return process.Result{Stdout: []byte(test.plan)}, nil
				}
				if options.Name == "ansible-playbook" {
					fmt.Fprintln(options.Stdout, "PLAY RECAP")
					drift := test.hostDrift && playbookArgument(options) == "external.yml"
					return fakePlaybooks{reports: []string{junitReport("external", junitCase("[fredrir-04] Configure host network access: firewall : Configure host input filtering", "roles/firewall/tasks/main.yml:19", junitResult(drift)))}}.execute(t, options)
				}
				t.Errorf("unexpected command %s", command)
				return process.Result{}, errors.New("unexpected command")
			}}}
			got := VerificationOutcome("", ScopeFull, commands.compareDeclarations(context.Background()))
			want := test.want
			want.Scope = ScopeFull
			want.Differences = append([]Difference{}, want.Differences...)
			want.Errors = append([]string{}, want.Errors...)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("declaration comparison reported %+v, want %+v", got, want)
			}
			if len(commandsRun) != 5 {
				t.Fatalf("declaration comparison ran %q", commandsRun)
			}
			var first struct {
				Message string `json:"@message"`
			}
			if err := json.Unmarshal([]byte(strings.SplitN(test.plan, "\n", 2)[0]), &first); err != nil {
				t.Fatal(err)
			}
			if output := stdout.String(); !strings.HasPrefix(output, "PLAY RECAP\n") || strings.Index(output, first.Message+"\n") < strings.LastIndex(output, "PLAY RECAP") {
				t.Fatalf("OpenTofu output interleaved with host output: %q", output)
			}
		})
	}
}

func TestDeclarationComparisonStreamsRealProcessOutput(t *testing.T) {
	bin := t.TempDir()
	for name, script := range map[string]string{
		"tofu": `#!/bin/sh
case "$2" in
init)
  for line in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do echo "stdout $line"; echo "stderr $line" >&2; done ;;
plan)
  for line in 1 2 3 4 5 6 7 8 9 10; do echo "warning $line" >&2; done
  echo '{"@level":"info","@message":"No changes.","type":"change_summary"}' ;;
esac
`,
		"ansible-playbook": `#!/bin/sh
for line in 1 2 3 4 5 6 7 8 9 10; do echo "TASK $line"; echo "warning $line" >&2; done
printf '<testsuites><testsuite name="reconcile"><testcase name="[fredrir-04] Configure Ubuntu hosts: ubuntu : Harden SSH authentication" classname="main.yml:1"><system-out>{"changed": false}</system-out></testcase></testsuite></testsuites>' > "$JUNIT_OUTPUT_DIR/reconcile-1.0.xml"
echo 'fredrir-04 : ok=1 changed=0 unreachable=0 failed=0 skipped=0'
`,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "ansible"), 0755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	commands := Commands{Work: t.TempDir(), Runner: ci.Runner{Dir: root, Stdout: &stdout, Stderr: &stderr}}
	if err := commands.compareDeclarations(context.Background()); err != nil {
		t.Fatalf("matching declarations reported %v\nstdout:\n%s\nstderr:\n%s", err, &stdout, &stderr)
	}
	for _, line := range []string{"stdout 20", "stderr 20", "warning 10", "No changes.", "TASK 10"} {
		if !strings.Contains(stdout.String(), line+"\n") {
			t.Errorf("combined output lost %q:\n%s", line, &stdout)
		}
	}
}

func TestDeepVerificationComparesDeclarationsDuringLiveChecks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fleet := testRunnerFleet()
	planning := make(chan struct{})
	var once sync.Once
	var stdout bytes.Buffer
	commands := Commands{Work: t.TempDir(), Runner: ci.Runner{Dir: writeRunnerFleet(t, fleet), Stdout: &stdout, Execute: func(ctx context.Context, options process.Options) (process.Result, error) {
		switch options.Name {
		case "kubectl":
			select {
			case <-planning:
				return process.Result{ExitCode: 1}, errors.New("kubectl failed: connection refused")
			case <-ctx.Done():
				return process.Result{ExitCode: 1}, ctx.Err()
			}
		case "tofu":
			if slices.Contains(options.Args, "plan") {
				once.Do(func() { close(planning) })
			}
			return process.Result{Stdout: []byte(`{"@level":"info","@message":"No changes.","type":"change_summary"}` + "\n")}, nil
		case "ansible-playbook":
			playbook := strings.TrimSuffix(playbookArgument(options), ".yml")
			return fakePlaybooks{reports: []string{junitReport(playbook, junitCase("[fredrir-04] Configure Ubuntu hosts: ubuntu : Harden SSH authentication", "roles/ubuntu/tasks/main.yml:22", junitResult(false)))}}.execute(t, options)
		case "gh":
			return runnerResponse(t, healthyRunners(fleet, queriedRepository(options))...), nil
		}
		t.Errorf("unexpected command %s", options.Name)
		return process.Result{}, errors.New("unexpected command")
	}}}
	plan := Plan{Revision: strings.Repeat("a", 40), Affected: Selection{Kubernetes: true, Ansible: true, HostScope: HostScopeRunners, Projects: []string{"portfolio"}}}
	got := VerificationOutcome("", ScopeFull, commands.VerifyFull(ctx, plan))
	want := Verification{Scope: ScopeFull, Outcome: OutcomeFailed, Differences: []Difference{}, Errors: []string{"kubectl failed: connection refused"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deep verification reported %+v, want %+v", got, want)
	}
	if !strings.Contains(stdout.String(), "No changes.\n") {
		t.Fatalf("declaration comparison output lost:\n%s", &stdout)
	}
}
