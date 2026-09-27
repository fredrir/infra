package dev

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dagger.io/dagger"
	"github.com/fredrir/infra/internal/process"
)

func TestStartedEngineAppliesItsCachePolicy(t *testing.T) {
	source := os.Getenv("INFRA_TEST_SOURCE_ROOT")
	if os.Getenv("INFRA_ENGINE_POLICY_TEST") != "1" || source == "" {
		t.Skip("INFRA_ENGINE_POLICY_TEST=1 and INFRA_TEST_SOURCE_ROOT start a disposable engine from the pinned image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	for _, name := range []string{"build/toolchain.json", ".bazelversion"} {
		data, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(root, name), string(data))
	}
	limits := BuildEngine().Limits
	limits.CPUs, limits.Memory, limits.CacheGiB, limits.EmergencyFreeGiB = 2, "2g", 12, 3
	opts := EngineOptions{State: NewState(root), Runner: process.Runner{Stderr: io.Discard}, Profile: EngineProfile{Name: fmt.Sprintf("infra-dagger-policy-%d", os.Getpid()), Limits: limits}}
	t.Cleanup(func() {
		if err := StopEngine(context.Background(), opts, true); err != nil {
			t.Error(err)
		}
	})
	status, err := StartEngine(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	client, err := dagger.Connect(ctx, dagger.WithRunnerHost(status.RunnerHost), dagger.WithLogOutput(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	cache := client.Engine().LocalCache()
	used, err := cache.MaxUsedSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	free, err := cache.MinFreeSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if used != limits.CacheGiB<<30 || free != limits.EmergencyFreeGiB<<30 {
		t.Fatalf("engine reports maxUsedSpace %d and minFreeSpace %d, want %d and %d from its policy", used, free, limits.CacheGiB<<30, limits.EmergencyFreeGiB<<30)
	}
}
