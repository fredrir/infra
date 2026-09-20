package operations

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/process"
)

func TestSDKUploadSignsRequestAndStopsForwarder(t *testing.T) {
	t.Setenv("SOPS_AGE_KEY_FILE", "test-age-key")
	t.Setenv("KUBECONFIG", "test-kubeconfig")
	for _, status := range []int{http.StatusOK, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var received atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received.Store(true)
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != "archive content" || r.Method != "PUT" || r.URL.Path != "/toolchains/MacOSX15.4.sdk.tar.zst" {
					t.Errorf("request %s %s %q: %v", r.Method, r.URL.Path, body, err)
				}
				auth := r.Header.Get("Authorization")
				if !strings.Contains(auth, "Credential=access-id/") || !strings.Contains(auth, "/garage/s3/aws4_request") || strings.Contains(auth, "private-secret") {
					t.Errorf("invalid authorization: %s", auth)
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			root := t.TempDir()
			archive := filepath.Join(root, "MacOSX15.4.sdk.tar.zst")
			if err := os.WriteFile(archive, []byte("archive content"), 0600); err != nil {
				t.Fatal(err)
			}
			var stopped atomic.Bool
			run := func(ctx context.Context, p process.Options) (process.Result, error) {
				if strings.Contains(strings.Join(p.Args, " "), "private-secret") {
					t.Error("secret leaked to arguments")
				}
				if p.Name == "sops" {
					if strings.Contains(p.Args[2], `["id"]`) {
						return process.Result{Stdout: []byte("access-id\n")}, nil
					}
					return process.Result{Stdout: []byte("private-secret\n")}, nil
				}
				if p.Name != "kubectl" {
					t.Errorf("unexpected process %s", p.Name)
				}
				if !strings.Contains(strings.Join(p.Args, " "), "--address 127.0.0.1") {
					t.Error("forwarder not loopback")
				}
				_, err := fmt.Fprintf(p.Stdout, "Forwarding from %s -> 3900\n", strings.TrimPrefix(server.URL, "http://"))
				if err != nil {
					return process.Result{}, err
				}
				<-ctx.Done()
				stopped.Store(true)
				return process.Result{}, ctx.Err()
			}
			err := UploadMacOSSDK(context.Background(), SDKUploadOptions{Root: root, Archive: archive, Run: run, Timeout: time.Minute})
			if (err == nil) != (status == 200) {
				t.Fatalf("status=%d err=%v", status, err)
			}
			if !received.Load() || !stopped.Load() {
				t.Fatal("upload missing or forwarder leaked")
			}
			if err != nil && strings.Contains(err.Error(), "private-secret") {
				t.Fatal("secret exposed")
			}
		})
	}
}

func TestForwardReadinessHandlesFragmentedOutput(t *testing.T) {
	ready := make(chan string, 1)
	out := &forwardOutput{ready: ready}
	for _, text := range []string{"initial message\nFor", "warding from 127.0.0.1:", "49152 -> 3900\n"} {
		if _, err := out.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case address := <-ready:
		if address != "127.0.0.1:49152" {
			t.Fatal(address)
		}
	default:
		t.Fatal("missing readiness")
	}
	for _, address := range []string{"0.0.0.0:3900", "127.0.0.1:0", "127.0.0.1:65536"} {
		if _, err := out.Write([]byte("Forwarding from " + address + " -> 3900\n")); err == nil {
			t.Fatal("accepted", address)
		}
	}
}

func TestSDKUploadStopsOnEarlyForwardFailure(t *testing.T) {
	t.Setenv("SOPS_AGE_KEY_FILE", "key")
	t.Setenv("KUBECONFIG", "config")
	root := t.TempDir()
	writeFixture(t, root, "MacOSX15.sdk.tar.zst", "archive")
	run := func(_ context.Context, p process.Options) (process.Result, error) {
		if p.Name == "sops" {
			return process.Result{Stdout: []byte("value")}, nil
		}
		return process.Result{}, fmt.Errorf("forward failed")
	}
	err := UploadMacOSSDK(context.Background(), SDKUploadOptions{Root: root, Archive: filepath.Join(root, "MacOSX15.sdk.tar.zst"), Run: run, Timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "before readiness") {
		t.Fatal(err)
	}
}
