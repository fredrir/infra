package contracts

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
	"go.yaml.in/yaml/v3"
)

func TestImageBuildCancellationPreservesVerifiedArtifactReuse(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct{ If string }
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(root(t), ".github/workflows/build-image.yml")), &workflow); err != nil {
		t.Fatal(err)
	}
	environment, err := cel.NewEnv(
		cel.Variable("github", cel.DynType),
		cel.Variable("inputs", cel.DynType),
		cel.Variable("needs", cel.DynType),
		cel.Variable("cancelledStatus", cel.BoolType),
		cel.Function("fromJSON", cel.Overload("fromJSON_string", []*cel.Type{cel.StringType}, cel.DynType, cel.UnaryBinding(func(value ref.Val) ref.Val {
			var decoded any
			if err := json.Unmarshal([]byte(value.(types.String)), &decoded); err != nil {
				return types.NewErr("invalid JSON: %v", err)
			}
			return types.DefaultTypeAdapter.NativeToValue(decoded)
		}))),
		cel.Function("contains", cel.Overload("contains_list", []*cel.Type{cel.ListType(cel.DynType), cel.DynType}, cel.BoolType, cel.BinaryBinding(func(values, value ref.Val) ref.Val {
			return values.(traits.Container).Contains(value)
		}))),
	)
	if err != nil {
		t.Fatal(err)
	}
	condition := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(workflow.Jobs["build"].If, "${{"), "}}"))
	if !strings.Contains(condition, "cancelled()") {
		t.Fatal("build guard must explicitly handle cancellation and skipped bootstrap jobs")
	}
	condition = strings.NewReplacer("cancelled()", "cancelledStatus", "always()", "true", "inputs.cli-artifact", "inputs['cli-artifact']", "inputs.release-cli", "inputs['release-cli']").Replace(condition)
	ast, issues := environment.Compile(condition)
	if issues.Err() != nil {
		t.Fatal(issues.Err())
	}
	program, err := environment.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, repository, bootstrap, artifact, image string
		cancelled, protected, released, allowed      bool
	}{
		{"infra bootstrap", "fredrir/infra", "success", "", "", false, true, false, true},
		{"infra reused artifact", "fredrir/infra", "skipped", "verified-artifact", "", false, true, false, true},
		{"infra missing artifact", "fredrir/infra", "skipped", "", "", false, true, false, false},
		{"cancelled bootstrap", "fredrir/infra", "success", "", "", true, true, false, false},
		{"cancelled artifact reuse", "fredrir/infra", "skipped", "verified-artifact", "", true, true, false, false},
		{"consumer released CLI", "fredrir/example", "skipped", "", "", false, true, false, true},
		{"consumer bootstrap", "fredrir/example", "success", "", "", false, true, false, true},
		{"consumer artifact substitution", "fredrir/example", "skipped", "caller-artifact", "", false, true, false, false},
		{"failed consumer bootstrap", "fredrir/example", "failure", "", "", false, true, false, false},
		{"unprotected source", "fredrir/infra", "success", "", "", false, false, false, false},
		{"thin runner released CLI", "fredrir/infra", "skipped", "", "ghcr.io/fredrir/infra-runner-deploy", false, true, true, true},
		{"release CLI wrong image", "fredrir/infra", "skipped", "", "ghcr.io/fredrir/other", false, true, true, false},
		{"release CLI foreign caller", "fredrir/example", "skipped", "", "ghcr.io/fredrir/infra-runner-deploy", false, true, true, false},
		{"release CLI artifact conflict", "fredrir/infra", "skipped", "artifact", "ghcr.io/fredrir/infra-runner-deploy", false, true, true, false},
		{"cancelled release CLI", "fredrir/infra", "skipped", "", "ghcr.io/fredrir/infra-runner-deploy", true, true, true, false},
		{"unprotected release CLI", "fredrir/infra", "skipped", "", "ghcr.io/fredrir/infra-runner-deploy", false, false, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, _, err := program.Eval(map[string]any{
				"github":          map[string]any{"repository": test.repository, "repository_owner_id": "114402558", "event_name": "push", "ref": "refs/heads/main", "ref_protected": test.protected},
				"inputs":          map[string]any{"cli-artifact": test.artifact, "release-cli": test.released, "image": test.image},
				"needs":           map[string]any{"cli": map[string]any{"result": test.bootstrap}},
				"cancelledStatus": test.cancelled,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result != types.Bool(test.allowed) {
				t.Fatalf("build allowed = %v, want %v", result, test.allowed)
			}
		})
	}
}
