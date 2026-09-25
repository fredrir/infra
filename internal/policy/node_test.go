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

type nodePolicy struct {
	conditions  []cel.Program
	variables   [][2]any
	validations []cel.Program
}

func loadNodePolicy(t *testing.T, name string) nodePolicy {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType), cel.Variable("request", cel.DynType), cel.Variable("variables", cel.MapType(cel.StringType, cel.DynType)))
	if err != nil {
		t.Fatal(err)
	}
	compile := func(expression string) cel.Program {
		ast, issues := env.Compile(expression)
		if issues.Err() != nil {
			t.Fatalf("compile %q: %v", expression, issues.Err())
		}
		program, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		return program
	}
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "platform/components/policy/nodes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range yamlObjects(t, data) {
		if document["kind"] != "ValidatingAdmissionPolicy" || at(document, "metadata", "name") != name {
			continue
		}
		var policy nodePolicy
		conditions, _ := at(document, "spec", "matchConditions").([]any)
		for _, condition := range conditions {
			policy.conditions = append(policy.conditions, compile(at(condition, "expression").(string)))
		}
		variables, _ := at(document, "spec", "variables").([]any)
		for _, variable := range variables {
			policy.variables = append(policy.variables, [2]any{at(variable, "name"), compile(at(variable, "expression").(string))})
		}
		for _, validation := range at(document, "spec", "validations").([]any) {
			policy.validations = append(policy.validations, compile(at(validation, "expression").(string)))
		}
		if len(policy.validations) == 0 {
			t.Fatalf("policy %s has no validations", name)
		}
		return policy
	}
	t.Fatalf("policy %s not found", name)
	return nodePolicy{}
}

func (p nodePolicy) admits(t *testing.T, input map[string]any) bool {
	t.Helper()
	values := object{}
	input["variables"] = values
	for _, variable := range p.variables {
		value, _, err := variable[1].(cel.Program).Eval(input)
		if err != nil {
			t.Fatalf("variable %v: %v", variable[0], err)
		}
		values[variable[0].(string)] = value
	}
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

func nodeRequest(node, old object, user string, groups ...any) map[string]any {
	return map[string]any{"object": node, "oldObject": old, "request": object{"userInfo": object{"username": user, "groups": groups}}}
}

func node(name string, taints ...any) object {
	result := object{"metadata": object{"name": name}, "spec": object{}}
	if taints != nil {
		set(result, taints, "spec", "taints")
	}
	return result
}

func TestOnlyInventoryNodesRegisterAndVolatileNodesStayTainted(t *testing.T) {
	policy := loadNodePolicy(t, "node-registration")
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
			user := "system:node:" + at(test.node, "metadata", "name").(string)
			if policy.admits(t, nodeRequest(test.node, test.old, user, test.groups...)) != test.admitted {
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
	env, err := cel.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	nodesYAML, err := os.ReadFile(filepath.Join(repoRoot(t), "platform/components/policy/nodes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string][]string{}
	for _, doc := range yamlObjects(t, nodesYAML) {
		if doc["kind"] != "ValidatingAdmissionPolicy" || at(doc, "metadata", "name") != "node-registration" {
			continue
		}
		for _, variable := range at(doc, "spec", "variables").([]any) {
			ast, issues := env.Compile(at(variable, "expression").(string))
			if issues.Err() != nil {
				t.Fatal(issues.Err())
			}
			program, err := env.Program(ast)
			if err != nil {
				t.Fatal(err)
			}
			value, _, err := program.Eval(map[string]any{})
			if err != nil {
				t.Fatal(err)
			}
			names, err := value.ConvertToNative(reflectStrings)
			if err != nil {
				t.Fatal(err)
			}
			declared[at(variable, "name").(string)] = slices.Sorted(slices.Values(names.([]string)))
		}
	}
	for variable, group := range map[string]string{"declared": "k3s_cluster", "volatile": "volatile"} {
		if !slices.Equal(declared[variable], groups[group]) {
			t.Errorf("node registration %s nodes %q, inventory %s %q", variable, declared[variable], group, groups[group])
		}
	}
}

func TestSharedFlannelIdentityChangesOnlyFlannelAnnotations(t *testing.T) {
	policy := loadNodePolicy(t, "node-flannel-writer")
	base := node("fredrir-09")
	set(base, object{"flannel.alpha.coreos.com/backend-data": "{}", "k3s.io/hostname": "fredrir-09"}, "metadata", "annotations")
	rotate := func(mutate func(object)) object {
		next := clone(base).(object)
		mutate(next)
		return next
	}
	controller := "system:k3s-controller"
	for _, test := range []struct {
		name     string
		node     object
		user     string
		admitted bool
	}{
		{name: "rotate flannel key", node: rotate(func(n object) { set(n, "{\"PublicKey\":\"new\"}", "metadata", "annotations", "flannel.alpha.coreos.com/backend-data") }), user: controller, admitted: true},
		{name: "set a new flannel annotation", node: rotate(func(n object) { set(n, "1.2.3.4", "metadata", "annotations", "flannel.alpha.coreos.com/public-ip") }), user: controller, admitted: true},
		{name: "unchanged", node: rotate(func(object) {}), user: controller, admitted: true},
		{name: "add a label", node: rotate(func(n object) { set(n, object{"node-restriction.kubernetes.io/critical": "true"}, "metadata", "labels") }), user: controller},
		{name: "change a foreign annotation", node: rotate(func(n object) { set(n, "evil", "metadata", "annotations", "k3s.io/hostname") }), user: controller},
		{name: "add a taint", node: rotate(func(n object) { set(n, []any{object{"key": "x", "effect": "NoSchedule"}}, "spec", "taints") }), user: controller},
		{name: "another identity is not constrained here", node: rotate(func(n object) { set(n, object{"x": "y"}, "metadata", "labels") }), user: "system:node:fredrir-09", admitted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if policy.admits(t, nodeRequest(test.node, base, test.user)) != test.admitted {
				t.Fatalf("admitted %t, want %t", !test.admitted, test.admitted)
			}
		})
	}
}

func TestNodesMayNotCreateStaticPods(t *testing.T) {
	policy := loadNodePolicy(t, "node-no-static-pods")
	pod := object{"metadata": object{"name": "mirror"}, "spec": object{"nodeName": "fredrir-09"}}
	if policy.admits(t, nodeRequest(pod, nil, "system:node:fredrir-09", "system:nodes")) {
		t.Fatal("a node created a pod")
	}
	if !policy.admits(t, nodeRequest(pod, nil, "system:serviceaccount:kube-system:default")) {
		t.Fatal("a controller pod was refused")
	}
}
