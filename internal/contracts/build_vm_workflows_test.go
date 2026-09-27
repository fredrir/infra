package contracts

import (
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestBuildVmJobsReachOnlyTheEngineAndRegistryTheirRunnerGrants(t *testing.T) {
	type step struct {
		If   string            `yaml:"if"`
		Uses string            `yaml:"uses"`
		Env  map[string]string `yaml:"env"`
		Run  string            `yaml:"run"`
	}
	for _, name := range []string{"build-image.yml", "packages-publish.yml"} {
		var workflow struct {
			Env  map[string]string
			Jobs map[string]struct {
				Env   map[string]string
				Steps []step
			}
		}
		if err := yaml.Unmarshal(read(t, filepath.Join(root(t), ".github/workflows", name)), &workflow); err != nil {
			t.Fatal(err)
		}
		environments := []map[string]string{workflow.Env}
		hostedEngines := 0
		for job, definition := range workflow.Jobs {
			environments = append(environments, definition.Env)
			for _, step := range definition.Steps {
				environments = append(environments, step.Env)
				if strings.HasPrefix(step.Uses, "docker/login-action") {
					t.Errorf("%s job %s keeps registry credentials outside the job's temporary directory", name, job)
				}
				if strings.Contains(step.Run, "DOCKER_CONFIG=") && !strings.Contains(step.Run, `"$RUNNER_TEMP/`) {
					t.Errorf("%s job %s points DOCKER_CONFIG outside RUNNER_TEMP", name, job)
				}
				if strings.Contains(step.Run, "docker run --detach --name infra-dagger") {
					hostedEngines++
					if !strings.Contains(step.If, "runner.environment == 'github-hosted'") || !strings.Contains(step.Run, `_EXPERIMENTAL_DAGGER_RUNNER_HOST=docker-container://infra-dagger\n' >> "$GITHUB_ENV"`) {
						t.Errorf("%s job %s starts an engine it does not select only on hosted runners", name, job)
					}
				}
			}
		}
		for _, environment := range environments {
			if host, ok := environment["_EXPERIMENTAL_DAGGER_RUNNER_HOST"]; ok {
				t.Errorf("%s overrides the runner's engine with %s", name, host)
			}
		}
		if hostedEngines == 0 {
			t.Errorf("%s never starts an isolated engine on hosted runners", name)
		}
	}
}
