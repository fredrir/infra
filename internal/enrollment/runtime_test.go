package enrollment

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/process"
)

func runtimeFixture(t *testing.T) (string, Target, []byte) {
	t.Helper()
	root := t.TempDir()
	target := fixtureTarget()
	target.KeyID = "kFixture123"
	payload, _ := json.Marshal(map[string]any{"key": fixtureKey, "metadata": map[string]any{"id": target.KeyID, "node": target.Node, "role": target.Role}})
	return root, target, payload
}

func TestRuntimeDeliveryIsPrivateAtomicAndRefusesOverwrite(t *testing.T) {
	root, target, payload := runtimeFixture(t)
	if err := RuntimeOperation("preflight", target, nil, root, os.Geteuid()); err != nil {
		t.Fatal(err)
	}
	if err := RuntimeOperation("deliver", target, bytes.NewReader(payload), root, os.Geteuid()); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "secrets", "tailscale-auth-key")
	info, err := os.Stat(keyPath)
	if err != nil || info.Mode().Perm() != 0o400 {
		t.Fatalf("key mode: %v %v", info, err)
	}
	if err := RuntimeOperation("deliver", target, bytes.NewReader(payload), root, os.Geteuid()); err == nil {
		t.Fatal("key overwritten")
	}
	if err := RuntimeOperation("cleanup", target, nil, root, os.Geteuid()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatal("key retained")
	}
	if err := RuntimeOperation("cleanup", target, nil, root, os.Geteuid()); err != nil {
		t.Fatal("cleanup not idempotent")
	}
}

func TestRuntimeRejectsReceiptThatCannotBeCleaned(t *testing.T) {
	root, target, _ := runtimeFixture(t)
	payload, _ := json.Marshal(map[string]any{"key": fixtureKey, "metadata": map[string]any{"id": target.KeyID, "node": target.Node, "role": target.Role, "extra": strings.Repeat("x", 5000)}})
	if err := RuntimeOperation("deliver", target, bytes.NewReader(payload), root, os.Geteuid()); err == nil {
		t.Fatal("oversized receipt accepted")
	}
	entries, err := os.ReadDir(filepath.Join(root, "secrets"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("key material retained: %v %v", entries, err)
	}
}

func TestRuntimeRefusesSymlinkParentAndAlteredKey(t *testing.T) {
	root, target, payload := runtimeFixture(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "secrets")); err != nil {
		t.Fatal(err)
	}
	if err := RuntimeOperation("deliver", target, bytes.NewReader(payload), root, os.Geteuid()); err == nil {
		t.Fatal("symlink parent followed")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("outside root modified")
	}
	if err := os.Remove(filepath.Join(root, "secrets")); err != nil {
		t.Fatal(err)
	}
	if err := RuntimeOperation("deliver", target, bytes.NewReader(payload), root, os.Geteuid()); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "secrets", "tailscale-auth-key")
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("other-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RuntimeOperation("cleanup", target, nil, root, os.Geteuid()); err == nil {
		t.Fatal("changed key removed")
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatal("changed key was deleted")
	}
}

func TestRuntimeCleanupIsBoundToReceiptAndRefusesHardlinks(t *testing.T) {
	root, target, payload := runtimeFixture(t)
	if err := RuntimeOperation("deliver", target, bytes.NewReader(payload), root, os.Geteuid()); err != nil {
		t.Fatal(err)
	}
	wrong := target
	wrong.KeyID = "kOther123"
	if err := RuntimeOperation("cleanup", wrong, nil, root, os.Geteuid()); err == nil {
		t.Fatal("foreign receipt removed")
	}
	keyPath := filepath.Join(root, "secrets", "tailscale-auth-key")
	if err := os.Link(keyPath, filepath.Join(root, "retained")); err != nil {
		t.Fatal(err)
	}
	if err := RuntimeOperation("cleanup", target, nil, root, os.Geteuid()); err == nil {
		t.Fatal("hardlinked key accepted")
	}
}

func TestSSHUsesStdinAsOnlyCredentialChannel(t *testing.T) {
	t.Setenv("TS_API_CLIENT_SECRET", "private-oauth")
	for _, sudo := range []bool{false, true} {
		transport, err := sshTransport("/usr/local/bin/infra", sudo, func(_ context.Context, o process.Options) (process.Result, error) {
			command := strings.Join(o.Args, " ")
			if strings.Contains(command, fixtureKey) || strings.Contains(strings.Join(o.Env, " "), "private-oauth") {
				t.Fatal("credentials escaped stdin")
			}
			if strings.Contains(command, "/usr/bin/sudo -n --") != sudo {
				t.Fatal("sudo option ignored")
			}
			if !strings.Contains(command, "StrictHostKeyChecking=yes") || !strings.Contains(command, "ForwardAgent=no") {
				t.Fatal("unsafe SSH options")
			}
			data, _ := io.ReadAll(o.Stdin)
			if !bytes.Contains(data, []byte(fixtureKey)) {
				t.Fatal("key not delivered on stdin")
			}
			return process.Result{Stdout: []byte(`{"result":"ok"}`)}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		target := fixtureTarget()
		target.KeyID = "kFixture123"
		if err := transport(context.Background(), "deliver", target, map[string]any{"key": fixtureKey}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInvalidRemoteBinaryAndTargetsFailBeforeAccess(t *testing.T) {
	for _, binary := range []string{"infra", "/tmp/../infra", "/tmp/infra;evil", "/tmp//infra", "/tmp/infra\n"} {
		if _, err := SSHTransport(binary, true); err == nil {
			t.Errorf("accepted %q", binary)
		}
	}
	for _, key := range []string{"/tmp/key", "/run/../key", "/run/a//key", "/run/key;evil"} {
		target := fixtureTarget()
		target.KeyFile = key
		if err := ValidateTarget(target); err == nil {
			t.Errorf("accepted %q", key)
		}
	}
}
