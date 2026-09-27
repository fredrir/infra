package contracts

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
