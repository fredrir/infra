package reconcile

import (
	"bytes"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type rbacRule struct {
	APIGroups     []string `yaml:"apiGroups"`
	Resources     []string `yaml:"resources"`
	ResourceNames []string `yaml:"resourceNames"`
	Verbs         []string `yaml:"verbs"`
}

type rbacObject struct {
	Kind     string
	Metadata struct{ Name, Namespace string }
	Rules    []rbacRule
	RoleRef  struct{ Kind, Name string } `yaml:"roleRef"`
	Subjects []struct{ Kind, Name, Namespace string }
}

func permissions(verbs, groups, resources, names []string, namespace string) map[string]bool {
	if len(names) == 0 {
		names = []string{""}
	}
	expanded := map[string]bool{}
	for _, verb := range verbs {
		for _, group := range groups {
			for _, resource := range resources {
				for _, name := range names {
					expanded[strings.Join([]string{verb, group, resource, name, namespace}, " ")] = true
				}
			}
		}
	}
	return expanded
}

func serviceAccountPermissions(t *testing.T, objects []rbacObject, account string) map[string]bool {
	t.Helper()
	roles := map[string][]rbacRule{}
	for _, object := range objects {
		switch object.Kind {
		case "ClusterRole":
			roles["ClusterRole/"+object.Metadata.Name] = object.Rules
		case "Role":
			roles["Role/"+object.Metadata.Namespace+"/"+object.Metadata.Name] = object.Rules
		}
	}
	effective := map[string]bool{}
	for _, object := range objects {
		if object.Kind != "ClusterRoleBinding" && object.Kind != "RoleBinding" {
			continue
		}
		if !slices.ContainsFunc(object.Subjects, func(subject struct{ Kind, Name, Namespace string }) bool {
			return subject.Kind == "ServiceAccount" && subject.Namespace+"/"+subject.Name == account
		}) {
			continue
		}
		namespace, role := "*", "ClusterRole/"+object.RoleRef.Name
		if object.Kind == "RoleBinding" {
			namespace = object.Metadata.Namespace
			if object.RoleRef.Kind == "Role" {
				role = "Role/" + namespace + "/" + object.RoleRef.Name
			}
		}
		rules, ok := roles[role]
		if !ok {
			t.Fatalf("%s %s references undeclared %s", object.Kind, object.Metadata.Name, role)
		}
		for _, rule := range rules {
			maps.Copy(effective, permissions(rule.Verbs, rule.APIGroups, rule.Resources, rule.ResourceNames, namespace))
		}
	}
	return effective
}

func reconciliationPolicy[T any](t *testing.T) []T {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "platform/components/policy/reconciliation.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var documents []T
	for {
		var document T
		if err := decoder.Decode(&document); errors.Is(err, io.EOF) {
			return documents
		} else if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, document)
	}
}

func expectedPermissions(grants ...map[string]bool) map[string]bool {
	expected := map[string]bool{}
	for _, grant := range grants {
		maps.Copy(expected, grant)
	}
	return expected
}

var (
	fluxObjectReads      = permissions([]string{"get", "list", "watch"}, []string{"source.toolkit.fluxcd.io", "kustomize.toolkit.fluxcd.io", "helm.toolkit.fluxcd.io"}, []string{"gitrepositories", "kustomizations", "helmreleases"}, nil, "*")
	platformSettingsRead = permissions([]string{"get"}, []string{""}, []string{"configmaps"}, []string{"platform-settings"}, "flux-system")
	workloadReads        = expectedPermissions(
		permissions([]string{"get", "list"}, []string{"apps"}, []string{"deployments", "statefulsets", "daemonsets"}, nil, "*"),
		permissions([]string{"get", "list"}, []string{"batch"}, []string{"jobs"}, nil, "*"),
		permissions([]string{"get", "list"}, []string{"actions.github.com"}, []string{"autoscalingrunnersets", "ephemeralrunners"}, nil, "*"),
	)
	listenerReads = permissions([]string{"list"}, []string{""}, []string{"pods"}, nil, "arc-system")
	artifactReads = expectedPermissions(
		permissions([]string{"get", "list", "watch"}, []string{"source.toolkit.fluxcd.io"}, []string{"externalartifacts"}, nil, "flux-system"),
		permissions([]string{"get"}, []string{"source.extensions.fluxcd.io"}, []string{"artifactgenerators"}, []string{"platform-artifacts"}, "flux-system"),
	)
)

func TestVerifyIdentityReadsOnlyWhatCloudVerificationReads(t *testing.T) {
	got := serviceAccountPermissions(t, reconciliationPolicy[rbacObject](t), "flux-system/infrastructure-verify")
	want := expectedPermissions(fluxObjectReads, platformSettingsRead, workloadReads, listenerReads, artifactReads)
	if !maps.Equal(got, want) {
		t.Errorf("verify permissions:\nextra %q\nmissing %q", permissionDifference(got, want), permissionDifference(want, got))
	}
	for permission := range got {
		fields := strings.Fields(permission)
		if !slices.Contains([]string{"get", "list", "watch"}, fields[0]) || strings.Contains(permission, " secrets ") {
			t.Errorf("verify identity may %s", permission)
		}
	}
}

func TestPlanAndApplyIdentityPermissions(t *testing.T) {
	objects := reconciliationPolicy[rbacObject](t)
	annotations := expectedPermissions(
		permissions([]string{"patch"}, []string{"source.toolkit.fluxcd.io"}, []string{"gitrepositories"}, []string{"flux-system"}, "*"),
		permissions([]string{"patch"}, []string{"kustomize.toolkit.fluxcd.io", "helm.toolkit.fluxcd.io"}, []string{"kustomizations", "helmreleases"}, nil, "*"),
	)
	for account, want := range map[string]map[string]bool{
		"flux-system/infrastructure-plan":  expectedPermissions(fluxObjectReads, platformSettingsRead),
		"flux-system/infrastructure-apply": expectedPermissions(fluxObjectReads, platformSettingsRead, workloadReads, listenerReads, artifactReads, annotations),
	} {
		if got := serviceAccountPermissions(t, objects, account); !maps.Equal(got, want) {
			t.Errorf("%s permissions:\nextra %q\nmissing %q", account, permissionDifference(got, want), permissionDifference(want, got))
		}
	}
}

func TestReconciliationIdentitiesHaveTokenSecrets(t *testing.T) {
	var accounts, tokens []string
	for _, document := range reconciliationPolicy[reconciliationDocument](t) {
		switch document.Kind {
		case "ServiceAccount":
			if document.AutomountServiceAccountToken == nil || *document.AutomountServiceAccountToken {
				t.Errorf("ServiceAccount %s mounts its token", document.Metadata.Name)
			}
			accounts = append(accounts, document.Metadata.Namespace+"/"+document.Metadata.Name)
		case "Secret":
			if document.Type != "kubernetes.io/service-account-token" || document.Metadata.Name != document.Metadata.Annotations["kubernetes.io/service-account.name"]+"-credentials" {
				t.Errorf("Secret %s is not its ServiceAccount's token", document.Metadata.Name)
			}
			tokens = append(tokens, document.Metadata.Namespace+"/"+document.Metadata.Annotations["kubernetes.io/service-account.name"])
		}
	}
	want := []string{"flux-system/infrastructure-apply", "flux-system/infrastructure-plan", "flux-system/infrastructure-verify"}
	slices.Sort(accounts)
	slices.Sort(tokens)
	if !slices.Equal(accounts, want) || !slices.Equal(tokens, want) {
		t.Errorf("service accounts %q and tokens %q, want %q", accounts, tokens, want)
	}
}

type reconciliationDocument struct {
	Kind     string
	Type     string
	Metadata struct {
		Name, Namespace string
		Annotations     map[string]string
	}
	AutomountServiceAccountToken *bool `yaml:"automountServiceAccountToken"`
}

func permissionDifference(a, b map[string]bool) []string {
	var missing []string
	for key := range a {
		if !b[key] {
			missing = append(missing, key)
		}
	}
	slices.Sort(missing)
	return missing
}
