package platformops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mergePatchReplicas(r *http.Request) (int, bool) {
	var patch struct{ Spec struct{ Replicas *int } }
	if r.Header.Get("Content-Type") != "application/merge-patch+json" || json.NewDecoder(r.Body).Decode(&patch) != nil || patch.Spec.Replicas == nil {
		return 0, false
	}
	return *patch.Spec.Replicas, true
}

func TestBackupRecoversWritersAfterExportFailure(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			replicas := 1
			var transitions []int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == "PATCH":
					patched, ok := mergePatchReplicas(r)
					if !ok {
						http.Error(w, "unprocessable patch", 422)
						return
					}
					replicas = patched
					transitions = append(transitions, replicas)
					fmt.Fprint(w, `{}`)
				case strings.HasSuffix(r.URL.Path, "/pods"):
					fmt.Fprint(w, `{"items":[]}`)
				default:
					fmt.Fprintf(w, `{"spec":{"replicas":%d,"selector":{"matchLabels":{"app":"api"}}}}`, replicas)
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := testBackupConfig(t)
			c.Kubernetes = API{URL: server.URL}
			c.Namespace, c.Writers = "app", []string{"api"}
			c.Run = func(context.Context, string, string, ...string) ([]byte, error) {
				if canceled {
					cancel()
				}
				return nil, errors.New("export failed")
			}
			if err := Backup(ctx, c, true); err == nil {
				t.Fatal("export succeeded")
			}
			if replicas != 1 || fmt.Sprint(transitions) != "[0 1]" {
				t.Fatalf("writer was not recovered: %v", transitions)
			}
			if _, err := os.Stat(filepath.Join(c.Work, "resume.json")); !os.IsNotExist(err) {
				t.Fatalf("recovery journal retained: %v", err)
			}
			if _, err := os.Stat(filepath.Join(c.Work, "source", "SHA256SUMS")); !os.IsNotExist(err) {
				t.Fatal("failed export marked valid")
			}
		})
	}
}

func TestBackupValidatesBeforeUpload(t *testing.T) {
	c := testBackupConfig(t)
	var calls []string
	c.Run = func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "pg_dump" {
			return nil, os.WriteFile(filepath.Join(c.Work, "source", "database.dump"), []byte("export"), 0o600)
		}
		if name == "restic" && len(args) > 2 && args[2] == "backup" {
			data, err := os.ReadFile(filepath.Join(c.Work, "source", "SHA256SUMS"))
			if err != nil || !strings.Contains(string(data), "./database.dump") {
				t.Fatal("upload preceded validation")
			}
		}
		return nil, nil
	}
	if err := Backup(context.Background(), c, false); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 || !strings.HasPrefix(calls[0], "restic cat config") || !strings.HasPrefix(calls[2], "pg_restore --list") {
		t.Fatalf("unexpected backup calls: %v", calls)
	}
}

func TestBackupRefusesUnfinishedRecovery(t *testing.T) {
	c := testBackupConfig(t)
	if err := os.WriteFile(filepath.Join(c.Work, "resume.json"), []byte(`[{"name":"api","replicas":1}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Run = func(context.Context, string, string, ...string) ([]byte, error) {
		t.Fatal("export attempted")
		return nil, nil
	}
	if err := Backup(context.Background(), c, true); err == nil || !strings.Contains(err.Error(), "unfinished") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestChecksumsRejectSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := WriteChecksums(root); err == nil {
		t.Fatal("symlink accepted")
	}
}

func testBackupConfig(t *testing.T) BackupConfig {
	t.Helper()
	return BackupConfig{Work: t.TempDir(), Files: t.TempDir(), Kind: "postgres", Project: "parser", PreflightTimeout: time.Second, ExportTimeout: time.Second, UploadTimeout: time.Second, QuiesceTimeout: time.Second, RecoveryTimeout: time.Second}
}
