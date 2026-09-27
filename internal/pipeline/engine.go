package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/process"
)

type engineContainer struct {
	HostConfig struct {
		NanoCpus int64
		Memory   int64
	}
	Args []string
}

func inspectEngine(ctx context.Context) (engineContainer, bool, error) {
	host := os.Getenv("_EXPERIMENTAL_DAGGER_RUNNER_HOST")
	if strings.HasPrefix(host, "unix://") {
		return declaredEngine()
	}
	name, ok := strings.CutPrefix(host, "docker-container://")
	if !ok {
		return engineContainer{}, false, nil
	}
	result, err := process.Run(ctx, process.Options{Name: "docker", Args: []string{"container", "inspect", "--format", "{{json .}}", name}, Timeout: 30 * time.Second})
	if err != nil {
		return engineContainer{}, false, fmt.Errorf("inspect Dagger engine %s: %w", name, err)
	}
	var engine engineContainer
	if err := json.Unmarshal(result.Stdout, &engine); err != nil {
		return engineContainer{}, false, fmt.Errorf("inspect Dagger engine %s: %w", name, err)
	}
	return engine, true, nil
}

func declaredEngine() (engineContainer, bool, error) {
	var engine engineContainer
	values := map[string]int64{}
	for _, name := range []string{"INFRA_ENGINE_CPUS", "INFRA_ENGINE_MEMORY_BYTES", "INFRA_ENGINE_PARALLELISM"} {
		value, err := strconv.ParseInt(os.Getenv(name), 10, 64)
		if err != nil || value <= 0 {
			return engine, false, fmt.Errorf("%s must declare the engine limit as a positive integer", name)
		}
		values[name] = value
	}
	engine.HostConfig.NanoCpus = values["INFRA_ENGINE_CPUS"] * 1e9
	engine.HostConfig.Memory = values["INFRA_ENGINE_MEMORY_BYTES"]
	engine.Args = []string{fmt.Sprintf("--oci-max-parallelism=%d", values["INFRA_ENGINE_PARALLELISM"])}
	return engine, true, nil
}

func goBuildArgs(engine engineContainer) map[string]string {
	cpus := float64(engine.HostConfig.NanoCpus) / 1e9
	if cpus <= 0 || engine.HostConfig.Memory <= 0 {
		return nil
	}
	parallelism := 1
	for _, arg := range engine.Args {
		if value, ok := strings.CutPrefix(arg, "--oci-max-parallelism="); ok {
			if count, err := strconv.Atoi(value); err == nil && count > 0 {
				parallelism = count
			}
		}
	}
	processes := int(math.Ceil(cpus / float64(parallelism)))
	memory := engine.HostConfig.Memory / int64(parallelism*processes) >> 20
	return map[string]string{
		"GO_BUILD_PARALLELISM":  strconv.Itoa(processes),
		"GO_BUILD_MEMORY_LIMIT": fmt.Sprintf("%dMiB", memory),
	}
}
