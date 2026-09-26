package reconciler

import (
	"context"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/reconcile"
)

func fullScope(t *testing.T, h *harness) {
	t.Helper()
	identity, knownHosts := filepath.Join(t.TempDir(), "ssh-identity"), filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(identity, pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: []byte("verify-identity")}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(knownHosts, []byte("fredrir-07 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.supervisor.Config.Scope, h.supervisor.Config.KnownHosts, h.supervisor.Identity = reconcile.ScopeFull, knownHosts, identity
}

func TestFullVerificationReachesHostsThroughTheRunSSHConfiguration(t *testing.T) {
	var engineArgs, engineEnv []string
	var config, home []string
	h := newHarness(t, verifyCredentialValues(), func(t *testing.T, args, env []string) (int, string) {
		engineArgs, engineEnv = args, env
		for _, variable := range env {
			if path, ok := strings.CutPrefix(variable, "ANSIBLE_SSH_EXTRA_ARGS=-F "); ok {
				data, _ := os.ReadFile(path)
				config = strings.Split(strings.TrimSpace(string(data)), "\n")
			}
			if path, ok := strings.CutPrefix(variable, "HOME="); ok {
				entries, _ := os.ReadDir(path)
				for _, entry := range entries {
					home = append(home, entry.Name())
				}
			}
		}
		return 0, verification("matches", []reconcile.Difference{}, []string{})
	}, nil)
	fullScope(t, h)
	if err := h.supervisor.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(engineArgs, "--scope=full") || !slices.Contains(engineEnv, "INFRA_RECONCILE_TAILNET=true") {
		t.Errorf("engine ran %q with %q", engineArgs, engineEnv)
	}
	want := []string{"Host *", "  IdentityFile " + h.supervisor.Identity, "  IdentitiesOnly yes", "  UserKnownHostsFile " + h.supervisor.Config.KnownHosts, "  GlobalKnownHostsFile /dev/null", "  StrictHostKeyChecking yes", "  UpdateHostKeys no", "  CheckHostIP no", "  ForwardAgent no", "  ForwardX11 no", "  BatchMode yes", "  PasswordAuthentication no", "  KbdInteractiveAuthentication no", "  PermitLocalCommand no"}
	if !slices.Equal(config, want) {
		t.Errorf("SSH configuration %q, want %q", config, want)
	}
	if len(home) != 0 {
		t.Errorf("HOME holds %q", home)
	}
	if uv := slices.IndexFunc(h.commands, func(command string) bool { return strings.HasPrefix(command, "uv sync --frozen --group ci") }); uv < 0 {
		t.Errorf("no Ansible environment in %q", h.commands)
	}
	if entries, err := os.ReadDir(h.supervisor.Config.State); err != nil || len(entries) != 0 {
		t.Errorf("state kept %v: %v", entries, err)
	}
	if entries, err := os.ReadDir(filepath.Dir(h.supervisor.Credentials)); err != nil || len(entries) != 0 {
		t.Errorf("runtime directory kept %v: %v", entries, err)
	}
}

func TestCloudVerificationHasNoHostAccess(t *testing.T) {
	var engineEnv []string
	h := newHarness(t, verifyCredentialValues(), func(t *testing.T, _, env []string) (int, string) {
		engineEnv = env
		return 0, verification("matches", []reconcile.Difference{}, []string{})
	}, nil)
	if err := h.supervisor.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(engineEnv, func(variable string) bool {
		return strings.HasPrefix(variable, "ANSIBLE_SSH_EXTRA_ARGS=") || strings.HasPrefix(variable, "INFRA_RECONCILE_TAILNET=")
	}) {
		t.Errorf("cloud verification ran with %q", engineEnv)
	}
}

func TestFullVerificationFailsWithoutHostAccess(t *testing.T) {
	for name, change := range map[string]func(h *harness){
		"readable identity": func(h *harness) {
			if err := os.Chmod(h.supervisor.Identity, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"identity path with spaces": func(h *harness) { h.supervisor.Identity = "/run/credentials/two words" },
		"empty known hosts": func(h *harness) {
			if err := os.WriteFile(h.supervisor.Config.KnownHosts, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, verifyCredentialValues(), func(t *testing.T, _, _ []string) (int, string) {
				t.Error("the engine ran without host access")
				return 0, ""
			}, nil)
			fullScope(t, h)
			change(h)
			if err := h.supervisor.Verify(context.Background()); err == nil {
				t.Fatal("verification ran without valid host access")
			}
		})
	}
}

func TestVerificationRequestsARepairOnlyForRepairableDifferences(t *testing.T) {
	for _, test := range []struct {
		name        string
		code        int
		report      string
		wantRequest bool
		withdraw    bool
	}{
		{name: "declaration drift", code: 1, report: verification("differs", []reconcile.Difference{{System: "opentofu", Item: "cloudflare_dns_record.grafana update"}, {System: "rulesets", Item: "production"}}, []string{}), wantRequest: true},
		{name: "rulesets only", code: 1, report: verification("differs", []reconcile.Difference{{System: "rulesets", Item: "production"}}, []string{})},
		{name: "matches", report: verification("matches", []reconcile.Difference{}, []string{}), withdraw: true},
		{name: "errors", code: 1, report: verification("failed", []reconcile.Difference{}, []string{"kubectl failed"})},
		{name: "lease held", code: 75, report: verification("failed", []reconcile.Difference{}, []string{"comparisons skipped: reconciliation locked"})},
	} {
		t.Run(test.name, func(t *testing.T) {
			origin, revision := originRepository(t)
			main := gitCommand(t, origin, "rev-parse", "main")
			h := newHarnessAt(t, origin, revision, verifyCredentialValues(), func(t *testing.T, _, _ []string) (int, string) {
				return test.code, test.report
			}, nil)
			earlier := Request{Kind: RequestRepair, Revision: strings.Repeat("f", 40), Full: true, Reason: "earlier drift", Requested: time.Date(2026, 9, 26, 2, 0, 0, 0, time.UTC)}
			if err := WriteRequest(h.supervisor.Config.Shared, earlier); err != nil {
				t.Fatal(err)
			}
			_ = h.supervisor.Verify(context.Background())
			requests, err := readRequests(h.supervisor.Config.Shared)
			if err != nil {
				t.Fatal(err)
			}
			request, requested := requests[RequestRepair]
			switch {
			case test.withdraw && requested:
				t.Fatalf("a matching verification left %+v", request)
			case test.wantRequest && (request.Revision != main || !request.Full || !strings.HasPrefix(request.Reason, "2 differences: opentofu") || !request.Requested.Equal(time.Date(2026, 9, 26, 3, 0, 0, 0, time.UTC))):
				t.Errorf("repair request %+v", request)
			case !test.wantRequest && !test.withdraw && request != earlier:
				t.Errorf("repair request changed to %+v", request)
			}
		})
	}
}

func TestOffMainProductionRequestsARepairOfMain(t *testing.T) {
	origin, forged := offMainOrigin(t)
	h := newHarnessAt(t, origin, forged, verifyCredentialValues(), func(t *testing.T, _, _ []string) (int, string) {
		t.Error("the engine ran for an off-main revision")
		return 0, ""
	}, nil)
	if err := h.supervisor.Verify(context.Background()); err == nil {
		t.Fatal("an off-main revision verified")
	}
	requests, err := readRequests(h.supervisor.Config.Shared)
	if err != nil || requests[RequestRepair].Revision != gitCommand(t, origin, "rev-parse", "main") {
		t.Fatalf("requests %+v, %v", requests, err)
	}
}

func TestVerificationWaitsForARunningApply(t *testing.T) {
	h := newHarness(t, verifyCredentialValues(), func(t *testing.T, _, _ []string) (int, string) {
		return 0, verification("matches", []reconcile.Difference{}, []string{})
	}, nil)
	h.supervisor.LockPoll = time.Millisecond
	release, err := acquireHostLock(context.Background(), h.supervisor.Config.Shared, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.supervisor.Verify(context.Background()) }()
	time.Sleep(30 * time.Millisecond)
	h.mu.Lock()
	started := len(h.commands)
	h.mu.Unlock()
	if started != 0 {
		t.Fatal("verification ran while an apply held the host")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
