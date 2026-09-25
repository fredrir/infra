package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/spf13/cobra"
)

func newScannerCommand() *cobra.Command {
	var cache, shared, leaseID string
	var leaseDuration time.Duration
	var java bool
	command := &cobra.Command{Use: "scanner", Short: "Reuse isolated scanner analysis and shared databases"}
	command.PersistentFlags().StringVar(&cache, "cache", os.Getenv("TRIVY_CACHE_DIR"), "Scanner analysis cache")
	prepare := &cobra.Command{Use: "prepare", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		var leases []ci.ScannerLease
		if leaseID != "" {
			if leaseDuration <= 0 || leaseDuration > 7*24*time.Hour-5*time.Minute {
				return fmt.Errorf("invalid scanner lease duration")
			}
			leases = append(leases, ci.ScannerLease{ID: leaseID, ExpiresAt: time.Now().Add(leaseDuration + 5*time.Minute)})
		}
		return ci.PrepareScanner(cmd.Context(), ci.Runner{Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()}, cache, shared, java, leases...)
	}}
	prepare.Flags().StringVar(&shared, "shared", filepath.Join(os.Getenv("HOME"), ".cache/infra/trivy-databases"), "Shared database directory")
	prepare.Flags().BoolVar(&java, "java", false, "Prepare the Java database")
	prepare.Flags().StringVar(&leaseID, "lease-id", "", "Workflow attempt lease")
	prepare.Flags().DurationVar(&leaseDuration, "lease-duration", 2*time.Hour, "Maximum workflow duration")
	release := &cobra.Command{Use: "release", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return ci.ReleaseScanner(cmd.Context(), cache, leaseID)
	}}
	release.Flags().StringVar(&leaseID, "lease-id", "", "Workflow attempt lease")
	scan := &cobra.Command{Use: "scan -- ARGS", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return ci.RunScanner(cmd.Context(), ci.Runner{Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()}, cache, args)
	}}
	command.AddCommand(prepare, scan, release)
	return command
}
