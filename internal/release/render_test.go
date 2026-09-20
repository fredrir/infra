package release

import (
	"encoding/json"
	"strings"
	"testing"
)

func releaseMetadata(t *testing.T) Metadata {
	t.Helper()
	var metadata Metadata
	err := json.Unmarshal([]byte(`{"workspace_members":["tool"],"packages":[{"id":"tool","name":"tool-cli","description":"Run a tool","license":"MIT OR Apache-2.0","manifest_path":"/src/crates/tool-cli/Cargo.toml","targets":[{"name":"tool","kind":["bin"]}],"metadata":{"release":{"maintainer":"Example <test@example.com>","features":["vendored"],"extra-files":["NOTICE"],"depends":{"arch":["dbus"]}}}}]}`), &metadata)
	if err != nil {
		t.Fatal(err)
	}
	return metadata
}

func TestReleaseConfigurationPreservesMetadataAndTargets(t *testing.T) {
	settings, err := ReleaseSettings(releaseMetadata(t), "fredrir/tool")
	if err != nil {
		t.Fatal(err)
	}
	if settings.NixLicense != "mit" || settings.License != "MIT OR Apache-2.0" || settings.Binary != "tool" {
		t.Fatalf("unexpected settings: %+v", settings)
	}
	config, err := GoReleaser(settings, "/src", "/dist", "/sdk")
	if err != nil {
		t.Fatal(err)
	}
	builds := config["builds"].([]map[string]any)
	if len(builds) != 3 {
		t.Fatal("missing release platforms")
	}
	for _, build := range builds {
		if build["dir"] != "crates/tool-cli" || build["binary"] != "tool" || strings.Join(build["flags"].([]string), " ") != "--release --locked --features=vendored" {
			t.Fatalf("wrong build: %v", build)
		}
	}
	if config["nix"].([]map[string]any)[0]["license"] != "mit" {
		t.Fatal("wrong Nix license")
	}
	if got := config["aurs"].([]map[string]any)[0]["depends"].([]string); len(got) != 1 || got[0] != "dbus" {
		t.Fatal("missing package dependency")
	}
}

func TestReleaseConfigurationRejectsAmbiguityAndUnsafeInputs(t *testing.T) {
	metadata := releaseMetadata(t)
	duplicate := metadata.Packages[0]
	duplicate.ID, duplicate.Name = "bench", "bench"
	metadata.Packages = append(metadata.Packages, duplicate)
	metadata.Members = append(metadata.Members, "bench")
	if _, err := ReleaseSettings(metadata, "fredrir/tool"); err == nil {
		t.Fatal("accepted ambiguous release")
	}
	metadata.Metadata.Release.Package = "tool-cli"
	if _, err := ReleaseSettings(metadata, "fredrir/tool"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../secret", "/secret", "$(false)"} {
		metadata.Packages[0].Metadata.Release.ExtraFiles = []string{path}
		if _, err := ReleaseSettings(metadata, "fredrir/tool"); err == nil {
			t.Errorf("accepted %q", path)
		}
	}
	if _, err := ReleaseSettings(releaseMetadata(t), "attacker/tool"); err == nil {
		t.Fatal("accepted foreign repository")
	}
	metadata = releaseMetadata(t)
	metadata.Packages[0].Metadata.Release.Binary = "unknown"
	if _, err := ReleaseSettings(metadata, "fredrir/tool"); err == nil {
		t.Fatal("accepted unknown binary")
	}
}
