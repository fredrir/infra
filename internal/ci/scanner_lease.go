package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

type ScannerLease struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
}

var scannerLeaseID = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
var errScannerBusy = errors.New("scanner cache is in use")

const scannerLeaseFile = ".database-lease.json"

func writeScannerLease(cache string, lease ScannerLease) error {
	data, err := json.Marshal(lease)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(cache, ".lease-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(cache, scannerLeaseFile))
}

func readScannerLease(cache string) (ScannerLease, error) {
	path := filepath.Join(cache, scannerLeaseFile)
	info, err := os.Lstat(path)
	if err != nil {
		return ScannerLease{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 4096 || !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return ScannerLease{}, fmt.Errorf("scanner lease must be an owned private regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ScannerLease{}, err
	}
	var lease ScannerLease
	if err := json.Unmarshal(data, &lease); err != nil {
		return ScannerLease{}, err
	}
	if !scannerLeaseID.MatchString(lease.ID) || lease.ExpiresAt.IsZero() {
		return ScannerLease{}, fmt.Errorf("invalid scanner lease")
	}
	return lease, nil
}

func ReleaseScanner(ctx context.Context, cache, id string) error {
	if cache == "" || !scannerLeaseID.MatchString(id) {
		return fmt.Errorf("scanner cache and lease ID are required")
	}
	lock, err := scannerLock(ctx, cache)
	if err != nil {
		return err
	}
	defer lock.Close()
	lease, err := readScannerLease(cache)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if lease.ID != id {
		return nil
	}
	return releaseScannerDatabases(cache)
}

func PruneScanner(ctx context.Context, root string, now time.Time) error {
	if root == "" {
		return fmt.Errorf("scanner cache root is required")
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() {
			continue
		}
		cache := filepath.Join(root, entry.Name())
		lease, err := readScannerLease(cache)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if now.Before(lease.ExpiresAt) {
			continue
		}
		lock, err := scannerCacheLock(ctx, cache, false)
		if errors.Is(err, errScannerBusy) {
			continue
		}
		if err != nil {
			return err
		}
		err = pruneScannerLease(cache, now)
		closeErr := lock.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return err
		}
	}
	return nil
}

func pruneScannerLease(cache string, now time.Time) error {
	lease, err := readScannerLease(cache)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if now.Before(lease.ExpiresAt) {
		return nil
	}
	return releaseScannerDatabases(cache)
}

func releaseScannerDatabases(cache string) error {
	for _, database := range scannerDatabases {
		if err := os.RemoveAll(filepath.Join(cache, database.directory)); err != nil {
			return err
		}
	}
	return os.Remove(filepath.Join(cache, scannerLeaseFile))
}
