package kata

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDaggerExecutesBoundedWorker(t *testing.T) {
	if os.Getenv("INFRA_KATA_ENGINE_TEST") != "1" {
		t.Skip("requires isolated bounded Dagger engine")
	}
	binary := os.Getenv("INFRA_KATA_BINARY")
	if e := validateBinary(binary); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out := t.TempDir()
	limits := DefaultLimits()
	limits.Timeout = 2 * time.Minute
	r := ContainerRequest{Image: "docker.io/library/ubuntu:24.04@sha256:224a1869083a311ef3f13648a154ba79832fbef6364d31493642ca03082da254", Mounts: []Mount{{binary, "/infra"}}, Args: []string{"/bin/cp", "/infra", "/output/infra"}, OutputPath: "/output", OutputDir: out, Limits: limits}
	if e := (DaggerExecutor{Log: os.Stderr}).Execute(ctx, r); e != nil {
		t.Fatal(e)
	}
	want, e := digestFile(binary)
	if e != nil {
		t.Fatal(e)
	}
	if e = verify(filepath.Join(out, "infra"), want); e != nil {
		t.Fatal(e)
	}
}
