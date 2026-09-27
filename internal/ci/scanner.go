package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fredrir/infra/internal/process"
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

var scannerAccount = os.Geteuid

const (
	scannerRefreshMargin = 2 * time.Hour
	scannerAbandonedAge  = 7 * 24 * time.Hour
)

func (database scannerDatabase) key() string {
	return fmt.Sprintf("%s-v%d", database.directory, database.schema)
}

func RefreshScanner(ctx context.Context, runner process.Runner, shared string, java bool) error {
	if shared == "" {
		return fmt.Errorf("shared scanner directory is required")
	}
	lock, err := scannerCacheLock(ctx, shared, 0o755)
	if err != nil {
		return err
	}
	defer lock.Close()
	now := time.Now()
	current := map[string]bool{".lock": true}
	for index, database := range scannerDatabases {
		published := filepath.Join(shared, database.key())
		current[database.key()] = true
		if generation, err := os.Readlink(published); err == nil {
			current[generation] = true
		}
		if index == 1 && !java || !scannerDatabaseDue(published, database, now) {
			continue
		}
		generation, err := publishScannerDatabase(ctx, runner, shared, database)
		if err != nil {
			return fmt.Errorf("prepare scanner %s: %w", database.directory, err)
		}
		current[generation] = true
	}
	entries, err := os.ReadDir(shared)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !current[entry.Name()] && now.Sub(info.ModTime()) > scannerAbandonedAge {
			if err := os.RemoveAll(filepath.Join(shared, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func publishScannerDatabase(ctx context.Context, runner process.Runner, shared string, database scannerDatabase) (string, error) {
	stage, err := os.MkdirTemp(shared, ".download-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if err := runner.Run(ctx, "trivy", "image", "--cache-dir", stage, database.flag, "--no-progress", "--timeout", "2m"); err != nil {
		return "", err
	}
	staged := filepath.Join(stage, database.directory)
	if !scannerDatabaseFresh(staged, database, time.Now()) {
		return "", fmt.Errorf("metadata is invalid or stale")
	}
	for _, name := range []string{database.filename, "metadata.json"} {
		if err := os.Chmod(filepath.Join(staged, name), 0o444); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		return "", err
	}
	generation := fmt.Sprintf("%s-%d", database.key(), time.Now().UnixNano())
	if err := os.Rename(staged, filepath.Join(shared, generation)); err != nil {
		return "", err
	}
	if err := replaceSymlink(generation, filepath.Join(shared, database.key())); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(shared)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if name := entry.Name(); strings.HasPrefix(name, database.key()+"-") && name != generation {
			if err := os.RemoveAll(filepath.Join(shared, name)); err != nil {
				return "", err
			}
		}
	}
	return generation, nil
}

func PrepareScanner(ctx context.Context, runner process.Runner, cache, shared string, java bool) error {
	if cache == "" || shared == "" {
		return fmt.Errorf("scanner cache and shared directory are required")
	}
	owned, err := ownedScannerDirectory(shared)
	if err != nil {
		return err
	}
	if owned {
		if err := RefreshScanner(ctx, runner, shared, java); err != nil {
			return err
		}
	}
	family, err := scannerLock(ctx, cache)
	if err != nil {
		return err
	}
	defer family.Close()
	for index, database := range scannerDatabases {
		if index == 1 && !java {
			continue
		}
		current := filepath.Join(shared, database.key())
		if !scannerDatabaseFresh(current, database, time.Now()) {
			return fmt.Errorf("scanner %s in %s is missing or stale", database.directory, shared)
		}
		link := filepath.Join(cache, database.directory)
		if target, err := os.Readlink(link); err == nil && target == current {
			continue
		}
		if err := replaceSymlink(current, link); err != nil {
			return err
		}
	}
	return nil
}

func ownedScannerDirectory(directory string) (bool, error) {
	info, err := os.Lstat(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok {
		return false, fmt.Errorf("shared scanner directory %s is not a directory", directory)
	}
	return stat.Uid == uint32(scannerAccount()), nil
}

func replaceSymlink(target, link string) error {
	temporary := fmt.Sprintf("%s.%d.link", link, time.Now().UnixNano())
	if err := os.Symlink(target, temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, link); err != nil {
		os.Remove(temporary)
		return err
	}
	return nil
}

func RunScanner(ctx context.Context, runner process.Runner, cache string, arguments []string) error {
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

type scannerMetadata struct {
	Version      int
	NextUpdate   time.Time
	DownloadedAt time.Time
}

func readScannerMetadata(directory string, database scannerDatabase) (scannerMetadata, bool) {
	var metadata scannerMetadata
	info, err := os.Lstat(filepath.Join(directory, database.filename))
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return metadata, false
	}
	data, err := os.ReadFile(filepath.Join(directory, "metadata.json"))
	if err != nil || json.Unmarshal(data, &metadata) != nil || metadata.Version != database.schema || metadata.DownloadedAt.IsZero() {
		return metadata, false
	}
	return metadata, true
}

func scannerDatabaseFresh(directory string, database scannerDatabase, now time.Time) bool {
	metadata, ok := readScannerMetadata(directory, database)
	return ok && !metadata.DownloadedAt.After(now) && (now.Before(metadata.NextUpdate) || now.Before(metadata.DownloadedAt.Add(database.grace)))
}

func scannerDatabaseDue(directory string, database scannerDatabase, now time.Time) bool {
	metadata, ok := readScannerMetadata(directory, database)
	return !ok || metadata.DownloadedAt.After(now) || !now.Before(metadata.DownloadedAt.Add(database.grace)) && !now.Add(scannerRefreshMargin).Before(metadata.NextUpdate)
}

func scannerLock(ctx context.Context, directory string) (*os.File, error) {
	return scannerCacheLock(ctx, directory, 0o700)
}

func ownedDirectory(directory string, mode fs.FileMode) error {
	if err := os.MkdirAll(directory, mode); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("scanner directory %s must be an owned directory", directory)
	}
	if info.Mode().Perm() == mode {
		return nil
	}
	return os.Chmod(directory, mode)
}

func scannerCacheLock(ctx context.Context, directory string, mode fs.FileMode) (*os.File, error) {
	if err := ownedDirectory(directory, mode); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(directory, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := lock.Stat()
	if err != nil {
		lock.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
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
