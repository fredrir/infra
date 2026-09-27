package policy

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

var primaryBackupProjects = map[string]string{"llunde-pyparser": "parser", "y": "y", "portfolio": "portfolio"}

func TestBackupJobsUseOnlyTheirDeclaredRepositories(t *testing.T) {
	jobs := map[string]bool{}
	for _, path := range []string{"platform/components/cache", "platform/components/backups", "platform/projects/llunde-pyparser", "platform/projects/y", "platform/projects/portfolio"} {
		for _, resource := range renderedTree(t, "platform", path) {
			name := at(resource, "metadata", "name")
			if resource["kind"] != "CronJob" || (name != "data-backup" && name != "repository-maintenance") {
				continue
			}
			namespace := at(resource, "metadata", "namespace").(string)
			job := namespace + "/" + name.(string)
			jobs[job] = true
			container := at(resource, "spec", "jobTemplate", "spec", "template", "spec", "containers", 0).(object)
			var sources []string
			for _, source := range container["envFrom"].([]any) {
				sources = append(sources, fmt.Sprint(lookup(source, "prefix"), lookup(source, "secretRef", "name")))
			}
			settings := map[string]string{}
			for _, entry := range container["env"].([]any) {
				value, _ := lookup(entry, "value").(string)
				if reference := lookup(entry, "valueFrom", "secretKeyRef"); reference != nil {
					value = fmt.Sprint("secret:", lookup(reference, "name"), "/", lookup(reference, "key"))
				}
				settings[at(entry, "name").(string)] = value
			}
			want := map[string]string{"BACKUP_REPOSITORIES": "offsite"}
			wantSources := []string{"OFFSITE_backup-repository"}
			if _, ok := primaryBackupProjects[namespace]; ok {
				want = map[string]string{"BACKUP_REPOSITORIES": "primary offsite", "PRIMARY_RESTIC_CACERT": "/usr/local/share/object-store/ca.crt", "PRIMARY_AWS_DEFAULT_REGION": "hel1"}
				wantSources = []string{"PRIMARY_backup-primary-repository", "OFFSITE_backup-repository"}
			}
			if name == "data-backup" {
				want["BACKUP_HEARTBEAT_TOKEN"] = "secret:backup-repository/BACKUP_HEARTBEAT_TOKEN"
			}
			if deadline, _ := lookup(resource, "spec", "jobTemplate", "spec", "activeDeadlineSeconds").(int); name == "data-backup" && deadline < len(wantSources)*(120+900)+180+600 {
				t.Errorf("%s ends after %d s, before its preflights, quiesce, export and uploads can time out", job, deadline)
			}
			if !slices.Equal(sources, wantSources) {
				t.Errorf("%s loads %v, want %v", job, sources, wantSources)
			}
			for key, value := range want {
				if settings[key] != value {
					t.Errorf("%s sets %s=%q, want %q", job, key, settings[key], value)
				}
			}
			for _, key := range []string{"RESTIC_REPOSITORY", "RESTIC_PASSWORD", "RESTIC_CACERT", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_DEFAULT_REGION"} {
				if _, ok := settings[key]; ok {
					t.Errorf("%s sets unprefixed repository setting %s", job, key)
				}
			}
		}
	}
	want := []string{"llunde-pyparser/data-backup", "llunde-pyparser/repository-maintenance", "nix-cache/data-backup", "nix-cache/repository-maintenance", "platform-backups/repository-maintenance", "portfolio/data-backup", "portfolio/repository-maintenance", "y/data-backup", "y/repository-maintenance"}
	if got := slices.Sorted(maps.Keys(jobs)); !slices.Equal(got, want) {
		t.Fatalf("backup jobs %v, want %v", got, want)
	}
}

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
