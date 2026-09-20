package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/fredrir/infra/internal/enrollment"
	"github.com/fredrir/infra/internal/process"
	"github.com/spf13/cobra"
)

func newEnrollmentCommand() *cobra.Command {
	var target enrollment.Target
	var binary string
	var sudo bool
	command := &cobra.Command{Use: "enrollment create-deliver|revoke-unused", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if args[0] != "create-deliver" && args[0] != "revoke-unused" {
			return fmt.Errorf("invalid enrollment action")
		}
		if target.Host == "" {
			target.Host = target.Node
		}
		transport, err := enrollment.SSHTransport(binary, sudo)
		if err != nil {
			return err
		}
		if err := enrollment.DisableCoreDumps(); err != nil {
			return fmt.Errorf("disable core dumps: %w", err)
		}
		environment := map[string]string{}
		for _, entry := range os.Environ() {
			name, value, _ := strings.Cut(entry, "=")
			environment[name] = value
		}
		client := enrollment.Client{API: enrollment.HTTPAPI("", nil), Transport: transport, Environment: environment}
		var result map[string]any
		if args[0] == "create-deliver" {
			result, err = client.CreateDeliver(cmd.Context(), target)
		} else {
			result, err = client.RevokeUnused(cmd.Context(), target)
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	enrollmentFlags(command, &target)
	command.Flags().StringVar(&binary, "remote-infra", "/usr/local/bin/infra", "Verified compiled binary on the SSH host")
	command.Flags().BoolVar(&sudo, "sudo", false, "Invoke the remote binary through noninteractive sudo")
	return command
}

func newRuntimeKeyCommand() *cobra.Command {
	var target enrollment.Target
	command := &cobra.Command{Use: "runtime-key preflight|deliver|cleanup", Hidden: true, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if runtime.GOOS != "linux" {
			return fmt.Errorf("runtime key operation requires Linux")
		}
		if err := enrollment.ValidateTarget(target); err != nil {
			return err
		}
		if args[0] != "preflight" && args[0] != "deliver" && args[0] != "cleanup" {
			return fmt.Errorf("invalid runtime operation")
		}
		if err := enrollment.DisableCoreDumps(); err != nil {
			return fmt.Errorf("disable core dumps failed")
		}
		if os.Geteuid() != 0 {
			binary, err := os.Executable()
			if err != nil {
				return fmt.Errorf("runtime binary unavailable")
			}
			arguments := []string{"-n", "--", binary, "operations", "runtime-key", args[0], "--node", target.Node, "--role", target.Role, "--host", target.Host, "--key-file", target.KeyFile}
			if target.KeyID != "" {
				arguments = append(arguments, "--key-id", target.KeyID)
			}
			if _, err := process.Run(cmd.Context(), process.Options{Name: "/usr/bin/sudo", Args: arguments, Stdin: cmd.InOrStdin(), Stdout: cmd.OutOrStdout()}); err != nil {
				return fmt.Errorf("runtime key privilege escalation failed; details withheld")
			}
			return nil
		}
		if err := enrollment.RuntimeOperation(args[0], target, cmd.InOrStdin(), "/run", 0); err != nil {
			return fmt.Errorf("runtime key operation failed; details withheld")
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"result": "ok"})
	}}
	enrollmentFlags(command, &target)
	return command
}

func enrollmentFlags(command *cobra.Command, target *enrollment.Target) {
	command.Flags().StringVar(&target.Node, "node", "", "Concrete fleet node")
	command.Flags().StringVar(&target.Role, "role", "", "control or worker")
	command.Flags().StringVar(&target.Host, "host", "", "Verified SSH host alias")
	command.Flags().StringVar(&target.KeyFile, "key-file", enrollment.KeyFile, "Private runtime key path")
	command.Flags().StringVar(&target.KeyID, "key-id", "", "Key identity for revocation")
	_ = command.MarkFlagRequired("node")
	_ = command.MarkFlagRequired("role")
}
