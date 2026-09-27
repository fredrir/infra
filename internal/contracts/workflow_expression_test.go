package contracts

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"cel.dev/cel-go/cel"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/types"
)

var (
	statusFunction     = regexp.MustCompile(`\b(success|failure|cancelled|always)\(\)`)
	hyphenatedProperty = regexp.MustCompile(`\.([A-Za-z_][A-Za-z0-9_]*(?:-[A-Za-z0-9_]+)+)`)
)

type workflowExpression struct{ root celast.Expr }

func unwrapExpression(expression string) string {
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(expression), "${{"), "}}"))
}

func workflowValue(t *testing.T, expression string) workflowExpression {
	t.Helper()
	expression = strings.NewReplacer("success()", "(!failedStatus && !cancelledStatus)", "failure()", "failedStatus", "cancelled()", "cancelledStatus", "always()", "true").Replace(unwrapExpression(expression))
	expression = hyphenatedProperty.ReplaceAllString(expression, "['$1']")
	environment, err := cel.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	parsed, issues := environment.Parse(expression)
	if issues.Err() != nil {
		t.Fatalf("%s: %v", expression, issues.Err())
	}
	return workflowExpression{root: parsed.NativeRep().Expr()}
}

func workflowCondition(t *testing.T, condition string) workflowExpression {
	t.Helper()
	expression := unwrapExpression(condition)
	if !statusFunction.MatchString(expression) {
		expression = "success() && (" + expression + ")"
	}
	return workflowValue(t, expression)
}

func (e workflowExpression) allows(context map[string]any) (bool, error) {
	value, err := evaluateWorkflow(e.root, context)
	return truthy(value), err
}

func (e workflowExpression) value(context map[string]any) (any, error) {
	return evaluateWorkflow(e.root, context)
}

func evaluateWorkflow(expression celast.Expr, context map[string]any) (any, error) {
	switch expression.Kind() {
	case celast.LiteralKind:
		switch literal := expression.AsLiteral().(type) {
		case types.String:
			return string(literal), nil
		case types.Bool:
			return bool(literal), nil
		case types.Int:
			return float64(literal), nil
		case types.Double:
			return float64(literal), nil
		case types.Null:
			return nil, nil
		}
		return nil, fmt.Errorf("unsupported literal %v", expression.AsLiteral())
	case celast.IdentKind:
		return property(context, expression.AsIdent()), nil
	case celast.SelectKind:
		operand, err := evaluateWorkflow(expression.AsSelect().Operand(), context)
		return property(operand, expression.AsSelect().FieldName()), err
	case celast.CallKind:
		return callWorkflow(expression.AsCall(), context)
	}
	return nil, fmt.Errorf("unsupported expression kind %v", expression.Kind())
}

func callWorkflow(call celast.CallExpr, context map[string]any) (any, error) {
	if call.IsMemberFunction() {
		return nil, fmt.Errorf("unsupported member call %s", call.FunctionName())
	}
	left, err := evaluateWorkflow(call.Args()[0], context)
	if err != nil {
		return nil, err
	}
	switch call.FunctionName() {
	case operators.LogicalAnd:
		if !truthy(left) {
			return left, nil
		}
		return evaluateWorkflow(call.Args()[1], context)
	case operators.LogicalOr:
		if truthy(left) {
			return left, nil
		}
		return evaluateWorkflow(call.Args()[1], context)
	case operators.LogicalNot:
		return !truthy(left), nil
	case "fromJSON":
		var decoded any
		return decoded, json.Unmarshal([]byte(workflowString(left)), &decoded)
	}
	right, err := evaluateWorkflow(call.Args()[1], context)
	if err != nil {
		return nil, err
	}
	switch call.FunctionName() {
	case operators.Equals:
		return looselyEqual(left, right), nil
	case operators.NotEquals:
		return !looselyEqual(left, right), nil
	case operators.Index:
		return property(left, workflowString(right)), nil
	case "contains":
		if items, ok := left.([]any); ok {
			for _, item := range items {
				if looselyEqual(item, right) {
					return true, nil
				}
			}
			return false, nil
		}
		return strings.Contains(strings.ToLower(workflowString(left)), strings.ToLower(workflowString(right))), nil
	case "startsWith":
		return strings.HasPrefix(strings.ToLower(workflowString(left)), strings.ToLower(workflowString(right))), nil
	}
	return nil, fmt.Errorf("unsupported function %s", call.FunctionName())
}

func property(value any, name string) any {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for key, item := range object {
		if strings.EqualFold(key, name) {
			if number, ok := item.(int); ok {
				return float64(number)
			}
			return item
		}
	}
	return nil
}

func truthy(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case float64:
		return value != 0 && !math.IsNaN(value)
	case string:
		return value != ""
	}
	return true
}

func looselyEqual(left, right any) bool {
	switch left := left.(type) {
	case nil:
		if right == nil {
			return true
		}
	case string:
		if right, ok := right.(string); ok {
			return strings.EqualFold(left, right)
		}
	case bool:
		if right, ok := right.(bool); ok {
			return left == right
		}
	case float64:
		if right, ok := right.(float64); ok {
			return left == right
		}
	case map[string]any, []any:
		return false
	}
	return workflowNumber(left) == workflowNumber(right)
}

func workflowNumber(value any) float64 {
	switch value := value.(type) {
	case nil:
		return 0
	case bool:
		if value {
			return 1
		}
		return 0
	case float64:
		return value
	case string:
		if strings.TrimSpace(value) == "" {
			return 0
		}
		if number, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
			return number
		}
	}
	return math.NaN()
}

func workflowString(value any) string {
	switch value := value.(type) {
	case nil:
		return ""
	case string:
		return value
	case bool:
		return strconv.FormatBool(value)
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestWorkflowExpressionsFollowGitHubSemantics(t *testing.T) {
	context := map[string]any{"github": map[string]any{"ref": "refs/heads/main", "event_name": "push", "ref_protected": true, "run_attempt": 1}}
	for expression, want := range map[string]any{
		"github.ref_protected && 'writer' || 'reader'":                          "writer",
		"github.missing && 'writer' || 'reader'":                                "reader",
		"github.missing && 'writer' || ''":                                      "",
		"github.REF == 'REFS/HEADS/MAIN'":                                       true,
		"github.run_attempt == '1'":                                             true,
		"github.event_name != 'pull_request' && github.event.pull_request.head": nil,
		`contains(fromJSON('["push","workflow_dispatch"]'), github.event_name)`: true,
		`contains(fromJSON('["pull_request"]'), github.event_name)`:             false,
		"startsWith(github.ref, 'refs/heads/')":                                 true,
		"!github.ref_protected || always()":                                     true,
		"steps.repository-cache.outputs.cache-hit != 'true'":                    true,
	} {
		got, err := workflowValue(t, "${{ "+expression+" }}").value(context)
		if err != nil || got != want {
			t.Errorf("%s = %#v (%v), want %#v", expression, got, err, want)
		}
	}
}
