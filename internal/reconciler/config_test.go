package reconciler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig() Config {
	return Config{
		Repository: "https://github.com/fredrir/infra.git",
		State:      "/var/lib/infra-verify",
		Bucket:     "llunde-pyparser-bucket",
		Prefix:     "reconciliation/production",
		Region:     "eu-north-1",
		Gatus:      "http://100.86.241.75:8080",
		Heartbeat:  "reconciliation_verification",
		Kubernetes: Kubernetes{Server: "https://100.115.121.9:6443", CertificateAuthority: "/etc/infra-reconcile/kubernetes-ca.crt"},
		Observer:   Observer{AppID: 5080610, InstallationID: 164992211, API: "https://api.github.com"},
	}
}

func writeConfig(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "verify.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigAcceptsTheDeclaredShape(t *testing.T) {
	want := validConfig()
	got, err := LoadConfig(writeConfig(t, want))
	if err != nil || got != want {
		t.Fatalf("loaded %+v, %v; want %+v", got, err, want)
	}
	local := validConfig()
	local.Repository, local.Endpoint = "git://10.0.2.2:9418/infra.git", "http://10.0.2.2:9000"
	if _, err := LoadConfig(writeConfig(t, local)); err != nil {
		t.Fatalf("local qualification configuration rejected: %v", err)
	}
}

func TestLoadConfigRejectsInvalidSettings(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"repository credentials":  func(c *Config) { c.Repository = "https://token@github.com/fredrir/infra.git" },
		"repository scheme":       func(c *Config) { c.Repository = "ssh://github.com/fredrir/infra.git" },
		"relative state":          func(c *Config) { c.State = "var/lib/infra-verify" },
		"unclean state":           func(c *Config) { c.State = "/var/lib/../infra-verify" },
		"bucket":                  func(c *Config) { c.Bucket = "Bucket_Name" },
		"prefix":                  func(c *Config) { c.Prefix = "/reconciliation" },
		"region":                  func(c *Config) { c.Region = "north" },
		"endpoint":                func(c *Config) { c.Endpoint = "s3.local" },
		"gatus":                   func(c *Config) { c.Gatus = "" },
		"heartbeat":               func(c *Config) { c.Heartbeat = "reconciliation/verification" },
		"plain kubernetes server": func(c *Config) { c.Kubernetes.Server = "http://100.115.121.9:6443" },
		"relative authority":      func(c *Config) { c.Kubernetes.CertificateAuthority = "kubernetes-ca.crt" },
		"observer app":            func(c *Config) { c.Observer.AppID = 0 },
		"observer installation":   func(c *Config) { c.Observer.InstallationID = -1 },
		"observer api":            func(c *Config) { c.Observer.API = "api.github.com" },
	} {
		t.Run(name, func(t *testing.T) {
			config := validConfig()
			change(&config)
			if _, err := LoadConfig(writeConfig(t, config)); err == nil {
				t.Fatalf("accepted %+v", config)
			}
		})
	}
}

func TestLoadConfigRejectsUnknownAndTrailingData(t *testing.T) {
	path := writeConfig(t, map[string]any{"repository": "https://github.com/fredrir/infra.git", "deep": true})
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field returned %v", err)
	}
	data, err := json.Marshal(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, "{}"...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("trailing data returned %v", err)
	}
}
