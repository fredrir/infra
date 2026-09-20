package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fredrir/infra/internal/objectstore"
	"github.com/fredrir/infra/internal/process"
)

type SDKUploadOptions struct {
	Root, Archive string
	Run           Runner
	HTTP          *http.Client
	Timeout       time.Duration
}

func UploadMacOSSDK(ctx context.Context, o SDKUploadOptions) (err error) {
	if o.Timeout <= 0 || o.Timeout > 2*time.Hour {
		return errors.New("upload timeout must be within two hours")
	}
	if os.Getenv("SOPS_AGE_KEY_FILE") == "" || os.Getenv("KUBECONFIG") == "" {
		return errors.New("SOPS_AGE_KEY_FILE and KUBECONFIG required")
	}
	if !regexp.MustCompile(`^MacOSX[0-9]+(\.[0-9]+)*\.sdk\.tar\.zst$`).MatchString(filepath.Base(o.Archive)) {
		return errors.New("MacOS SDK archive required")
	}
	archive, e := os.Open(o.Archive)
	if e != nil {
		return e
	}
	defer archive.Close()
	st, e := archive.Stat()
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() || st.Size() == 0 {
		return errors.New("nonempty regular archive required")
	}
	if o.Run == nil {
		o.Run = process.Run
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	secret := filepath.Join(o.Root, "platform/components/build-cache/provisioner.secret.sops.yaml")
	credentials := map[string]string{}
	for _, field := range []string{"id", "secret"} {
		r, e := o.Run(ctx, process.Options{Name: "sops", Args: []string{"decrypt", "--extract", `["stringData"]["` + field + `"]`, secret}, Timeout: 30 * time.Second})
		if e != nil {
			return fmt.Errorf("decrypt toolchain credentials: %w", e)
		}
		value := strings.TrimSpace(string(r.Stdout))
		if value == "" || strings.ContainsAny(value, "\r\n\t ") {
			return errors.New("invalid toolchain credentials")
		}
		credentials[field] = value
	}
	forwardCtx, stop := context.WithCancel(ctx)
	ready := make(chan string, 1)
	out := &forwardOutput{ready: ready}
	done := make(chan error, 1)
	go func() {
		_, e := o.Run(forwardCtx, process.Options{Name: "kubectl", Args: []string{"--namespace", "build-cache", "port-forward", "--address", "127.0.0.1", "service/garage", ":3900"}, Stdout: out})
		done <- e
	}()
	defer func() { stop(); <-done }()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	var address string
	select {
	case address = <-ready:
	case e = <-done:
		done <- e
		return errors.New("Garage port-forward exited before readiness")
	case <-timer.C:
		return errors.New("Garage port-forward readiness timeout")
	case <-ctx.Done():
		return ctx.Err()
	}
	client := objectstore.Client{Endpoint: "http://" + address, Region: "garage", AccessKey: credentials["id"], SecretKey: credentials["secret"], HTTP: o.HTTP}
	return client.Upload(ctx, "toolchains", filepath.Base(o.Archive), archive)
}

type forwardOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	ready  chan<- string
}

func (f *forwardOutput) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	size := len(p)
	if f.buffer.Len()+size > 8192 {
		return 0, errors.New("port-forward output too large")
	}
	f.buffer.Write(p)
	for {
		line, e := f.buffer.ReadString('\n')
		if e == io.EOF {
			f.buffer.WriteString(line)
			return size, nil
		}
		if e != nil {
			return 0, e
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Forwarding from ") || !strings.HasSuffix(line, " -> 3900") {
			continue
		}
		address := strings.TrimSuffix(strings.TrimPrefix(line, "Forwarding from "), " -> 3900")
		host, port, e := net.SplitHostPort(address)
		if e != nil || host != "127.0.0.1" {
			return 0, errors.New("port-forward must bind loopback")
		}
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return 0, errors.New("invalid forwarding port")
		}
		select {
		case f.ready <- address:
		default:
		}
	}
}
