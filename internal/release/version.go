package release

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

var plainVersion = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)$`)
var versionLine = regexp.MustCompile(`^(\s*version\s*=\s*["'])([^"']+)(["'].*)$`)
var tableLine = regexp.MustCompile(`^\s*\[([^\[\]]+)\]\s*(#.*)?$`)

func NextVersion(current string, tags []string) (string, error) {
	manual, err := parseVersion(current)
	if err != nil {
		return "", err
	}
	var newest [3]uint64
	found := false
	for _, tag := range tags {
		if !strings.HasPrefix(tag, "v") {
			continue
		}
		version, err := parseVersion(strings.TrimPrefix(tag, "v"))
		if err != nil {
			continue
		}
		if !found || compareVersion(version, newest) > 0 {
			newest, found = version, true
		}
	}
	if !found || compareVersion(manual, newest) > 0 {
		return current, nil
	}
	if newest[2] == ^uint64(0) {
		return "", fmt.Errorf("release patch version overflow")
	}
	return fmt.Sprintf("%d.%d.%d", newest[0], newest[1], newest[2]+1), nil
}

func parseVersion(input string) ([3]uint64, error) {
	var version [3]uint64
	match := plainVersion.FindStringSubmatch(input)
	if match == nil {
		return version, fmt.Errorf("version %q is not a plain release version", input)
	}
	for index := range version {
		value, err := strconv.ParseUint(match[index+1], 10, 64)
		if err != nil {
			return version, err
		}
		version[index] = value
	}
	return version, nil
}

func compareVersion(left, right [3]uint64) int {
	for index := range left {
		if left[index] < right[index] {
			return -1
		}
		if left[index] > right[index] {
			return 1
		}
	}
	return 0
}

func UpdateVersion(path string, tags []string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var manifest struct {
		Package   struct{ Version any }
		Workspace struct{ Package struct{ Version any } }
	}
	if err := toml.Unmarshal(data, &manifest); err != nil {
		return "", fmt.Errorf("decode Cargo manifest: %w", err)
	}
	table, current := "workspace.package", manifest.Workspace.Package.Version
	if _, ok := current.(string); !ok {
		table, current = "package", manifest.Package.Version
	}
	value, ok := current.(string)
	if !ok {
		return "", fmt.Errorf("Cargo.toml has no literal release version")
	}
	next, err := NextVersion(value, tags)
	if err != nil || next == value {
		return next, err
	}
	lines := strings.SplitAfter(string(data), "\n")
	section := ""
	for index, line := range lines {
		ending := ""
		if strings.HasSuffix(line, "\n") {
			ending = "\n"
		}
		line = strings.TrimSuffix(line, "\n")
		if header := tableLine.FindStringSubmatch(line); header != nil {
			section = strings.TrimSpace(header[1])
			continue
		}
		if match := versionLine.FindStringSubmatch(line); section == table && match != nil {
			lines[index] = match[1] + next + match[3] + ending
			info, err := os.Stat(path)
			if err != nil {
				return "", err
			}
			if err := os.WriteFile(path, []byte(strings.Join(lines, "")), info.Mode().Perm()); err != nil {
				return "", err
			}
			return next, nil
		}
	}
	return "", fmt.Errorf("no version line in [%s]", table)
}
