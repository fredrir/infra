package platformops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/fredrir/infra/internal/process"
)

func TestBackupRestoresOnlyOriginallyRunningWriters(t *testing.T) {
	for _, failScale := range []bool{false, true} {
		t.Run(fmt.Sprint(failScale), func(t *testing.T) {
			var mu sync.Mutex
			state := map[string]int{"review": 1, "worker": 0}
			transitions := map[string][]int{}
			failed := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if strings.HasSuffix(r.URL.Path, "/pods") {
					w.Write([]byte(`{"items":[]}`))
					return
				}
				parts := strings.Split(r.URL.Path, "/")
				name := parts[len(parts)-1]
				if name == "scale" {
					name = parts[len(parts)-2]
				}
				if r.Method == http.MethodPatch {
					replicas, ok := mergePatchReplicas(r)
					if !ok {
						w.WriteHeader(422)
						return
					}
					state[name] = replicas
					transitions[name] = append(transitions[name], state[name])
					if failScale && !failed && state[name] == 0 {
						failed = true
						w.WriteHeader(500)
						return
					}
					w.Write([]byte(`{}`))
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"spec": map[string]any{"replicas": state[name], "selector": map[string]any{"matchLabels": map[string]string{"app": name}}}})
			}))
			defer server.Close()
			c := testBackupConfig(t)
			c.Kubernetes = API{URL: server.URL}
			c.Namespace = "parser"
			c.Writers = []string{"review", "worker"}
			c.Run = func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
				if name == "pg_dump" {
					return nil, os.WriteFile(filepath.Join(c.Work, "source/database.dump"), []byte("dump"), 0600)
				}
				return nil, nil
			}
			e := Backup(context.Background(), c, true)
			if (e == nil) == failScale {
				t.Fatalf("failure=%t: %v", failScale, e)
			}
			mu.Lock()
			defer mu.Unlock()
			if state["review"] != 1 || state["worker"] != 0 || len(transitions["worker"]) != 0 {
				t.Fatalf("writer state changed: %v %v", state, transitions)
			}
			_, e = os.Stat(filepath.Join(c.Work, "source/SHA256SUMS"))
			if (e == nil) == failScale {
				t.Fatalf("checksum validity differs: %v", e)
			}
		})
	}
}

func TestMongoBackupRequiresCollectionsAndReadableArchive(t *testing.T) {
	for _, tc := range []struct {
		log     string
		restore bool
		ok      bool
	}{{"done dumping myAppDB.posts (3 documents)", true, true}, {"no collections to dump", true, false}, {"done dumping myAppDB.posts (3 documents)", false, false}} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			c := testBackupConfig(t)
			c.Kind = "mongodb"
			c.MongoURI = "mongodb://user:private@host/db"
			c.Run = func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
				if strings.Contains(strings.Join(args, " "), c.MongoURI) {
					t.Fatal("MongoDB secret in arguments")
				}
				if name == "mongodump" {
					s, e := os.Stat(filepath.Join(c.Work, "mongodb.json"))
					if e != nil || s.Mode().Perm() != 0600 {
						t.Fatal("MongoDB config permissions")
					}
					if e = os.WriteFile(filepath.Join(c.Work, "source/database.archive.gz"), []byte("archive"), 0600); e != nil {
						return nil, e
					}
					return []byte(tc.log), nil
				}
				if name == "mongorestore" && !tc.restore {
					return nil, errors.New("archive unreadable")
				}
				return nil, nil
			}
			e := Backup(context.Background(), c, true)
			if (e == nil) != tc.ok {
				t.Fatalf("got %v", e)
			}
			_, e = os.Stat(filepath.Join(c.Work, "source/SHA256SUMS"))
			if (e == nil) != tc.ok {
				t.Fatal("invalid backup checksum state")
			}
			if _, e = os.Stat(filepath.Join(c.Work, "mongodb.json")); !os.IsNotExist(e) {
				t.Fatal("MongoDB credentials retained")
			}
		})
	}
}

func TestBackupLocalFileBudgetAndExclusions(t *testing.T) {
	if _, e := exec.LookPath("tar"); e != nil {
		t.Skip("tar required")
	}
	for _, oversize := range []bool{false, true} {
		t.Run(fmt.Sprint(oversize), func(t *testing.T) {
			c := testBackupConfig(t)
			c.LocalFiles = true
			c.LocalFilesMaxBytes = 1024
			c.LocalFilesExclude = "./metadata.db*"
			for _, name := range []string{"keep.txt", "metadata.db", "metadata.db-wal"} {
				if e := os.WriteFile(filepath.Join(c.Files, name), []byte(name), 0644); e != nil {
					t.Fatal(e)
				}
			}
			tarred := false
			c.Run = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
				switch name {
				case "pg_dump":
					return nil, os.WriteFile(filepath.Join(c.Work, "source/database.dump"), []byte("dump"), 0600)
				case "du":
					if oversize {
						return []byte("999999999\t/files\n"), nil
					}
					return []byte("512\t/files\n"), nil
				case "tar":
					tarred = true
					r, e := process.Run(ctx, process.Options{Name: name, Args: args, Dir: dir})
					return r.Stdout, e
				}
				return nil, nil
			}
			e := Backup(context.Background(), c, true)
			if oversize {
				if e == nil || !strings.Contains(e.Error(), "scratch budget") || tarred {
					t.Fatalf("budget failed: %v", e)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			r, e := process.Run(context.Background(), process.Options{Name: "tar", Args: []string{"--list", "--file", filepath.Join(c.Work, "source/local-files.tar")}})
			if e != nil {
				t.Fatal(e)
			}
			if !strings.Contains(string(r.Stdout), "keep.txt") || strings.Contains(string(r.Stdout), "metadata.db") {
				t.Fatalf("excluded files retained: %s", r.Stdout)
			}
		})
	}
}

func TestSQLiteBackupCreatesReadableSnapshot(t *testing.T) {
	if _, e := exec.LookPath("sqlite3"); e != nil {
		t.Skip("sqlite3 required")
	}
	c := testBackupConfig(t)
	c.Kind = "sqlite"
	r, e := process.Run(context.Background(), process.Options{Name: "sqlite3", Args: []string{filepath.Join(c.Files, "metadata.db"), "PRAGMA journal_mode=WAL; CREATE TABLE nar(id); INSERT INTO nar VALUES (1);"}})
	if e != nil {
		t.Fatalf("initialize sqlite: %v %s", e, r.Stderr)
	}
	c.Run = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		r, e := process.Run(ctx, process.Options{Name: name, Dir: dir, Args: args})
		return r.Stdout, e
	}
	if e = Backup(context.Background(), c, true); e != nil {
		t.Fatal(e)
	}
	r, e = process.Run(context.Background(), process.Options{Name: "sqlite3", Args: []string{filepath.Join(c.Work, "source/metadata.db"), "SELECT count(*) FROM nar;"}})
	if e != nil || strings.TrimSpace(string(r.Stdout)) != "1" {
		t.Fatalf("snapshot unreadable: %v %s", e, r.Stdout)
	}
}

func TestBackupRefusesStaleSourceBeforeStoppingWriters(t *testing.T) {
	c := testBackupConfig(t)
	if e := os.Mkdir(filepath.Join(c.Work, "source"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(c.Work, "source/previous.dump"), []byte("old"), 0600); e != nil {
		t.Fatal(e)
	}
	c.Run = func(context.Context, string, string, ...string) ([]byte, error) {
		t.Fatal("operation attempted before source validation")
		return nil, nil
	}
	if e := Backup(context.Background(), c, true); e == nil || !strings.Contains(e.Error(), "empty") {
		t.Fatalf("stale source accepted: %v", e)
	}
}

func TestBackupHeartbeatFailureDoesNotInvalidateUpload(t *testing.T) {
	c := testBackupConfig(t)
	c.HeartbeatToken = "too-short"
	var log bytes.Buffer
	c.Log = &log
	c.Run = func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		if name == "pg_dump" {
			return nil, os.WriteFile(filepath.Join(c.Work, "source/database.dump"), []byte("dump"), 0600)
		}
		return nil, nil
	}
	if e := Backup(context.Background(), c, false); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(log.String(), "heartbeat failed") {
		t.Fatal("heartbeat failure not reported")
	}
}

func TestProjectedTokenMissingRefusesBeforeStoppingWriters(t *testing.T) {
	account := t.TempDir()
	if e := os.WriteFile(filepath.Join(account, "namespace"), []byte("parser"), 0600); e != nil {
		t.Fatal(e)
	}
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	api, ns, e := KubernetesAPI(server.URL, account)
	if e != nil {
		t.Fatal(e)
	}
	c := testBackupConfig(t)
	c.Kubernetes = api
	c.Namespace = ns
	c.Writers = []string{"api"}
	c.Run = func(context.Context, string, string, ...string) ([]byte, error) {
		t.Fatal("export before authorization")
		return nil, nil
	}
	if e = Backup(context.Background(), c, true); e == nil {
		t.Fatal("missing token accepted")
	}
	if called {
		t.Fatal("unauthorized API request attempted")
	}
}
