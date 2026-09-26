package reconciler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func roleFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "ansible", "roles", "reconciler", path))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type reconcilerDefaults struct {
	Credentials []string       `yaml:"reconciler_verify_credentials"`
	Verify      map[string]any `yaml:"reconciler_verify"`
}

func roleDefaults(t *testing.T) reconcilerDefaults {
	t.Helper()
	var defaults reconcilerDefaults
	if err := yaml.Unmarshal(roleFile(t, "defaults/main.yml"), &defaults); err != nil {
		t.Fatal(err)
	}
	return defaults
}

func TestVerifyUnitDecryptsOnlyTheVerifyCredentials(t *testing.T) {
	if got := roleDefaults(t).Credentials; !slices.Equal(got, VerifyCredentials) {
		t.Fatalf("role requires verify credentials %q, supervisor reads %q", got, VerifyCredentials)
	}
	service := string(roleFile(t, "templates/infra-reconcile-verify.service.j2"))
	credentials := "%t/infra-reconcile-verify/credentials.json"
	decrypts := regexp.MustCompile(`(?m)^ExecStartPre=\+.*sops decrypt (.*)$`).FindAllStringSubmatch(service, -1)
	if len(decrypts) != 1 || decrypts[0][1] != `--extract '["verify"]' --output-type json --output `+credentials+" /etc/infra-reconcile/credentials.sops.yaml" {
		t.Fatalf("verify unit decrypts %q", decrypts)
	}
	for _, directive := range []string{
		"User=infra-verify", "RuntimeDirectory=infra-reconcile-verify", "RuntimeDirectoryMode=0700", "UMask=0077", "NoNewPrivileges=yes", "ProtectSystem=strict", "PrivateTmp=yes", "CapabilityBoundingSet=\n",
		"ExecStartPre=+/usr/bin/chown infra-verify:infra-verify " + credentials + "\n",
		"ExecStart=/usr/local/bin/infra reconcile run verify --config=/etc/infra-reconcile/verify.json --credentials=" + credentials + "\n",
	} {
		if !strings.Contains(service, directive) {
			t.Errorf("verify unit lacks %q", directive)
		}
	}
	if strings.Contains(service, "apply") || strings.Contains(service, "LoadCredential") {
		t.Error("verify unit reaches beyond the verify credentials")
	}
}

func TestRoleConfigurationMatchesTheSupervisorSchema(t *testing.T) {
	declared := roleDefaults(t).Verify
	templated := regexp.MustCompile(`\{\{.*\}\}`)
	var resolve func(any) any
	resolve = func(value any) any {
		switch value := value.(type) {
		case map[string]any:
			for key, item := range value {
				value[key] = resolve(item)
			}
		case string:
			return templated.ReplaceAllString(value, "100.64.0.1")
		}
		return value
	}
	resolve(declared)
	data, err := json.Marshal(declared)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "verify.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("role configuration: %v", err)
	}
	if config.Heartbeat != "reconciliation_verification" || config.Repository != "https://github.com/fredrir/infra.git" || config.State != "/var/lib/infra-verify" {
		t.Fatalf("role configuration %+v", config)
	}
}
