package contracts

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

func TestReconcilerApplyTokenCoversExactlyTheManagedZones(t *testing.T) {
	repository := root(t)
	var settings struct {
		Zones map[string]string `json:"platform_dns_zones"`
	}
	if err := json.Unmarshal(read(t, filepath.Join(repository, "tofu/production.tfvars.json")), &settings); err != nil {
		t.Fatal(err)
	}
	managed := slices.Collect(maps.Values(settings.Zones))
	for _, match := range regexp.MustCompile(`(?m)^\s*zone_id\s*=\s*"([0-9a-f]{32})"`).FindAllStringSubmatch(string(read(t, filepath.Join(repository, "tofu/cloudflare.tf"))), -1) {
		managed = append(managed, match[1])
	}
	block := regexp.MustCompile(`(?s)managed_zones\s*=\s*\{(.*?)\}`).FindStringSubmatch(string(read(t, filepath.Join(repository, "tofu/reconciler/credentials.tf"))))
	if block == nil {
		t.Fatal("tofu/reconciler/credentials.tf declares no managed_zones")
	}
	var granted []string
	for _, match := range regexp.MustCompile(`"([0-9a-f]{32})"`).FindAllStringSubmatch(block[1], -1) {
		granted = append(granted, match[1])
	}
	slices.Sort(managed)
	slices.Sort(granted)
	if managed = slices.Compact(managed); !slices.Equal(granted, managed) {
		t.Fatalf("the reconciler's apply token edits zones %v; the fleet root manages %v", granted, managed)
	}
}
