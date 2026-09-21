package cli

import (
	"os"
	"path/filepath"

	"github.com/fredrir/infra/internal/ci"
	"github.com/spf13/cobra"
)

func newScannerCommand() *cobra.Command {
	var cache, shared string
	var java bool
	command := &cobra.Command{Use: "scanner", Short: "Reuse isolated scanner analysis and shared databases"}
	command.PersistentFlags().StringVar(&cache, "cache", os.Getenv("TRIVY_CACHE_DIR"), "Scanner analysis cache")
	prepare := &cobra.Command{Use: "prepare", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return ci.PrepareScanner(cmd.Context(), ci.Runner{Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()}, cache, shared, java)
	}}
	prepare.Flags().StringVar(&shared, "shared", filepath.Join(os.Getenv("HOME"), ".cache/infra/trivy-databases"), "Shared database directory")
	prepare.Flags().BoolVar(&java, "java", false, "Prepare the Java database")
	scan := &cobra.Command{Use: "scan -- ARGS", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return ci.RunScanner(cmd.Context(), ci.Runner{Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()}, cache, args)
	}}
	command.AddCommand(prepare, scan)
	return command
}
