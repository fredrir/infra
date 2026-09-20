package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNextVersion(t *testing.T) {
	for _, test := range []struct {
		current string
		tags    []string
		want    string
	}{
		{"0.1.13", []string{"v0.1.12", "v0.1.13", "v0.1.14-rc.1", "nightly"}, "0.1.14"},
		{"0.1.13", []string{"v0.1.13", "v0.1.20"}, "0.1.21"},
		{"0.1.13", nil, "0.1.13"}, {"0.2.0", []string{"v0.1.13"}, "0.2.0"},
	} {
		got, err := NextVersion(test.current, test.tags)
		if err != nil || got != test.want {
			t.Errorf("NextVersion(%s)=%s,%v; want %s", test.current, got, err, test.want)
		}
	}
	if _, err := NextVersion("0.2.0-beta.1", nil); err == nil {
		t.Fatal("accepted prerelease manifest")
	}
}

func TestUpdateVersionPreservesWorkspaceManifest(t *testing.T) {
	manifest := "[workspace]\nmembers = [\"a\"]\n\n[workspace.package]\nversion = \"1.2.0\" # shared\n\n[package]\nname = \"a\"\nversion.workspace = true\n"
	path := filepath.Join(t.TempDir(), "Cargo.toml")
	if err := os.WriteFile(path, []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	version, err := UpdateVersion(path, []string{"v1.2.0"})
	if err != nil || version != "1.2.1" {
		t.Fatalf("%s: %v", version, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != strings.ReplaceAll(manifest, "1.2.0", "1.2.1") {
		t.Fatalf("unexpected rewrite: %s", data)
	}
}
