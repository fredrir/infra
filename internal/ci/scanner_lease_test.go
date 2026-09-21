package ci

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/process"
)

func TestScannerLeaseReleasePreservesSharedDatabaseAndAnalysis(t *testing.T) {
	root := t.TempDir()
	cache, shared := filepath.Join(root, "trivy-v3", "frontend"), filepath.Join(root, "shared")
	var calls atomic.Int32
	lease := ScannerLease{ID: "100-1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := PrepareScanner(context.Background(), scannerDownloader(t, &calls), cache, shared, true, lease); err != nil {
		t.Fatal(err)
	}
	analysis := filepath.Join(cache, "fanal", "fanal.db")
	if err := os.Mkdir(filepath.Dir(analysis), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(analysis, []byte("retained analysis"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseScanner(context.Background(), cache, lease.ID); err != nil {
		t.Fatal(err)
	}
	for _, database := range scannerDatabases {
		if _, err := os.Stat(filepath.Join(cache, database.directory)); !os.IsNotExist(err) {
			t.Fatalf("family database retained: %v", err)
		}
		if _, err := os.Stat(filepath.Join(shared, ToolAssets["trivy"].Digest, database.directory, database.filename)); err != nil {
			t.Fatalf("shared database lost: %v", err)
		}
	}
	if content, err := os.ReadFile(analysis); err != nil || string(content) != "retained analysis" {
		t.Fatalf("analysis changed: %q %v", content, err)
	}
	if err := PrepareScanner(context.Background(), scannerDownloader(t, &calls), cache, shared, true, ScannerLease{ID: "101-1", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("released database was downloaded again: %d", calls.Load())
	}
}

func TestCancelledAttemptCannotReleaseNewerScannerLease(t *testing.T) {
	root := t.TempDir()
	cache, shared := filepath.Join(root, "trivy-v3", "frontend"), filepath.Join(root, "shared")
	var calls atomic.Int32
	for _, id := range []string{"100-1", "101-1"} {
		if err := PrepareScanner(context.Background(), scannerDownloader(t, &calls), cache, shared, false, ScannerLease{ID: id, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ReleaseScanner(context.Background(), cache, "100-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, "db", "trivy.db")); err != nil {
		t.Fatalf("cancelled old attempt removed new database: %v", err)
	}
	lease, err := readScannerLease(cache)
	if err != nil || lease.ID != "101-1" {
		t.Fatalf("new lease changed: %+v %v", lease, err)
	}
}

func TestScannerExpirySkipsActiveScanAndCollectsAfterCancellation(t *testing.T) {
	root := t.TempDir()
	cache, shared := filepath.Join(root, "trivy-v3", "frontend"), filepath.Join(root, "shared")
	var calls atomic.Int32
	lease := ScannerLease{ID: "100-1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := PrepareScanner(context.Background(), scannerDownloader(t, &calls), cache, shared, false, lease); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	finished := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := Runner{Execute: func(ctx context.Context, _ process.Options) (process.Result, error) {
		close(entered)
		<-ctx.Done()
		return process.Result{}, ctx.Err()
	}}
	go func() { finished <- RunScanner(ctx, runner, cache, []string{"image", "example"}) }()
	<-entered
	pruneContext, stopPrune := context.WithTimeout(context.Background(), time.Second)
	defer stopPrune()
	if err := PruneScanner(pruneContext, filepath.Dir(cache), lease.ExpiresAt.Add(time.Second)); err != nil {
		t.Fatalf("expiry sweep waited for active scanner: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "db", "trivy.db")); err != nil {
		t.Fatalf("active scan database removed: %v", err)
	}
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("scan cancellation was lost: %v", err)
	}
	if err := PruneScanner(context.Background(), filepath.Dir(cache), lease.ExpiresAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, "db")); !os.IsNotExist(err) {
		t.Fatalf("expired database retained after cancellation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shared, ToolAssets["trivy"].Digest, "db", "trivy.db")); err != nil {
		t.Fatal(err)
	}
}

func TestScannerPruningPreservesNewLeasesAndOlderWorkflowNamespace(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	var calls atomic.Int32
	legacy := filepath.Join(root, "trivy-v2", "frontend")
	if err := PrepareScanner(context.Background(), scannerDownloader(t, &calls), legacy, shared, false); err != nil {
		t.Fatal(err)
	}
	if err := writeScannerLease(legacy, ScannerLease{ID: "legacy", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(root, "trivy-v3", "frontend")
	if err := PrepareScanner(context.Background(), scannerDownloader(t, &calls), current, shared, false, ScannerLease{ID: "new", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := PruneScanner(context.Background(), filepath.Dir(current), time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, cache := range []string{legacy, current} {
		if _, err := os.Stat(filepath.Join(cache, "db", "trivy.db")); err != nil {
			t.Fatalf("workflow database removed: %s %v", cache, err)
		}
	}
	if err := PruneScanner(context.Background(), filepath.Dir(current), time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "db", "trivy.db")); err != nil {
		t.Fatalf("older CLI namespace changed: %v", err)
	}
}

func TestScannerFailedPreparationCanReleaseLease(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "trivy-v3", "frontend")
	failure := errors.New("download unavailable")
	runner := Runner{Execute: func(context.Context, process.Options) (process.Result, error) { return process.Result{}, failure }}
	if err := PrepareScanner(context.Background(), runner, cache, filepath.Join(root, "shared"), false, ScannerLease{ID: "100-1", ExpiresAt: time.Now().Add(time.Hour)}); !errors.Is(err, failure) {
		t.Fatalf("download failure lost: %v", err)
	}
	if err := ReleaseScanner(context.Background(), cache, "100-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, scannerLeaseFile)); !os.IsNotExist(err) {
		t.Fatalf("failed preparation retained lease: %v", err)
	}
}

func TestScannerLeaseRejectsInvalidExpirationAndSymlinks(t *testing.T) {
	for _, lease := range []ScannerLease{{ID: "../escape", ExpiresAt: time.Now().Add(time.Hour)}, {ID: "old", ExpiresAt: time.Now().Add(-time.Hour)}, {ID: "forever", ExpiresAt: time.Now().Add(8 * 24 * time.Hour)}} {
		if err := PrepareScanner(context.Background(), Runner{}, filepath.Join(t.TempDir(), "cache"), filepath.Join(t.TempDir(), "shared"), false, lease); err == nil {
			t.Fatalf("invalid lease accepted: %+v", lease)
		}
	}
	cache := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte(`{"id":"100-1","expires_at":"2026-01-01T00:00:00Z"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cache, scannerLeaseFile)); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseScanner(context.Background(), cache, "100-1"); err == nil {
		t.Fatal("symlinked lease accepted")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("external lease changed")
	}
}
