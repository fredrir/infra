package ci

import (
	"strings"
	"testing"
)

func TestBuildArgumentsPreserveLiteralValues(t *testing.T) {
	arguments, err := BuildArguments(strings.Repeat("a", 40), "VITE_SITE_KEY=public key\nAPP_VERSION=$(false); literal")
	if err != nil {
		t.Fatal(err)
	}
	if arguments["VITE_SITE_KEY"] != "public key" || arguments["APP_VERSION"] != "$(false); literal" || arguments["REVISION"] != strings.Repeat("a", 40) {
		t.Fatalf("unexpected arguments: %v", arguments)
	}
}

func TestBuildArgumentsRejectUnsafeInputs(t *testing.T) {
	for _, input := range []string{"TOKEN=private", "GIT_SHA=override", "REVISION=override", "CI_REVISION=override", "VITE_A=one\nVITE_A=two", "VITE_A", "VITE_A=one\r"} {
		t.Run(input, func(t *testing.T) {
			if _, err := BuildArguments(strings.Repeat("a", 40), input); err == nil {
				t.Fatal("accepted unsafe build argument")
			}
		})
	}
}

func TestRustArgumentsRejectConfigurationOverrides(t *testing.T) {
	for _, input := range []string{"--manifest-path=other/Cargo.toml", "--config net.offline=true", "-Zunstable", "--features=$(false)", "--all-features\n--offline"} {
		if _, err := RustArguments(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
	arguments, err := RustArguments("--all-features --features=one,two")
	if err != nil || len(arguments) != 2 {
		t.Fatalf("arguments %v: %v", arguments, err)
	}
}

func TestParserDependencyInputsRequireImmutableApprovedImage(t *testing.T) {
	for _, value := range []string{"models", "ghcr.io/fredrir/pyparser-dependencies@sha256:" + strings.Repeat("a", 64)} {
		if _, err := BuildArguments(strings.Repeat("b", 40), "PARSER_DEPENDENCIES="+value); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"ghcr.io/fredrir/pyparser-dependencies:latest", "ghcr.io/other/deps@sha256:" + strings.Repeat("a", 64), "models\r"} {
		if _, err := BuildArguments(strings.Repeat("b", 40), "PARSER_DEPENDENCIES="+value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}
