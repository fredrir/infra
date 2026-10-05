package policy

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
	jsonpatch "github.com/evanphx/json-patch/v5"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
)

const gatePolicies = "platform/components/policy/netpol-gate.yaml"

func gateDocuments(t *testing.T) map[string]object {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), gatePolicies))
	if err != nil {
		t.Fatal(err)
	}
	documents := map[string]object{}
	for _, doc := range yamlObjects(t, data) {
		documents[doc["kind"].(string)+"/"+at(doc, "metadata", "name").(string)] = doc
	}
	return documents
}

type structs struct{ *types.Registry }

func (s structs) FindStructType(name string) (*types.Type, bool) {
	if name == "JSONPatch" || strings.HasPrefix(name, "Object.") {
		return types.NewTypeTypeWithParam(types.NewObjectType(name)), true
	}
	return s.Registry.FindStructType(name)
}

func (s structs) NewValue(name string, fields map[string]ref.Val) ref.Val {
	value := map[string]any{}
	for field, v := range fields {
		value[field] = native(v)
	}
	return types.DefaultTypeAdapter.NativeToValue(value)
}

func native(v ref.Val) any {
	switch v := v.(type) {
	case traits.Mapper:
		value := map[string]any{}
		for iterator := v.Iterator(); iterator.HasNext() == types.True; {
			key := iterator.Next()
			value[fmt.Sprint(key.Value())] = native(v.Get(key))
		}
		return value
	case traits.Lister:
		value := []any{}
		for iterator := v.Iterator(); iterator.HasNext() == types.True; {
			value = append(value, native(iterator.Next()))
		}
		return value
	default:
		return v.Value()
	}
}

type mutation struct {
	condition, patch cel.Program
}

func gateMutation(t *testing.T) mutation {
	t.Helper()
	policy := gateDocuments(t)["MutatingAdmissionPolicy/netpol-gate"]
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType), cel.Variable("request", cel.DynType), cel.CustomTypeProvider(structs{types.NewEmptyRegistry()}))
	if err != nil {
		t.Fatal(err)
	}
	program := func(expression string) cel.Program {
		ast, issues := env.Parse(expression)
		if issues.Err() != nil {
			t.Fatal(issues.Err())
		}
		p, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	conditions := at(policy, "spec", "matchConditions").([]any)
	mutations := at(policy, "spec", "mutations").([]any)
	if len(conditions) != 1 || len(mutations) != 1 || at(mutations[0], "patchType") != "JSONPatch" {
		t.Fatal("the gate is one conditional JSONPatch")
	}
	return mutation{condition: program(at(conditions[0], "expression").(string)), patch: program(at(mutations[0], "jsonPatch", "expression").(string))}
}

func (m mutation) apply(t *testing.T, pod object) object {
	t.Helper()
	input := map[string]any{"object": clone(pod), "oldObject": nil, "request": object{"operation": "CREATE"}}
	matched, _, err := m.condition.Eval(input)
	if err != nil {
		t.Fatal(err)
	}
	if matched != types.True {
		return pod
	}
	patch, _, err := m.patch.Eval(input)
	if err != nil {
		t.Fatal(err)
	}
	operations, _ := json.Marshal(native(patch))
	decoded, err := jsonpatch.DecodePatch(operations)
	if err != nil {
		t.Fatal(err)
	}
	document, _ := json.Marshal(pod)
	patched, err := decoded.Apply(document)
	if err != nil {
		t.Fatalf("gate patch %s does not apply: %v", operations, err)
	}
	var result object
	if err := json.Unmarshal(patched, &result); err != nil {
		t.Fatal(err)
	}
	return clone(normalize(result)).(object)
}

func normalize(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for key, value := range v {
			v[key] = normalize(value)
		}
	case []any:
		for i, value := range v {
			v[i] = normalize(value)
		}
	case float64:
		if v == float64(int64(v)) {
			return int64(v)
		}
	}
	return v
}

func runnerPods(t *testing.T) map[string]object {
	t.Helper()
	deploy := runner(t, "infra/deploy-values.yaml")
	deploy["metadata"] = object{"name": "deploy-1", "labels": object{"actions.github.com/scale-set-name": "deploy-amd64"}}
	pods := map[string]object{"deploy": deploy}
	for _, variant := range []string{"pr", "main", "release"} {
		pods["rust-"+variant] = rustRunner(t, variant)
	}
	return pods
}

func projectPods(t *testing.T) map[string]object {
	t.Helper()
	pods := map[string]object{}
	for _, path := range []string{"platform/projects", "platform/projects/llunde-pyparser/application", "platform/projects/llunde-pyparser/migration"} {
		for _, resource := range renderedTree(t, "platform", path) {
			var template any
			switch resource["kind"] {
			case "Deployment", "StatefulSet", "Job":
				template = at(resource, "spec", "template")
			case "CronJob":
				template = at(resource, "spec", "jobTemplate", "spec", "template")
			default:
				continue
			}
			pods[fmt.Sprintf("%s/%s", at(resource, "metadata", "namespace"), at(resource, "metadata", "name"))] = clone(template).(object)
		}
	}
	if len(pods) < 10 {
		t.Fatalf("expected every project workload, got %d", len(pods))
	}
	return pods
}

func gate(t *testing.T, m mutation) object {
	t.Helper()
	return at(m.apply(t, object{"spec": object{"containers": []any{}}}), "spec", "initContainers", 0).(object)
}

func TestInjectedGatePassesAdmissionForEveryRunner(t *testing.T) {
	t.Parallel()
	e := newEvaluator(t)
	m := gateMutation(t)
	for name, p := range runnerPods(t) {
		t.Run(name, func(t *testing.T) {
			namespace := map[bool]string{true: "ci-nsql", false: "ci-infra"}[strings.HasPrefix(name, "rust")]
			gated := m.apply(t, p)
			if names := at(gated, "spec", "initContainers").([]any); len(names) != 1 || at(names[0], "name") != "netpol-gate" {
				t.Fatalf("expected only the gate, got %v", names)
			}
			if !e.runner(gated, namespace, controller) || !e.admitted([]string{"netpol-gate"}, gated, namespace, controller, "CREATE", nil) {
				t.Fatal("gated runner rejected")
			}
			if e.admitted([]string{"netpol-gate"}, p, namespace, controller, "CREATE", nil) {
				t.Fatal("ungated runner admitted")
			}
			if twice := m.apply(t, gated); fmt.Sprint(twice) != fmt.Sprint(gated) {
				t.Fatal("the gate is injected twice")
			}
			doubled := clone(gated).(object)
			appendAt(doubled, clone(at(gated, "spec", "initContainers", 0)), "spec", "initContainers")
			foreign := clone(gated).(object)
			set(foreign, "setup", "spec", "initContainers", 0, "name")
			for _, rejected := range []object{doubled, foreign} {
				if e.runner(rejected, namespace, controller) {
					t.Fatal("CI admitted an init container besides the gate")
				}
			}
		})
	}
}

func TestInjectedGateRunsFirstInEveryProjectPod(t *testing.T) {
	t.Parallel()
	e := newEvaluator(t)
	m := gateMutation(t)
	own := object{"name": "own-init", "image": "ghcr.io/fredrir/own@sha256:" + strings.Repeat("0", 64), "securityContext": object{"allowPrivilegeEscalation": false}, "resources": object{"requests": object{"cpu": "10m", "memory": "16Mi"}, "limits": object{"cpu": "10m", "memory": "16Mi"}}}
	for name, template := range projectPods(t) {
		t.Run(name, func(t *testing.T) {
			withOwn := clone(template).(object)
			appendAt(withOwn, clone(own), "spec", "initContainers")
			for _, p := range []object{template, withOwn} {
				gated := m.apply(t, p)
				before, _ := at(p, "spec").(object)["initContainers"].([]any)
				after := at(gated, "spec", "initContainers").([]any)
				if len(after) != len(before)+1 || at(after, 0, "name") != "netpol-gate" || fmt.Sprint(after[1:]) != fmt.Sprint(before) {
					t.Fatalf("gate not inserted ahead of %v: %v", before, after)
				}
				if !e.admitted([]string{"netpol-gate"}, gated, "portfolio", reconciler, "CREATE", nil) || !e.project("workload-isolation", gated) {
					t.Fatal("gated project pod rejected")
				}
			}
		})
	}
}

func TestGateValidationRejectsAlteredGates(t *testing.T) {
	t.Parallel()
	e := newEvaluator(t)
	m := gateMutation(t)
	pod := m.apply(t, runnerPods(t)["deploy"])
	for name, alter := range map[string]func(c object){
		"name":         func(c object) { c["name"] = "setup" },
		"image":        func(c object) { c["image"] = "docker.io/library/busybox:latest" },
		"arguments":    func(c object) { c["args"] = []any{"serve"} },
		"canary":       func(c object) { c["args"] = []any{"wait", "--canary", "10.42.0.9"} },
		"command":      func(c object) { c["command"] = []any{"/bin/true"} },
		"working dir":  func(c object) { c["workingDir"] = "/tmp" },
		"environment":  func(c object) { c["env"] = []any{object{"name": "GODEBUG", "value": "netdns=cgo"}} },
		"env imports":  func(c object) { c["envFrom"] = []any{object{"secretRef": object{"name": "application"}}} },
		"binary mount": func(c object) { c["volumeMounts"] = []any{object{"name": "shim", "mountPath": "/usr/local/bin"}} },
		"token mount beside a binary mount": func(c object) {
			c["volumeMounts"] = []any{serviceAccountMount(), object{"name": "shim", "mountPath": "/usr/local/bin", "readOnly": true}}
		},
		"writable token path": func(c object) {
			c["volumeMounts"] = []any{object{"name": "shim", "mountPath": "/var/run/secrets/kubernetes.io/serviceaccount"}}
		},
		"device":        func(c object) { c["volumeDevices"] = []any{object{"name": "disk", "devicePath": "/dev/xvda"}} },
		"sidecar gate":  func(c object) { c["restartPolicy"] = "Always" },
		"shadowed gate": func(c object) {},
	} {
		t.Run(name, func(t *testing.T) {
			altered := clone(pod).(object)
			alter(at(altered, "spec", "initContainers", 0).(object))
			if name == "shadowed gate" {
				set(altered, append([]any{object{"name": "own-init", "image": "x"}}, at(altered, "spec", "initContainers").([]any)...), "spec", "initContainers")
			}
			if e.admitted([]string{"netpol-gate"}, altered, "ci-infra", controller, "CREATE", nil) {
				t.Fatal("altered gate admitted")
			}
			injected := m.apply(t, altered)
			if e.admitted([]string{"netpol-gate"}, injected, "ci-infra", controller, "CREATE", nil) && fmt.Sprint(at(injected, "spec", "initContainers", 0)) != fmt.Sprint(at(pod, "spec", "initContainers", 0)) {
				t.Fatal("a pre-declared altered gate runs first")
			}
		})
	}
}

func serviceAccountMount() object {
	return object{"name": "kube-api-access-m6kxp", "mountPath": "/var/run/secrets/kubernetes.io/serviceaccount", "readOnly": true}
}

func TestGateAcceptsTheServiceAccountTokenMount(t *testing.T) {
	t.Parallel()
	e := newEvaluator(t)
	pod := gateMutation(t).apply(t, projectPods(t)["portfolio/api"])
	appendAt(pod, serviceAccountMount(), "spec", "initContainers", 0, "volumeMounts")
	if !e.admitted([]string{"netpol-gate"}, pod, "portfolio", reconciler, "CREATE", nil) {
		t.Fatal("pods that automount a service account token are rejected")
	}
}

func TestInitContainerImagesAreImmutable(t *testing.T) {
	t.Parallel()
	e := newEvaluator(t)
	m := gateMutation(t)
	for name, p := range projectPods(t) {
		gated := m.apply(t, p)
		relabelled := clone(gated).(object)
		relabelled["metadata"] = object{"labels": object{"updated": "true"}}
		swapped := clone(gated).(object)
		set(swapped, "docker.io/library/busybox:latest", "spec", "initContainers", 0, "image")
		legacy := clone(p).(object)
		legacy["metadata"] = object{"labels": object{"updated": "true"}}
		if !e.admitted([]string{"netpol-gate"}, relabelled, "portfolio", reconciler, "UPDATE", gated) || !e.admitted([]string{"netpol-gate"}, legacy, "portfolio", reconciler, "UPDATE", p) {
			t.Fatalf("%s: metadata updates rejected", name)
		}
		if e.admitted([]string{"netpol-gate"}, swapped, "portfolio", reconciler, "UPDATE", gated) {
			t.Fatalf("%s: gate image swapped on a running pod", name)
		}
	}
}

func TestEphemeralContainersWaitForTheGate(t *testing.T) {
	t.Parallel()
	e := newEvaluator(t)
	m := gateMutation(t)
	for name, p := range projectPods(t) {
		gated := m.apply(t, p)
		status := func(state object) object {
			withStatus := clone(gated).(object)
			withStatus["status"] = object{"initContainerStatuses": []any{object{"name": "netpol-gate", "state": state}}}
			return withStatus
		}
		for state, allowed := range map[string]bool{"running": false, "failed": false, "passed": true, "none": false} {
			pod := map[string]object{
				"running": status(object{"running": object{"startedAt": "now"}}),
				"failed":  status(object{"terminated": object{"exitCode": 1}}),
				"passed":  status(object{"terminated": object{"exitCode": 0}}),
				"none":    gated,
			}[state]
			if e.admitted([]string{"netpol-gate-ephemeral"}, pod, "portfolio", "kubernetes-admin", "UPDATE", pod) != allowed {
				t.Fatalf("%s: ephemeral container while the gate is %s: allowed %t", name, state, !allowed)
			}
		}
		if !e.admitted([]string{"netpol-gate-ephemeral"}, p, "portfolio", "kubernetes-admin", "UPDATE", p) {
			t.Fatalf("%s: pods started before the gate existed cannot be debugged", name)
		}
	}
}

func namespaces(t *testing.T) map[string]labels.Set {
	t.Helper()
	found := map[string]labels.Set{"kube-system": {}, "default": {}}
	for tree, paths := range map[string][]string{"platform/components": {"platform/components/policy", "platform/components/runners", canaryComponent}, "platform": {"platform/projects"}} {
		for _, path := range paths {
			for _, resource := range renderedTree(t, tree, path) {
				if resource["kind"] != "Namespace" {
					continue
				}
				set := labels.Set{}
				declared, _ := at(resource, "metadata").(object)["labels"].(object)
				for key, value := range declared {
					set[key] = value.(string)
				}
				found[at(resource, "metadata", "name").(string)] = set
			}
		}
	}
	return found
}

func namespaceSelector(t *testing.T, selector object) labels.Selector {
	t.Helper()
	matcher := labels.NewSelector()
	matchLabels, _ := selector["matchLabels"].(object)
	for key, value := range matchLabels {
		requirement, err := labels.NewRequirement(key, selection.Equals, []string{value.(string)})
		if err != nil {
			t.Fatal(err)
		}
		matcher = matcher.Add(*requirement)
	}
	expressions, _ := selector["matchExpressions"].([]any)
	for _, expression := range expressions {
		var values []string
		for _, value := range at(expression, "values").([]any) {
			values = append(values, value.(string))
		}
		requirement, err := labels.NewRequirement(at(expression, "key").(string), selection.Operator(strings.ToLower(at(expression, "operator").(string))), values)
		if err != nil {
			t.Fatal(err)
		}
		matcher = matcher.Add(*requirement)
	}
	return matcher
}

func TestGatePoliciesSelectExactlyTheUntrustedTiers(t *testing.T) {
	t.Parallel()
	documents := gateDocuments(t)
	all := namespaces(t)
	var untrusted []string
	for name, set := range all {
		if set["infra.fredrir.com/tier"] == "ci" || set["infra.fredrir.com/tier"] == "project" {
			untrusted = append(untrusted, name)
		}
	}
	slices.Sort(untrusted)
	if len(untrusted) < 7 || slices.Contains(untrusted, "netpol-canary") {
		t.Fatalf("unexpected untrusted namespaces %v", untrusted)
	}
	rules := map[string]string{
		"MutatingAdmissionPolicy/netpol-gate":             "[map[apiGroups:[] apiVersions:[v1] operations:[CREATE] resources:[pods]]]",
		"ValidatingAdmissionPolicy/netpol-gate":           "[map[apiGroups:[] apiVersions:[v1] operations:[CREATE UPDATE] resources:[pods]]]",
		"ValidatingAdmissionPolicy/netpol-gate-ephemeral": "[map[apiGroups:[] apiVersions:[v1] operations:[UPDATE] resources:[pods/ephemeralcontainers]]]",
	}
	for key, want := range rules {
		policy := documents[key]
		if policy == nil {
			t.Fatalf("%s missing", key)
		}
		constraints := at(policy, "spec", "matchConstraints").(object)
		if got := fmt.Sprint(constraints["resourceRules"]); got != want {
			t.Errorf("%s matches %s", key, got)
		}
		if at(policy, "spec", "failurePolicy") != "Fail" || constraints["objectSelector"] != nil || constraints["excludeResourceRules"] != nil {
			t.Errorf("%s must fail closed for every pod", key)
		}
		matcher := namespaceSelector(t, constraints["namespaceSelector"].(object))
		var selected []string
		for name, set := range all {
			if matcher.Matches(set) {
				selected = append(selected, name)
			}
		}
		slices.Sort(selected)
		if slices.Contains(selected, "netpol-canary") || slices.Contains(selected, "kube-system") {
			t.Errorf("%s gates the canary or cluster DNS, so no gated pod can ever start", key)
		}
		if !slices.Equal(selected, untrusted) {
			t.Errorf("%s selects %v, want %v", key, selected, untrusted)
		}
	}
}

func TestGatePolicyBindingsEnforce(t *testing.T) {
	t.Parallel()
	documents := gateDocuments(t)
	for key := range documents {
		kind, name, _ := strings.Cut(key, "/")
		if !strings.HasSuffix(kind, "AdmissionPolicy") {
			continue
		}
		binding := documents[kind+"Binding/"+name]
		if binding == nil || at(binding, "spec", "policyName") != name || at(binding, "spec").(object)["matchResources"] != nil || at(binding, "spec").(object)["paramRef"] != nil {
			t.Fatalf("%s is not bound to every pod it matches", key)
		}
		if kind == "ValidatingAdmissionPolicy" && fmt.Sprint(at(binding, "spec", "validationActions")) != "[Deny]" {
			t.Fatalf("%s only warns or audits", key)
		}
	}
	if len(documents) != 6 {
		t.Fatalf("expected three policies and their bindings, got %v", slices.Sorted(maps.Keys(documents)))
	}
}

func TestCanaryRunsTheGateImageAtTheGateAddresses(t *testing.T) {
	t.Parallel()
	injected := gate(t, gateMutation(t))
	statefulSet, services, _ := canaryResources(t)
	if image := at(statefulSet, "spec", "template", "spec", "containers", 0, "image"); image != injected["image"] {
		t.Fatalf("canary runs %v, the gate %v", image, injected["image"])
	}
	var canaries []any
	for replica := range at(statefulSet, "spec", "replicas").(int) {
		canaries = append(canaries, "--canary", at(services[fmt.Sprintf("canary-%d", replica)], "spec", "clusterIP"))
	}
	if want := append([]any{"wait"}, canaries...); fmt.Sprint(injected["args"]) != fmt.Sprint(want) {
		t.Fatalf("gate arguments %v, canary services %v", injected["args"], want)
	}
}
