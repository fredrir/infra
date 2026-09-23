package dev

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/kata"
	"github.com/fredrir/infra/internal/pipeline"
)

const EngineName = "infra-dagger-dev"

type EngineLimits struct {
	CPUs             int
	Memory           string
	Pids             int
	Parallelism      int
	CacheGiB         int
	FreeGiB          int
	EmergencyFreeGiB int
	NamedCacheHours  int
}

type EngineProfile struct {
	Name   string
	Limits EngineLimits
}

func (p EngineProfile) Volume() string     { return p.Name + "-cache" }
func (p EngineProfile) RunnerHost() string { return "docker-container://" + p.Name }

func BuildEngine() EngineProfile {
	return EngineProfile{Name: EngineName, Limits: EngineLimits{CPUs: 4, Memory: "8g", Pids: 1024, Parallelism: 4, CacheGiB: 20, FreeGiB: 8, EmergencyFreeGiB: 4, NamedCacheHours: 48}}
}

func KataEngine() EngineProfile {
	limits := kata.DefaultLimits()
	return EngineProfile{Name: EngineName + "-kata", Limits: EngineLimits{CPUs: limits.CPUs, Memory: fmt.Sprintf("%dg", limits.MemoryBytes>>30), Pids: limits.PIDs, Parallelism: limits.CPUs, CacheGiB: 20, FreeGiB: 8, EmergencyFreeGiB: 4, NamedCacheHours: 48}}
}

func EngineProfiles() map[string]EngineProfile {
	return map[string]EngineProfile{"build": BuildEngine(), "kata": KataEngine()}
}

type EngineOptions struct {
	State   State
	Runner  ci.Runner
	Profile EngineProfile
	Log     io.Writer
}

type EngineStatus struct {
	Name       string `json:"name"`
	Image      string `json:"image,omitempty"`
	Running    bool   `json:"running"`
	RunnerHost string `json:"runner_host,omitempty"`
}

func (opts EngineOptions) defaults() EngineOptions {
	if opts.Runner.Dir == "" {
		opts.Runner.Dir = opts.State.Root
	}
	if opts.Profile.Name == "" {
		opts.Profile = BuildEngine()
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	return opts
}

func InspectEngine(ctx context.Context, opts EngineOptions) (EngineStatus, error) {
	opts = opts.defaults()
	status := EngineStatus{Name: opts.Profile.Name}
	output, err := capture(ctx, opts.Runner, "docker", "inspect", "--format", "{{.State.Running}} {{.Config.Image}}", opts.Profile.Name)
	if err != nil {
		if absent(err) {
			return status, nil
		}
		return status, err
	}
	running, image, _ := strings.Cut(output, " ")
	status.Image, status.Running = image, running == "true"
	if status.Running {
		status.RunnerHost = opts.Profile.RunnerHost()
	}
	return status, nil
}

func StartEngine(ctx context.Context, opts EngineOptions) (EngineStatus, error) {
	opts = opts.defaults()
	config, err := pipeline.ReadToolchain(opts.State.Root)
	if err != nil {
		return EngineStatus{}, err
	}
	status, err := InspectEngine(ctx, opts)
	if err != nil {
		return status, err
	}
	if status.Running && status.Image == config.EngineImage {
		return status, nil
	}
	if status.Image != "" {
		if _, err := capture(ctx, opts.Runner, "docker", "rm", "--force", opts.Profile.Name); err != nil {
			return status, err
		}
	}
	if err := os.MkdirAll(opts.State.Engine(), 0o755); err != nil {
		return status, err
	}
	policy, err := filepath.Abs(filepath.Join(opts.State.Engine(), opts.Profile.Name+".toml"))
	if err != nil {
		return status, err
	}
	if err := os.WriteFile(policy, []byte(gcPolicy(opts.Profile.Limits)), 0o644); err != nil {
		return status, err
	}
	limits := opts.Profile.Limits
	arguments := []string{"run", "--detach", "--rm", "--name", opts.Profile.Name, "--privileged",
		fmt.Sprintf("--cpus=%d", limits.CPUs), "--memory=" + limits.Memory, "--memory-swap=" + limits.Memory, fmt.Sprintf("--pids-limit=%d", limits.Pids),
		"--volume", opts.Profile.Volume() + ":/var/lib/dagger", "--volume", policy + ":/etc/buildkit/buildkitd.toml:ro",
		config.EngineImage, fmt.Sprintf("--oci-max-parallelism=%d", limits.Parallelism), "--oci-worker-gc"}
	if _, err := capture(ctx, opts.Runner, "docker", arguments...); err != nil {
		return status, fmt.Errorf("start %s: %w", opts.Profile.Name, err)
	}
	fmt.Fprintln(opts.Log, "Started:", opts.Profile.Name, config.EngineImage)
	return InspectEngine(ctx, opts)
}

func StopEngine(ctx context.Context, opts EngineOptions, volumes bool) error {
	opts = opts.defaults()
	status, err := InspectEngine(ctx, opts)
	if err != nil {
		return err
	}
	if status.Image != "" {
		if _, err := capture(ctx, opts.Runner, "docker", "stop", "--time", "30", opts.Profile.Name); err != nil {
			return err
		}
		if _, err := capture(ctx, opts.Runner, "docker", "rm", "--force", opts.Profile.Name); err != nil && !absent(err) {
			return err
		}
		fmt.Fprintln(opts.Log, "Stopped:", opts.Profile.Name)
	}
	if volumes {
		if _, err := capture(ctx, opts.Runner, "docker", "volume", "rm", opts.Profile.Volume()); err != nil && !absent(err) {
			return err
		}
		fmt.Fprintln(opts.Log, "Removed:", opts.Profile.Volume())
	}
	return nil
}

func gcPolicy(limits EngineLimits) string {
	used := limits.CacheGiB * 4 / 5
	return fmt.Sprintf(`[[worker.oci.gcpolicy]]
filters = ["type==regular,type==source.local,type==source.git.checkout,type==source.http"]
reservedSpace = "1GiB"
maxUsedSpace = "%dGiB"
minFreeSpace = "%dGiB"

[[worker.oci.gcpolicy]]
filters = ["type==exec.cachemount"]
keepDuration = "%dh"
reservedSpace = "1GiB"
maxUsedSpace = "%dGiB"
minFreeSpace = "%dGiB"

[[worker.oci.gcpolicy]]
all = true
reservedSpace = "1GiB"
maxUsedSpace = "%dGiB"
minFreeSpace = "%dGiB"
`, used, limits.FreeGiB, limits.NamedCacheHours, used, limits.FreeGiB, limits.CacheGiB, limits.EmergencyFreeGiB)
}

func absent(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such")
}
