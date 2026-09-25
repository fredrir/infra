package contracts

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type renovatePin struct {
	start, end int
	groups     map[string]string
}

func TestRenovateUpdatesEveryPinWithItsDigest(t *testing.T) {
	repository := root(t)
	var config struct {
		CustomManagers []struct {
			ManagerFilePatterns []string `json:"managerFilePatterns"`
			MatchStrings        []string `json:"matchStrings"`
			DatasourceTemplate  string   `json:"datasourceTemplate"`
		} `json:"customManagers"`
		CustomDatasources map[string]json.RawMessage `json:"customDatasources"`
	}
	if err := json.Unmarshal(read(t, filepath.Join(repository, "renovate.json")), &config); err != nil {
		t.Fatal(err)
	}
	pins := func(path string) (string, []renovatePin) {
		content := string(read(t, filepath.Join(repository, path)))
		var found []renovatePin
		for _, manager := range config.CustomManagers {
			applies := false
			for _, pattern := range manager.ManagerFilePatterns {
				if len(pattern) < 2 || !strings.HasPrefix(pattern, "/") || !strings.HasSuffix(pattern, "/") {
					t.Fatalf("manager file pattern %q is not a regular expression", pattern)
				}
				applies = applies || regexp.MustCompile(pattern[1:len(pattern)-1]).MatchString(path)
			}
			if !applies {
				continue
			}
			for _, matchString := range manager.MatchStrings {
				expression := regexp.MustCompile(matchString)
				for _, match := range expression.FindAllStringSubmatchIndex(content, -1) {
					pin := renovatePin{start: match[0], end: match[1], groups: map[string]string{"datasource": manager.DatasourceTemplate}}
					for index, name := range expression.SubexpNames() {
						if name != "" && match[2*index] >= 0 {
							pin.groups[name] = content[match[2*index]:match[2*index+1]]
						}
					}
					if _, ok := config.CustomDatasources[strings.TrimPrefix(pin.groups["datasource"], "custom.")]; !ok || !strings.HasPrefix(pin.groups["datasource"], "custom.") {
						t.Errorf("%s pins %s through %q, which is not a declared custom datasource", path, pin.groups["currentValue"], pin.groups["datasource"])
					}
					if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(pin.groups["currentDigest"]) {
						t.Errorf("%s pins %s without a sha256 digest", path, pin.groups["currentValue"])
					}
					found = append(found, pin)
				}
			}
		}
		return content, found
	}
	var fleet struct {
		Version string `json:"version"`
		SHA256  string `json:"sha256"`
	}
	if err := json.Unmarshal(read(t, filepath.Join(repository, "build/runners.json")), &fleet); err != nil {
		t.Fatal(err)
	}
	if _, runner := pins("build/runners.json"); len(runner) != 1 || runner[0].groups["currentValue"] != fleet.Version || runner[0].groups["currentDigest"] != fleet.SHA256 {
		t.Errorf("Renovate does not update the declared runner version and archive digest together: %+v", runner)
	}
	containerfiles, err := filepath.Glob(filepath.Join(repository, "images/*/Containerfile"))
	if err != nil || len(containerfiles) == 0 {
		t.Fatalf("Containerfiles unavailable: %v", err)
	}
	annotated := regexp.MustCompile(`(?m)^# renovate:.*$`)
	digestArgument := regexp.MustCompile(`(?m)^ARG (\S+_SHA256)=.*$`)
	for _, path := range containerfiles {
		relative, err := filepath.Rel(repository, path)
		if err != nil {
			t.Fatal(err)
		}
		content, found := pins(relative)
		covered := func(offset int) bool {
			for _, pin := range found {
				if pin.start <= offset && offset < pin.end {
					return true
				}
			}
			return false
		}
		for _, line := range annotated.FindAllStringIndex(content, -1) {
			if !covered(line[0]) {
				t.Errorf("%s: Renovate does not update %q with a digest", relative, content[line[0]:line[1]])
			}
		}
		for _, argument := range digestArgument.FindAllStringSubmatchIndex(content, -1) {
			name := content[argument[2]:argument[3]]
			if !covered(argument[0]) {
				t.Errorf("%s: Renovate does not update %s", relative, name)
			}
			if !strings.Contains(content[argument[1]:], `"$`+name+`"`) {
				t.Errorf("%s: %s is never checked", relative, name)
			}
		}
	}
}
