package pipeline

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeWorkspace(t *testing.T) (string, string, string) {
	t.Helper()
	root, tools := t.TempDir(), t.TempDir()
	digest := strings.Repeat("a", 64)
	calls, bazel := filepath.Join(tools, "calls"), filepath.Join(tools, "bazel")
	for path, content := range map[string]string{
		filepath.Join(root, ".bazelversion"):           "9.2.0\n",
		filepath.Join(root, "build", "toolchain.json"): `{"bazel":"9.2.0","go":"1.27.1","dagger":"0.21.9","image":"golang@sha256:` + digest + `","bazel_sha256":"` + digest + `","engine_image":"registry.dagger.io/engine:v0.21.9@sha256:` + digest + `"}`,
		bazel: "#!/bin/sh\ncase \"$1\" in\n--version) echo 'bazel 9.2.0' ;;\n*) echo \"$*\" >> '" + calls + "' ;;\nesac\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, bazel, calls
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func grpcServer(t *testing.T, status string) string {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 || r.URL.Path != capabilitiesMethod || r.Header.Get("Content-Type") != "application/grpc" {
			http.Error(w, "not a capabilities call", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", status)
		w.WriteHeader(http.StatusOK)
	}))
	server.Config.Protocols = new(http.Protocols)
	server.Config.Protocols.SetUnencryptedHTTP2(true)
	server.Start()
	t.Cleanup(server.Close)
	return "grpc://" + server.Listener.Addr().String()
}

func TestRemoteCacheProbeAcceptsOnlyAnAnsweringCache(t *testing.T) {
	if err := probeRemoteCache(context.Background(), grpcServer(t, "0")); err != nil {
		t.Fatalf("healthy gRPC cache rejected: %v", err)
	}
	for status, reason := range map[string]string{"14": "unavailable", "16": "unauthenticated"} {
		if err := probeRemoteCache(context.Background(), grpcServer(t, status)); err == nil {
			t.Errorf("accepted a cache answering %s", reason)
		}
	}
	missing := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(missing.Close)
	if err := probeRemoteCache(context.Background(), missing.URL); err != nil {
		t.Errorf("HTTP cache answering 404 for an unknown action rejected: %v", err)
	}
	for _, endpoint := range []string{"grpc://127.0.0.1:1", "cache.example:9092", "unix:///run/cache.sock"} {
		if err := probeRemoteCache(context.Background(), endpoint); err == nil {
			t.Errorf("accepted %s", endpoint)
		}
	}
}

func TestRemoteCacheProbeGivesUpWithinItsDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { connection.Close() })
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := probeRemoteCache(ctx, "grpc://"+listener.Addr().String()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("silent cache returned %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("probe waited %s for a silent cache", elapsed)
	}
}

func TestUnavailableRemoteCacheIsLeftOutOfTheBazelInvocation(t *testing.T) {
	for _, test := range []struct {
		endpoint, report string
		used             bool
	}{
		{endpoint: grpcServer(t, "0"), report: "used", used: true},
		{endpoint: "grpc://127.0.0.1:1", report: "unavailable"},
	} {
		root, bazel, calls := fakeWorkspace(t)
		var log strings.Builder
		report, err := Run(context.Background(), Options{Root: root, Local: true, Bazel: bazel, Operation: "build", RemoteCache: test.endpoint, ReadOnlyCache: true, ReportDir: t.TempDir(), Log: &log})
		if err != nil {
			t.Fatal(err)
		}
		invocation := readFile(t, calls)
		if report.RemoteCache != test.report || strings.Contains(invocation, "--remote_cache=") != test.used {
			t.Errorf("%s: report %q, invocation %q", test.endpoint, report.RemoteCache, invocation)
		}
		if test.used && (!strings.Contains(invocation, "--remote_cache_compression") || !strings.Contains(invocation, "--remote_timeout=") || !strings.Contains(invocation, "--jobs=") || !strings.Contains(invocation, "--remote_upload_local_results=false") || !strings.Contains(invocation, "--experimental_build_event_upload_strategy=local")) {
			t.Errorf("remote invocation %q lacks compression, timeout, concurrent lookups, read-only uploads or local build events", invocation)
		}
		if !test.used && strings.Contains(invocation, "--jobs=") {
			t.Errorf("local invocation %q raises the job count", invocation)
		}
		if !test.used && !strings.Contains(log.String(), "remote cache unavailable") {
			t.Errorf("fallback not logged: %q", log.String())
		}
	}
}
