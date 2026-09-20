package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/platformops"
	"github.com/fredrir/infra/internal/process"
	"github.com/spf13/cobra"
)

func newPlatformCommand() *cobra.Command {
	root := &cobra.Command{Use: "platform", Short: "Operate platform services", RunE: missingCommand}
	var interval time.Duration
	var once bool
	slots := &cobra.Command{Use: "ci-slots", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		api, _, err := platformops.KubernetesAPI(os.Getenv("KUBERNETES_API_URL"), os.Getenv("SERVICE_ACCOUNT_DIR"))
		if err != nil {
			return err
		}
		return platformops.RunSlots(cmd.Context(), api, interval, once, cmd.ErrOrStderr())
	}}
	slots.Flags().DurationVar(&interval, "interval", 30*time.Second, "Reconciliation interval")
	slots.Flags().BoolVar(&once, "once", false, "Reconcile once")
	root.AddCommand(slots, &cobra.Command{Use: "runner-hook", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		var event *os.File
		if path := os.Getenv("GITHUB_EVENT_PATH"); path != "" {
			var err error
			event, err = os.Open(path)
			if err != nil {
				return err
			}
			defer event.Close()
		}
		return platformops.CheckRunnerJob(platformops.RunnerJob{Pool: os.Getenv("CI_POOL"), Event: os.Getenv("GITHUB_EVENT_NAME"), Ref: os.Getenv("GITHUB_REF"), Protected: os.Getenv("GITHUB_REF_PROTECTED") == "true", Repository: os.Getenv("GITHUB_REPOSITORY"), OwnerID: os.Getenv("GITHUB_REPOSITORY_OWNER_ID")}, event)
	}}, &cobra.Command{Use: "heartbeat PROJECT", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return platformops.Heartbeat(cmd.Context(), os.Getenv("BACKUP_HEARTBEAT_URL"), args[0], os.Getenv("BACKUP_HEARTBEAT_TOKEN"))
	}}, newCacheProvisionCommand(), newBackupCommand(), newControlBackupCommand(), &cobra.Command{Use: "repository-maintenance", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		for _, args := range [][]string{{"--retry-lock", "10m", "check", "--read-data-subset=10%"}, {"--retry-lock", "10m", "forget", "--group-by", "host,tags", "--keep-daily", "7", "--keep-weekly", "4", "--keep-monthly", "12", "--prune"}} {
			if _, err := process.Run(cmd.Context(), process.Options{Name: "restic", Args: args, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()}); err != nil {
				return err
			}
		}
		return nil
	}})
	return root
}

func newControlBackupCommand() *cobra.Command {
	return &cobra.Command{Use: "control-backup", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
		defer cancel()
		return platformops.ControlBackup(ctx, platformops.ControlBackupConfig{WorkRoot: envDefault("BACKUP_WORK_DIR", "/var/lib/platform-backups"), Metrics: envDefault("BACKUP_METRICS_DIR", "/var/lib/node_exporter/textfile_collector"), ServerToken: "/var/lib/rancher/k3s/server/token", AgentToken: "/etc/rancher/k3s/agent-token", K3sConfig: "/etc/rancher/k3s/config.yaml", HeartbeatEndpoint: os.Getenv("BACKUP_HEARTBEAT_URL"), HeartbeatToken: os.Getenv("BACKUP_HEARTBEAT_TOKEN"), Run: func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
			result, err := process.Run(ctx, process.Options{Name: name, Args: args, Dir: dir})
			return result.Stdout, err
		}, Export: func(ctx context.Context, name string, args []string, path string) error {
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			_, runErr := process.Run(ctx, process.Options{Name: name, Args: args, Stdout: file})
			return errors.Join(runErr, file.Sync(), file.Close())
		}})
	}}
}

func newCacheProvisionCommand() *cobra.Command {
	return &cobra.Command{Use: "provision-cache", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		api, namespace, err := platformops.KubernetesAPI(os.Getenv("KUBERNETES_API_URL"), os.Getenv("SERVICE_ACCOUNT_DIR"))
		if err != nil {
			return err
		}
		values := map[string]int64{}
		for _, key := range []string{"LAYOUT_CAPACITY_BYTES", "MAIN_QUOTA_BYTES", "RELEASE_QUOTA_BYTES", "TOOLCHAINS_QUOTA_BYTES", "EXPIRATION_DAYS", "QUOTA_ALERT_PERCENT", "WAIT_ATTEMPTS", "WAIT_SECONDS"} {
			fallback := ""
			if key == "WAIT_ATTEMPTS" {
				fallback = "60"
			}
			if key == "WAIT_SECONDS" {
				fallback = "5"
			}
			value, err := strconv.ParseInt(envDefault(key, fallback), 10, 64)
			if err != nil {
				return fmt.Errorf("invalid %s", key)
			}
			values[key] = value
		}
		adminURL, token := os.Getenv("GARAGE_ADMIN_URL"), os.Getenv("GARAGE_ADMIN_TOKEN")
		if adminURL == "" || token == "" {
			return fmt.Errorf("Garage admin URL and token required")
		}
		return platformops.ProvisionCache(cmd.Context(), platformops.CacheConfig{Admin: platformops.API{URL: adminURL, Token: func() (string, error) { return token, nil }}, Kubernetes: api, Namespace: namespace, KeyID: os.Getenv("PROVISIONER_KEY_ID"), KeySecret: os.Getenv("PROVISIONER_KEY_SECRET"), Capacity: values["LAYOUT_CAPACITY_BYTES"], MainQuota: values["MAIN_QUOTA_BYTES"], ReleaseQuota: values["RELEASE_QUOTA_BYTES"], ToolchainQuota: values["TOOLCHAINS_QUOTA_BYTES"], ExpirationDays: int(values["EXPIRATION_DAYS"]), AlertPercent: int(values["QUOTA_ALERT_PERCENT"]), WaitAttempts: int(values["WAIT_ATTEMPTS"]), WaitInterval: time.Duration(values["WAIT_SECONDS"]) * time.Second, Log: cmd.ErrOrStderr()})
	}}
}

func newBackupCommand() *cobra.Command {
	var exportOnly, resumeOnly bool
	cmd := &cobra.Command{Use: "backup", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c := platformops.BackupConfig{Work: envDefault("BACKUP_WORK_DIR", "/work"), Files: envDefault("BACKUP_FILES_DIR", "/files"), Kind: os.Getenv("BACKUP_KIND"), Project: os.Getenv("BACKUP_PROJECT"), Writers: strings.Fields(os.Getenv("BACKUP_WRITERS")), MongoURI: os.Getenv("MONGODB_URI"), LocalFiles: os.Getenv("BACKUP_LOCAL_FILES") == "true", LocalFilesExclude: os.Getenv("BACKUP_LOCAL_FILES_EXCLUDE"), HeartbeatEndpoint: os.Getenv("BACKUP_HEARTBEAT_URL"), HeartbeatToken: os.Getenv("BACKUP_HEARTBEAT_TOKEN"), Log: cmd.ErrOrStderr()}
		for _, setting := range []struct {
			key, fallback string
			target        *time.Duration
		}{{"BACKUP_PREFLIGHT_TIMEOUT", "120", &c.PreflightTimeout}, {"BACKUP_EXPORT_TIMEOUT", "600", &c.ExportTimeout}, {"BACKUP_UPLOAD_TIMEOUT", "900", &c.UploadTimeout}, {"BACKUP_QUIESCE_TIMEOUT", "180", &c.QuiesceTimeout}, {"BACKUP_KILL_GRACE", "120", &c.RecoveryTimeout}} {
			n, err := strconv.ParseInt(envDefault(setting.key, setting.fallback), 10, 32)
			if err != nil || n <= 0 {
				return fmt.Errorf("invalid %s", setting.key)
			}
			*setting.target = time.Duration(n) * time.Second
		}
		if c.LocalFiles {
			var err error
			c.LocalFilesMaxBytes, err = strconv.ParseInt(os.Getenv("BACKUP_LOCAL_FILES_MAX_BYTES"), 10, 64)
			if err != nil {
				return fmt.Errorf("invalid local files budget")
			}
		}
		if len(c.Writers) > 0 || resumeOnly {
			var err error
			c.Kubernetes, c.Namespace, err = platformops.KubernetesAPI(os.Getenv("KUBERNETES_API_URL"), os.Getenv("BACKUP_SERVICE_ACCOUNT_DIR"))
			if err != nil {
				return err
			}
		}
		c.Run = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
			result, err := process.Run(ctx, process.Options{Name: name, Args: args, Dir: dir, KillGrace: 2 * time.Second})
			return append(result.Stdout, result.Stderr...), err
		}
		if resumeOnly {
			return platformops.ResumeBackup(cmd.Context(), c)
		}
		return platformops.Backup(cmd.Context(), c, exportOnly)
	}}
	cmd.Flags().BoolVar(&exportOnly, "export-only", false, "Export and validate without upload")
	cmd.Flags().BoolVar(&resumeOnly, "resume-only", false, "Recover stopped writers")
	cmd.MarkFlagsMutuallyExclusive("export-only", "resume-only")
	return cmd
}
