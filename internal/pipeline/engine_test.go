package pipeline

import (
	"maps"
	"testing"
)

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
		{"build VM", engine(8, 12<<30, "--oci-max-parallelism=3", "--oci-worker-gc"), map[string]string{"GO_BUILD_PARALLELISM": "3", "GO_BUILD_MEMORY_LIMIT": "1365MiB"}},
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
