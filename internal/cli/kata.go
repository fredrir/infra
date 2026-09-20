package cli

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/fredrir/infra/internal/kata"
	"github.com/spf13/cobra"
)

func newKataCommand() *cobra.Command {
	root := &cobra.Command{Use: "kata", Short: "Build and qualify Kata runtime artifacts"}
	var o kata.BuildOptions
	var engine string
	o.Limits = kata.DefaultLimits()
	build := &cobra.Command{Use: "build qemu|kernel|guest|virtiofsd|package", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		o.Component = args[0]
		o.Log = c.ErrOrStderr()
		return kata.Build(c.Context(), o, kata.DaggerExecutor{Log: c.ErrOrStderr(), EngineContainer: engine})
	}}
	build.Flags().StringVar(&o.RepoDir, "root", ".", "Repository directory")
	build.Flags().StringVar(&o.WorkDir, "work-dir", "", "Build output directory")
	build.Flags().StringVar(&o.Binary, "binary", "", "Prebuilt static Linux amd64 infra binary")
	build.Flags().StringVar(&engine, "engine-container", "", "Locally verifiable bounded Docker engine container")
	build.Flags().BoolVar(&o.AllowPrivilegedGuest, "allow-privileged-guest", false, "Allow guest build on an isolated engine VM")
	build.Flags().DurationVar(&o.Limits.Timeout, "timeout", 2*time.Hour, "Build deadline")
	_ = build.MarkFlagRequired("work-dir")
	root.AddCommand(build)
	var limits = kata.DefaultLimits()
	var engineBounded bool
	worker := &cobra.Command{Use: "worker -- command [args...]", Hidden: true, Args: cobra.MinimumNArgs(1), RunE: func(c *cobra.Command, args []string) error {
		return kata.Worker(c.Context(), args, limits, engineBounded, c.OutOrStdout())
	}}
	worker.Flags().IntVar(&limits.CPUs, "cpus", 2, "CPU limit")
	worker.Flags().BoolVar(&engineBounded, "engine-bounded", false, "Engine ancestor resource limits verified by client")
	worker.Flags().Int64Var(&limits.MemoryBytes, "memory", 4<<30, "Memory limit")
	worker.Flags().IntVar(&limits.PIDs, "pids", 256, "PID limit")
	worker.Flags().DurationVar(&limits.Timeout, "timeout", 2*time.Hour, "Execution deadline")
	root.AddCommand(worker)
	root.AddCommand(&cobra.Command{Use: "doctor", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		r, e := kata.Doctor(kata.DefaultLimits())
		return errors.Join(json.NewEncoder(c.OutOrStdout()).Encode(r), e)
	}})
	for _, name := range []string{"prepare-builder", "guest-worker", "kernel-worker", "virtiofsd-worker", "qemu-worker"} {
		name := name
		root.AddCommand(&cobra.Command{Use: name, Hidden: true, Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
			switch name {
			case "prepare-builder":
				return kata.PrepareBuilder(c.Context(), c.OutOrStdout())
			case "guest-worker":
				return kata.Guest(c.Context(), c.OutOrStdout())
			case "kernel-worker":
				return kata.Kernel(c.Context(), c.OutOrStdout())
			case "qemu-worker":
				return kata.Qemu(c.Context(), c.OutOrStdout())
			default:
				return kata.Virtiofsd(c.Context(), c.OutOrStdout())
			}
		}})
	}
	var q kata.QualifyOptions
	qualify := &cobra.Command{Use: "qualify", Short: "Qualify an installed candidate on a disposable KVM host", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		q.Log = c.ErrOrStderr()
		r, e := kata.Qualify(c.Context(), q)
		return errors.Join(json.NewEncoder(c.OutOrStdout()).Encode(r), e)
	}}
	qualify.Flags().StringVar(&q.Archive, "archive", "", "Candidate runtime archive")
	qualify.Flags().StringVar(&q.Manifest, "manifest", "", "Candidate checksum manifest")
	qualify.Flags().StringVar(&q.Image, "image", "", "Digest-pinned qualification image")
	qualify.Flags().StringVar(&q.Output, "output", "", "Qualification evidence JSON")
	qualify.Flags().StringVar(&q.Address, "containerd-address", "/run/containerd/containerd.sock", "Disposable host containerd socket")
	qualify.Flags().DurationVar(&q.Timeout, "timeout", 10*time.Minute, "Qualification deadline")
	qualify.Flags().BoolVar(&q.DisposableHost, "disposable-host", false, "Confirm isolated disposable native qualification host")
	for _, name := range []string{"archive", "manifest", "image", "output"} {
		_ = qualify.MarkFlagRequired(name)
	}
	root.AddCommand(qualify)
	return root
}
