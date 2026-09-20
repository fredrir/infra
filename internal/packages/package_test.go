package packages

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/release"
)

func TestPackageConfigurationPreservesFilesModesAndFormatDependencies(t *testing.T) {
	payload := t.TempDir()
	for _, name := range []string{"tool", "LICENSE", "NOTICE"} {
		if err := os.WriteFile(filepath.Join(payload, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	config, err := NFPMConfig(release.Settings{Name: "tool", Binary: "tool", Section: "database", ExtraFiles: []string{"NOTICE"}, Optional: []string{"neovim: editor"}, Depends: map[string][]string{"arch": {"dbus"}}}, "1.2.3", "arm64", payload)
	if err != nil {
		t.Fatal(err)
	}
	destinations := map[string]int{}
	for _, entry := range config["contents"].([]map[string]any) {
		destinations[entry["dst"].(string)] = entry["file_info"].(map[string]any)["mode"].(int)
	}
	if !reflect.DeepEqual(destinations, map[string]int{"/usr/bin/tool": 0755, "/usr/share/licenses/tool/LICENSE": 0644, "/usr/share/licenses/tool/NOTICE": 0644}) {
		t.Fatalf("unexpected files %v", destinations)
	}
	deb := config["overrides"].(map[string]any)["deb"].(map[string]any)
	if !reflect.DeepEqual(deb["recommends"], []string{"neovim"}) || !reflect.DeepEqual(deb["depends"], []string{}) {
		t.Fatalf("unexpected dependencies %v", deb)
	}
	if config["arch"] != "arm64" || config["release"] != "1" || config["rpm"].(map[string]any)["signature"].(map[string]any)["key_file"] != "${RPM_SIGNING_KEY}" {
		t.Fatal("package identity or signing configuration changed")
	}
}

func TestInstallerPreservesConsumerContractAndRejectsInjection(t *testing.T) {
	tools := Tools{"tool": {Repository: "fredrir/tool", Version: "1.2.3", Binary: "tool"}}
	script, err := Installer(tools)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "tool) repository=fredrir/tool version=1.2.3 binary=tool ;;") {
		t.Fatal("missing installer entry")
	}
	for _, tool := range []Tool{{Repository: "evil/tool", Version: "1", Binary: "tool"}, {Repository: "fredrir/tool", Version: "1;id", Binary: "tool"}, {Repository: "fredrir/tool", Version: "1", Binary: "x y"}} {
		if _, err := Installer(Tools{"tool": tool}); err == nil {
			t.Fatal("accepted unsafe installer metadata")
		}
	}
}

func TestNURIndexAddsEachPackageOnce(t *testing.T) {
	index := "{ pkgs ? import <nixpkgs> { } }:\n{\n  lib = import ./lib { inherit pkgs; };\n}\n"
	updated, err := NURIndex(index, "tool")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated, "  tool = pkgs.callPackage ./pkgs/tool { };\n}") {
		t.Fatal("missing NUR package")
	}
	again, err := NURIndex(updated, "tool")
	if err != nil || again != updated {
		t.Fatal("NUR index update is not idempotent")
	}
}

func TestSmokeScopesRetainDistributionCoverage(t *testing.T) {
	quick, err := SmokeTargets("quick")
	if err != nil || len(quick) != 3 {
		t.Fatalf("quick targets %v: %v", quick, err)
	}
	full, err := SmokeTargets("full")
	if err != nil || len(full) != 7 {
		t.Fatalf("full targets %v: %v", full, err)
	}
	for _, target := range full {
		if !strings.Contains(target.Image, "@sha256:") {
			t.Fatalf("unpinned smoke image %s", target.Image)
		}
	}
	if _, err := SmokeTargets("unknown"); err == nil {
		t.Fatal("accepted unknown smoke scope")
	}
}
