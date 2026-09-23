package dev

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

func TestSetupInstallsPinnedToolsAndSyncsAnsible(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#!/bin/sh\necho " + strings.TrimPrefix(r.URL.Path, "/") + "\n"))
	}))
	t.Cleanup(server.Close)
	assets := func(name string) (ci.ToolAsset, bool) {
		content := "#!/bin/sh\necho " + name + "\n"
		digest := sha256.Sum256([]byte(content))
		return ci.ToolAsset{URL: server.URL + "/" + name, Digest: hex.EncodeToString(digest[:])}, true
	}
	root := t.TempDir()
	state := NewState(root)
	var calls []process.Options
	runner := ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		calls = append(calls, options)
		return process.Result{}, nil
	}}
	var log strings.Builder
	if err := Setup(context.Background(), SetupOptions{State: state, Runner: runner, Client: server.Client(), Platform: "linux/amd64", Assets: assets, Log: &log}); err != nil {
		t.Fatal(err)
	}
	for _, tool := range Tools {
		path := filepath.Join(state.Tools(), tool.Name)
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s not installed: %v", tool.Name, err)
		}
		if _, err := os.Stat(path + ".json"); err != nil {
			t.Fatalf("%s has no receipt: %v", tool.Name, err)
		}
		if !strings.Contains(log.String(), "Installed: "+tool.Name+" ") {
			t.Errorf("%s missing from the installation log", tool.Name)
		}
	}
	binary, err := filepath.Abs(filepath.Join(state.Bin(), "infra"))
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].Name != "go" || !reflect.DeepEqual(calls[0].Args, []string{"build", "-o", binary, "./cmd/infra"}) {
		t.Fatalf("infra binary not built first: %+v", calls)
	}
	if calls[1].Name != filepath.Join(state.Tools(), "uv") || calls[1].Dir != root || !reflect.DeepEqual(calls[1].Args, []string{"sync", "--frozen", "--group", "ci", "--no-install-project"}) {
		t.Fatalf("unexpected Ansible environment sync: %+v", calls)
	}
	server.Close()
	if err := Setup(context.Background(), SetupOptions{State: state, Runner: runner, Client: server.Client(), Platform: "linux/amd64", Assets: assets, Log: &log}); err != nil {
		t.Fatalf("installed tools were downloaded again: %v", err)
	}
}

func TestSetupBuildsTheBinaryButRefusesToolsOnUnsupportedPlatforms(t *testing.T) {
	state := NewState(t.TempDir())
	var calls []process.Options
	runner := ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		calls = append(calls, options)
		return process.Result{}, nil
	}}
	err := Setup(context.Background(), SetupOptions{State: state, Runner: runner, Platform: "darwin/arm64"})
	if err == nil || !strings.Contains(err.Error(), "darwin/arm64") {
		t.Fatalf("unsupported platform accepted: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "go" || calls[0].Args[0] != "build" {
		t.Fatalf("expected only the binary build, got %+v", calls)
	}
	if _, err := os.Stat(state.Tools()); !os.IsNotExist(err) {
		t.Fatal("tools directory created on unsupported platform")
	}
}

func TestCleanRemovesLocalState(t *testing.T) {
	state := NewState(t.TempDir())
	if err := os.MkdirAll(state.Tools(), 0o755); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	if err := Clean(context.Background(), CleanOptions{State: state, Log: &log}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state.Cache); !os.IsNotExist(err) {
		t.Fatal("state directory retained")
	}
	if !strings.Contains(log.String(), state.Cache) {
		t.Fatal("removed path not reported")
	}
	if err := Clean(context.Background(), CleanOptions{State: state}); err != nil {
		t.Fatal("cleaning an absent directory failed:", err)
	}
}
