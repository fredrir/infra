package policy

import "testing"

func TestSharedModelsRunOnTheirReservedNodes(t *testing.T) {
	t.Parallel()
	e := newEvaluator(t)
	served := map[string]bool{}
	nodes := map[string]string{"granite": "fredrir-09", "paddleocr": "fredrir-04", "litellm": "fredrir-04"}
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
		environment := map[string]string{}
		for _, env := range at(resource, "spec", "template", "spec", "containers", 0, "env").([]any) {
			environment[at(env, "name").(string)], _ = lookup(env, "value").(string)
		}
		if environment["PYPARSER_GRANITE_ENDPOINT"] != "http://litellm.llm.svc.cluster.local:4000" || environment["PYPARSER_GRANITE_API"] != "chat-completions" {
			t.Errorf("%s reaches granite with %v", at(resource, "metadata", "name"), environment)
		}
		var authenticated bool
		for _, env := range at(resource, "spec", "template", "spec", "containers", 0, "env").([]any) {
			if at(env, "name") == "PYPARSER_GRANITE_API_KEY" {
				authenticated = at(env, "valueFrom", "secretKeyRef", "name") == "llm-gateway"
			}
		}
		if !authenticated {
			t.Fatalf("%s has no gateway credential", at(resource, "metadata", "name"))
		}
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

func TestPublicModelIngressExcludesAdministration(t *testing.T) {
	t.Parallel()
	allowed := map[string]bool{"/v1/models": true, "/v1/chat/completions": true, "/v1/responses": true, "/health/liveliness": true}
	var found bool
	for _, resource := range renderedTree(t, "platform", "platform/components/llm") {
		if resource["kind"] != "Ingress" {
			continue
		}
		found = true
		for _, rule := range at(resource, "spec", "rules").([]any) {
			if at(rule, "host") != "llm.fredrir.com" {
				t.Fatal("unexpected public model host")
			}
			for _, path := range at(rule, "http", "paths").([]any) {
				if !allowed[at(path, "path").(string)] || at(path, "pathType") != "Exact" || at(path, "backend", "service", "name") != "litellm" {
					t.Fatal("public ingress exposes an unintended endpoint")
				}
			}
		}
	}
	if !found {
		t.Fatal("public model ingress missing")
	}
}
