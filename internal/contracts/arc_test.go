package contracts

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

var arcCharts = []string{"gha-runner-scale-set", "gha-runner-scale-set-controller"}

type packageGroup struct {
	MatchPackageNames []string `json:"matchPackageNames"`
	GroupName         string   `json:"groupName"`
}

func TestRunnerScaleSetsUseTheControllerChartVersion(t *testing.T) {
	repository := root(t)
	versions := map[string][]string{}
	err := filepath.WalkDir(filepath.Join(repository, "platform/components"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		decoder := yaml.NewDecoder(bytes.NewReader(read(t, path)))
		for {
			var release struct {
				Kind string
				Spec struct {
					Chart struct {
						Spec struct{ Chart, Version string }
					}
				}
			}
			if err := decoder.Decode(&release); errors.Is(err, io.EOF) {
				return nil
			} else if err != nil {
				return err
			}
			if chart := release.Spec.Chart.Spec; release.Kind == "HelmRelease" && slices.Contains(arcCharts, chart.Chart) {
				relative, _ := filepath.Rel(repository, path)
				versions[chart.Version] = append(versions[chart.Version], relative+" "+chart.Chart)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatalf("ARC charts span versions %v; a scale set chart that differs from the controller marks its runner set outdated and deletes busy runners", versions)
	}
	for _, releases := range versions {
		for _, chart := range arcCharts {
			if !slices.ContainsFunc(releases, func(release string) bool { return strings.HasSuffix(release, " "+chart) }) {
				t.Fatalf("no %s release among %v", chart, releases)
			}
		}
	}
	var config struct {
		PackageRules []packageGroup `json:"packageRules"`
	}
	if err := json.Unmarshal(read(t, filepath.Join(repository, "renovate.json")), &config); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(config.PackageRules, func(rule packageGroup) bool {
		return rule.GroupName != "" && slices.Equal(slices.Sorted(slices.Values(rule.MatchPackageNames)), arcCharts)
	}) {
		t.Error("Renovate may bump the ARC controller and scale set charts in separate pull requests")
	}
}
