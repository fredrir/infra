package ci

import (
	"context"
	"encoding/json"
	"errors"
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

func scannerFixture(t *testing.T, directory string, database scannerDatabase, contents string, downloaded, next time.Time) {
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

func scannerDownloader(t *testing.T, calls *atomic.Int32) Runner {
	t.Helper()
	return Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		calls.Add(1)
		if options.Name != "trivy" || len(options.Args) < 5 || options.Args[1] != "--cache-dir" {
			t.Errorf("unexpected scanner command: %s %q", options.Name, options.Args)
		}
		for _, database := range scannerDatabases {
			if slices.Contains(options.Args, database.flag) {
				scannerFixture(t, filepath.Join(options.Args[2], database.directory), database, database.filename, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
			}
		}
		return process.Result{}, nil
	}}
}

func TestScannerSharesDatabasesAndRetainsFamilyAnalysis(t *testing.T) {
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
			local, err := os.Stat(filepath.Join(cache, database.directory, database.filename))
			if err != nil {
				t.Fatal(err)
			}
			central, err := os.Stat(filepath.Join(shared, ToolAssets["trivy"].Digest, database.directory, database.filename))
			if err != nil || !os.SameFile(local, central) {
				t.Fatalf("database is duplicated: %v", err)
			}
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("got %d downloads, want one per database", calls.Load())
	}
}

func TestScannerSeedsFreshCacheWithoutDownload(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "family")
	scannerFixture(t, filepath.Join(cache, "db"), scannerDatabases[0], "database", time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
	runner := Runner{Execute: func(context.Context, process.Options) (process.Result, error) {
		t.Error("fresh database should not download")
		return process.Result{}, nil
	}}
	for range 2 {
		if err := PrepareScanner(context.Background(), runner, cache, filepath.Join(root, "shared"), false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScannerRefreshPreservesExistingReadersAndRejectsFailedRefresh(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	cache := filepath.Join(root, "family")
	current := filepath.Join(shared, ToolAssets["trivy"].Digest, "db")
	scannerFixture(t, current, scannerDatabases[0], "old generation", time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
	if err := os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := linkScannerDatabase(current, filepath.Join(cache, "db"), scannerDatabases[0]); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(filepath.Join(cache, "db", "trivy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	failure := errors.New("registry unavailable")
	runner := Runner{Execute: func(context.Context, process.Options) (process.Result, error) { return process.Result{}, failure }}
	if err := PrepareScanner(context.Background(), runner, cache, shared, false); !errors.Is(err, failure) {
		t.Fatalf("failed refresh was accepted: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(current, "trivy.db")); string(got) != "old generation" {
		t.Fatalf("failed refresh changed database: %q", got)
	}
	var calls atomic.Int32
	if err := PrepareScanner(context.Background(), scannerDownloader(t, &calls), cache, shared, false); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 32)
	n, err := reader.ReadAt(buffer, 0)
	if string(buffer[:n]) != "old generation" {
		t.Fatalf("active reader was mutated: %q, %v", buffer[:n], err)
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
	runner := Runner{Execute: func(context.Context, process.Options) (process.Result, error) { return process.Result{}, failure }}
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
			runner := Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
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
	directory := filepath.Join(shared, ToolAssets["trivy"].Digest, "db")
	if err := os.MkdirAll(directory, 0700); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "trivy.db"), []byte(strings.Repeat("db", 1024)), 0600); err != nil {
		b.Fatal(err)
	}
	metadata, _ := json.Marshal(map[string]any{"Version": 2, "DownloadedAt": time.Now().Add(-time.Minute), "NextUpdate": time.Now().Add(time.Hour)})
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), metadata, 0600); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if err := PrepareScanner(context.Background(), Runner{}, cache, shared, false); err != nil {
			b.Fatal(err)
		}
	}
}
