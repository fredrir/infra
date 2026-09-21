package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type scannerDatabase struct {
	directory string
	filename  string
	flag      string
	schema    int
	grace     time.Duration
}

var scannerDatabases = []scannerDatabase{
	{"db", "trivy.db", "--download-db-only", 2, time.Hour},
	{"java-db", "trivy-java.db", "--download-java-db-only", 1, 24 * time.Hour},
}

func PrepareScanner(ctx context.Context, runner Runner, cache, shared string, java bool, leases ...ScannerLease) error {
	if cache == "" || shared == "" {
		return fmt.Errorf("scanner cache and shared directory are required")
	}
	if len(leases) > 1 {
		return fmt.Errorf("only one scanner lease is allowed")
	}
	if len(leases) == 1 {
		if !scannerLeaseID.MatchString(leases[0].ID) || !leases[0].ExpiresAt.After(time.Now()) || leases[0].ExpiresAt.After(time.Now().Add(7*24*time.Hour)) {
			return fmt.Errorf("invalid scanner lease ID or expiration")
		}
		if err := PruneScanner(ctx, filepath.Dir(cache), time.Now()); err != nil {
			return err
		}
	}
	shared = filepath.Join(shared, ToolAssets["trivy"].Digest)
	family, err := scannerLock(ctx, cache)
	if err != nil {
		return err
	}
	defer family.Close()
	if len(leases) == 1 {
		if err := writeScannerLease(cache, leases[0]); err != nil {
			return err
		}
	} else if err := os.Remove(filepath.Join(cache, scannerLeaseFile)); err != nil && !os.IsNotExist(err) {
		return err
	}
	global, err := scannerLock(ctx, shared)
	if err != nil {
		return err
	}
	defer global.Close()
	for index, database := range scannerDatabases {
		if index == 1 && !java {
			continue
		}
		current := filepath.Join(shared, database.directory)
		if !scannerDatabaseFresh(current, database, time.Now()) {
			stage, err := os.MkdirTemp(shared, ".download-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(stage)
			staged := filepath.Join(stage, database.directory)
			if scannerDatabaseFresh(filepath.Join(cache, database.directory), database, time.Now()) {
				err = linkScannerDatabase(filepath.Join(cache, database.directory), staged, database)
			} else {
				err = runner.Run(ctx, "trivy", "image", "--cache-dir", stage, database.flag, "--no-progress", "--timeout", "2m")
			}
			if err != nil {
				return fmt.Errorf("prepare scanner %s: %w", database.directory, err)
			}
			if !scannerDatabaseFresh(staged, database, time.Now()) {
				return fmt.Errorf("scanner %s metadata is invalid or stale", database.directory)
			}
			if err := replaceDirectory(staged, current); err != nil {
				return err
			}
		}
		local := filepath.Join(cache, database.directory)
		if sameScannerDatabase(current, local, database) {
			continue
		}
		stage, err := os.MkdirTemp(cache, ".database-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(stage)
		staged := filepath.Join(stage, database.directory)
		if err := linkScannerDatabase(current, staged, database); err != nil {
			return err
		}
		if err := replaceDirectory(staged, filepath.Join(cache, database.directory)); err != nil {
			return err
		}
	}
	return nil
}

func RunScanner(ctx context.Context, runner Runner, cache string, arguments []string) error {
	if cache == "" || len(arguments) == 0 {
		return fmt.Errorf("scanner cache and arguments are required")
	}
	lock, err := scannerLock(ctx, cache)
	if err != nil {
		return err
	}
	defer lock.Close()
	arguments = append(append([]string{"--cache-dir", cache}, arguments...), "--skip-db-update", "--skip-java-db-update")
	return runner.Run(ctx, "trivy", arguments...)
}

func scannerDatabaseFresh(directory string, database scannerDatabase, now time.Time) bool {
	var metadata struct {
		Version      int
		NextUpdate   time.Time
		DownloadedAt time.Time
	}
	info, err := os.Lstat(filepath.Join(directory, database.filename))
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return false
	}
	data, err := os.ReadFile(filepath.Join(directory, "metadata.json"))
	if err != nil || json.Unmarshal(data, &metadata) != nil || metadata.Version != database.schema || metadata.DownloadedAt.IsZero() || metadata.DownloadedAt.After(now) {
		return false
	}
	return now.Before(metadata.NextUpdate) || now.Before(metadata.DownloadedAt.Add(database.grace))
}

func sameScannerDatabase(source, destination string, database scannerDatabase) bool {
	for _, name := range []string{database.filename, "metadata.json"} {
		left, leftErr := os.Stat(filepath.Join(source, name))
		right, rightErr := os.Stat(filepath.Join(destination, name))
		if leftErr != nil || rightErr != nil || !os.SameFile(left, right) {
			return false
		}
	}
	return true
}

func linkScannerDatabase(source, destination string, database scannerDatabase) error {
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	for _, name := range []string{database.filename, "metadata.json"} {
		if err := os.Link(filepath.Join(source, name), filepath.Join(destination, name)); err != nil {
			return fmt.Errorf("link scanner database: %w", err)
		}
	}
	return nil
}

func scannerLock(ctx context.Context, directory string) (*os.File, error) {
	return scannerCacheLock(ctx, directory, true)
}

func scannerCacheLock(ctx context.Context, directory string, wait bool) (*os.File, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("scanner cache must be an owned private directory")
	}
	lock, err := os.OpenFile(filepath.Join(directory, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err = lock.Stat()
	if err != nil {
		lock.Close()
		return nil, err
	}
	stat, ok = info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		lock.Close()
		return nil, fmt.Errorf("scanner lock must be an owned private regular file")
	}
	for {
		if err := ctx.Err(); err != nil {
			lock.Close()
			return nil, err
		}
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			lock.Close()
			return nil, err
		}
		if !wait {
			lock.Close()
			return nil, errScannerBusy
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			lock.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
