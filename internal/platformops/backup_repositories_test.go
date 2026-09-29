package platformops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

func TestBackupRepositoryInterval(t *testing.T) {
	now := time.Date(2026, 9, 29, 0, 15, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		inventory string
		due       bool
		fails     bool
	}{
		{"empty", `[]`, true, false},
		{"recent", `[{"time":"2026-09-16T00:15:00Z"}]`, false, false},
		{"due", `[{"time":"2026-09-15T00:15:00Z"}]`, true, false},
		{"due despite export duration", `[{"time":"2026-09-15T00:45:00Z"}]`, true, false},
		{"newest controls interval", `[{"time":"2026-09-01T00:15:00Z"},{"time":"2026-09-28T00:15:00Z"}]`, false, false},
		{"unreadable", `{`, false, true},
		{"missing timestamp", `[{}]`, false, true},
		{"future timestamp", `[{"time":"2026-10-01T00:15:00Z"}]`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restic := func(context.Context, Repository, ...string) ([]byte, error) { return []byte(tc.inventory), nil }
			due, err := repositoryDue(context.Background(), restic, Repository{IntervalDays: 14}, now)
			if due != tc.due || (err != nil) != tc.fails {
				t.Fatalf("due=%v error=%v", due, err)
			}
		})
	}
}

func TestBackupPrunesOnlyAfterSuccessfulReplacement(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			trace := new(backupTrace)
			c := tracedBackup(t, trace, nil)
			c.Repositories = []Repository{{Name: "offsite", KeepLast: 1}}
			var operations []string
			c.Restic = func(_ context.Context, _ Repository, args ...string) ([]byte, error) {
				op := args[0]
				if op == "--retry-lock" {
					op = args[2]
				}
				operations = append(operations, op)
				if op == "backup" && failed {
					return nil, errors.New("upload failed")
				}
				if op == "forget" && !slices.Equal(args, []string{"--retry-lock", "10m", "forget", "--group-by", "", "--keep-last", "1", "--prune"}) {
					t.Fatalf("retention: %v", args)
				}
				return nil, nil
			}
			err := Backup(context.Background(), c, false)
			if (err != nil) != failed {
				t.Fatal(err)
			}
			want := []string{"cat", "backup", "forget", "check"}
			if failed {
				want = want[:2]
			}
			if !slices.Equal(operations, want) {
				t.Fatalf("operations=%v", operations)
			}
		})
	}
}

func TestRecentOffsiteBackupDoesNotPreventPrimaryBackup(t *testing.T) {
	trace := new(backupTrace)
	c := tracedBackup(t, trace, nil)
	c.Repositories[1].IntervalDays = 14
	c.Now = func() time.Time { return time.Date(2026, 9, 29, 0, 15, 0, 0, time.UTC) }
	run := c.Restic
	c.Restic = func(ctx context.Context, r Repository, args ...string) ([]byte, error) {
		if args[0] == "snapshots" {
			return []byte(`[{"time":"2026-09-28T00:15:00Z"}]`), nil
		}
		return run(ctx, r, args...)
	}
	if err := Backup(context.Background(), c, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(trace.events, "primary backup") || slices.Contains(trace.events, "offsite backup") {
		t.Fatalf("events=%v", trace.events)
	}
}

func TestDatasetExportFailurePreventsUploadAndPruning(t *testing.T) {
	for _, failure := range []string{"empty", "oversize", "copy", "check"} {
		t.Run(failure, func(t *testing.T) {
			trace := new(backupTrace)
			c := tracedBackup(t, trace, nil)
			c.Dataset = "dataset:parser-dataset"
			c.DatasetMaxBytes = 100
			run := c.Run
			c.Run = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
				if name != "rclone" {
					return run(ctx, dir, name, args...)
				}
				switch args[0] {
				case "size":
					if failure == "empty" {
						return []byte(`{"count":0,"bytes":0}`), nil
					}
					if failure == "oversize" {
						return []byte(`{"count":1,"bytes":101}`), nil
					}
					return []byte(`{"count":1,"bytes":10}`), nil
				case failure:
					return nil, errors.New("dataset failed")
				}
				return nil, nil
			}
			if err := Backup(context.Background(), c, false); err == nil {
				t.Fatal("incomplete dataset accepted")
			}
			if !slices.Contains(trace.events, "scale 1") || slices.Contains(trace.events, "primary backup") || slices.Contains(trace.events, "offsite backup") {
				t.Fatalf("events=%v", trace.events)
			}
		})
	}
}

func TestResticRetentionRestoresNewestSnapshots(t *testing.T) {
	binary, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic is unavailable")
	}
	for _, keep := range []int{1, 3} {
		t.Run(fmt.Sprint(keep), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			repository := Repository{Name: "test", KeepLast: keep}
			run := func(ctx context.Context, _ Repository, args ...string) ([]byte, error) {
				cmd := exec.CommandContext(ctx, binary, args...)
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TZ=UTC", "RESTIC_REPOSITORY=" + filepath.Join(root, "repository"), "RESTIC_PASSWORD=test-password", "RESTIC_CACHE_DIR=" + filepath.Join(root, "cache")}
				return cmd.CombinedOutput()
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			must := func(args ...string) []byte {
				t.Helper()
				out, err := run(ctx, repository, args...)
				if err != nil {
					t.Fatalf("restic %v: %v\n%s", args, err, out)
				}
				return out
			}
			must("init")
			source := filepath.Join(root, "source.txt")
			for day := 1; day <= 4; day++ {
				if err := os.WriteFile(source, fmt.Appendf(nil, "snapshot %d", day), 0600); err != nil {
					t.Fatal(err)
				}
				must("backup", source, "--time", fmt.Sprintf("2020-01-%02d 00:00:00", day), "--host", fmt.Sprintf("host-%d", day), "--tag", fmt.Sprintf("tag-%d", day))
			}
			must(retentionArguments(repository)...)
			var snapshots []struct {
				Time time.Time `json:"time"`
			}
			if err := json.Unmarshal(must("snapshots", "--json"), &snapshots); err != nil {
				t.Fatal(err)
			}
			if len(snapshots) != keep {
				t.Fatalf("kept %d snapshots instead of %d", len(snapshots), keep)
			}
			for _, snapshot := range snapshots {
				if snapshot.Time.Day() < 5-keep {
					t.Fatalf("old snapshot retained: %s", snapshot.Time)
				}
			}
			if got := string(must("dump", "latest", source)); got != "snapshot 4" {
				t.Fatalf("restore: %q", got)
			}
			must("check", "--read-data")
		})
	}
}
