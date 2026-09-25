package policy

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"go.yaml.in/yaml/v3"
)

var reflectStrings = reflect.TypeOf([]string{})

type nodeRegistration struct {
	conditions, validations []cel.Program
	variables               map[string]cel.Program
}

func loadNodeRegistration(t *testing.T) nodeRegistration {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType), cel.Variable("request", cel.DynType), cel.Variable("variables", cel.MapType(cel.StringType, cel.DynType)))
	if err != nil {
		t.Fatal(err)
	}
	compile := func(expression string) cel.Program {
		ast, issues := env.Compile(expression)
		if issues.Err() != nil {
			t.Fatalf("compile %s: %v", expression, issues.Err())
		}
		program, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		return program
	}
	policy := nodeRegistration{variables: map[string]cel.Program{}}
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "platform/components/policy/nodes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range yamlObjects(t, data) {
		if document["kind"] != "ValidatingAdmissionPolicy" {
			continue
		}
		for _, condition := range at(document, "spec", "matchConditions").([]any) {
			policy.conditions = append(policy.conditions, compile(at(condition, "expression").(string)))
		}
		for _, variable := range at(document, "spec", "variables").([]any) {
			policy.variables[at(variable, "name").(string)] = compile(at(variable, "expression").(string))
		}
		for _, validation := range at(document, "spec", "validations").([]any) {
			policy.validations = append(policy.validations, compile(at(validation, "expression").(string)))
		}
	}
	if len(policy.validations) == 0 {
		t.Fatal("node registration policy missing")
	}
	return policy
}

func (p nodeRegistration) values(t *testing.T) map[string]any {
	t.Helper()
	values := map[string]any{}
	for name, program := range p.variables {
		value, _, err := program.Eval(map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		native, err := value.ConvertToNative(reflectStrings)
		if err != nil {
			t.Fatal(err)
		}
		values[name] = native
	}
	return values
}

func (p nodeRegistration) admitted(t *testing.T, node, old object, groups ...any) bool {
	t.Helper()
	input := map[string]any{"object": node, "oldObject": old, "request": object{"userInfo": object{"username": "system:node:" + at(node, "metadata", "name").(string), "groups": groups}}, "variables": p.values(t)}
	for _, condition := range p.conditions {
		if value, _, err := condition.Eval(input); err != nil || value != types.True {
			return true
		}
	}
	for _, validation := range p.validations {
		if value, _, err := validation.Eval(input); err != nil || value != types.True {
			return false
		}
	}
	return true
}

func node(name string, taints ...any) object {
	result := object{"metadata": object{"name": name}, "spec": object{}}
	if taints != nil {
		set(result, taints, "spec", "taints")
	}
	return result
}

func TestOnlyInventoryNodesRegisterAndVolatileNodesStayTainted(t *testing.T) {
	policy := loadNodeRegistration(t)
	volatile := object{"key": "node-restriction.kubernetes.io/volatile", "value": "true", "effect": "NoSchedule"}
	kubelet := []any{"system:nodes", "system:authenticated"}
	for _, test := range []struct {
		name     string
		node     object
		old      object
		groups   []any
		admitted bool
	}{
		{name: "declared worker", node: node("fredrir-09"), groups: kubelet, admitted: true},
		{name: "undeclared node", node: node("rogue-01", volatile), groups: kubelet},
		{name: "volatile node with its taint", node: node("fredrir-10", volatile), groups: kubelet, admitted: true},
		{name: "volatile node without taints", node: node("fredrir-10"), groups: kubelet},
		{name: "volatile node with another taint effect", node: node("fredrir-10", object{"key": "node-restriction.kubernetes.io/volatile", "value": "true", "effect": "PreferNoSchedule"}), groups: kubelet},
		{name: "volatile node removing its taint", node: node("fredrir-10", object{"key": "example", "effect": "NoSchedule"}), old: node("fredrir-10", volatile), groups: kubelet},
		{name: "administrator", node: node("fredrir-10"), groups: []any{"system:masters"}, admitted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if policy.admitted(t, test.node, test.old, test.groups...) != test.admitted {
				t.Fatalf("admitted %t, want %t", !test.admitted, test.admitted)
			}
		})
	}
}

func TestNodeRegistrationDeclaresTheInventory(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "ansible/inventory/production.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	groups := map[string][]string{}
	var visit func(string, map[string]any) []string
	visit = func(name string, group map[string]any) []string {
		hosts, _ := group["hosts"].(map[string]any)
		members := slices.Collect(maps.Keys(hosts))
		children, _ := group["children"].(map[string]any)
		for child, definition := range children {
			nested, _ := definition.(map[string]any)
			members = append(members, visit(child, nested)...)
		}
		slices.Sort(members)
		groups[name] = slices.Compact(members)
		return groups[name]
	}
	visit("all", document["all"].(map[string]any))
	values := loadNodeRegistration(t).values(t)
	for variable, group := range map[string]string{"declared": "k3s_cluster", "volatile": "volatile"} {
		declared := slices.Sorted(slices.Values(values[variable].([]string)))
		if !slices.Equal(declared, groups[group]) {
			t.Errorf("node registration %s nodes %q, inventory %s %q", variable, declared, group, groups[group])
		}
	}
}
