package ci

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/process"
)

func TestRustPreparationPersistsLiteralArgumentsAndDisablesUnavailableCache(t *testing.T) {
	directory := t.TempDir()
	environment := filepath.Join(directory, "environment")
	t.Setenv("GITHUB_ENV", environment)
	t.Setenv("RUSTC_WRAPPER", "sccache")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCCACHE_ENDPOINT", endpoint)
	var warning bytes.Buffer
	var executed []string
	runner := Runner{Stdout: &bytes.Buffer{}, Stderr: &warning, Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		executed = append(executed, options.Name)
		return process.Result{}, nil
	}}
	if err := PrepareRust(context.Background(), runner, directory, "--all-features", "--features vendored-openssl,cli --no-fail-fast"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "rust-args.json"))
	if err != nil {
		t.Fatal(err)
	}
	var flags RustOptions
	if err := json.Unmarshal(data, &flags); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(flags.Clippy, []string{"--all-features"}) || !reflect.DeepEqual(flags.Test, []string{"--features", "vendored-openssl,cli", "--no-fail-fast"}) {
		t.Fatalf("flags: %+v", flags)
	}
	data, err = os.ReadFile(environment)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "RUSTC_WRAPPER=\n") || !strings.Contains(warning.String(), "::warning::") || !reflect.DeepEqual(executed, []string{"rustup"}) {
		t.Fatalf("fallback: %s %s %v", data, warning.String(), executed)
	}
}
func TestRustPreparationRejectsOverridesBeforeWritingOrExecuting(t *testing.T) {
	for _, input := range []string{"--features $(id)", "--all-features; curl x", "--config build.rustc-wrapper=x", "--manifest-path ../other/Cargo.toml", "-Zunstable-options", "`id`"} {
		t.Run(input, func(t *testing.T) {
			directory := t.TempDir()
			runner := Runner{Execute: func(context.Context, process.Options) (process.Result, error) {
				t.Fatal("invalid arguments executed external command")
				return process.Result{}, nil
			}}
			if err := PrepareRust(context.Background(), runner, directory, "", input); err == nil {
				t.Fatal("unsafe arguments accepted")
			}
			files, err := os.ReadDir(directory)
			if err != nil || len(files) != 0 {
				t.Fatal("invalid arguments wrote configuration")
			}
		})
	}
}
