package pipeline

import (
	"context"
	"maps"
	"testing"
)

func TestEngineInspectionOnlyTargetsDockerHostedEngines(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("_EXPERIMENTAL_DAGGER_RUNNER_HOST", "")
	if _, found, err := inspectEngine(context.Background()); found || err != nil {
		t.Fatalf("engine without a Docker host inspected: found=%t err=%v", found, err)
	}
	t.Setenv("_EXPERIMENTAL_DAGGER_RUNNER_HOST", "docker-container://infra-dagger")
	if _, found, err := inspectEngine(context.Background()); found || err == nil {
		t.Fatalf("unreadable Docker engine reported limits: found=%t err=%v", found, err)
	}
}

func TestGoBuildArgsShareTheEngineAcrossParallelBuilds(t *testing.T) {
	engine := func(cpus float64, memory int64, args ...string) engineContainer {
		var shape engineContainer
		shape.HostConfig.NanoCpus, shape.HostConfig.Memory, shape.Args = int64(cpus*1e9), memory, args
		return shape
	}
	for _, test := range []struct {
		name   string
		engine engineContainer
		want   map[string]string
	}{
		{"build VM", engine(8, 10<<30, "--oci-max-parallelism=3", "--oci-worker-gc"), map[string]string{"GO_BUILD_PARALLELISM": "3", "GO_BUILD_MEMORY_LIMIT": "1137MiB"}},
		{"hosted runner", engine(4, 12000<<20), map[string]string{"GO_BUILD_PARALLELISM": "4", "GO_BUILD_MEMORY_LIMIT": "3000MiB"}},
		{"fractional CPUs", engine(2.5, 3<<30, "--oci-max-parallelism=1"), map[string]string{"GO_BUILD_PARALLELISM": "3", "GO_BUILD_MEMORY_LIMIT": "1024MiB"}},
		{"unbounded engine", engine(0, 0), nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := goBuildArgs(test.engine); !maps.Equal(got, test.want) {
				t.Fatalf("build arguments %v, want %v", got, test.want)
			}
		})
	}
}
