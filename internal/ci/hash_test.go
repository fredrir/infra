package ci

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNixHashMatchesKnownDigestsAndRejectsUnsupportedFormats(t *testing.T) {
	for _, tc := range []struct {
		content  []byte
		expected string
	}{
		{nil, "0mdqa9w1p6cmli6976v4wi0sw9r4p5prkj7lzfd1877wk11c9c73"},
		{[]byte("test"), "020ay2q1av2xs4n842rb3d7vz8qms1dcb87a5yd6azaci20x11lz"},
		{make([]byte, 65537), "07z0n280p2sncvj0v28axknrpi807smbjgmxqc38s9xy657k0rij"},
	} {
		path := filepath.Join(t.TempDir(), "input")
		if err := os.WriteFile(path, tc.content, 0600); err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if err := NixHash([]string{"--type", "sha256", "--flat", "--base32", path}, &output); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(output.String()) != tc.expected {
			t.Fatalf("digest = %s, want %s", output.String(), tc.expected)
		}
	}
	for _, args := range [][]string{nil, {"--type", "sha512", "--flat", "--base32", "unused"}, {"--type", "sha256", "--base32", "unused"}} {
		if err := NixHash(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted unsupported invocation %v", args)
		}
	}
}
