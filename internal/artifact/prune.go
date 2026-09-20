package artifact

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

type PruneOptions struct {
	CacheDir     string
	KeepRevision string
	MaxBytes     int64
	MaxAge       time.Duration
	Now          time.Time
}

type PruneResult struct {
	Removed       int   `json:"removed"`
	FreedBytes    int64 `json:"freed_bytes"`
	RetainedBytes int64 `json:"retained_bytes"`
}

func Prune(o PruneOptions) (PruneResult, error) {
	var result PruneResult
	if o.CacheDir == "" || o.MaxBytes <= 0 || o.MaxAge <= 0 {
		return result, fmt.Errorf("positive artifact cache limits required")
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if _, err := os.Lstat(o.CacheDir); os.IsNotExist(err) {
		return result, nil
	} else if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lock, err := lockCache(ctx, o.CacheDir)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	root, err := os.OpenRoot(o.CacheDir)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer root.Close()
	type entry struct {
		directory string
		size      int64
		used      time.Time
		keep      bool
	}
	var entries []entry
	pattern := regexp.MustCompile(`^[a-f0-9]{40}/(?:linux|darwin)-(?:amd64|arm64)/[a-f0-9]{64}/infra$`)
	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !pattern.MatchString(path) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("cache artifact is not regular")
		}
		used := info.ModTime()
		directory := filepath.Dir(path)
		if stamp, err := root.Stat(filepath.Join(directory, "last-used")); err == nil {
			used = stamp.ModTime()
		} else if !os.IsNotExist(err) {
			return err
		}
		entries = append(entries, entry{directory: directory, size: info.Size(), used: used, keep: strings.Split(path, "/")[0] == o.KeepRevision})
		result.RetainedBytes += info.Size()
		return nil
	})
	if err != nil {
		return result, err
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].used.Equal(entries[j].used) {
			return entries[i].directory < entries[j].directory
		}
		return entries[i].used.Before(entries[j].used)
	})
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if entry.keep || (o.Now.Sub(entry.used) <= o.MaxAge && result.RetainedBytes <= o.MaxBytes) {
			continue
		}
		if err := root.Remove(filepath.Join(entry.directory, "infra")); err != nil {
			return result, err
		}
		if err := root.Remove(filepath.Join(entry.directory, "last-used")); err != nil && !os.IsNotExist(err) {
			return result, err
		}
		if err := root.Remove(entry.directory); err != nil && !errors.Is(err, syscall.ENOTEMPTY) && !os.IsNotExist(err) {
			return result, err
		}
		result.Removed++
		result.FreedBytes += entry.size
		result.RetainedBytes -= entry.size
	}
	return result, nil
}
