package cli

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/fredrir/infra/internal/platformops"
	"github.com/spf13/cobra"
)

func newPrefetchCommand() *cobra.Command {
	var opts platformops.PrefetchOptions
	var apply bool
	cmd := &cobra.Command{Use: "prefetch IMAGE...", Short: "Warm node image layers without running application code", Args: cobra.RangeArgs(1, 8), RunE: func(cmd *cobra.Command, args []string) error {
		var nonce [6]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		opts.Name = "infra-prefetch-" + hex.EncodeToString(nonce[:])
		opts.Images = args
		manifest, err := platformops.PrefetchManifest(opts)
		if err != nil {
			return err
		}
		if apply {
			return platformops.Prefetch(cmd.Context(), opts, nil)
		}
		_, err = cmd.OutOrStdout().Write(append(manifest, '\n'))
		return err
	}}
	cmd.Flags().StringVar(&opts.Namespace, "namespace", "", "Project namespace")
	cmd.Flags().StringVar(&opts.Node, "node", "", "Destination node hostname")
	cmd.Flags().StringVar(&opts.UtilityImage, "utility-image", "", "Verified platform tools image digest")
	cmd.Flags().StringSliceVar(&opts.PullSecrets, "pull-secret", []string{"ghcr"}, "Namespace image pull secrets")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", 30*time.Second, "Prefetch deadline, at most one minute")
	cmd.Flags().BoolVar(&apply, "apply", false, "Create, wait for and remove the prefetch job")
	return cmd
}
