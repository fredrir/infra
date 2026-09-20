package platformops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func controlConfig(t *testing.T) ControlBackupConfig {
	t.Helper()
	inputs := t.TempDir()
	for _, name := range []string{"server-token", "agent-token", "config.yaml"} {
		if e := os.WriteFile(filepath.Join(inputs, name), []byte(name), 0600); e != nil {
			t.Fatal(e)
		}
	}
	return ControlBackupConfig{WorkRoot: t.TempDir(), Metrics: t.TempDir(), ServerToken: filepath.Join(inputs, "server-token"), AgentToken: filepath.Join(inputs, "agent-token"), K3sConfig: filepath.Join(inputs, "config.yaml"), Now: func() time.Time { return time.Unix(12345, 0) }}
}
func snapshotCommand(t *testing.T, args []string) error {
	t.Helper()
	for i, arg := range args {
		if arg == "--dir" && i+1 < len(args) {
			return os.WriteFile(filepath.Join(args[i+1], "snapshot"), []byte("snapshot"), 0600)
		}
	}
	t.Fatal("snapshot output directory missing")
	return nil
}

func TestControlBackupStreamsCompleteRecoverySetAndCleansScratch(t *testing.T) {
	c := controlConfig(t)
	large := strings.Repeat("s", 9<<20)
	uploaded := false
	c.Export = func(_ context.Context, name string, args []string, dest string) error {
		if name != "k3s" {
			t.Fatal(name)
		}
		content := "k3s version"
		if args[0] == "kubectl" {
			content = large
		}
		return os.WriteFile(dest, []byte(content), 0600)
	}
	c.Run = func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		if name == "k3s" && args[0] == "etcd-snapshot" {
			return nil, snapshotCommand(t, args)
		}
		if name == "restic" && len(args) > 2 && args[2] == "backup" {
			uploaded = true
			root := args[3]
			for _, file := range []string{"snapshots/snapshot", "server-token", "agent-token", "k3s-config.yaml", "kubernetes-secrets.yaml", "k3s-version.txt", "SHA256SUMS"} {
				st, e := os.Stat(filepath.Join(root, file))
				if e != nil {
					t.Fatal(e)
				}
				if st.Mode().Perm() != 0600 {
					t.Fatalf("insecure recovery file %s: %v", file, st.Mode())
				}
			}
			data, e := os.ReadFile(filepath.Join(root, "kubernetes-secrets.yaml"))
			if e != nil || string(data) != large {
				t.Fatal("secrets export truncated")
			}
			checksums, e := os.ReadFile(filepath.Join(root, "SHA256SUMS"))
			if e != nil || !strings.Contains(string(checksums), "./snapshots/snapshot") || !strings.Contains(string(checksums), "./kubernetes-secrets.yaml") {
				t.Fatal("incomplete recovery checksums")
			}
		}
		return nil, nil
	}
	if e := ControlBackup(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	if !uploaded {
		t.Fatal("recovery set not uploaded")
	}
	entries, e := os.ReadDir(c.WorkRoot)
	if e != nil || len(entries) != 0 {
		t.Fatalf("scratch retained: %v", entries)
	}
	status, e := os.ReadFile(filepath.Join(c.Metrics, "control_backup_status.prom"))
	if e != nil || !strings.Contains(string(status), "success 1") {
		t.Fatal("success status missing")
	}
	stamp, e := os.ReadFile(filepath.Join(c.Metrics, "control_backup_success.prom"))
	if e != nil || !strings.Contains(string(stamp), "12345") {
		t.Fatal("success timestamp missing")
	}
}

func TestControlBackupFailureDoesNotPublishSuccess(t *testing.T) {
	for _, phase := range []string{"preflight", "snapshot", "export", "upload"} {
		t.Run(phase, func(t *testing.T) {
			c := controlConfig(t)
			c.Export = func(context.Context, string, []string, string) error { return errors.New("export failed") }
			c.Run = func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
				if name == "restic" && args[0] == "cat" && phase == "preflight" {
					return nil, errors.New("repository unavailable")
				}
				if name == "k3s" && args[0] == "etcd-snapshot" {
					if phase == "snapshot" {
						return nil, errors.New("snapshot failed")
					}
					return nil, snapshotCommand(t, args)
				}
				if name == "restic" && len(args) > 2 && args[2] == "backup" {
					if phase == "upload" {
						return nil, errors.New("upload failed")
					}
					t.Fatal("partial export uploaded")
				}
				return nil, nil
			}
			if phase == "upload" {
				c.Export = func(_ context.Context, _ string, _ []string, dest string) error {
					return os.WriteFile(dest, []byte("export"), 0600)
				}
			}
			if e := ControlBackup(context.Background(), c); e == nil {
				t.Fatal("failed backup succeeded")
			}
			entries, e := os.ReadDir(c.WorkRoot)
			if e != nil || len(entries) != 0 {
				t.Fatal("failure leaked scratch")
			}
			status, e := os.ReadFile(filepath.Join(c.Metrics, "control_backup_status.prom"))
			if e != nil || !strings.Contains(string(status), "success 0") {
				t.Fatal("failure metric missing")
			}
			if _, e = os.Stat(filepath.Join(c.Metrics, "control_backup_success.prom")); !os.IsNotExist(e) {
				t.Fatal("failed backup marked successful")
			}
		})
	}
}

func TestControlBackupRejectsEmptyOrMultipleSnapshots(t *testing.T) {
	for _, count := range []int{0, 2} {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			c := controlConfig(t)
			c.Run = func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
				if name == "k3s" {
					for i, arg := range args {
						if arg == "--dir" {
							for n := 0; n < count; n++ {
								if e := os.WriteFile(filepath.Join(args[i+1], string(rune('a'+n))), []byte("snapshot"), 0600); e != nil {
									return nil, e
								}
							}
						}
					}
				}
				return nil, nil
			}
			if e := ControlBackup(context.Background(), c); e == nil || !strings.Contains(e.Error(), "one recovery snapshot") {
				t.Fatalf("invalid snapshots accepted: %v", e)
			}
		})
	}
}
