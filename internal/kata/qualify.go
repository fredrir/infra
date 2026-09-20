package kata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type QualifyOptions struct {
	Archive, Manifest, Image, Output, Address string
	Timeout                                   time.Duration
	DisposableHost                            bool
	Log                                       io.Writer
}
type Qualification struct {
	ArchiveSHA256 string    `json:"archiveSha256"`
	Image         string    `json:"image"`
	Kernel        string    `json:"kernel"`
	Passed        bool      `json:"passed"`
	Checks        []string  `json:"checks"`
	FinishedAt    time.Time `json:"finishedAt"`
}

func Qualify(ctx context.Context, o QualifyOptions) (r Qualification, err error) {
	if !o.DisposableHost {
		return r, errors.New("qualification requires --disposable-host")
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return r, errors.New("native Linux amd64 required")
	}
	if o.Timeout <= 0 || o.Timeout > 30*time.Minute {
		return r, errors.New("qualification deadline must be within 30 minutes")
	}
	if !strings.Contains(o.Image, "@sha256:") || !validDigest(strings.Split(o.Image, "@sha256:")[1]) {
		return r, errors.New("digest-pinned qualification image required")
	}
	if o.Output == "" {
		return r, errors.New("qualification output required")
	}
	if _, e := os.Stat(o.Output); !errors.Is(e, os.ErrNotExist) {
		return r, errors.New("qualification output already exists")
	}
	b, e := os.ReadFile(o.Manifest)
	if e != nil {
		return r, e
	}
	var m Manifest
	if e = json.Unmarshal(b, &m); e != nil {
		return r, e
	}
	if !m.NativeQualificationRequired || len(m.Files) == 0 {
		return r, errors.New("candidate manifest required")
	}
	if e = verify(o.Archive, m.SHA256); e != nil {
		return r, e
	}
	if e = verifyArchive(ctx, o.Archive, m.Files); e != nil {
		return r, e
	}
	r.ArchiveSHA256 = m.SHA256
	r.Image = o.Image
	for name, digest := range m.Files {
		if !validArchivePath(name) {
			return r, errors.New("invalid candidate path")
		}
		if e = verify("/"+name, digest); e != nil {
			return r, fmt.Errorf("installed candidate: %w", e)
		}
	}
	r.Checks = append(r.Checks, "installed candidate matches archive manifest")
	kvm, e := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if e != nil {
		return r, e
	}
	if e = kvm.Close(); e != nil {
		return r, e
	}
	r.Checks = append(r.Checks, "KVM accessible")
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	kernel, e := output(ctx, "", "uname", "-r")
	if e != nil {
		return r, e
	}
	r.Kernel = strings.TrimSpace(string(kernel))
	work, e := os.MkdirTemp("", "infra-kata-qualification-")
	if e != nil {
		return r, e
	}
	defer os.RemoveAll(work)
	id := filepath.Base(work)
	base := []string{"ctr", "--address", o.Address, "--namespace", id}
	ctr := func(c context.Context, args ...string) error {
		return run(c, "", nil, o.Log, append(append([]string{}, base...), args...)...)
	}
	capture := func(args ...string) ([]byte, error) {
		return output(ctx, "", append(append([]string{}, base...), args...)...)
	}
	pulled, created := false, false
	defer func() {
		cleanup, c := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer c()
		if created {
			if e := ctr(cleanup, "tasks", "kill", "--signal", "SIGKILL", id); e != nil {
				err = errors.Join(err, e)
			}
			if e := ctr(cleanup, "tasks", "delete", id); e != nil {
				err = errors.Join(err, e)
			}
			if e := ctr(cleanup, "containers", "delete", id); e != nil {
				err = errors.Join(err, e)
			}
		}
		if pulled {
			if e := ctr(cleanup, "images", "remove", o.Image); e != nil {
				err = errors.Join(err, e)
			}
		}
		if e := ctr(cleanup, "namespaces", "remove", id); e != nil {
			err = errors.Join(err, e)
		}
		r.Passed = err == nil
		r.FinishedAt = time.Now().UTC()
		if r.Passed {
			r.Checks = append(r.Checks, "runtime cleanup")
		}
		if e := writeJSON(o.Output, r); e != nil {
			err = errors.Join(err, e)
		}
	}()
	if e = ctr(ctx, "namespaces", "create", id); e != nil {
		return r, e
	}
	if e = ctr(ctx, "images", "pull", o.Image); e != nil {
		return r, e
	}
	pulled = true
	if e = os.WriteFile(filepath.Join(work, "sentinel"), []byte(id), 0644); e != nil {
		return r, e
	}
	created = true
	if e = ctr(ctx, "run", "--runtime", "/opt/kata/runtime-rs/bin/containerd-shim-kata-v2", "--runtime-config-path", "/opt/kata/share/defaults/kata-containers/runtime-rs/configuration.toml", "--mount", "type=bind,src="+work+",dst=/qualification,options=rbind:rw", "--memory-limit", "268435456", "--cpus", "1", "--detach", o.Image, id, "/bin/sleep", "300"); e != nil {
		return r, e
	}
	r.Checks = append(r.Checks, "runtime create and agent start")
	b, e = capture("tasks", "exec", "--exec-id", "read", id, "/bin/cat", "/qualification/sentinel")
	if e != nil {
		return r, e
	}
	if string(b) != id {
		return r, errors.New("virtiofs read mismatch")
	}
	if e = ctr(ctx, "tasks", "exec", "--exec-id", "write", id, "/bin/touch", "/qualification/from-guest"); e != nil {
		return r, e
	}
	if _, e = os.Stat(filepath.Join(work, "from-guest")); e != nil {
		return r, e
	}
	r.Checks = append(r.Checks, "virtiofs read and write")
	b, e = capture("tasks", "exec", "--exec-id", "memory", id, "/bin/cat", "/sys/fs/cgroup/memory.max")
	if e != nil {
		return r, e
	}
	if strings.TrimSpace(string(b)) != "268435456" {
		return r, errors.New("container memory limit mismatch")
	}
	r.Checks = append(r.Checks, "container memory limit")
	b, e = capture("tasks", "exec", "--exec-id", "cpu", id, "/bin/cat", "/sys/fs/cgroup/cpu.max")
	if e != nil {
		return r, e
	}
	parts := strings.Fields(string(b))
	if len(parts) != 2 || parts[0] != parts[1] {
		return r, errors.New("container CPU limit mismatch")
	}
	r.Checks = append(r.Checks, "container CPU limit")
	return r, nil
}
