package policy

import (
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestSharedModelsRunOnTheirReservedNodes(t *testing.T) {
	t.Parallel()
	e := newEvaluator(t)
	served := map[string]bool{}
	nodes := map[string]string{"granite": "fredrir-09", "paddleocr": "fredrir-04", "pp-structure": "fredrir-04", "litellm": "fredrir-04", "tailnet": "fredrir-04"}
	for _, resource := range renderedTree(t, "platform", "platform/components/llm") {
		if resource["kind"] != "Deployment" {
			continue
		}
		name := at(resource, "metadata", "name").(string)
		served[name] = true
		pod := clone(at(resource, "spec", "template")).(object)
		if !e.admitted([]string{"workload-isolation"}, pod, "llm", reconciler, "CREATE", nil) {
			t.Fatalf("%s workload violates the isolation policy", name)
		}
		spec := at(pod, "spec").(object)
		if at(spec, "nodeSelector", "kubernetes.io/hostname") != nodes[name] {
			t.Fatalf("%s does not use its reserved node", name)
		}
		container := at(spec, "containers", 0).(object)
		if at(container, "securityContext", "readOnlyRootFilesystem") != true || at(spec, "securityContext", "runAsNonRoot") != true {
			t.Fatalf("%s runs with a writable root or as root", name)
		}
	}
	for name := range nodes {
		if !served[name] {
			t.Fatalf("%s deployment missing", name)
		}
	}
}

func TestParserUsesTheAuthenticatedModelGateway(t *testing.T) {
	t.Parallel()
	for _, resource := range renderedTree(t, "platform", "platform/projects/llunde-pyparser/application") {
		if resource["kind"] != "Deployment" {
			continue
		}
		name := at(resource, "metadata", "name")
		var url string
		var authenticated bool
		for _, env := range at(resource, "spec", "template", "spec", "containers", 0, "env").([]any) {
			switch variable := at(env, "name").(string); {
			case strings.HasPrefix(variable, "PYPARSER_GRANITE_"):
				t.Fatalf("%s reaches a model outside the gateway with %s", name, variable)
			case variable == "LITELLM_API_URL":
				url, _ = lookup(env, "value").(string)
			case variable == "LITELLM_API_KEY":
				authenticated = at(env, "valueFrom", "secretKeyRef", "name") == "llm-gateway" && at(env, "valueFrom", "secretKeyRef", "key") == "LITELLM_API_KEY"
			}
		}
		if url != "http://litellm.llm.svc.cluster.local:4000/v1" {
			t.Errorf("%s reaches the gateway at %q", name, url)
		}
		if !authenticated {
			t.Fatalf("%s has no gateway credential", name)
		}
	}
}

func TestLayoutRouteRequiresGatewayKeys(t *testing.T) {
	t.Parallel()
	var config struct {
		GeneralSettings struct {
			PassThroughEndpoints []struct {
				Path           string `yaml:"path"`
				Target         string `yaml:"target"`
				IncludeSubpath bool   `yaml:"include_subpath"`
				Auth           bool   `yaml:"auth"`
				ForwardHeaders bool   `yaml:"forward_headers"`
			} `yaml:"pass_through_endpoints"`
		} `yaml:"general_settings"`
	}
	for _, resource := range renderedTree(t, "platform", "platform/components/llm") {
		if resource["kind"] == "ConfigMap" && strings.HasPrefix(at(resource, "metadata", "name").(string), "litellm") {
			if err := yaml.Unmarshal([]byte(at(resource, "data", "config.yaml").(string)), &config); err != nil {
				t.Fatal(err)
			}
		}
	}
	routes := config.GeneralSettings.PassThroughEndpoints
	if len(routes) != 1 {
		t.Fatalf("unexpected pass-through routes %v", routes)
	}
	route := routes[0]
	if route.Path != "/pp-structure" || route.Target != "http://pp-structure:8012" || !route.IncludeSubpath {
		t.Fatalf("layout route misrouted: %+v", route)
	}
	if !route.Auth || route.ForwardHeaders {
		t.Fatal("layout route bypasses gateway keys or leaks client credentials")
	}
}

func TestParserGatewayEgressCannotEscapeItsApprovedService(t *testing.T) {
	t.Parallel()
	e := newEvaluator(t)
	var policy object
	for _, resource := range renderedTree(t, "platform", "platform/projects/llunde-pyparser") {
		if resource["kind"] == "NetworkPolicy" && at(resource, "metadata", "name") == "llm-gateway-egress" {
			policy = resource
		}
		if resource["kind"] == "Deployment" && at(resource, "metadata", "name") == "granite" {
			t.Fatal("parser retains a separate model deployment")
		}
	}
	if policy == nil {
		t.Fatal("parser gateway egress missing")
	}
	boundary := []string{"project-network-boundary"}
	if !e.admitted(boundary, policy, "llunde-pyparser", reconciler, "CREATE", nil) {
		t.Fatal("approved gateway egress rejected")
	}
	if e.admitted(boundary, policy, "llunde-pyparser", projectRunner, "CREATE", nil) || e.admitted(boundary, policy, "portfolio", reconciler, "CREATE", nil) {
		t.Fatal("gateway egress granted outside its owner or namespace")
	}
	for name, mutate := range map[string]func(object){
		"other port":     func(p object) { set(p, 8080, "spec", "egress", 0, "ports", 0, "port") },
		"port range":     func(p object) { set(p, 4001, "spec", "egress", 0, "ports", 0, "endPort") },
		"other protocol": func(p object) { set(p, "UDP", "spec", "egress", 0, "ports", 0, "protocol") },
		"any namespace":  func(p object) { set(p, object{}, "spec", "egress", 0, "to", 0, "namespaceSelector") },
		"other workload": func(p object) {
			set(p, "postgres", "spec", "egress", 0, "to", 0, "podSelector", "matchLabels", "app.kubernetes.io/name")
		},
		"other client": func(p object) {
			set(p, "database", "spec", "podSelector", "matchLabels", "app.kubernetes.io/component")
		},
		"other policy": func(p object) { set(p, "application-egress", "metadata", "name") },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := clone(policy).(object)
			mutate(candidate)
			if e.admitted(boundary, candidate, "llunde-pyparser", reconciler, "CREATE", nil) {
				t.Fatal("gateway egress escape admitted")
			}
		})
	}
}

func TestModelGatewayIsPublicOnlyBehindTheAdminAccessHost(t *testing.T) {
	t.Parallel()
	var ingresses []string
	for _, resource := range renderedTree(t, "platform", "platform/components/llm") {
		if resource["kind"] != "Ingress" {
			continue
		}
		ingresses = append(ingresses, at(resource, "metadata", "name").(string))
		for _, rule := range at(resource, "spec", "rules").([]any) {
			if at(rule, "host") != "llm-admin.fredrir.com" {
				t.Fatalf("model gateway served publicly on %v", at(rule, "host"))
			}
			for _, path := range at(rule, "http", "paths").([]any) {
				if at(path, "path") != "/" || at(path, "pathType") != "Prefix" || at(path, "backend", "service", "name") != "litellm" {
					t.Fatalf("admin host exposes an unintended endpoint %v", at(path, "path"))
				}
			}
		}
	}
	if !slices.Equal(ingresses, []string{"llm-admin"}) {
		t.Fatalf("model ingresses %v, want only llm-admin", ingresses)
	}
}
