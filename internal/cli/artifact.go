package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/fredrir/infra/internal/artifact"
	"github.com/spf13/cobra"
)

func newArtifactCommand() *cobra.Command {
	root := &cobra.Command{Use: "artifact", Short: "Install verified compiled artifacts", RunE: missingCommand}
	var options artifact.InstallOptions
	install := &cobra.Command{Use: "install", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		result, err := artifact.Install(cmd.Context(), options)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cache, _ := os.UserCacheDir()
	install.Flags().StringVar(&options.URL, "url", "", "HTTPS binary URL")
	install.Flags().StringVar(&options.SHA256, "sha256", "", "Trusted binary SHA-256")
	install.Flags().StringVar(&options.Revision, "revision", "", "Full source revision")
	install.Flags().StringVar(&options.Platform, "platform", runtime.GOOS+"/"+runtime.GOARCH, "Binary platform")
	install.Flags().StringVar(&options.CacheDir, "cache-dir", filepath.Join(cache, "infra", "artifacts"), "Private artifact cache directory")
	install.Flags().StringVar(&options.Destination, "destination", "", "Atomic installation path")
	root.AddCommand(install)
	var prune artifact.PruneOptions
	cleanup := &cobra.Command{Use: "prune", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		result, err := artifact.Prune(prune)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cleanup.Flags().StringVar(&prune.CacheDir, "cache-dir", filepath.Join(cache, "infra", "artifacts"), "Private artifact cache directory")
	cleanup.Flags().StringVar(&prune.KeepRevision, "keep-revision", "", "Retain all binaries for this revision")
	cleanup.Flags().Int64Var(&prune.MaxBytes, "max-bytes", 1<<30, "Maximum retained binary bytes")
	cleanup.Flags().DurationVar(&prune.MaxAge, "max-age", 30*24*time.Hour, "Maximum unused artifact age")
	root.AddCommand(cleanup)
	return root
}
