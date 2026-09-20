package artifact

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

type SourcePruneOptions struct {
	CacheDir string
	Keep     int
	MaxAge   time.Duration
	Now      time.Time
}

func PruneSource(ctx context.Context, o SourcePruneOptions) (PruneResult, error) {
	var result PruneResult
	if o.CacheDir == "" || o.Keep < 1 || o.MaxAge <= 0 {
		return result, errors.New("source cache directory and positive retention limits required")
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if _, err := os.Lstat(o.CacheDir); os.IsNotExist(err) {
		return result, nil
	} else if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	lock, err := lockCache(ctx, o.CacheDir)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	root, err := os.OpenRoot(o.CacheDir)
	if err != nil {
		return result, err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return result, err
	}
	type candidate struct {
		name  string
		used  time.Time
		bytes int64
		files []string
	}
	var candidates []candidate
	digest := regexp.MustCompile(`^[a-f0-9]{64}$`)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if !entry.IsDir() || !digest.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return result, err
		}
		item := candidate{name: entry.Name(), used: info.ModTime()}
		for _, name := range []string{"infra", "sha256", "inputs", "source-revision", "last-used"} {
			info, err := root.Lstat(filepath.Join(entry.Name(), name))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return result, err
			}
			if !info.Mode().IsRegular() {
				return result, fmt.Errorf("source cache file must be regular: %s", name)
			}
			item.files = append(item.files, name)
			item.bytes += info.Size()
			if name == "last-used" {
				item.used = info.ModTime()
			}
		}
		if len(item.files) > 0 {
			candidates = append(candidates, item)
			result.RetainedBytes += item.bytes
		}
	}
	slices.SortFunc(candidates, func(a, b candidate) int {
		if a.used.Equal(b.used) {
			return strings.Compare(a.name, b.name)
		}
		return b.used.Compare(a.used)
	})
	for i, item := range candidates {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if i < o.Keep && o.Now.Sub(item.used) <= o.MaxAge {
			continue
		}
		for _, name := range item.files {
			if err := root.Remove(filepath.Join(item.name, name)); err != nil {
				return result, err
			}
		}
		if err := root.Remove(item.name); err != nil && !errors.Is(err, syscall.ENOTEMPTY) {
			return result, err
		}
		result.Removed++
		result.FreedBytes += item.bytes
		result.RetainedBytes -= item.bytes
	}
	return result, nil
}
