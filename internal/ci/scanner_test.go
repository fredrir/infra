package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/process"
)

func scannerFixture(t testing.TB, directory string, database scannerDatabase, contents string, downloaded, next time.Time) {
	t.Helper()
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, database.filename), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	metadata, err := json.Marshal(map[string]any{"Version": database.schema, "DownloadedAt": downloaded, "NextUpdate": next})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
}

func scannerDownloader(t testing.TB, calls *atomic.Int32) process.Runner {
	t.Helper()
	return process.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		calls.Add(1)
		if options.Name != "trivy" || len(options.Args) < 5 || options.Args[1] != "--cache-dir" {
			t.Errorf("unexpected scanner command: %s %q", options.Name, options.Args)
		}
		for _, database := range scannerDatabases {
			if slices.Contains(options.Args, database.flag) {
				scannerFixture(t, filepath.Join(options.Args[2], database.directory), database, database.filename, time.Now().Add(-time.Minute), time.Now().Add(24*time.Hour))
			}
		}
		return process.Result{}, nil
	}}
}

func scannerGeneration(t *testing.T, shared, name string, database scannerDatabase, contents string, downloaded, next time.Time) string {
	t.Helper()
	scannerFixture(t, filepath.Join(shared, name), database, contents, downloaded, next)
	if err := os.Chmod(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceSymlink(name, filepath.Join(shared, database.key())); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(shared, name)
}

func scannerTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		target, _ := os.Readlink(path)
		tree[path] = fmt.Sprintf("%v %d %s %d", info.Mode(), info.Size(), target, info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestScannerSharesOneReadOnlyGenerationAcrossFamilies(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	var calls atomic.Int32
	runner := scannerDownloader(t, &calls)
	for _, family := range []string{"frontend", "backend", "application-change", "dependency-change"} {
		cache := filepath.Join(root, family)
		if err := os.MkdirAll(filepath.Join(cache, "fanal"), 0700); err != nil {
			t.Fatal(err)
		}
		analysis := filepath.Join(cache, "fanal", "fanal.db")
		if err := os.WriteFile(analysis, []byte(family), 0600); err != nil {
			t.Fatal(err)
		}
		if err := PrepareScanner(context.Background(), runner, cache, shared, true); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(analysis); err != nil || string(got) != family {
			t.Fatalf("family analysis changed: %q, %v", got, err)
		}
		for _, database := range scannerDatabases {
			if target, err := os.Readlink(filepath.Join(cache, database.directory)); err != nil || target != filepath.Join(shared, database.key()) {
				t.Fatalf("%s links to %q, %v instead of the published generation", database.directory, target, err)
			}
			info, err := os.Stat(filepath.Join(cache, database.directory, database.filename))
			if err != nil || info.Mode().Perm() != 0o444 {
				t.Fatalf("published %s is writable or missing: %v, %v", database.filename, info, err)
			}
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("got %d downloads, want one per database", calls.Load())
	}
	for _, directory := range []string{shared} {
		if info, err := os.Stat(directory); err != nil || info.Mode().Perm() != 0o755 {
			t.Fatalf("%s is not readable by other accounts: %v, %v", directory, info, err)
		}
	}
}

func TestScannerPrepareOnlyReadsASharedDirectoryOfAnotherAccount(t *testing.T) {
	root := t.TempDir()
	fresh := filepath.Join(root, "fresh")
	stale := filepath.Join(root, "stale")
	generation := scannerGeneration(t, fresh, "db-v2-1", scannerDatabases[0], "fresh", time.Now().Add(-time.Minute), time.Now().Add(30*time.Minute))
	scannerGeneration(t, stale, "db-v2-1", scannerDatabases[0], "stale", time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
	for _, directory := range []string{fresh, stale} {
		err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
			if err != nil || !entry.IsDir() {
				return err
			}
			return os.Chmod(path, 0o555)
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
				if err == nil && entry.IsDir() {
					return os.Chmod(path, 0o755)
				}
				return nil
			})
		})
	}
	previous := scannerAccount
	scannerAccount = func() int { return os.Geteuid() + 1 }
	t.Cleanup(func() { scannerAccount = previous })
	runner := process.Runner{Execute: func(context.Context, process.Options) (process.Result, error) {
		t.Error("a shared directory of another account was refreshed")
		return process.Result{}, nil
	}}
	before := scannerTree(t, fresh)
	cache := filepath.Join(root, "family")
	if err := PrepareScanner(context.Background(), runner, cache, fresh, false); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(filepath.Join(cache, "db", "trivy.db")); err != nil || string(contents) != "fresh" {
		t.Fatalf("family reads %q, %v instead of %s", contents, err, generation)
	}
	if after := scannerTree(t, fresh); !maps.Equal(before, after) {
		t.Fatalf("preparation wrote the shared directory:\n%v\n%v", before, after)
	}
	if err := PrepareScanner(context.Background(), runner, cache, stale, false); err == nil || !strings.Contains(err.Error(), "missing or stale") {
		t.Fatalf("stale shared database accepted: %v", err)
	}
}

func TestScannerRefreshReplacesGenerationsWithoutDisturbingReaders(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	cache := filepath.Join(root, "family")
	scannerGeneration(t, shared, "db-v2-1", scannerDatabases[0], "stale", time.Now().Add(-72*time.Hour), time.Now().Add(-48*time.Hour))
	abandoned := filepath.Join(shared, "db-v1-1")
	recent := filepath.Join(shared, "db-v3-1")
	for _, directory := range []string{abandoned, recent} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(abandoned, old, old); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("registry unavailable")
	failing := process.Runner{Execute: func(context.Context, process.Options) (process.Result, error) { return process.Result{}, failure }}
	if err := PrepareScanner(context.Background(), failing, cache, shared, false); !errors.Is(err, failure) {
		t.Fatalf("failed refresh was accepted: %v", err)
	}
	if target, _ := os.Readlink(filepath.Join(shared, "db-v2")); target != "db-v2-1" {
		t.Fatalf("failed refresh replaced the published generation with %q", target)
	}
	reader, err := os.Open(filepath.Join(shared, "db-v2", "trivy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var calls atomic.Int32
	for range 3 {
		scannerGeneration(t, shared, fmt.Sprintf("db-v2-%d", time.Now().UnixNano()), scannerDatabases[0], "stale", time.Now().Add(-72*time.Hour), time.Now().Add(-48*time.Hour))
		if err := PrepareScanner(context.Background(), scannerDownloader(t, &calls), cache, shared, false); err != nil {
			t.Fatal(err)
		}
	}
	buffer := make([]byte, 32)
	if n, _ := reader.ReadAt(buffer, 0); string(buffer[:n]) != "stale" {
		t.Fatalf("active reader was mutated: %q", buffer[:n])
	}
	if contents, err := os.ReadFile(filepath.Join(cache, "db", "trivy.db")); err != nil || string(contents) != "trivy.db" {
		t.Fatalf("family does not read the new generation: %q, %v", contents, err)
	}
	generations, err := filepath.Glob(filepath.Join(shared, "db-v2-*"))
	if err != nil || len(generations) != 1 {
		t.Fatalf("superseded generations kept on disk: %v, %v", generations, err)
	}
	for path, kept := range map[string]bool{abandoned: false, recent: true} {
		if _, err := os.Stat(path); (err == nil) != kept {
			t.Errorf("%s kept=%t after refresh: %v", path, kept, err)
		}
	}
}

func TestScannerTrivyPinsOfOneSchemaShareTheirGeneration(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	var calls atomic.Int32
	runner := scannerDownloader(t, &calls)
	pinned := ToolAssets["trivy"]
	t.Cleanup(func() { ToolAssets["trivy"] = pinned })
	for index, digest := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("a", 64)} {
		ToolAssets["trivy"] = ToolAsset{URL: pinned.URL, Digest: digest, Member: pinned.Member}
		if err := PrepareScanner(context.Background(), runner, filepath.Join(root, fmt.Sprint(index)), shared, true); err != nil {
			t.Fatal(err)
		}
		for previous := range index + 1 {
			if _, err := os.Stat(filepath.Join(root, fmt.Sprint(previous), "db", "trivy.db")); err != nil {
				t.Fatalf("pin %d lost the database of pin %d: %v", index, previous, err)
			}
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("trivy pins of one schema downloaded %d times, want once per database", calls.Load())
	}
}

func TestScannerRefreshesAheadOfExpiryWhileReadersStillAccept(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	scannerGeneration(t, shared, "db-v2-1", scannerDatabases[0], "expiring", time.Now().Add(-23*time.Hour), time.Now().Add(30*time.Minute))
	previous := scannerAccount
	scannerAccount = func() int { return os.Geteuid() + 1 }
	reader := process.Runner{Execute: func(context.Context, process.Options) (process.Result, error) {
		t.Error("a reader refreshed the shared databases")
		return process.Result{}, nil
	}}
	if err := PrepareScanner(context.Background(), reader, filepath.Join(root, "reader"), shared, false); err != nil {
		t.Fatalf("a database 30 minutes from its next update was rejected: %v", err)
	}
	scannerAccount = previous
	var calls atomic.Int32
	if err := RefreshScanner(context.Background(), scannerDownloader(t, &calls), shared, false); err != nil || calls.Load() != 1 {
		t.Fatalf("refresh within the margin of the next update downloaded %d times: %v", calls.Load(), err)
	}
}

func TestScannerConcurrentFamiliesDownloadOnce(t *testing.T) {
	root := t.TempDir()
	var calls atomic.Int32
	runner := scannerDownloader(t, &calls)
	var group sync.WaitGroup
	for _, family := range []string{"frontend", "backend", "parser", "worker"} {
		group.Go(func() {
			if err := PrepareScanner(context.Background(), runner, filepath.Join(root, family), filepath.Join(root, "shared"), false); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent downloads: %d", calls.Load())
	}
}

func TestScannerFailureAndLockCancellation(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "family")
	failure := errors.New("vulnerabilities found")
	runner := process.Runner{Execute: func(context.Context, process.Options) (process.Result, error) { return process.Result{}, failure }}
	if err := RunScanner(context.Background(), runner, cache, []string{"image", "test"}); !errors.Is(err, failure) {
		t.Fatalf("scan failure lost: %v", err)
	}
	lock, err := scannerLock(context.Background(), cache)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := RunScanner(ctx, runner, cache, []string{"image", "test"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock cancellation lost: %v", err)
	}
}

func TestScannerRejectsInvalidDownloadAndUnsafeCache(t *testing.T) {
	for _, scenario := range []string{"empty", "stale", "future", "schema", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			runner := process.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				if scenario == "empty" {
					return process.Result{}, nil
				}
				downloaded, next := time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour)
				database := scannerDatabases[0]
				if scenario == "future" {
					downloaded = time.Now().Add(time.Hour)
				}
				if scenario == "schema" {
					database.schema = 99
				}
				scannerFixture(t, filepath.Join(options.Args[2], "db"), database, "invalid", downloaded, next)
				return process.Result{}, nil
			}}
			cache := filepath.Join(root, "family")
			if scenario == "symlink" {
				if err := os.Symlink(t.TempDir(), cache); err != nil {
					t.Fatal(err)
				}
			}
			if err := PrepareScanner(context.Background(), runner, cache, filepath.Join(root, "shared"), false); err == nil {
				t.Fatal("invalid database or cache accepted")
			}
		})
	}
}

func BenchmarkScannerWarmPreparation(b *testing.B) {
	root := b.TempDir()
	cache, shared := filepath.Join(root, "family"), filepath.Join(root, "shared")
	var calls atomic.Int32
	if err := RefreshScanner(context.Background(), scannerDownloader(b, &calls), shared, false); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if err := PrepareScanner(context.Background(), process.Runner{}, cache, shared, false); err != nil {
			b.Fatal(err)
		}
	}
}

func TestScannerRefreshAheadOfExpiryDownloadsOncePerGrace(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	var calls atomic.Int32
	runner := process.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		calls.Add(1)
		scannerFixture(t, filepath.Join(options.Args[2], "db"), scannerDatabases[0], "published", time.Now().Add(-time.Minute), time.Now().Add(30*time.Minute))
		return process.Result{}, nil
	}}
	for range 5 {
		if err := RefreshScanner(context.Background(), runner, shared, false); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("an upstream database already inside the refresh margin was downloaded %d times", calls.Load())
	}
}
