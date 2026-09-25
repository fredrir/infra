package reconcile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

func junitReport(cases ...string) string {
	return `<?xml version="1.0" ?><testsuites><testsuite name="external" tests="` + fmt.Sprint(len(cases)) + `">` + strings.Join(cases, "") + `</testsuite></testsuites>`
}

func environment(options process.Options, name string) string {
	for _, entry := range slices.Backward(options.Env) {
		if value, ok := strings.CutPrefix(entry, name+"="); ok {
			return value
		}
	}
	return ""
}

func TestHostComparisonRunsEveryHostPlayInCheckMode(t *testing.T) {
	ok := `<testcase name="[fredrir-07] Configure Ubuntu hosts: ubuntu : Harden SSH authentication" classname="roles/ubuntu/tasks/main.yml:22"><system-out>{}</system-out></testcase>`
	skipped := `<testcase name="[fredrir-06] Configure K3s workers: k3s : Assign worker capabilities"><skipped message="Conditional result was False"/></testcase>`
	changed := `<testcase name="[fredrir-04] Configure host network access: firewall : Configure host input filtering"><failure message="rc=0"/></testcase>`
	failed := `<testcase name="[fredrir-09] Configure K3s workers: k3s : Wait for the node to become ready"><failure message="non-zero return code"/></testcase>`
	broken := `<testcase name="[fredrir-05] Configure Ubuntu hosts: ubuntu : Set the inventory hostname"><error message="Task failed"/></testcase>`
	for _, test := range []struct {
		name      string
		reports   []string
		exit      int
		want      []string
		unwanted  []string
		succeeded bool
	}{
		{name: "matching", reports: []string{junitReport(ok, skipped)}, succeeded: true},
		{name: "changed", reports: []string{junitReport(ok, changed)}, want: []string{"[fredrir-04] Configure host network access: firewall : Configure host input filtering"}, unwanted: []string{"Harden SSH"}},
		{name: "failed", reports: []string{junitReport(changed, failed), junitReport(changed, failed, broken)}, exit: 2, want: []string{"Wait for the node to become ready", "Set the inventory hostname", "exited 2"}},
		{name: "unreported", exit: 0, want: []string{"recorded no task results"}},
		{name: "unreadable", reports: []string{"<testsuites"}, want: []string{"host comparison report"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls [][]string
			work := t.TempDir()
			commands := Commands{Work: work, Runner: ci.Runner{Dir: "/source", Env: []string{"JUNIT_OUTPUT_DIR=/elsewhere"}, Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				calls = append(calls, options.Args)
				for _, setting := range []string{"ANSIBLE_CALLBACKS_ENABLED=ansible.builtin.junit", "JUNIT_FAIL_ON_CHANGE=true", "JUNIT_HIDE_TASK_ARGUMENTS=true", "ANSIBLE_CONFIG=/source/ansible/ansible.cfg"} {
					name, value, _ := strings.Cut(setting, "=")
					if environment(options, name) != value {
						t.Errorf("%s=%q, want %q", name, environment(options, name), value)
					}
				}
				reports := environment(options, "JUNIT_OUTPUT_DIR")
				if filepath.Dir(reports) != work {
					t.Errorf("reports written to %s outside the private work directory", reports)
				}
				for index, report := range test.reports {
					if err := os.WriteFile(filepath.Join(reports, fmt.Sprintf("reconcile-%d.xml", index)), []byte(report), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if test.exit != 0 {
					return process.Result{ExitCode: test.exit}, fmt.Errorf("ansible-playbook exited %d", test.exit)
				}
				return process.Result{}, nil
			}}}
			err := commands.compareHosts(context.Background())
			if want := [][]string{{"-i", "inventory/production.yml", "reconcile.yml", "external.yml", "--check", "--skip-tags=runners"}}; !slices.EqualFunc(calls, want, slices.Equal) {
				t.Fatalf("host comparison ran %q, want %q", calls, want)
			}
			if (err == nil) != test.succeeded {
				t.Fatalf("host comparison returned %v", err)
			}
			for _, fragment := range test.want {
				if strings.Count(fmt.Sprint(err), fragment) != 1 {
					t.Errorf("error %q does not report %q exactly once", err, fragment)
				}
			}
			for _, fragment := range test.unwanted {
				if strings.Contains(fmt.Sprint(err), fragment) {
					t.Errorf("error %q reports matching task %q", err, fragment)
				}
			}
		})
	}
}

func TestDeclarationComparisonCombinesOpenTofuAndHosts(t *testing.T) {
	for _, test := range []struct {
		name      string
		tofuExit  int
		hostDrift bool
		want      []string
		succeeded bool
	}{
		{name: "matching", succeeded: true},
		{name: "OpenTofu drift", tofuExit: 2, want: []string{"OpenTofu comparison"}},
		{name: "host drift", hostDrift: true, want: []string{"hosts differ", "Configure host input filtering"}},
		{name: "both", tofuExit: 2, hostDrift: true, want: []string{"OpenTofu comparison", "hosts differ"}},
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
				switch {
				case command == "tofu -chdir=tofu init -input=false -lockfile=readonly":
					return process.Result{}, nil
				case strings.HasPrefix(command, "tofu -chdir=tofu plan ") && strings.Contains(command, " -detailed-exitcode "):
					fmt.Fprintln(options.Stdout, "planned changes")
					if test.tofuExit != 0 {
						return process.Result{ExitCode: test.tofuExit}, errors.New("exit status 2")
					}
					return process.Result{}, nil
				case options.Name == "ansible-playbook":
					fmt.Fprintln(options.Stdout, "PLAY RECAP")
					report := junitReport()
					if test.hostDrift {
						report = junitReport(`<testcase name="[fredrir-04] Configure host network access: firewall : Configure host input filtering"><failure message="rc=0"/></testcase>`)
					}
					return process.Result{}, os.WriteFile(filepath.Join(environment(options, "JUNIT_OUTPUT_DIR"), "external.xml"), []byte(report), 0600)
				}
				t.Errorf("unexpected command %s", command)
				return process.Result{}, errors.New("unexpected command")
			}}}
			err := commands.compareDeclarations(context.Background())
			if (err == nil) != test.succeeded {
				t.Fatalf("declaration comparison returned %v", err)
			}
			for _, fragment := range test.want {
				if !strings.Contains(fmt.Sprint(err), fragment) {
					t.Errorf("error %q does not report %q", err, fragment)
				}
			}
			if len(commandsRun) != 3 {
				t.Fatalf("declaration comparison ran %q", commandsRun)
			}
			if got := stdout.String(); got != "PLAY RECAP\nplanned changes\n" {
				t.Fatalf("OpenTofu output interleaved with host output: %q", got)
			}
		})
	}
}
