package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/dev"
	"github.com/spf13/cobra"
)

func newDevCommand() *cobra.Command {
	var root string
	cmd := &cobra.Command{Use: "dev", Short: "Develop, simulate and benchmark locally", RunE: missingCommand}
	cmd.PersistentFlags().StringVar(&root, "root", ".", "Repository directory")
	var bazel string
	doctor := &cobra.Command{Use: "doctor", Short: "Check pinned tools, Docker, KVM, kubeconfig and the Ansible environment", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := dev.Doctor(cmd.Context(), dev.DoctorOptions{State: dev.NewState(root), Runner: ci.Runner{Stderr: io.Discard}, Bazel: bazel})
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(checks); err != nil {
				return err
			}
			for _, check := range checks {
				if !check.OK {
					return errors.New("development environment diagnostics failed")
				}
			}
			return nil
		}}
	doctor.Flags().StringVar(&bazel, "bazel", "bazel", "Local Bazel executable")
	var timeout time.Duration
	setup := &cobra.Command{Use: "setup", Short: "Install pinned tools into .cache/dev/tools and sync the Ansible environment", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if timeout <= 0 {
				return errors.New("timeout must be positive")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			runner := ci.Runner{Stdout: cmd.ErrOrStderr(), Stderr: cmd.ErrOrStderr()}
			return dev.Setup(ctx, dev.SetupOptions{State: dev.NewState(root), Runner: runner, Client: &http.Client{Timeout: 2 * time.Minute}, Log: cmd.ErrOrStderr()})
		}}
	setup.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "Installation timeout")
	var all bool
	clean := &cobra.Command{Use: "clean", Short: "Remove local development state", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return dev.Clean(cmd.Context(), dev.CleanOptions{State: dev.NewState(root), Runner: ci.Runner{Stderr: io.Discard}, All: all, Log: cmd.ErrOrStderr()})
		}}
	clean.Flags().BoolVar(&all, "all", false, "Also stop the engine and remove its cache volume")
	var project, out string
	render := &cobra.Command{Use: "render", Short: "Render the platform tree offline with the controller's Flux build and settings substitution", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report, err := dev.Render(cmd.Context(), dev.RenderOptions{State: dev.NewState(root), Runner: ci.Runner{Stderr: cmd.ErrOrStderr()}, Project: project, Output: out, Stdout: cmd.OutOrStdout()})
			if err != nil {
				return err
			}
			if out == "-" {
				return json.NewEncoder(cmd.ErrOrStderr()).Encode(report)
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(report)
		}}
	render.Flags().StringVar(&project, "project", "", "Render one project under platform/projects")
	render.Flags().StringVar(&out, "out", "", "Output file; - streams YAML to standard output")
	diff := &cobra.Command{Use: "diff", Short: "Server-side dry-run of the local platform tree against the KUBECONFIG cluster", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return dev.Diff(cmd.Context(), dev.DiffOptions{State: dev.NewState(root), Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()})
		}}
	engine := &cobra.Command{Use: "engine", Short: "Bounded local Dagger engines: build (engine role limits) and kata (Kata worker limits)", RunE: missingCommand}
	var profile string
	engine.PersistentFlags().StringVar(&profile, "profile", "build", "Engine profile: build or kata")
	engineOptions := func(cmd *cobra.Command) (dev.EngineOptions, error) {
		selected, ok := dev.EngineProfiles()[profile]
		if !ok {
			return dev.EngineOptions{}, fmt.Errorf("unknown engine profile %q", profile)
		}
		return dev.EngineOptions{State: dev.NewState(root), Runner: ci.Runner{Stderr: io.Discard}, Profile: selected, Log: cmd.ErrOrStderr()}, nil
	}
	engineStart := &cobra.Command{Use: "start", Short: "Start the pinned engine image", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			options, err := engineOptions(cmd)
			if err != nil {
				return err
			}
			status, err := dev.StartEngine(cmd.Context(), options)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "export _EXPERIMENTAL_DAGGER_RUNNER_HOST="+status.RunnerHost)
			return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
		}}
	var volumes bool
	engineStop := &cobra.Command{Use: "stop", Short: "Stop the engine", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			options, err := engineOptions(cmd)
			if err != nil {
				return err
			}
			return dev.StopEngine(cmd.Context(), options, volumes)
		}}
	engineStop.Flags().BoolVar(&volumes, "volumes", false, "Also remove the engine cache volume")
	engineStatus := &cobra.Command{Use: "status", Short: "Print the engine state", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			options, err := engineOptions(cmd)
			if err != nil {
				return err
			}
			status, err := dev.InspectEngine(cmd.Context(), options)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
		}}
	engine.AddCommand(engineStart, engineStop, engineStatus)
	qualify := &cobra.Command{Use: "qualify SUITE [-- go test flags]", Short: "Run a gated qualification suite: " + strings.Join(dev.SuiteNames(), ", "), Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runner := ci.Runner{Stdout: cmd.ErrOrStderr(), Stderr: cmd.ErrOrStderr()}
			return dev.Qualify(cmd.Context(), dev.QualifyOptions{State: dev.NewState(root), Runner: runner, Suite: args[0], Args: args[1:], Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr(), Log: cmd.ErrOrStderr()})
		}}
	cluster := &cobra.Command{Use: "cluster", Short: "Disposable K3s cluster reconciling the local platform tree with Flux", RunE: missingCommand}
	var clusterProfile string
	var clusterTimeout time.Duration
	cluster.PersistentFlags().StringVar(&clusterProfile, "profile", "minimal", "Kustomization profile: minimal or platform")
	cluster.PersistentFlags().DurationVar(&clusterTimeout, "timeout", 5*time.Minute, "Readiness timeout per Kustomization")
	clusterOptions := func(cmd *cobra.Command) dev.ClusterOptions {
		return dev.ClusterOptions{State: dev.NewState(root), Runner: ci.Runner{Stderr: cmd.ErrOrStderr()}, Profile: clusterProfile, Timeout: clusterTimeout, Log: cmd.ErrOrStderr()}
	}
	report := func(cmd *cobra.Command, status dev.ClusterStatus, err error) error {
		return errors.Join(err, json.NewEncoder(cmd.OutOrStdout()).Encode(status))
	}
	clusterUp := &cobra.Command{Use: "up", Short: "Create or start the cluster, then sync", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			status, err := dev.ClusterUp(cmd.Context(), clusterOptions(cmd))
			return report(cmd, status, err)
		}}
	clusterSync := &cobra.Command{Use: "sync", Short: "Push the working tree as an OCI artifact and wait for the Kustomizations", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			status, err := dev.ClusterSync(cmd.Context(), clusterOptions(cmd))
			return report(cmd, status, err)
		}}
	clusterStatus := &cobra.Command{Use: "status", Short: "Print cluster and Kustomization state", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			status, err := dev.InspectCluster(cmd.Context(), clusterOptions(cmd))
			return report(cmd, status, err)
		}}
	clusterDown := &cobra.Command{Use: "down", Short: "Delete the cluster", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return dev.ClusterDown(cmd.Context(), clusterOptions(cmd))
		}}
	cluster.AddCommand(clusterUp, clusterSync, clusterStatus, clusterDown)
	hosts := &cobra.Command{Use: "hosts", Short: "Ubuntu guests under QEMU/KVM for Ansible playbooks", RunE: missingCommand}
	var hostsTimeout time.Duration
	hosts.PersistentFlags().DurationVar(&hostsTimeout, "timeout", 10*time.Minute, "Boot timeout until SSH answers")
	hostsOptions := func(cmd *cobra.Command) dev.HostsOptions {
		return dev.HostsOptions{State: dev.NewState(root), Runner: ci.Runner{Stderr: cmd.ErrOrStderr()}, Timeout: hostsTimeout, Log: cmd.ErrOrStderr()}
	}
	hostsUp := &cobra.Command{Use: "up", Short: "Download the pinned image, boot the guests and write the dev inventory", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			status, err := dev.HostsUp(cmd.Context(), hostsOptions(cmd))
			return errors.Join(err, json.NewEncoder(cmd.OutOrStdout()).Encode(status))
		}}
	hostsStatus := &cobra.Command{Use: "status", Short: "Print guest state", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			status, err := dev.InspectHosts(cmd.Context(), hostsOptions(cmd))
			return errors.Join(err, json.NewEncoder(cmd.OutOrStdout()).Encode(status))
		}}
	var purge bool
	hostsDown := &cobra.Command{Use: "down", Short: "Stop the guests", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return dev.HostsDown(cmd.Context(), hostsOptions(cmd), purge)
		}}
	hostsDown.Flags().BoolVar(&purge, "purge", false, "Also remove guest disks, the inventory and known hosts")
	play := func(check bool) func(cmd *cobra.Command, args []string) error {
		return func(cmd *cobra.Command, args []string) error {
			return dev.HostsPlay(cmd.Context(), dev.PlayOptions{Hosts: hostsOptions(cmd), Playbook: args[0], Check: check, Args: args[1:], Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()})
		}
	}
	hostsPlay := &cobra.Command{Use: "play PLAYBOOK [-- ansible-playbook flags]", Short: "Run a playbook against the guests", Args: cobra.MinimumNArgs(1), RunE: play(false)}
	hostsCheck := &cobra.Command{Use: "check PLAYBOOK [-- ansible-playbook flags]", Short: "Run a playbook in check and diff mode", Args: cobra.MinimumNArgs(1), RunE: play(true)}
	hostsSSH := &cobra.Command{Use: "ssh NODE [-- command]", Short: "Open a shell on a guest", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return dev.HostsSSH(cmd.Context(), dev.SSHOptions{Hosts: hostsOptions(cmd), Node: args[0], Args: args[1:], Stdin: os.Stdin, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()})
		}}
	hosts.AddCommand(hostsUp, hostsStatus, hostsDown, hostsPlay, hostsCheck, hostsSSH)
	bench := &cobra.Command{Use: "bench", Short: "Benchmark local commands with hyperfine and Go benchmarks with benchstat", RunE: missingCommand}
	var baseline string
	var threshold float64
	benchRun := &cobra.Command{Use: "run [SCENARIO...]", Short: "Sample dev/bench/scenarios.yaml; JSON summary under .cache/dev/bench", Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			runner := ci.Runner{Stdout: cmd.ErrOrStderr(), Stderr: cmd.ErrOrStderr()}
			summary, err := dev.BenchRun(cmd.Context(), dev.BenchOptions{State: dev.NewState(root), Runner: runner, Names: args, Threshold: threshold, Log: cmd.ErrOrStderr()})
			if baseline == "" {
				return errors.Join(err, json.NewEncoder(cmd.OutOrStdout()).Encode(summary))
			}
			base, readErr := dev.ReadBenchSummary(baseline)
			if readErr != nil {
				return errors.Join(err, readErr)
			}
			deltas, regressed := dev.BenchCompare(base, summary, threshold)
			if regressed {
				err = errors.Join(err, errors.New("benchmark regression against baseline"))
			}
			return errors.Join(err, json.NewEncoder(cmd.OutOrStdout()).Encode(deltas))
		}}
	benchRun.Flags().StringVar(&baseline, "baseline", "", "Summary JSON to compare against")
	benchRun.Flags().Float64Var(&threshold, "threshold", 0.10, "Median regression fraction that fails the comparison")
	var compareThreshold float64
	benchCompare := &cobra.Command{Use: "compare BASE CANDIDATE", Short: "Compare two summary files", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			base, err := dev.ReadBenchSummary(args[0])
			if err != nil {
				return err
			}
			candidate, err := dev.ReadBenchSummary(args[1])
			if err != nil {
				return err
			}
			deltas, regressed := dev.BenchCompare(base, candidate, compareThreshold)
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(deltas); err != nil {
				return err
			}
			if regressed {
				return errors.New("benchmark regression against baseline")
			}
			return nil
		}}
	benchCompare.Flags().Float64Var(&compareThreshold, "threshold", 0.10, "Median regression fraction that fails the comparison")
	var count int
	benchGo := &cobra.Command{Use: "go [PACKAGE...]", Short: "Run Go benchmarks; benchstat against .cache/dev/bench/go-baseline.txt when present", Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			output, err := dev.BenchGo(cmd.Context(), dev.BenchGoOptions{State: dev.NewState(root), Packages: args, Count: count, Stdout: cmd.OutOrStdout(), Log: cmd.ErrOrStderr()})
			fmt.Fprintln(cmd.ErrOrStderr(), "Recorded:", output)
			return err
		}}
	benchGo.Flags().IntVar(&count, "count", 6, "Benchmark repetitions")
	bench.AddCommand(benchRun, benchCompare, benchGo)
	cmd.AddCommand(doctor, setup, clean, render, diff, engine, qualify, cluster, hosts, bench)
	return cmd
}
