package cli

import (
	"fmt"
	"time"

	"github.com/fredrir/infra/internal/operations"
	"github.com/spf13/cobra"
)

func newOperationsCommand() *cobra.Command {
	root := &cobra.Command{Use: "operations", Short: "Infrastructure maintenance tools", RunE: missingCommand}
	sdk := &cobra.Command{Use: "macos-sdk", Short: "Package and publish the Apple SDK", RunE: missingCommand}
	sdk.AddCommand(&cobra.Command{Use: "package DIRECTORY", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		a, e := operations.PackageMacOSSDK(c.Context(), args[0])
		if e != nil {
			return e
		}
		_, e = fmt.Fprintf(c.OutOrStdout(), "MACOS_SDK_OBJECT: %s\nMACOS_SDK_SHA256: %s\n", a.Object, a.SHA256)
		return e
	}})
	var upload operations.SDKUploadOptions
	put := &cobra.Command{Use: "upload ARCHIVE", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		upload.Archive = args[0]
		if e := operations.UploadMacOSSDK(c.Context(), upload); e != nil {
			return e
		}
		_, e := fmt.Fprintf(c.OutOrStdout(), "Uploaded %s\n", args[0])
		return e
	}}
	put.Flags().StringVar(&upload.Root, "root", ".", "Repository directory")
	put.Flags().DurationVar(&upload.Timeout, "timeout", 30*time.Minute, "Upload deadline")
	sdk.AddCommand(put)
	var mail operations.MailOptions
	check := &cobra.Command{Use: "test-mail-module", Short: "Test the mail module with mocked providers", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		mail.Log = c.OutOrStdout()
		return operations.TestMailModule(c.Context(), mail)
	}}
	check.Flags().StringVar(&mail.Root, "root", ".", "Repository directory")
	check.Flags().StringVar(&mail.ProviderDirectory, "provider-directory", "", "Local OpenTofu providers")
	_ = check.MarkFlagRequired("provider-directory")
	root.AddCommand(sdk, check, newEnrollmentCommand(), newRuntimeKeyCommand())
	return root
}
