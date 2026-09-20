package kata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"dagger.io/dagger"
)

type DaggerExecutor struct {
	Log             io.Writer
	EngineContainer string
}

func (x DaggerExecutor) Execute(ctx context.Context, r ContainerRequest) error {
	if e := r.Limits.Validate(); e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(ctx, r.Limits.Timeout)
	defer cancel()
	engine := x.EngineContainer
	if engine == "" {
		host := os.Getenv("_EXPERIMENTAL_DAGGER_RUNNER_HOST")
		if !strings.HasPrefix(host, "docker-container://") {
			return errors.New("Kata requires an explicitly selected, locally verifiable Docker engine")
		}
		engine = strings.TrimPrefix(host, "docker-container://")
	}
	if os.Getenv("DAGGER_SESSION_PORT") != "" {
		return errors.New("Kata engine verification requires a dedicated Dagger session")
	}
	id, e := verifyEngine(ctx, engine, r.Limits)
	if e != nil {
		return e
	}
	c, e := dagger.Connect(ctx, dagger.WithLogOutput(x.Log), dagger.WithRunnerHost("docker-container://"+id))
	if e != nil {
		return e
	}
	defer c.Close()
	container := c.Container(dagger.ContainerOpts{Platform: "linux/amd64"})
	container = container.From(r.Image)
	if r.BuildContext != "" {
		container = container.WithFile("/infra", c.Host().File(r.BuildContext+"/infra")).WithFile("/inputs/ca.deb", c.Host().File(r.BuildContext+"/ca.deb")).WithFile("/etc/apt/sources.list.d/ubuntu.sources", c.Host().File(r.BuildContext+"/guest-builder.sources"))
		container = container.WithExec([]string{"/infra", "kata", "worker", "--engine-bounded", "--", "/infra", "kata", "prepare-builder"})
	}
	for _, m := range r.Mounts {
		st, e := os.Stat(m.Source)
		if e != nil {
			return e
		}
		if st.IsDir() {
			container = container.WithDirectory(m.Target, c.Host().Directory(m.Source))
		} else {
			container = container.WithFile(m.Target, c.Host().File(m.Source))
		}
	}
	for _, source := range r.Sources {
		if len(source.Revision) != 40 {
			return errors.New("full Git source revision required")
		}
		tree := c.Git(source.Repository).Commit(source.Revision).Tree()
		container = container.WithDirectory(source.Target, tree)
	}
	container = container.WithDirectory(r.OutputPath, c.Directory())
	keys := make([]string, 0, len(r.Env))
	for k := range r.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		container = container.WithEnvVariable(k, r.Env[k])
	}
	if r.WorkDir != "" {
		container = container.WithWorkdir(r.WorkDir)
	}
	args := []string{"/infra", "kata", "worker", "--engine-bounded", "--cpus", strconv.Itoa(r.Limits.CPUs), "--memory", strconv.FormatInt(r.Limits.MemoryBytes, 10), "--pids", strconv.Itoa(r.Limits.PIDs), "--timeout", r.Limits.Timeout.String(), "--"}
	args = append(args, r.Args...)
	container = container.WithExec(args, dagger.ContainerWithExecOpts{InsecureRootCapabilities: r.Privileged})
	if _, e = container.Directory(r.OutputPath).Export(ctx, r.OutputDir); e != nil {
		return fmt.Errorf("Kata container: %w", e)
	}
	return nil
}

type engineLimits struct {
	NanoCPUs   int64
	CPUQuota   int64
	CPUPeriod  int64
	Memory     int64
	MemorySwap int64
	PidsLimit  int64
}

func verifyEngine(ctx context.Context, name string, l Limits) (string, error) {
	if name == "" || strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-") != "" {
		return "", errors.New("invalid engine container name")
	}
	id, e := output(ctx, "", "docker", "inspect", "--format", "{{.Id}}", name)
	if e != nil {
		return "", e
	}
	value := strings.TrimSpace(string(id))
	if len(value) != 64 {
		return "", errors.New("invalid engine identity")
	}
	b, e := output(ctx, "", "docker", "inspect", "--format", "{{json .HostConfig}}", value)
	if e != nil {
		return "", e
	}
	var limits engineLimits
	if e = json.Unmarshal(b, &limits); e != nil {
		return "", e
	}
	if e = limits.validate(l); e != nil {
		return "", e
	}
	return value, nil
}
func (l engineLimits) validate(want Limits) error {
	cpu := float64(l.NanoCPUs) / 1e9
	if cpu == 0 && l.CPUQuota > 0 && l.CPUPeriod > 0 {
		cpu = float64(l.CPUQuota) / float64(l.CPUPeriod)
	}
	if cpu <= 0 || cpu > float64(want.CPUs) || l.Memory <= 0 || l.Memory > want.MemoryBytes || l.MemorySwap != l.Memory || l.PidsLimit <= 0 || l.PidsLimit > int64(want.PIDs) {
		return errors.New("engine must enforce CPU, memory, no-swap, and PID build limits")
	}
	return nil
}
