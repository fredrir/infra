package ci

import (
	"fmt"
	"regexp"
	"strings"
)

var revisionPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var publicArgumentPattern = regexp.MustCompile(`^((VITE_|PUBLIC_|NEXT_PUBLIC_)[A-Z0-9_]+|APP_VERSION)$`)
var rustArgumentPattern = regexp.MustCompile(`^[A-Za-z0-9_,=:/. -]*$`)

func BuildArguments(revision, input string) (map[string]string, error) {
	if !revisionPattern.MatchString(revision) {
		return nil, fmt.Errorf("invalid source revision")
	}
	arguments := map[string]string{"GIT_SHA": revision, "REVISION": revision, "CI_REVISION": revision}
	for _, line := range strings.Split(input, "\n") {
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.ContainsAny(line, "\r\x00") || !publicArgumentPattern.MatchString(name) {
			return nil, fmt.Errorf("invalid public build argument %q", name)
		}
		if _, exists := arguments[name]; exists {
			return nil, fmt.Errorf("duplicate build argument %q", name)
		}
		arguments[name] = value
	}
	return arguments, nil
}

func RustArguments(input string) ([]string, error) {
	if !rustArgumentPattern.MatchString(input) {
		return nil, fmt.Errorf("Rust arguments contain unsupported characters")
	}
	arguments := strings.Fields(input)
	for _, argument := range arguments {
		if strings.HasPrefix(argument, "--manifest-path") || strings.HasPrefix(argument, "--config") || strings.HasPrefix(argument, "-Z") {
			return nil, fmt.Errorf("Rust arguments may not set %s", argument)
		}
	}
	return arguments, nil
}
