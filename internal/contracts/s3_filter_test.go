package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestS3FilterTestBuildsCaddyWithTheImagePins(t *testing.T) {
	repository := root(t)
	containerfile := string(read(t, filepath.Join(repository, "images/caddy/Containerfile")))
	command := regexp.MustCompile(`go get ([^\\&\n]+)`).FindStringSubmatch(containerfile)
	if command == nil {
		t.Fatal("images/caddy/Containerfile pins no modules")
	}
	module := string(read(t, filepath.Join(repository, "platform/components/object-store/filtertest/go.mod")))
	pins := strings.Fields(command[1])
	if len(pins) < 5 {
		t.Fatalf("images/caddy/Containerfile pins %v", pins)
	}
	for _, pin := range pins {
		path, version, ok := strings.Cut(pin, "@")
		if !ok {
			t.Fatalf("unpinned module %s in images/caddy/Containerfile", pin)
		}
		required := regexp.MustCompile(`(?m)^(?:require )?\s*` + regexp.QuoteMeta(path) + ` (\S+)`).FindStringSubmatch(module)
		if required == nil || required[1] != version {
			t.Errorf("filter test module requires %s %v, image builds %s", path, required, version)
		}
	}
}

func TestS3FilterChangesRunTheBehaviouralTest(t *testing.T) {
	repository := root(t)
	sum := sha256.Sum256(read(t, filepath.Join(repository, "platform/components/object-store/s3-filter.caddyfile")))
	want := hex.EncodeToString(sum[:]) + "  s3-filter.caddyfile\n"
	if recorded := string(read(t, filepath.Join(repository, "platform/components/object-store/filtertest/s3-filter.caddyfile.sha256"))); recorded != want {
		t.Fatalf("filtertest/s3-filter.caddyfile.sha256 records %q, the Caddyfile hashes to %q; rerun the filter test and update the digest", recorded, want)
	}
	var workflow struct {
		On map[string]struct {
			Paths []string `yaml:"paths"`
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, ".github/workflows/s3-filter.yml")), &workflow); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"push", "pull_request"} {
		if !slices.Contains(workflow.On[event].Paths, "platform/components/object-store/filtertest/**") {
			t.Errorf("s3-filter.yml does not run on %s changes to filtertest/", event)
		}
	}
}
