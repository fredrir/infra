package cli

import (
	"cmp"
	"os"
	"path/filepath"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
	"github.com/spf13/cobra"
)

func newScannerCommand() *cobra.Command {
	var cache, shared string
	var java bool
	command := &cobra.Command{Use: "scanner", Short: "Reuse isolated scanner analysis and shared databases"}
	command.PersistentFlags().StringVar(&cache, "cache", os.Getenv("TRIVY_CACHE_DIR"), "Scanner analysis cache")
	command.PersistentFlags().StringVar(&shared, "shared", cmp.Or(os.Getenv("INFRA_SCANNER_DATABASES"), filepath.Join(os.Getenv("HOME"), ".cache/infra/trivy-v4-databases")), "Shared database directory")
	command.PersistentFlags().BoolVar(&java, "java", false, "Prepare the Java database")
	refresh := &cobra.Command{Use: "refresh", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return ci.RefreshScanner(cmd.Context(), process.Runner{Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()}, shared, java)
	}}
	prepare := &cobra.Command{Use: "prepare", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return ci.PrepareScanner(cmd.Context(), process.Runner{Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()}, cache, shared, java)
	}}
	scan := &cobra.Command{Use: "scan -- ARGS", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return ci.RunScanner(cmd.Context(), process.Runner{Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()}, cache, args)
	}}
	command.AddCommand(refresh, prepare, scan)
	return command
}
