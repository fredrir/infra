package packages

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

func TestSmokeBatchInstallsEveryPackageOnceAndExecutesEveryBinary(t *testing.T) {
	for _, format := range []string{"deb", "rpm", "apk"} {
		t.Run(format, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("PATH", root)
			for _, directory := range []string{"etc/apt/sources.list.d", "etc/yum.repos.d", "etc/apk/keys", "repo/keys"} {
				if err := os.MkdirAll(filepath.Join(root, directory), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "repo/keys/fredrir.rsa.pub"), []byte("fixture key"), 0644); err != nil {
				t.Fatal(err)
			}
			var commands []string
			var install []string
			failure := errors.New("broken binary")
			var binaryFailure error
			runner := ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				commands = append(commands, options.Name+" "+strings.Join(options.Args, " "))
				if options.Args[0] == "install" || options.Args[0] == "add" {
					install = append(install, options.Args[len(options.Args)-2:]...)
				}
				if options.Name == "second-bin" {
					return process.Result{}, binaryFailure
				}
				return process.Result{}, nil
			}}
			identities := []string{"first", "first-bin", "second", "second-bin"}
			if err := smokeInstallBatch(context.Background(), runner, format, identities, root); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(install, []string{"first", "second"}) {
				t.Fatalf("package installation was not one transaction: %v", commands)
			}
			if !reflect.DeepEqual(commands[len(commands)-2:], []string{"first-bin --version", "second-bin --version"}) {
				t.Fatalf("missing binary verification: %v", commands)
			}
			if format == "deb" && strings.Count(strings.Join(commands, "\n"), "apt-get update") != 1 {
				t.Fatalf("repository refreshed repeatedly: %v", commands)
			}
			if format == "rpm" && strings.Count(strings.Join(commands, "\n"), "rpm --import") != 1 {
				t.Fatalf("signing key imported repeatedly: %v", commands)
			}
			binaryFailure = failure
			if err := smokeInstallBatch(context.Background(), runner, format, identities, root); !errors.Is(err, failure) {
				t.Fatalf("binary verification failure was lost: %v", err)
			}
		})
	}
}

func TestSmokeBatchRejectsMalformedIdentitiesBeforeInstalling(t *testing.T) {
	runner := ci.Runner{Execute: func(context.Context, process.Options) (process.Result, error) {
		t.Fatal("invalid identities reached package manager")
		return process.Result{}, nil
	}}
	for _, identities := range [][]string{nil, {"one"}, {"one", "../binary"}, {"one", "one", "one", "two"}} {
		if err := SmokeInstallBatch(context.Background(), runner, "deb", identities); err == nil {
			t.Fatalf("accepted malformed identities: %v", identities)
		}
	}
}

func TestSmokeDistributionsRunsTwoChecksAndCancelsAfterFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	finished := make(chan error, 1)
	failure := errors.New("invalid package signature")
	var active, peak atomic.Int32
	go func() {
		finished <- smokeDistributions(ctx, []SmokeTarget{{Image: "one"}, {Image: "two"}, {Image: "three"}}, func(ctx context.Context, _ SmokeTarget) error {
			current := active.Add(1)
			defer active.Add(-1)
			for previous := peak.Load(); current > previous && !peak.CompareAndSwap(previous, current); previous = peak.Load() {
			}
			started <- struct{}{}
			select {
			case <-release:
				return failure
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("two distribution checks did not start concurrently")
		}
	}
	select {
	case <-started:
		t.Fatal("more than two distributions ran simultaneously")
	default:
	}
	close(release)
	if err := <-finished; !errors.Is(err, failure) {
		t.Fatalf("package signature failure was lost: %v", err)
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrency = %d, want 2", peak.Load())
	}
}

func TestFullSmokeKeepsPackageDependenciesIsolated(t *testing.T) {
	names := []string{"one", "two"}
	tools := Tools{"one": {Binary: "one-bin"}, "two": {Binary: "two-bin"}}
	quick := smokePackageBatches(names, tools, "quick")
	if !reflect.DeepEqual(quick, [][]string{{"one", "one-bin", "two", "two-bin"}}) {
		t.Fatalf("quick smoke did not batch packages: %v", quick)
	}
	full := smokePackageBatches(names, tools, "full")
	if !reflect.DeepEqual(full, [][]string{{"one", "one-bin"}, {"two", "two-bin"}}) {
		t.Fatalf("full smoke lost independent package installation: %v", full)
	}
}
