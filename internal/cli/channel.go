package cli

import (
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
	"github.com/spf13/cobra"
)

func newCIChannelCommand() *cobra.Command {
	var root, tag, revision string
	command := &cobra.Command{Use: "channel release|prepare|propose|promote", Short: "Promote shared CI releases", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, args []string) error {
		api := ci.ChannelAPI{Client: &http.Client{Timeout: 30 * time.Second}, Base: "https://api.github.com", Token: os.Getenv("GH_TOKEN")}
		runner := process.Runner{Dir: root, Stdout: command.OutOrStdout(), Stderr: command.ErrOrStderr()}
		switch args[0] {
		case "release":
			if err := runner.Run(command.Context(), "git", "merge-base", "--is-ancestor", revision, "origin/main"); err != nil {
				return err
			}
			if err := api.Publish(command.Context(), tag, revision); err != nil {
				return err
			}
			return ci.PrepareChannel(root, ci.CIChannel{Schema: 1, Tag: "ci-v1", Release: tag, Revision: revision})
		case "propose":
			return ci.ProposeChannel(command.Context(), runner)
		case "prepare":
			revision, err := api.ReleaseRevision(command.Context(), tag)
			if err != nil {
				return err
			}
			if err := api.Checked(command.Context(), revision); err != nil {
				return err
			}
			if err := runner.Run(command.Context(), "git", "merge-base", "--is-ancestor", revision, "origin/main"); err != nil {
				return err
			}
			return ci.PrepareChannel(root, ci.CIChannel{Schema: 1, Tag: "ci-v1", Release: tag, Revision: revision})
		case "promote":
			return api.Promote(command.Context(), root)
		default:
			return errors.New("unknown channel action")
		}
	}}
	command.Flags().StringVar(&root, "root", ".", "Infrastructure checkout")
	command.Flags().StringVar(&tag, "release", "", "Immutable CI release tag")
	command.Flags().StringVar(&revision, "revision", os.Getenv("GITHUB_SHA"), "Checked source revision")
	return command
}
