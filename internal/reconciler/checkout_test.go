package reconciler

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func hardenedExecutor(t *testing.T) executor {
	t.Helper()
	return executor{env: append([]string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}, hardenedGit...)}
}

func TestCheckoutBuildsOnlyThePublishedRevision(t *testing.T) {
	ctx := context.Background()
	origin, published := originRepository(t)
	if main := gitCommand(t, origin, "rev-parse", "main"); main == published {
		t.Fatal("fixture main does not advance past production")
	}
	source := filepath.Join(t.TempDir(), "source")
	checked, err := hardenedExecutor(t).checkoutPublished(ctx, source, "file://"+origin)
	if err != nil || checked != (publication{Revision: published, OnMain: true}) {
		t.Fatalf("checkout resolved %+v, %v; want production %s on main", checked, err, published)
	}
	if head := gitCommand(t, source, "rev-parse", "HEAD"); head != published {
		t.Fatalf("checkout at %s, want %s", head, published)
	}
	if _, err := os.Stat(filepath.Join(source, "unpublished")); !os.IsNotExist(err) {
		t.Fatalf("unpublished main content checked out: %v", err)
	}
	if hooks, err := os.ReadDir(filepath.Join(source, ".git", "hooks")); err == nil && len(hooks) > 0 {
		t.Fatalf("checkout copied hook templates %v", hooks)
	}
	gitCommand(t, origin, "branch", "-D", "production")
	if _, err := hardenedExecutor(t).checkoutPublished(ctx, filepath.Join(t.TempDir(), "source"), "file://"+origin); err == nil {
		t.Fatal("missing production branch resolved")
	}
}

func offMainOrigin(t *testing.T) (string, string) {
	t.Helper()
	origin, _ := originRepository(t)
	gitCommand(t, origin, "checkout", "--quiet", "production")
	if err := os.WriteFile(filepath.Join(origin, "forged"), []byte("unreviewed"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, origin, "add", ".")
	gitCommand(t, origin, "commit", "--quiet", "--no-gpg-sign", "-m", "forged")
	forged := gitCommand(t, origin, "rev-parse", "HEAD")
	gitCommand(t, origin, "checkout", "--quiet", "main")
	return origin, forged
}

func TestCheckoutRefusesProductionOffMain(t *testing.T) {
	ctx := context.Background()
	origin, forged := offMainOrigin(t)
	source := filepath.Join(t.TempDir(), "source")
	checked, err := hardenedExecutor(t).checkoutPublished(ctx, source, "file://"+origin)
	if err != nil || checked != (publication{Revision: forged}) {
		t.Fatalf("off-main production resolved %+v, %v; want %s refused", checked, err, forged)
	}
	if entries, err := os.ReadDir(source); err != nil || len(entries) != 1 || entries[0].Name() != ".git" {
		t.Fatalf("off-main production checked out %v: %v", entries, err)
	}
	gitCommand(t, origin, "checkout", "--quiet", "--detach", "production")
	gitCommand(t, origin, "branch", "-D", "main")
	if _, err := hardenedExecutor(t).checkoutPublished(ctx, filepath.Join(t.TempDir(), "source"), "file://"+origin); err == nil {
		t.Fatal("production ancestry accepted without main")
	}
}

func TestHardenedGitIgnoresPlantedHooksAndReplacements(t *testing.T) {
	ctx := context.Background()
	origin, published := originRepository(t)
	source := filepath.Join(t.TempDir(), "source")
	hardened := hardenedExecutor(t)
	if _, err := hardened.checkoutPublished(ctx, source, "file://"+origin); err != nil {
		t.Fatal(err)
	}
	decoy := gitCommand(t, origin, "rev-parse", "main")
	if _, err := hardened.output(ctx, source, "git", "fetch", "--quiet", "origin", "+refs/heads/main:refs/remotes/origin/main"); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, source, "replace", published, decoy)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	if err := os.MkdirAll(filepath.Join(source, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, hook := range []string{"post-checkout", "reference-transaction"} {
		if err := os.WriteFile(filepath.Join(source, ".git", "hooks", hook), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitCommand(t, source, "config", "core.fsmonitor", "touch "+marker+"; false")
	if _, err := hardened.output(ctx, source, "git", "fetch", "--quiet", "origin", "+refs/heads/main:refs/remotes/origin/fetched"); err != nil {
		t.Fatal(err)
	}
	for _, step := range [][]string{{"checkout", "--quiet", "--detach", "origin/main"}, {"checkout", "--quiet", "--detach", published}, {"status", "--porcelain"}} {
		if _, err := hardened.output(ctx, source, "git", step...); err != nil {
			t.Fatal(err)
		}
	}
	if message, err := hardened.output(ctx, source, "git", "log", "-1", "--format=%s", published); err != nil || message != "declare" {
		t.Fatalf("hardened git read %s as %q: %v", published, message, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a planted hook ran under hardened git: %v", err)
	}
	plain := executor{env: slices.DeleteFunc(slices.Clone(hardened.env), func(variable string) bool { return slices.Contains(hardenedGit, variable) })}
	if message, _ := plain.output(ctx, source, "git", "log", "-1", "--format=%s", published); message != "unpublished" {
		t.Fatalf("fixture replacement not visible to plain git: %q", message)
	}
	if _, err := plain.output(ctx, source, "git", "checkout", "--quiet", "--detach", "origin/main"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("fixture hook did not run under plain git: %v", err)
	}
}

func TestClearDirectoryRemovesReadOnlyTrees(t *testing.T) {
	state := t.TempDir()
	plantPoison(t, state)
	if err := clearDirectory(state); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(state); err != nil || len(entries) != 0 {
		t.Fatalf("state kept %v: %v", entries, err)
	}
	if err := removeTree(filepath.Join(state, "absent")); err != nil {
		t.Fatalf("absent tree: %v", err)
	}
	if strings.Contains(state, "..") {
		t.Fatal("unexpected state path")
	}
}
