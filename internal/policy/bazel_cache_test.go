package policy

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

const bazelCache = "platform/components/bazel-cache"

func bazelCacheStatefulSet(t *testing.T) (object, string) {
	t.Helper()
	var statefulSet object
	var filter string
	for _, item := range renderedTree(t, bazelCache, bazelCache) {
		switch item["kind"] {
		case "StatefulSet":
			statefulSet = item
		case "ConfigMap":
			filter, _ = at(item, "data", "nginx.conf").(string)
		}
	}
	if statefulSet == nil || filter == "" {
		t.Fatal("bazel cache renders no StatefulSet or filter")
	}
	return statefulSet, filter
}

func bazelCacheContainer(t *testing.T, pod object, name string) object {
	t.Helper()
	for _, container := range at(pod, "spec", "containers").([]any) {
		if at(container, "name") == name {
			return container.(object)
		}
	}
	t.Fatalf("no %s container", name)
	return nil
}

func TestBazelCacheListensOnlyThroughItsFilterOnTheTailnet(t *testing.T) {
	t.Parallel()
	statefulSet, filter := bazelCacheStatefulSet(t)
	pod := at(statefulSet, "spec", "template").(object)
	if at(pod, "spec", "hostNetwork") != true || at(pod, "spec", "nodeSelector", "kubernetes.io/hostname") != "fredrir-09" || at(pod, "spec", "nodeSelector", "node-restriction.kubernetes.io/stateful") != "true" {
		t.Fatalf("bazel cache placement %v", at(pod, "spec"))
	}
	server := bazelCacheContainer(t, pod, "bazel-remote")
	if at(server, "image") != at(load(t, "platform/versions.yaml"), "images", "bazel-remote") {
		t.Errorf("bazel-remote image %v is not the pinned release", at(server, "image"))
	}
	if !slices.Equal(at(server, "command").([]any), []any{"/bazel-remote-linux-amd64"}) {
		t.Errorf("bazel-remote keeps the image entrypoint and its TCP listeners: %v", at(server, "command"))
	}
	listeners := map[string]string{}
	for _, argument := range at(server, "args").([]any) {
		name, value, _ := strings.Cut(strings.TrimPrefix(argument.(string), "--"), "=")
		if strings.HasSuffix(name, "address") || strings.HasSuffix(name, "port") || strings.HasSuffix(name, "host") {
			listeners[name] = value
		}
	}
	socket := "unix:///run/bazel-remote/"
	if len(listeners) != 2 || !strings.HasPrefix(listeners["grpc_address"], socket) || !strings.HasPrefix(listeners["http_address"], socket) {
		t.Errorf("bazel-remote listens on %v, want only its gRPC and HTTP sockets", listeners)
	}
	filterContainer := bazelCacheContainer(t, pod, "filter")
	if at(filterContainer, "image") != at(load(t, "platform/versions.yaml"), "images", "nginx") {
		t.Errorf("filter image %v is not the pinned nginx release", at(filterContainer, "image"))
	}
	servers := strings.Split(filter, "\n\tserver {\n")[1:]
	var ports []string
	for _, server := range servers {
		listen := regexp.MustCompile(`(?m)^\s*listen (\S+);$`).FindAllStringSubmatch(server, -1)
		if len(listen) != 1 {
			t.Errorf("filter server listens on %v, want one address", listen)
			continue
		}
		address, port, _ := strings.Cut(listen[0][1], ":")
		ports = append(ports, port)
		if !strings.HasPrefix(address, "100.") {
			t.Errorf("filter listens on %s, not the node's tailnet address", listen[0][1])
		}
		if !strings.Contains(server, "location / {\n\t\t\treturn 403;\n\t\t}") || !strings.Contains(server, "if ($request_uri != $uri) {\n\t\t\treturn 403;") || !strings.Contains(server, "if ($grpc_call = 0) {\n\t\t\treturn 403;") {
			t.Errorf("filter server on %s forwards requests outside its exact gRPC paths", listen[0][1])
		}
		if port == "9095" {
			continue
		}
		access := regexp.MustCompile(`(?m)^\s*(allow|deny) (\S+);$`).FindAllStringSubmatch(server, -1)
		if len(access) != 3 || access[0][1] != "deny" || access[0][2] != address || access[1][1] != "allow" || access[1][2] != "100.64.0.0/10" || access[2][1] != "deny" || access[2][2] != "all" {
			t.Errorf("filter on %s admits %v, want tailnet peers other than the node itself", listen[0][1], access)
		}
	}
	if !slices.Equal(ports, []string{"9092", "9093", "9095"}) {
		t.Errorf("filter listens on ports %v, want the reader, writer and health ports", ports)
	}
	if strings.Contains(filter, "env ") || strings.Contains(filter, "include ") || strings.Contains(filter, "load_module") {
		t.Error("filter depends on the environment, other files or modules")
	}
}

func TestBazelCacheFitsItsQuotaAndVolume(t *testing.T) {
	t.Parallel()
	statefulSet, _ := bazelCacheStatefulSet(t)
	var quota object
	for _, item := range renderedTree(t, "platform/components/policy", "platform/components/policy") {
		if item["kind"] == "ResourceQuota" && at(item, "metadata", "namespace") == "bazel-cache" {
			quota = item
		}
	}
	if quota == nil {
		t.Fatal("bazel-cache has no namespace budget")
	}
	for _, request := range []string{"cpu", "memory"} {
		total := resource.MustParse("0")
		for _, container := range at(statefulSet, "spec", "template", "spec", "containers").([]any) {
			total.Add(resource.MustParse(at(container, "resources", "requests", request).(string)))
		}
		if limit := resource.MustParse(at(quota, "spec", "hard", "requests."+request).(string)); total.Cmp(limit) > 0 {
			t.Errorf("bazel cache requests %s %s above its budget %s", total.String(), request, limit.String())
		}
	}
	claim := at(statefulSet, "spec", "volumeClaimTemplates", 0).(object)
	size := resource.MustParse(at(claim, "spec", "resources", "requests", "storage").(string))
	if size.Cmp(resource.MustParse(at(quota, "spec", "hard", "requests.storage").(string))) > 0 {
		t.Errorf("bazel cache volume %s above its budget", size.String())
	}
	var maximum, blob string
	for _, argument := range at(bazelCacheContainer(t, at(statefulSet, "spec", "template").(object), "bazel-remote"), "args").([]any) {
		if value, found := strings.CutPrefix(argument.(string), "--max_size="); found {
			maximum = value
		}
		if value, found := strings.CutPrefix(argument.(string), "--max_blob_size="); found {
			blob = value
		}
	}
	limit, err := resource.ParseQuantity(maximum + "Gi")
	if err != nil || limit.Value()*4 > size.Value()*3 {
		t.Errorf("bazel-remote may fill %sGi of its %s volume", maximum, size.String())
	}
	if blobLimit, err := resource.ParseQuantity(blob); err != nil || blobLimit.Value()*100 > limit.Value() {
		t.Errorf("one %q byte blob may take more than a hundredth of the %sGi cache", blob, maximum)
	}
}

func TestBazelCachePodPassesOnlyItsHostNetworkPolicy(t *testing.T) {
	t.Parallel()
	e := newEvaluator(t)
	statefulSet, _ := bazelCacheStatefulSet(t)
	pod := clone(at(statefulSet, "spec", "template")).(object)
	const policy = "bazel-cache-host-network"
	review := func(resource, operation string, candidate, old object) bool {
		return !e.intercepts(policy, resource, operation) || e.admitted([]string{policy}, candidate, "bazel-cache", "system:serviceaccount:flux-system:platform-reconciler", operation, old)
	}
	if !review("pods", "CREATE", pod, nil) {
		t.Fatal("bazel cache pod rejected by its own policy")
	}
	for name, mutate := range map[string]func(object){
		"host path": func(p object) {
			appendAt(p, object{"name": "host", "hostPath": object{"path": "/"}}, "spec", "volumes")
		},
		"host PID":                func(p object) { set(p, true, "spec", "hostPID") },
		"service account token":   func(p object) { set(p, true, "spec", "automountServiceAccountToken") },
		"root pod":                func(p object) { set(p, false, "spec", "securityContext", "runAsNonRoot") },
		"root pod user":           func(p object) { set(p, 0, "spec", "securityContext", "runAsUser") },
		"unconfined pod seccomp":  func(p object) { set(p, object{"type": "Unconfined"}, "spec", "securityContext", "seccompProfile") },
		"unconfined pod AppArmor": func(p object) { set(p, object{"type": "Unconfined"}, "spec", "securityContext", "appArmorProfile") },
		"pod SELinux type":        func(p object) { set(p, object{"type": "spc_t"}, "spec", "securityContext", "seLinuxOptions") },
		"sysctl": func(p object) {
			set(p, []any{object{"name": "kernel.shm_rmid_forced", "value": "0"}}, "spec", "securityContext", "sysctls")
		},
		"missing limits": func(p object) {
			set(p, object{"requests": object{"cpu": "10m", "memory": "8Mi"}}, "spec", "containers", 1, "resources")
		},
		"missing memory limit":     func(p object) { set(p, object{"cpu": "500m"}, "spec", "containers", 1, "resources", "limits") },
		"shared process namespace": func(p object) { set(p, true, "spec", "shareProcessNamespace") },
	} {
		candidate := clone(pod).(object)
		mutate(candidate)
		if review("pods", "CREATE", candidate, nil) {
			t.Errorf("%s admitted", name)
		}
	}
	debugger := func() object {
		return object{"name": "debug", "image": at(pod, "spec", "containers", 1, "image"), "securityContext": object{
			"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": object{"drop": []any{"ALL"}},
		}}
	}
	placements := map[string]func(mutate func(object)) bool{
		"container": func(mutate func(object)) bool {
			candidate := clone(pod).(object)
			mutate(at(candidate, "spec", "containers", 0).(object))
			return review("pods", "CREATE", candidate, nil)
		},
		"init container": func(mutate func(object)) bool {
			candidate, init := clone(pod).(object), debugger()
			mutate(init)
			set(candidate, []any{init}, "spec", "initContainers")
			return review("pods", "CREATE", candidate, nil)
		},
		"ephemeral container": func(mutate func(object)) bool {
			candidate, debug := clone(pod).(object), debugger()
			mutate(debug)
			set(candidate, []any{debug}, "spec", "ephemeralContainers")
			return review("pods/ephemeralcontainers", "UPDATE", candidate, pod)
		},
	}
	for name, mutate := range map[string]func(object){
		"mounted cache volume": func(c object) {
			set(c, []any{object{"name": "data", "mountPath": "/data", "readOnly": true}}, "volumeMounts")
		},
		"mounted bazel-remote socket": func(c object) {
			set(c, []any{object{"name": "sockets", "mountPath": "/run/bazel-remote"}}, "volumeMounts")
		},
		"bazel-remote as target": func(c object) { set(c, "bazel-remote", "targetContainerName") },
	} {
		if placements["ephemeral container"](mutate) {
			t.Errorf("ephemeral container with %s admitted", name)
		}
	}
	for placement, admitted := range placements {
		if !admitted(func(object) {}) {
			t.Errorf("compliant %s rejected", placement)
		}
		for name, mutate := range map[string]func(object){
			"privileged":         func(c object) { set(c, true, "securityContext", "privileged") },
			"escalation":         func(c object) { set(c, true, "securityContext", "allowPrivilegeEscalation") },
			"added capability":   func(c object) { set(c, []any{"NET_ADMIN"}, "securityContext", "capabilities", "add") },
			"kept capabilities":  func(c object) { set(c, object{}, "securityContext", "capabilities") },
			"writable root":      func(c object) { set(c, false, "securityContext", "readOnlyRootFilesystem") },
			"root user":          func(c object) { set(c, 0, "securityContext", "runAsUser") },
			"root allowed":       func(c object) { set(c, false, "securityContext", "runAsNonRoot") },
			"unconfined seccomp": func(c object) { set(c, object{"type": "Unconfined"}, "securityContext", "seccompProfile") },
			"unconfined AppArmor": func(c object) {
				set(c, object{"type": "Unconfined"}, "securityContext", "appArmorProfile")
			},
			"SELinux type":  func(c object) { set(c, object{"type": "spc_t"}, "securityContext", "seLinuxOptions") },
			"foreign image": func(c object) { set(c, "docker.io/library/busybox:latest", "image") },
			"host port": func(c object) {
				set(c, []any{object{"containerPort": 22, "hostPort": 22}}, "ports")
			},
			"sysadmin debug profile": func(c object) { set(c, object{"privileged": true}, "securityContext") },
			"root debug container": func(c object) {
				set(c, object{"runAsUser": 0, "runAsNonRoot": false, "allowPrivilegeEscalation": true, "seccompProfile": object{"type": "Unconfined"}}, "securityContext")
			},
		} {
			if admitted(mutate) {
				t.Errorf("%s with %s admitted", placement, name)
			}
		}
	}
}

func TestBazelCachePolicyAdmitsExactlyTheShippedImages(t *testing.T) {
	t.Parallel()
	statefulSet, _ := bazelCacheStatefulSet(t)
	var shipped []string
	for _, container := range at(statefulSet, "spec", "template", "spec", "containers").([]any) {
		shipped = append(shipped, at(container, "image").(string))
	}
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "platform/components/policy/admission.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var allowed []string
	for _, doc := range yamlObjects(t, data) {
		if doc["kind"] != "ValidatingAdmissionPolicy" || at(doc, "metadata", "name") != "bazel-cache-host-network" {
			continue
		}
		for _, validation := range at(doc, "spec", "validations").([]any) {
			for _, image := range regexp.MustCompile(`"([^"\s]+@sha256:[a-f0-9]{64})"`).FindAllStringSubmatch(at(validation, "expression").(string), -1) {
				allowed = append(allowed, image[1])
			}
		}
	}
	slices.Sort(shipped)
	slices.Sort(allowed)
	if !slices.Equal(shipped, allowed) {
		t.Errorf("bazel cache policy admits images %v, want the shipped %v", allowed, shipped)
	}
}
