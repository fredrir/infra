package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

var primaryBackupProjects = map[string]string{"llunde-pyparser": "parser", "y": "y", "portfolio": "portfolio"}

func TestPrimaryBackupBucketsKeepHistoryFromTheirWriters(t *testing.T) {
	var spec struct {
		Cells []struct {
			Name    string
			Buckets []struct {
				Name           string
				LockDays       int `json:"lockDays"`
				NoncurrentDays int `json:"noncurrentDays"`
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(repoRoot(t), objectStore, "buckets.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(yamlObjects(t, data)[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &spec); err != nil {
		t.Fatal(err)
	}
	locked := map[string]bool{}
	for _, cell := range spec.Cells {
		for _, bucket := range cell.Buckets {
			if cell.Name == "hel1" && bucket.LockDays >= 30 && bucket.NoncurrentDays > bucket.LockDays {
				locked[bucket.Name] = true
			}
		}
	}
	for _, project := range primaryBackupProjects {
		if !locked["restic-"+project] {
			t.Errorf("restic-%s is not a COMPLIANCE-locked hel1 bucket holding 30 days of history", project)
		}
	}
}

func backupObjectStoreEgress(name, workload string) object {
	return object{"metadata": object{"name": name}, "spec": object{
		"podSelector": object{"matchLabels": object{"app.kubernetes.io/name": workload}},
		"egress": []any{object{
			"to":    []any{object{"namespaceSelector": object{"matchLabels": object{"kubernetes.io/metadata.name": "object-store"}}, "podSelector": object{"matchLabels": object{"app.kubernetes.io/name": "seaweedfs", "app.kubernetes.io/instance": "hel1"}}}},
			"ports": []any{object{"port": 8333, "protocol": "TCP"}},
		}},
	}}
}

func TestOnlyBackupJobsReachTheirPrimaryRepositoryCell(t *testing.T) {
	e := newEvaluator(t)
	boundary := []string{"project-network-boundary"}
	policies := map[string]string{"backup-object-store-egress": "data-backup", "maintenance-object-store-egress": "repository-maintenance"}
	for namespace := range primaryBackupProjects {
		found := 0
		for _, resource := range renderedTree(t, "platform", "platform/projects/"+namespace) {
			name, _ := lookup(resource, "metadata", "name").(string)
			workload, ok := policies[name]
			if resource["kind"] != "NetworkPolicy" || !ok {
				continue
			}
			found++
			for _, field := range []string{"podSelector", "egress"} {
				if got, want := fmt.Sprint(lookup(resource, "spec", field)), fmt.Sprint(lookup(backupObjectStoreEgress(name, workload), "spec", field)); got != want {
					t.Errorf("%s/%s %s is %s, want %s", namespace, name, field, got, want)
				}
			}
		}
		if found != len(policies) {
			t.Errorf("%s renders %d of the %d backup egress policies", namespace, found, len(policies))
		}
	}
	for name, workload := range policies {
		other := map[string]string{"data-backup": "repository-maintenance", "repository-maintenance": "data-backup"}[workload]
		for namespace := range primaryBackupProjects {
			if !e.admitted(boundary, backupObjectStoreEgress(name, workload), namespace, reconciler, "CREATE", nil) {
				t.Errorf("%s rejected in %s", name, namespace)
			}
		}
		if e.admitted(boundary, backupObjectStoreEgress(name, workload), "llunde", reconciler, "CREATE", nil) {
			t.Errorf("%s admitted outside the primary backup projects", name)
		}
		if e.admitted(boundary, backupObjectStoreEgress(name, workload), "y", projectRunner, "CREATE", nil) {
			t.Errorf("project runner granted %s", name)
		}
		for mutation, mutate := range map[string]func(object){
			"other name":       func(p object) { set(p, "object-store-egress", "metadata", "name") },
			"application pods": func(p object) { set(p, "application", "spec", "podSelector", "matchLabels", "app.kubernetes.io/name") },
			"extra pod label":  func(p object) { set(p, "y", "spec", "podSelector", "matchLabels", "app.kubernetes.io/part-of") },
			"every pod":        func(p object) { set(p, object{}, "spec", "podSelector") },
			"pod expression": func(p object) {
				set(p, []any{object{"key": "tier", "operator": "Exists"}}, "spec", "podSelector", "matchExpressions")
			},
			"other cell": func(p object) {
				set(p, "nl", "spec", "egress", 0, "to", 0, "podSelector", "matchLabels", "app.kubernetes.io/instance")
			},
			"any cell pod":      func(p object) { set(p, object{}, "spec", "egress", 0, "to", 0, "podSelector") },
			"any namespace":     func(p object) { set(p, object{}, "spec", "egress", 0, "to", 0, "namespaceSelector") },
			"gRPC port":         func(p object) { set(p, 18334, "spec", "egress", 0, "ports", 0, "port") },
			"port range":        func(p object) { set(p, 18334, "spec", "egress", 0, "ports", 0, "endPort") },
			"UDP":               func(p object) { set(p, "UDP", "spec", "egress", 0, "ports", 0, "protocol") },
			"extra port":        func(p object) { appendAt(p, object{"port": 9327}, "spec", "egress", 0, "ports") },
			"no ports":          func(p object) { delete(at(p, "spec", "egress", 0).(object), "ports") },
			"swapped workloads": func(p object) { set(p, other, "spec", "podSelector", "matchLabels", "app.kubernetes.io/name") },
		} {
			p := backupObjectStoreEgress(name, workload)
			mutate(p)
			if e.admitted(boundary, p, "y", reconciler, "CREATE", nil) {
				t.Errorf("%s with %s admitted", name, mutation)
			}
		}
	}
}
