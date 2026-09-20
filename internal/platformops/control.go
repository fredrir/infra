package platformops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type ControlBackupConfig struct {
	WorkRoot          string
	Metrics           string
	ServerToken       string
	AgentToken        string
	K3sConfig         string
	HeartbeatEndpoint string
	HeartbeatToken    string
	Run               Command
	Export            func(context.Context, string, []string, string) error
	Now               func() time.Time
}

func ControlBackup(ctx context.Context, c ControlBackupConfig) (result error) {
	if c.Run == nil || c.WorkRoot == "" || c.Metrics == "" {
		return fmt.Errorf("control backup configuration required")
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	defer func() {
		success := 0
		if result == nil {
			success = 1
		}
		result = errors.Join(result, writeMetric(c.Metrics, "control_backup_status.prom", fmt.Sprintf("platform_control_backup_last_run_success %d\n", success)))
	}()
	if _, err := c.Run(ctx, "", "restic", "cat", "config"); err != nil {
		return err
	}
	work, err := os.MkdirTemp(c.WorkRoot, "recovery.")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, os.RemoveAll(work)) }()
	snapshots := filepath.Join(work, "snapshots")
	if err := os.Mkdir(snapshots, 0o700); err != nil {
		return err
	}
	if _, err := c.Run(ctx, "", "k3s", "etcd-snapshot", "save", "--name", "platform-recovery", "--dir", snapshots, "--s3=false"); err != nil {
		return err
	}
	entries, err := os.ReadDir(snapshots)
	if err != nil {
		return err
	}
	if len(entries) != 1 || !entries[0].Type().IsRegular() {
		return fmt.Errorf("expected one recovery snapshot")
	}
	for name, path := range map[string]string{"server-token": c.ServerToken, "agent-token": c.AgentToken, "k3s-config.yaml": c.K3sConfig} {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(work, name), data, 0o600); err != nil {
			return err
		}
	}
	for _, item := range []struct {
		name string
		args []string
	}{{"kubernetes-secrets.yaml", []string{"kubectl", "get", "secrets", "--all-namespaces", "-o", "yaml"}}, {"k3s-version.txt", []string{"--version"}}} {
		if c.Export != nil {
			path := filepath.Join(work, item.name)
			if err := c.Export(ctx, "k3s", item.args, path); err != nil {
				return err
			}
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || info.Size() == 0 {
				return fmt.Errorf("empty recovery export: %s", item.name)
			}
			continue
		}
		data, err := c.Run(ctx, "", "k3s", item.args...)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			return fmt.Errorf("empty recovery export: %s", item.name)
		}
		if err := os.WriteFile(filepath.Join(work, item.name), data, 0o600); err != nil {
			return err
		}
	}
	if err := WriteChecksums(work); err != nil {
		return err
	}
	if _, err := c.Run(ctx, "", "restic", "--retry-lock", "10m", "backup", work, "--host", "fredrir-07", "--tag", "control", "--tag", "k3s", "--json"); err != nil {
		return err
	}
	if err := Heartbeat(ctx, c.HeartbeatEndpoint, "control", c.HeartbeatToken); err != nil {
		return err
	}
	return writeMetric(c.Metrics, "control_backup_success.prom", fmt.Sprintf("platform_control_backup_last_success_timestamp_seconds %d\n", c.Now().Unix()))
}

func writeMetric(root, name, value string) error {
	file, err := os.CreateTemp(root, ".metric-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString(value); err != nil {
		file.Close()
		return err
	}
	if err := file.Chmod(0o644); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(root, name))
}
