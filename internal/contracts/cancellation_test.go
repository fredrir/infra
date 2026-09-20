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
	condition = strings.NewReplacer("cancelled()", "cancelledStatus", "always()", "true", "inputs.cli-artifact", "inputs['cli-artifact']").Replace(condition)
	ast, issues := environment.Compile(condition)
	if issues.Err() != nil {
		t.Fatal(issues.Err())
	}
	program, err := environment.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, repository, bootstrap, artifact string
		cancelled, protected, allowed         bool
	}{
		{"infra bootstrap", "fredrir/infra", "success", "", false, true, true},
		{"infra reused artifact", "fredrir/infra", "skipped", "verified-artifact", false, true, true},
		{"infra missing artifact", "fredrir/infra", "skipped", "", false, true, false},
		{"cancelled bootstrap", "fredrir/infra", "success", "", true, true, false},
		{"cancelled artifact reuse", "fredrir/infra", "skipped", "verified-artifact", true, true, false},
		{"consumer released CLI", "fredrir/example", "skipped", "", false, true, true},
		{"consumer bootstrap", "fredrir/example", "success", "", false, true, true},
		{"consumer artifact substitution", "fredrir/example", "skipped", "caller-artifact", false, true, false},
		{"failed consumer bootstrap", "fredrir/example", "failure", "", false, true, false},
		{"unprotected source", "fredrir/infra", "success", "", false, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, _, err := program.Eval(map[string]any{
				"github":          map[string]any{"repository": test.repository, "repository_owner_id": "114402558", "event_name": "push", "ref": "refs/heads/main", "ref_protected": test.protected},
				"inputs":          map[string]any{"cli-artifact": test.artifact},
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
