package reconciler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func unitDuration(t *testing.T, service, directive string) time.Duration {
	t.Helper()
	match := regexp.MustCompile(`(?m)^` + directive + `=(\d+)min$`).FindStringSubmatch(service)
	if match == nil {
		t.Fatalf("unit has no %s in minutes", directive)
	}
	duration, err := time.ParseDuration(match[1] + "m")
	if err != nil {
		t.Fatal(err)
	}
	return duration
}

func TestApplyUnitDecryptsOnlyTheApplyCredentialsAndOutlivesTheLockWait(t *testing.T) {
	t.Parallel()
	defaults := roleDefaults(t)
	if !slices.Equal(defaults.ApplyCredentials, ApplyCredentials) {
		t.Fatalf("role requires apply credentials %q, supervisor reads %q", defaults.ApplyCredentials, ApplyCredentials)
	}
	service := string(roleFile(t, "templates/infra-reconcile-apply.service.j2"))
	credentials := "%t/infra-reconcile-apply/credentials.json"
	decrypts := regexp.MustCompile(`(?m)^ExecStartPre=\+.*sops decrypt (.*)$`).FindAllStringSubmatch(service, -1)
	if len(decrypts) != 1 || decrypts[0][1] != `--extract '["apply"]' --output-type json --output `+credentials+" /etc/infra-reconcile/credentials.sops.yaml" {
		t.Fatalf("apply unit decrypts %q", decrypts)
	}
	for _, directive := range []string{
		"User=infra-apply\n", "Group=infra-apply\n", "RuntimeDirectory=infra-reconcile-apply\n", "RuntimeDirectoryMode=0700", "StateDirectory=infra-apply\n", "CacheDirectory=infra-apply\n", "CacheDirectoryMode=0700", "WorkingDirectory=/var/lib/infra-apply\n",
		"ExecCondition=/usr/local/bin/infra reconcile run pending --config=/etc/infra-reconcile/apply.json\n",
		"ExecStartPre=+/usr/bin/chown infra-apply:infra-apply " + credentials + "\n",
		"ExecStart=/usr/local/bin/infra reconcile run apply --config=/etc/infra-reconcile/apply.json --credentials=" + credentials + " --ssh-identity=%t/infra-reconcile-apply/ssh-identity\n",
	} {
		if !strings.Contains(service, directive) {
			t.Errorf("apply unit lacks %q", directive)
		}
	}
	if strings.Contains(service, "verify") || strings.Contains(service, "ReadWritePaths") {
		t.Error("apply unit reaches beyond its own credentials, state and cache")
	}
	verify := string(roleFile(t, "templates/infra-reconcile-verify.service.j2"))
	hardening := func(unit string) []string {
		var lines []string
		for _, line := range strings.Split(unit, "\n") {
			if regexp.MustCompile(`^(Capability|Ambient|NoNew|Private|Protect|Proc|Restrict|Lock|MemoryDeny|SystemCall|UMask|Memory(High|Max)|TasksMax)`).MatchString(line) {
				lines = append(lines, line)
			}
		}
		return lines
	}
	if !slices.Equal(hardening(service), hardening(verify)) || len(hardening(service)) < 20 {
		t.Errorf("apply unit sandbox %q differs from the verify unit's %q", hardening(service), hardening(verify))
	}
	if limit, needed := unitDuration(t, service, "TimeoutStartSec"), unitDuration(t, verify, "TimeoutStartSec")+applyDeadline+finishTimeout; limit < needed {
		t.Errorf("apply unit stops after %s, before the lock wait, apply deadline and finish of %s", limit, needed)
	}
	timer := string(roleFile(t, "templates/infra-reconcile-apply.timer.j2"))
	if !strings.Contains(timer, "OnCalendar={{ reconciler_apply_schedule }}\n") || !strings.Contains(timer, "AccuracySec=1s\n") || defaults.ApplySchedule != "*:*:0/30" || strings.Contains(timer, "Persistent") {
		t.Errorf("apply timer %q with schedule %q", timer, defaults.ApplySchedule)
	}
}

func TestApplyRoleConfigurationMatchesTheSupervisorSchema(t *testing.T) {
	t.Parallel()
	defaults := roleDefaults(t)
	declared := defaults.Apply
	runner, _ := declared["runner"].(map[string]any)
	if installation, found := runner["installation_id"]; !found || installation != nil && installation.(int) <= 0 {
		t.Fatalf("runner installation %v", installation)
	}
	runner["installation_id"] = 1
	templated := regexp.MustCompile(`\{\{.*\}\}`)
	var resolve func(any) any
	resolve = func(value any) any {
		switch value := value.(type) {
		case map[string]any:
			for key, item := range value {
				value[key] = resolve(item)
			}
		case string:
			return templated.ReplaceAllString(strings.NewReplacer("{{ reconciler_shared }}", defaults.Shared, "{{ reconciler_known_hosts }}", defaults.KnownHosts).Replace(value), "100.64.0.1")
		}
		return value
	}
	resolve(declared)
	data, err := json.Marshal(declared)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "apply.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadApplyConfig(path)
	if err != nil {
		t.Fatalf("role configuration: %v", err)
	}
	if config.Heartbeat != "reconciliation_apply" || config.State != "/var/lib/infra-apply" || config.Cache != "/var/cache/infra-apply" || config.Shared != defaults.Shared || config.KnownHosts != defaults.KnownHosts || config.Runner.AppID != 4924976 {
		t.Errorf("role configuration %+v", config)
	}
	var tasks []struct {
		Name   string         `yaml:"name"`
		Assert map[string]any `yaml:"ansible.builtin.assert"`
	}
	if err := yaml.Unmarshal(roleFile(t, "tasks/main.yml"), &tasks); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(tasks, func(task struct {
		Name   string         `yaml:"name"`
		Assert map[string]any `yaml:"ansible.builtin.assert"`
	}) bool {
		conditions, _ := task.Assert["that"].([]any)
		return slices.Contains(conditions, any("reconciler_apply.runner.installation_id is integer")) && slices.Contains(conditions, any("reconciler_apply.runner.installation_id > 0"))
	}) {
		t.Error("the role installs apply.json without a runner installation")
	}
}

type gatusAlert struct {
	FailureThreshold int `yaml:"failure-threshold"`
}

type gatusExternalEndpoint struct {
	Name      string `yaml:"name"`
	Group     string `yaml:"group"`
	Heartbeat struct {
		Interval string `yaml:"interval"`
	} `yaml:"heartbeat"`
	Alerts []gatusAlert `yaml:"alerts"`
}

func TestApplyHeartbeatOutlastsReadinessAndToleratesOneRetry(t *testing.T) {
	t.Parallel()
	template, err := os.ReadFile(filepath.Join("..", "..", "ansible", "roles", "gatus", "templates", "config.yaml.j2"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		External []gatusExternalEndpoint `yaml:"external-endpoints"`
	}
	if err := yaml.Unmarshal(regexp.MustCompile(`\{\{.*?\}\}`).ReplaceAll(template, []byte("templated")), &config); err != nil {
		t.Fatal(err)
	}
	heartbeat := roleDefaults(t).Apply["heartbeat"]
	index := slices.IndexFunc(config.External, func(endpoint gatusExternalEndpoint) bool {
		return endpoint.Group+"_"+endpoint.Name == heartbeat
	})
	if index < 0 {
		t.Fatalf("Gatus has no endpoint for heartbeat %v", heartbeat)
	}
	endpoint := config.External[index]
	interval, err := time.ParseDuration(endpoint.Heartbeat.Interval)
	lockWait := unitDuration(t, string(roleFile(t, "templates/infra-reconcile-verify.service.j2")), "TimeoutStartSec")
	if err != nil || interval <= readinessInterval+lockWait {
		t.Errorf("heartbeat interval %q does not outlast the %s readiness check behind a %s lock wait", endpoint.Heartbeat.Interval, readinessInterval, lockWait)
	}
	if len(endpoint.Alerts) == 0 || slices.ContainsFunc(endpoint.Alerts, func(alert gatusAlert) bool { return alert.FailureThreshold < 2 }) {
		t.Errorf("apply alerts %+v page on a single retry", endpoint.Alerts)
	}
}
