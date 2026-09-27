package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func guardedBazel(t *testing.T) (string, string, string) {
	t.Helper()
	root, bazel, calls := fakeWorkspace(t)
	for _, name := range []string{
		"ARC_GITHUB_APP_PRIVATE_KEY", "AUR_SSH_KEY", "AWS_SECRET_ACCESS_KEY", "CLOUDFLARE_API_TOKEN",
		"HCLOUD_TOKEN", "LINODE_TOKEN", "PACKAGES_APK_KEY", "PACKAGES_GPG_KEY", "PLATFORM_WATCHDOG_SMTP_PASSWORD",
		"TAILSCALE_ENROLL_CLIENT_SECRET", "TAILSCALE_POLICY_CLIENT_SECRET", "GH_TOKEN", "GITHUB_TOKEN",
		"ACTIONS_RUNTIME_TOKEN", "DAGGER_CLOUD_TOKEN", "_EXPERIMENTAL_DAGGER_RUNNER_HOST", "UNRELATED_BUILD_SECRET",
	} {
		t.Setenv(name, "excluded-from-bazel")
	}
	home, temporary, cache := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", temporary)
	t.Setenv("XDG_CACHE_HOME", cache)
	path := filepath.Dir(bazel) + string(os.PathListSeparator) + "/usr/bin:/bin"
	t.Setenv("PATH", path)
	contents := `#!/bin/sh
set -eu
if env | cut -d= -f1 | grep -Eq '^(ARC_GITHUB_APP_PRIVATE_KEY|AUR_SSH_KEY|AWS_SECRET_ACCESS_KEY|CLOUDFLARE_API_TOKEN|HCLOUD_TOKEN|LINODE_TOKEN|PACKAGES_APK_KEY|PACKAGES_GPG_KEY|PLATFORM_WATCHDOG_SMTP_PASSWORD|TAILSCALE_ENROLL_CLIENT_SECRET|TAILSCALE_POLICY_CLIENT_SECRET|GH_TOKEN|GITHUB_TOKEN|ACTIONS_RUNTIME_TOKEN|DAGGER_CLOUD_TOKEN|_EXPERIMENTAL_DAGGER_RUNNER_HOST|UNRELATED_BUILD_SECRET)$'; then
  echo 'unrelated credentials reached Bazel' >&2
  exit 81
fi
test "$HOME" = '` + home + `'
test "$TMPDIR" = '` + temporary + `'
test "$XDG_CACHE_HOME" = '` + cache + `'
test "$PATH" = '` + path + `'
printf '%s\n' "$1" >> '` + calls + `'
case "$1" in
  --version) echo 'bazel 9.2.0' ;;
  query) echo //internal/example:example_test ;;
esac
`
	if err := os.WriteFile(bazel, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	return root, bazel, calls
}

func TestLocalBazelCommandsReceiveOnlyBuildEnvironment(t *testing.T) {
	root, bazel, calls := guardedBazel(t)
	config, err := ReadToolchain(root)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Root: root, Bazel: filepath.Base(bazel), Local: true, Operation: "test", ReportDir: t.TempDir()}
	if err := runLocal(context.Background(), opts, config, "//internal/example:example_test", nil, &Report{}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, calls); got != "--version\nquery\ntest\n" {
		t.Fatalf("Bazel launch phases: %q", got)
	}
	if os.Getenv("DAGGER_CLOUD_TOKEN") != "excluded-from-bazel" || os.Getenv("_EXPERIMENTAL_DAGGER_RUNNER_HOST") != "excluded-from-bazel" {
		t.Fatal("Bazel environment filtering changed the caller's Dagger environment")
	}
}

func TestBazelDoctorDoesNotForwardOperatorCredentials(t *testing.T) {
	root, bazel, calls := guardedBazel(t)
	for _, check := range Doctor(context.Background(), root, bazel, false) {
		if check.Name == bazel && !check.OK {
			t.Fatalf("Bazel diagnostic failed: %s", check.Detail)
		}
	}
	if got := strings.TrimSpace(readFile(t, calls)); got != "--version" {
		t.Fatalf("Bazel diagnostic launch: %q", got)
	}
}
