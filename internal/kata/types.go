package kata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/process"
)

type Component struct {
	Archive                 string  `json:"archive,omitempty"`
	Snapshot                string  `json:"snapshot,omitempty"`
	Version                 string  `json:"version,omitempty"`
	Tag                     string  `json:"tag,omitempty"`
	SourceRevision          string  `json:"sourceRevision,omitempty"`
	Repository              string  `json:"repository,omitempty"`
	Builder                 string  `json:"builder,omitempty"`
	URL                     string  `json:"url,omitempty"`
	SHA256                  string  `json:"sha256,omitempty"`
	AgentSHA256             string  `json:"agentSha256,omitempty"`
	CargoLockSHA256         string  `json:"cargoLockSha256,omitempty"`
	BootstrapCABundleSHA256 string  `json:"bootstrapCaBundleSha256,omitempty"`
	MakedevScriptSHA256     string  `json:"makedevScriptSha256,omitempty"`
	ConfigVersion           string  `json:"configVersion,omitempty"`
	PackagingPatches        []Patch `json:"packagingPatches,omitempty"`
}

type Patch struct {
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	SourcePath string `json:"sourcePath"`
	Action     string `json:"action"`
}
type Pins struct {
	Architecture string               `json:"architecture"`
	Files        map[string]string    `json:"files"`
	Components   map[string]Component `json:"components"`
}
type Limits struct {
	CPUs        int
	MemoryBytes int64
	PIDs        int
	Timeout     time.Duration
}

func DefaultLimits() Limits { return Limits{2, 4 << 30, 256, 2 * time.Hour} }
func (l Limits) Validate() error {
	if l.CPUs < 1 || l.CPUs > 2 || l.MemoryBytes < 1 || l.MemoryBytes > 4<<30 || l.PIDs < 1 || l.PIDs > 256 || l.Timeout <= 0 || l.Timeout > 2*time.Hour {
		return errors.New("limits must not exceed 2 CPUs, 4 GiB, 256 PIDs, 2 hours")
	}
	return nil
}

type Mount struct{ Source, Target string }
type GitSource struct{ Repository, Revision, Target string }
type ContainerRequest struct {
	Image, BuildContext, WorkDir, OutputDir, OutputPath string
	Mounts                                              []Mount
	Sources                                             []GitSource
	Args                                                []string
	Env                                                 map[string]string
	Privileged                                          bool
	Limits                                              Limits
}
type Executor interface {
	Execute(context.Context, ContainerRequest) error
}
type BuildOptions struct {
	Component, WorkDir, RepoDir, Binary string
	AllowPrivilegedGuest                bool
	Limits                              Limits
	Log                                 io.Writer
}

func LoadPins(root string) (Pins, error) {
	var p Pins
	b, e := os.ReadFile(filepath.Join(root, "ansible/roles/ci_runtime/files/kata-runtime.json"))
	if e != nil {
		return p, e
	}
	e = json.Unmarshal(b, &p)
	if e != nil {
		return p, e
	}
	if p.Architecture != "amd64" {
		return p, errors.New("invalid Kata pins")
	}
	for name, digest := range p.Files {
		if !validArchivePath(name) || !validDigest(digest) {
			return p, fmt.Errorf("invalid file pin %q", name)
		}
	}
	return p, nil
}
func validDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == sha256.Size
}
func digestFile(name string) (string, error) {
	f, e := os.Open(name)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func verify(name, expected string) error {
	if !validDigest(expected) {
		return errors.New("invalid SHA256 pin")
	}
	got, e := digestFile(name)
	if e != nil {
		return e
	}
	if got != expected {
		return fmt.Errorf("checksum mismatch: %s", name)
	}
	return nil
}
func writeJSON(name string, v any) error {
	data, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return os.WriteFile(name, append(data, '\n'), 0644)
}
func run(ctx context.Context, dir string, env map[string]string, log io.Writer, args ...string) error {
	_, e := process.Run(ctx, process.Options{Name: args[0], Args: args[1:], Dir: dir, Env: cleanEnv(env), Stdout: log, Stderr: log})
	return e
}

func cleanEnv(extra map[string]string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	for _, key := range []string{"RUSTUP_HOME", "RUSTUP_TOOLCHAIN", "LD_LIBRARY_PATH", "SSL_CERT_FILE", "OMP_NUM_THREADS", "OMP_THREAD_LIMIT", "MAKEFLAGS"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}
func output(ctx context.Context, dir string, args ...string) ([]byte, error) {
	r, e := process.Run(ctx, process.Options{Name: args[0], Args: args[1:], Dir: dir, Env: cleanEnv(nil)})
	return r.Stdout, e
}

func copyFile(from, to string, mode os.FileMode) error {
	f, e := os.Open(from)
	if e != nil {
		return e
	}
	defer f.Close()
	if e = os.MkdirAll(filepath.Dir(to), 0755); e != nil {
		return e
	}
	o, e := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if e != nil {
		return e
	}
	_, e = io.Copy(o, f)
	return errors.Join(e, o.Close())
}
func validArchivePath(name string) bool {
	return strings.HasPrefix(name, "opt/kata/") && filepath.IsLocal(name) && filepath.ToSlash(filepath.Clean(name)) == name && !strings.ContainsAny(name, "\\\x00")
}
