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
		value, _, err := validation.Eval(input)
		if err != nil {
			t.Fatalf("validation: %v", err)
		}
		if value != types.True {
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

func TestSharedFlannelIdentityWritesOnlyItsOwnNodeNetwork(t *testing.T) {
	policy := loadNodePolicy(t, "node-flannel-writer")
	const flannel = "flannel.alpha.coreos.com/"
	base := node("fredrir-09")
	set(base, object{
		flannel + "backend-type": "wireguard",
		flannel + "backend-data": `{"PublicKey":"own"}`,
		flannel + "public-ip":    "100.87.168.66",
		"k3s.io/hostname":        "fredrir-09",
	}, "metadata", "annotations")
	base["status"] = object{
		"addresses":   []any{object{"type": "InternalIP", "address": "100.87.168.66"}, object{"type": "ExternalIP", "address": "100.87.168.66"}},
		"capacity":    object{"cpu": "4", "infra.fredrir.com/ci-slot": "2"},
		"allocatable": object{"cpu": "3750m", "infra.fredrir.com/ci-slot": "2"},
		"conditions":  []any{object{"type": "Ready", "status": "True"}},
	}
	change := func(mutate func(object)) object {
		next := clone(base).(object)
		mutate(next)
		return next
	}
	annotate := func(key string, value any) object {
		return change(func(n object) { set(n, value, "metadata", "annotations", key) })
	}
	fresh := change(func(n object) {
		annotations := at(n, "metadata", "annotations").(object)
		delete(annotations, flannel+"backend-type")
		delete(annotations, flannel+"backend-data")
	})
	withKey := clone(fresh).(object)
	set(withKey, object{flannel + "backend-type": "wireguard", flannel + "backend-data": `{"PublicKey":"own"}`, flannel + "public-ip": "100.87.168.66", "k3s.io/hostname": "fredrir-09"}, "metadata", "annotations")
	controller := "system:k3s-controller"
	for _, test := range []struct {
		name     string
		node     object
		old      object
		user     string
		admitted bool
	}{
		{name: "unchanged", node: change(func(object) {}), user: controller, admitted: true},
		{name: "publish a backend on a node without one", node: withKey, old: fresh, user: controller, admitted: true},
		{name: "rewrite the backend key", node: annotate(flannel+"backend-data", `{"PublicKey":"other"}`), user: controller},
		{name: "rewrite the backend type", node: annotate(flannel+"backend-type", "vxlan"), user: controller},
		{name: "remove the backend key", node: change(func(n object) { delete(at(n, "metadata", "annotations").(object), flannel+"backend-data") }), user: controller},
		{name: "public address of another node", node: annotate(flannel+"public-ip", "100.66.14.60"), user: controller},
		{name: "public address override to another node", node: annotate(flannel+"public-ip-overwrite", "100.66.14.60"), user: controller},
		{name: "public address override to its own address", node: annotate(flannel+"public-ip-overwrite", "100.87.168.66"), user: controller, admitted: true},
		{name: "network unavailable condition", node: change(func(n object) {
			set(n, append(at(n, "status", "conditions").([]any), object{"type": "NetworkUnavailable", "status": "False"}), "status", "conditions")
		}), user: controller, admitted: true},
		{name: "forged ready condition", node: change(func(n object) { set(n, "False", "status", "conditions", 0, "status") }), user: controller},
		{name: "forged addresses", node: change(func(n object) { set(n, "100.66.14.60", "status", "addresses", 1, "address") }), user: controller},
		{name: "forged allocatable", node: change(func(n object) { set(n, "40", "status", "allocatable", "infra.fredrir.com/ci-slot") }), user: controller},
		{name: "forged capacity", node: change(func(n object) { set(n, "64", "status", "capacity", "cpu") }), user: controller},
		{name: "add a label", node: change(func(n object) {
			set(n, object{"node-restriction.kubernetes.io/critical": "true"}, "metadata", "labels")
		}), user: controller},
		{name: "add an owner", node: change(func(n object) { set(n, []any{object{"kind": "Namespace", "name": "x"}}, "metadata", "ownerReferences") }), user: controller},
		{name: "change a foreign annotation", node: annotate("k3s.io/hostname", "evil"), user: controller},
		{name: "add a taint", node: change(func(n object) { set(n, []any{object{"key": "x", "effect": "NoSchedule"}}, "spec", "taints") }), user: controller},
		{name: "administrator repairs the backend", node: annotate(flannel+"backend-data", `{"PublicKey":"repaired"}`), user: "system:admin", admitted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			old := test.old
			if old == nil {
				old = base
			}
			if policy.admits(t, nodeRequest(test.node, old, test.user)) != test.admitted {
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
