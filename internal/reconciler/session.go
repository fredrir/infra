package reconciler

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/fredrir/infra/internal/process"
)

const identityLimit = 16 << 10

type session struct {
	work, home, source string
	commands           executor
}

func openSession(root string, execute func(context.Context, process.Options) (process.Result, error), log io.Writer, hosts bool) (session, func() error, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return session{}, nil, err
	}
	if err := clearDirectory(root); err != nil {
		return session{}, nil, err
	}
	work, err := os.MkdirTemp(root, "run-")
	if err != nil {
		return session{}, nil, err
	}
	cleanup := func() error { return removeTree(work) }
	home := filepath.Join(work, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		return session{}, nil, errors.Join(err, cleanup())
	}
	path := filepath.Join(work, "tools") + ":/usr/local/bin:/usr/bin:/bin"
	if hosts {
		path = filepath.Join(work, "venv", "bin") + ":" + path
	}
	environment := append([]string{"PATH=" + path, "HOME=" + home, "LANG=C.UTF-8", "TF_IN_AUTOMATION=true"}, hardenedGit...)
	return session{work: work, home: home, source: filepath.Join(work, "source"), commands: executor{execute: execute, env: environment, log: log}}, cleanup, nil
}

func (s session) kubeconfig(kubernetes Kubernetes, account, token string) (string, error) {
	authority, err := os.ReadFile(kubernetes.CertificateAuthority)
	if err != nil {
		return "", err
	}
	config, err := Kubeconfig(kubernetes.Server, authority, account, token)
	if err != nil {
		return "", err
	}
	path := filepath.Join(s.work, "kubeconfig")
	return path, os.WriteFile(path, config, 0o600)
}

func (s session) hostAccess(identity, knownHosts string) error {
	key, err := readPrivate(identity, identityLimit)
	if err != nil {
		return fmt.Errorf("SSH identity: %w", err)
	}
	if block, _ := pem.Decode(key); block == nil || block.Type != "OPENSSH PRIVATE KEY" {
		return errors.New("SSH identity is not an OpenSSH private key")
	}
	hosts, err := os.ReadFile(knownHosts)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(hosts)) == 0 {
		return fmt.Errorf("%s declares no host keys", knownHosts)
	}
	directory := filepath.Join(s.home, ".ssh")
	if err := os.Mkdir(directory, 0o700); err != nil {
		return err
	}
	return errors.Join(os.WriteFile(filepath.Join(directory, "id_ed25519"), key, 0o600), os.WriteFile(filepath.Join(directory, "known_hosts"), hosts, 0o600))
}

func (s session) pythonEnvironment(ctx context.Context, cache string) error {
	environment := []string{"UV_CACHE_DIR=" + filepath.Join(cache, "uv"), "UV_PROJECT_ENVIRONMENT=" + filepath.Join(s.work, "venv"), "UV_PYTHON_DOWNLOADS=never", "UV_LINK_MODE=copy", "UV_NO_CONFIG=1"}
	if _, err := s.commands.run(ctx, s.source, environment, "uv", "sync", "--frozen", "--group", "ci", "--no-install-project"); err != nil {
		return fmt.Errorf("Ansible environment: %w", err)
	}
	return nil
}

func readPrivate(path string, limit int64) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("must be a regular file readable only by its owner")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("exceeds %d bytes", limit)
	}
	return data, err
}
