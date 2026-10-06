package ci

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/process"
)

func projectFixture(t *testing.T) (string, Project) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "build/projects"), 0755); err != nil {
		t.Fatal(err)
	}
	project := Project{Schema: 1, Repository: "fredrir/example", RepositoryID: "123", Inputs: ProjectInputs{Schema: 1, Shared: []string{"Cargo.lock"}, Ignored: []string{"docs/"}, Targets: map[string][]string{"example-api": {"api/"}, "example-web": {"web/"}}}, Images: []ProjectImage{{Image: "ghcr.io/fredrir/example-api", Dockerfile: "Containerfile", Timeout: 45}, {Image: "ghcr.io/fredrir/example-web", Dockerfile: "web/Dockerfile", Timeout: 45}}, Checks: []ProjectCheck{{Name: "rust", Recipe: "build/projects/example/checks.Containerfile", Timeout: 45}, {Name: "mutation", Recipe: "build/projects/example/checks.Containerfile", Timeout: 45, Scheduled: true}}}
	data, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "build/projects/example.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	return root, project
}

func TestProjectProfilesRejectRepositorySubstitutionAndUnknownFields(t *testing.T) {
	root, _ := projectFixture(t)
	if _, err := ReadProject(root, "fredrir/example", "123"); err != nil {
		t.Fatal(err)
	}
	for _, identity := range [][2]string{{"fredrir/example", "124"}, {"attacker/example", "123"}, {"fredrir/../example", "123"}, {"fredrir/missing", "123"}} {
		if _, err := ReadProject(root, identity[0], identity[1]); err == nil {
			t.Fatalf("untrusted identity accepted: %v", identity)
		}
	}
	file := filepath.Join(root, "build/projects/example.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	data = append([]byte(`{"unexpected":true,`), data[1:]...)
	if err := os.WriteFile(file, data, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadProject(root, "fredrir/example", "123"); err == nil {
		t.Fatal("unknown profile field accepted")
	}
}

func TestProjectPlanConservativelySelectsImagesAndScheduledChecks(t *testing.T) {
	_, project := projectFixture(t)
	for _, test := range []struct {
		paths string
		names []string
	}{{"api/main.rs\x00", []string{"ghcr.io/fredrir/example-api"}}, {"docs/readme.md\x00", nil}, {"Cargo.lock\x00", []string{"ghcr.io/fredrir/example-api", "ghcr.io/fredrir/example-web"}}, {"unmapped.txt\x00", []string{"ghcr.io/fredrir/example-api", "ghcr.io/fredrir/example-web"}}} {
		runner := process.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
			if options.Args[0] == "rev-parse" {
				return process.Result{Stdout: []byte(strings.Repeat("a", 40))}, nil
			}
			return process.Result{Stdout: []byte(test.paths)}, nil
		}}
		plan, err := PlanProject(context.Background(), runner, project, "HEAD", false, nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, image := range plan.Images {
			names = append(names, image.Image)
		}
		checks := 1
		if test.paths == "docs/readme.md\x00" {
			checks = 0
		}
		if !slices.Equal(names, test.names) || len(plan.Checks) != checks {
			t.Fatalf("paths %q: %+v", test.paths, plan)
		}
		plan, err = PlanProject(context.Background(), runner, project, "", true, nil)
		if err != nil || len(plan.Checks) != 2 || len(plan.Images) != 2 {
			t.Fatalf("scheduled plan: %+v, %v", plan, err)
		}
	}
}

func TestPublicBuildVariablesExcludeUnselectedSecretsAndRefuseInjection(t *testing.T) {
	response := `{"VITE_API_URL":"https://api.example.com","PRIVATE_TOKEN":"never-output"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" || r.URL.Query().Get("secrets") != "VITE_API_URL" {
			t.Error("unscoped public variable request")
		}
		io.WriteString(w, response)
	}))
	defer server.Close()
	profile := ProjectDoppler{Project: "example", Config: "prd", Public: []string{"VITE_API_URL"}}
	values, err := PublicProjectVariables(context.Background(), server.Client(), server.URL, "test-token", profile)
	if err != nil || len(values) != 1 || values["VITE_API_URL"] != "https://api.example.com" {
		t.Fatalf("public variables: %v, %v", values, err)
	}
	response = `{"VITE_API_URL":"good\nPRIVATE_TOKEN=leak"}`
	if _, err := PublicProjectVariables(context.Background(), server.Client(), server.URL, "test-token", profile); err == nil {
		t.Fatal("multiline variable accepted")
	}
}

func TestWorkflowOutputsRejectPartialMultilineWrites(t *testing.T) {
	file := filepath.Join(t.TempDir(), "output")
	if err := WriteOutputs(file, map[string]string{"safe": "yes", "unsafe": "line\nINJECTED=true"}); err == nil {
		t.Fatal("workflow injection accepted")
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial outputs written")
	}
}

func TestGeneratedFileDriftFailsBeforeDependencyAudit(t *testing.T) {
	root := t.TempDir()
	files := []string{"packages/api-client/openapi.json", "packages/api-client/src/schema.ts"}
	for _, file := range files {
		path := filepath.Join(root, file)
		os.MkdirAll(filepath.Dir(path), 0755)
		os.WriteFile(path, []byte("original"), 0644)
	}
	audited := false
	runner := process.Runner{Dir: root, Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		if options.Name == "cargo" {
			audited = true
		}
		if options.Name == "bun" {
			return process.Result{}, os.WriteFile(filepath.Join(root, files[0]), []byte("drift"), 0644)
		}
		return process.Result{}, nil
	}}
	if err := ProjectCheckCommands(context.Background(), runner, "portfolio", "quality", ""); err == nil || audited {
		t.Fatal("generated drift did not stop auditing")
	}
}

func TestDatabaseCleanupRunsAfterCheckFailureAndCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		stopped := false
		runner := process.Runner{Execute: func(ctx context.Context, options process.Options) (process.Result, error) {
			if slices.Contains(options.Args, "stop") {
				stopped = true
				if ctx.Err() != nil {
					t.Error("cleanup inherited cancelled context")
				}
			}
			return process.Result{}, nil
		}}
		failure := errors.New("check failed")
		err := withDatabase(ctx, runner, Database{User: "postgres", Name: "postgres", Port: 5432, Variables: []string{"DATABASE_URL"}}, t.TempDir(), nil, func(_ context.Context, runner process.Runner) error {
			if !slices.Contains(runner.Env, "DATABASE_URL=postgresql://postgres@127.0.0.1:5432/postgres") {
				t.Error("database URL missing")
			}
			if cancelled {
				cancel()
			}
			return failure
		})
		cancel()
		if !errors.Is(err, failure) || !stopped {
			t.Fatalf("failed check did not clean database: %v", err)
		}
	}
}

func TestParserDatabaseNameAndEnvironmentReachTests(t *testing.T) {
	var commands []process.Options
	runner := process.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		commands = append(commands, options)
		return process.Result{}, nil
	}}
	ctx := context.Background()
	if err := withDatabase(ctx, runner, Database{User: "pyparser", Name: "pyparser_ci", Port: 5433, Variables: []string{"PYPARSER_DATABASE_URL"}}, t.TempDir(), nil, func(ctx context.Context, runner process.Runner) error {
		return runner.Run(ctx, "python", "-m", "pytest")
	}); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 5 || commands[2].Name != "createdb" || !slices.Contains(commands[2].Args, "pyparser_ci") || commands[3].Name != "python" || !slices.Contains(commands[3].Env, "PYPARSER_DATABASE_URL=postgresql://pyparser@127.0.0.1:5433/pyparser_ci") {
		t.Fatalf("parser database lifecycle: %+v", commands)
	}
	if !slices.Contains(commands[0].Args, "--encoding=UTF8") || !slices.Contains(commands[0].Args, "--locale=C") {
		t.Fatal("test database depends on installed locale")
	}
}

func TestProjectPlanSeparatesRuntimeInputsFromTestChanges(t *testing.T) {
	_, project := projectFixture(t)
	inputs := project.Inputs
	inputs.Targets = make(map[string][]string)
	for target, paths := range project.Inputs.Targets {
		inputs.Targets[target] = slices.Clone(paths)
	}
	inputs.Ignored = append(slices.Clone(inputs.Ignored), "tests/")
	project.ImageInputs = &inputs
	project.Inputs.Targets["example-api"] = append(project.Inputs.Targets["example-api"], "tests/")
	for _, test := range []struct {
		path           string
		images, checks int
	}{
		{"tests/runtime_test.go", 0, 1},
		{"api/main.rs", 1, 1},
		{"docs/readme.md", 0, 0},
		{"unmapped.txt", 2, 1},
		{"Cargo.lock", 2, 1},
	} {
		t.Run(test.path, func(t *testing.T) {
			runner := process.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				if options.Args[0] == "rev-parse" {
					return process.Result{Stdout: []byte(strings.Repeat("a", 40))}, nil
				}
				return process.Result{Stdout: []byte(test.path + "\x00")}, nil
			}}
			plan, err := PlanProject(context.Background(), runner, project, "HEAD", false, nil)
			if err != nil || len(plan.Images) != test.images || len(plan.Checks) != test.checks {
				t.Fatalf("plan: %+v, %v", plan, err)
			}
		})
	}
}
