package platformops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func dualRepositories() []Repository {
	return []Repository{
		{Name: "primary", Env: []string{"RESTIC_REPOSITORY=s3:https://seaweedfs-hel1.object-store.svc.cluster.local:8333/restic-parser", "RESTIC_PASSWORD=primary-secret", "RESTIC_CACERT=/usr/local/share/object-store/ca.crt"}},
		{Name: "offsite", Env: []string{"RESTIC_REPOSITORY=s3:s3.eu-north-1.amazonaws.com/llunde-pyparser-bucket/restic/platform/parser", "RESTIC_PASSWORD=offsite-secret"}},
	}
}

type backupTrace struct {
	mutex  sync.Mutex
	events []string
}

func (trace *backupTrace) add(event string) {
	trace.mutex.Lock()
	defer trace.mutex.Unlock()
	trace.events = append(trace.events, event)
}

func tracedBackup(t *testing.T, trace *backupTrace, failing map[string]bool) BackupConfig {
	t.Helper()
	replicas := 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPatch:
			patched, ok := mergePatchReplicas(r)
			if !ok {
				http.Error(w, "unprocessable patch", 422)
				return
			}
			replicas = patched
			trace.add(fmt.Sprintf("scale %d", patched))
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/pods"):
			fmt.Fprint(w, `{"items":[]}`)
		default:
			fmt.Fprintf(w, `{"spec":{"replicas":%d,"selector":{"matchLabels":{"app":"api"}}}}`, replicas)
		}
	}))
	t.Cleanup(server.Close)
	c := testBackupConfig(t)
	c.Kubernetes = API{URL: server.URL}
	c.Namespace, c.Writers = "app", []string{"api"}
	c.Repositories = dualRepositories()
	c.Run = func(_ context.Context, _ string, name string, _ ...string) ([]byte, error) {
		trace.add(name)
		if name == "pg_dump" {
			return nil, os.WriteFile(filepath.Join(c.Work, "source", "database.dump"), []byte("export"), 0o600)
		}
		return nil, nil
	}
	c.Restic = func(_ context.Context, repository Repository, args ...string) ([]byte, error) {
		operation := args[0]
		if operation == "--retry-lock" {
			operation = args[2]
		}
		trace.add(repository.Name + " " + operation)
		for _, other := range dualRepositories() {
			if other.Name != repository.Name && slices.ContainsFunc(repository.Env, func(entry string) bool { return slices.Contains(other.Env, entry) }) {
				t.Errorf("%s runs with %s settings", repository.Name, other.Name)
			}
		}
		if failing[repository.Name+" "+operation] {
			return nil, errors.New(operation + " failed")
		}
		return nil, nil
	}
	return c
}

func TestBackupUploadsToEveryRepositoryAfterResumingWriters(t *testing.T) {
	trace := &backupTrace{}
	heartbeats := 0
	heartbeat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { heartbeats++ }))
	defer heartbeat.Close()
	c := tracedBackup(t, trace, nil)
	c.HeartbeatEndpoint, c.HeartbeatToken = heartbeat.URL, strings.Repeat("h", 32)
	if err := Backup(context.Background(), c, false); err != nil {
		t.Fatal(err)
	}
	want := []string{"primary cat", "offsite cat", "scale 0", "pg_dump", "pg_restore", "scale 1", "primary backup", "offsite backup"}
	if !slices.Equal(trace.events, want) {
		t.Fatalf("backup ran %v, want %v", trace.events, want)
	}
	if heartbeats != 1 {
		t.Fatalf("complete backup sent %d heartbeats", heartbeats)
	}
}

func TestBackupReachesTheOffsiteCopyWhenThePrimaryFails(t *testing.T) {
	for failure, want := range map[string][]string{
		"primary cat":    {"primary cat", "offsite cat", "scale 0", "pg_dump", "pg_restore", "scale 1", "offsite backup"},
		"primary backup": {"primary cat", "offsite cat", "scale 0", "pg_dump", "pg_restore", "scale 1", "primary backup", "offsite backup"},
		"offsite backup": {"primary cat", "offsite cat", "scale 0", "pg_dump", "pg_restore", "scale 1", "primary backup", "offsite backup"},
	} {
		t.Run(failure, func(t *testing.T) {
			trace := &backupTrace{}
			heartbeats := 0
			heartbeat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { heartbeats++ }))
			defer heartbeat.Close()
			c := tracedBackup(t, trace, map[string]bool{failure: true})
			c.HeartbeatEndpoint, c.HeartbeatToken = heartbeat.URL, strings.Repeat("h", 32)
			err := Backup(context.Background(), c, false)
			if err == nil || !strings.Contains(err.Error(), strings.Fields(failure)[0]) {
				t.Fatalf("failure of %s not reported: %v", failure, err)
			}
			if !slices.Equal(trace.events, want) {
				t.Fatalf("backup ran %v, want %v", trace.events, want)
			}
			if heartbeats != 0 {
				t.Fatal("heartbeat sent for an incomplete backup")
			}
		})
	}
}

func TestBackupLeavesWritersAloneWhenNoRepositoryIsReachable(t *testing.T) {
	trace := &backupTrace{}
	c := tracedBackup(t, trace, map[string]bool{"primary cat": true, "offsite cat": true})
	err := Backup(context.Background(), c, false)
	if err == nil || !strings.Contains(err.Error(), "primary") || !strings.Contains(err.Error(), "offsite") {
		t.Fatalf("unreachable repositories not reported: %v", err)
	}
	if !slices.Equal(trace.events, []string{"primary cat", "offsite cat"}) {
		t.Fatalf("backup touched writers or exported: %v", trace.events)
	}
}

func TestRepositoriesComeFromTheirPrefixedSettings(t *testing.T) {
	environment := map[string]string{
		"PRIMARY_RESTIC_REPOSITORY":     "s3:https://seaweedfs-hel1.object-store.svc.cluster.local:8333/restic-y",
		"PRIMARY_RESTIC_PASSWORD":       "primary",
		"PRIMARY_RESTIC_CACERT":         "/usr/local/share/object-store/ca.crt",
		"PRIMARY_AWS_ACCESS_KEY_ID":     "0B1C2D3E4F5061728394",
		"PRIMARY_AWS_SECRET_ACCESS_KEY": "primary-secret",
		"PRIMARY_BACKUP_HEARTBEAT":      "ignored",
		"OFFSITE_RESTIC_REPOSITORY":     "s3:s3.eu-north-1.amazonaws.com/llunde-pyparser-bucket/restic/platform/y",
		"OFFSITE_RESTIC_PASSWORD":       "offsite",
		"OFFSITE_AWS_DEFAULT_REGION":    "eu-north-1",
		"RESTIC_REPOSITORY":             "unprefixed",
	}
	lookup := func(key string) (string, bool) {
		value, ok := environment[key]
		return value, ok
	}
	repositories, err := RepositoriesFromEnvironment([]string{"primary", "offsite"}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	want := []Repository{
		{Name: "primary", Env: []string{"RESTIC_REPOSITORY=" + environment["PRIMARY_RESTIC_REPOSITORY"], "RESTIC_PASSWORD=primary", "RESTIC_CACERT=/usr/local/share/object-store/ca.crt", "AWS_ACCESS_KEY_ID=0B1C2D3E4F5061728394", "AWS_SECRET_ACCESS_KEY=primary-secret"}},
		{Name: "offsite", Env: []string{"RESTIC_REPOSITORY=" + environment["OFFSITE_RESTIC_REPOSITORY"], "RESTIC_PASSWORD=offsite", "AWS_DEFAULT_REGION=eu-north-1"}},
	}
	if fmt.Sprint(repositories) != fmt.Sprint(want) {
		t.Fatalf("repositories %v, want %v", repositories, want)
	}
	for _, names := range [][]string{nil, {"primary", "primary"}, {"Primary"}, {"primary-1"}, {"missing"}} {
		if _, err := RepositoriesFromEnvironment(names, lookup); err == nil {
			t.Errorf("repositories %v accepted", names)
		}
	}
	delete(environment, "OFFSITE_RESTIC_PASSWORD")
	if _, err := RepositoriesFromEnvironment([]string{"offsite"}, lookup); err == nil {
		t.Error("repository without a password accepted")
	}
}

func TestMaintenanceCoversEveryRepository(t *testing.T) {
	var calls []string
	restic := func(_ context.Context, repository Repository, args ...string) ([]byte, error) {
		calls = append(calls, repository.Name+" "+args[2])
		if repository.Name == "primary" && args[2] == "check" {
			return nil, errors.New("check failed")
		}
		return nil, nil
	}
	err := MaintainRepositories(context.Background(), restic, dualRepositories(), time.Minute)
	if err == nil || !strings.Contains(err.Error(), "maintenance of primary") {
		t.Fatalf("primary maintenance failure not reported: %v", err)
	}
	if !slices.Equal(calls, []string{"primary check", "offsite check", "offsite forget"}) {
		t.Fatalf("maintenance ran %v", calls)
	}
	if err := MaintainRepositories(context.Background(), restic, nil, time.Minute); err == nil {
		t.Fatal("maintenance without repositories accepted")
	}
	succeeding := func(context.Context, Repository, ...string) ([]byte, error) { return nil, nil }
	if err := MaintainRepositories(context.Background(), succeeding, dualRepositories(), 0); err == nil {
		t.Fatal("maintenance without a timeout accepted")
	}
}

func TestSlowRepositoryMaintenanceLeavesTheOthersTheirTime(t *testing.T) {
	var calls []string
	restic := func(ctx context.Context, repository Repository, args ...string) ([]byte, error) {
		calls = append(calls, repository.Name+" "+args[2])
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 200*time.Millisecond {
			return nil, errors.New(repository.Name + " runs beyond its own maintenance time")
		}
		if repository.Name == "primary" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) < 150*time.Millisecond {
			return nil, errors.New("offsite started without its own time")
		}
		return nil, nil
	}
	started := time.Now()
	err := MaintainRepositories(context.Background(), restic, dualRepositories(), 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "maintenance of primary") || strings.Contains(err.Error(), "offsite") {
		t.Fatalf("maintenance returned %v", err)
	}
	if !slices.Equal(calls, []string{"primary check", "offsite check", "offsite forget"}) || time.Since(started) > time.Second {
		t.Fatalf("maintenance ran %v in %v", calls, time.Since(started))
	}
}
