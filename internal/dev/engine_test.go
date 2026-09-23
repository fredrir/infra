package dev

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

const fixtureEngineImage = "registry.dagger.io/engine:v0.21.9@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func toolchainRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "build/toolchain.json"), `{"bazel":"9.2.0","go":"1.27.1","dagger":"0.21.9","image":"golang:1.27.1@sha256:`+strings.Repeat("a", 64)+`","bazel_sha256":"`+strings.Repeat("a", 64)+`","engine_image":"`+fixtureEngineImage+`"}`)
	writeFile(t, filepath.Join(root, ".bazelversion"), "9.2.0\n")
	return root
}

type fakeDocker struct {
	image    string
	running  bool
	volume   bool
	commands [][]string
}

func (d *fakeDocker) runner(t *testing.T, root string) ci.Runner {
	t.Helper()
	return ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		if options.Dir != root {
			t.Errorf("%s ran outside the repository: %s", options.Name, options.Dir)
		}
		call := append([]string{options.Name}, options.Args...)
		d.commands = append(d.commands, call)
		if options.Name != "docker" {
			return process.Result{}, nil
		}
		switch options.Args[0] {
		case "inspect":
			if d.image == "" {
				fmt.Fprintln(options.Stderr, "Error: No such object: infra-dagger-dev")
				return process.Result{ExitCode: 1}, errors.New("docker failed: exit status 1")
			}
			return process.Result{Stdout: []byte(fmt.Sprintf("%t %s\n", d.running, d.image))}, nil
		case "run":
			d.image, d.running, d.volume = options.Args[len(options.Args)-3], true, true
		case "stop":
			d.running = false
		case "rm":
			d.image = ""
		case "volume":
			if !d.volume {
				fmt.Fprintln(options.Stderr, "Error response from daemon: get infra-dagger-dev-cache: no such volume")
				return process.Result{ExitCode: 1}, errors.New("docker failed: exit status 1")
			}
			d.volume = false
		}
		return process.Result{}, nil
	}}
}

func (d *fakeDocker) count(prefix ...string) int {
	total := 0
	for _, command := range d.commands {
		if len(command) >= len(prefix) && slices.Equal(command[:len(prefix)], prefix) {
			total++
		}
	}
	return total
}

func TestStartEngineRunsPinnedImageWithRoleLimits(t *testing.T) {
	root := toolchainRoot(t)
	docker := &fakeDocker{}
	opts := EngineOptions{State: NewState(root), Runner: docker.runner(t, root)}
	status, err := StartEngine(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Running || status.Image != fixtureEngineImage || status.RunnerHost != "docker-container://infra-dagger-dev" {
		t.Fatalf("unexpected status %+v", status)
	}
	policy, err := filepath.Abs(filepath.Join(root, ".cache/dev/engine/infra-dagger-dev.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var run []string
	for _, command := range docker.commands {
		if command[0] == "docker" && command[1] == "run" {
			run = command
		}
	}
	expected := []string{"docker", "run", "--detach", "--rm", "--name", "infra-dagger-dev", "--privileged", "--cpus=4", "--memory=8g", "--memory-swap=8g", "--pids-limit=1024", "--volume", "infra-dagger-dev-cache:/var/lib/dagger", "--volume", policy + ":/etc/buildkit/buildkitd.toml:ro", fixtureEngineImage, "--oci-max-parallelism=4", "--oci-worker-gc"}
	if !slices.Equal(run, expected) {
		t.Fatalf("unexpected docker run:\n%v\n%v", run, expected)
	}
	data, err := os.ReadFile(policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`maxUsedSpace = "16GiB"`, `minFreeSpace = "8GiB"`, `keepDuration = "48h"`, `maxUsedSpace = "20GiB"`, `minFreeSpace = "4GiB"`} {
		if !strings.Contains(string(data), fragment) {
			t.Errorf("GC policy lacks %s", fragment)
		}
	}
	if _, err := StartEngine(context.Background(), opts); err != nil || docker.count("docker", "run") != 1 {
		t.Fatalf("running engine was restarted: %v, runs=%d", err, docker.count("docker", "run"))
	}
	docker.image = "registry.dagger.io/engine:v0.0.1@sha256:" + strings.Repeat("c", 64)
	if _, err := StartEngine(context.Background(), opts); err != nil || docker.count("docker", "rm") != 1 || docker.count("docker", "run") != 2 || docker.image != fixtureEngineImage {
		t.Fatalf("stale engine not replaced: %v, %v", err, docker.commands)
	}
}

func TestKataEngineProfileMatchesKataLimits(t *testing.T) {
	root := toolchainRoot(t)
	docker := &fakeDocker{}
	opts := EngineOptions{State: NewState(root), Runner: docker.runner(t, root), Profile: KataEngine()}
	status, err := StartEngine(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if status.Name != "infra-dagger-dev-kata" || status.RunnerHost != "docker-container://infra-dagger-dev-kata" {
		t.Fatalf("unexpected status %+v", status)
	}
	var run []string
	for _, command := range docker.commands {
		if command[0] == "docker" && command[1] == "run" {
			run = command
		}
	}
	for _, argument := range []string{"--cpus=2", "--memory=4g", "--memory-swap=4g", "--pids-limit=256", "infra-dagger-dev-kata-cache:/var/lib/dagger", "--oci-max-parallelism=2"} {
		if !slices.Contains(run, argument) {
			t.Errorf("kata engine lacks %s: %v", argument, run)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".cache/dev/engine/infra-dagger-dev-kata.toml")); err != nil {
		t.Fatal(err)
	}
}

func TestStopEngineRemovesContainerAndOptionallyVolume(t *testing.T) {
	root := toolchainRoot(t)
	docker := &fakeDocker{image: fixtureEngineImage, running: true, volume: true}
	opts := EngineOptions{State: NewState(root), Runner: docker.runner(t, root)}
	if err := StopEngine(context.Background(), opts, false); err != nil {
		t.Fatal(err)
	}
	if docker.image != "" || !docker.volume || docker.count("docker", "stop", "--time", "30", "infra-dagger-dev") != 1 {
		t.Fatalf("container not stopped gracefully: %v", docker.commands)
	}
	if err := StopEngine(context.Background(), opts, true); err != nil || docker.volume {
		t.Fatalf("volume retained: %v", err)
	}
	if err := StopEngine(context.Background(), opts, true); err != nil {
		t.Fatalf("absent volume failed the stop: %v", err)
	}
	if docker.count("docker", "stop") != 1 {
		t.Fatalf("absent container was stopped again: %v", docker.commands)
	}
	status, err := InspectEngine(context.Background(), opts)
	if err != nil || status.Running || status.Image != "" || status.RunnerHost != "" {
		t.Fatalf("absent engine reported as %+v, %v", status, err)
	}
}

func TestCleanAllStopsEngineAndRemovesVolume(t *testing.T) {
	root := toolchainRoot(t)
	state := NewState(root)
	if err := os.MkdirAll(state.Tools(), 0o755); err != nil {
		t.Fatal(err)
	}
	docker := &fakeDocker{image: fixtureEngineImage, running: true, volume: true}
	if err := Clean(context.Background(), CleanOptions{State: state, Runner: docker.runner(t, root), All: true}); err != nil {
		t.Fatal(err)
	}
	if docker.image != "" || docker.volume {
		t.Fatalf("engine state retained: %+v", docker)
	}
	if _, err := os.Stat(state.Cache); !os.IsNotExist(err) {
		t.Fatal("state directory retained")
	}
}
