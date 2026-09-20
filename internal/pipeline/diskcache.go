package pipeline

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"dagger.io/dagger"
)

const diskCachePath = "/root/.cache/infra-bazel-actions"
const diskCacheLimit int64 = 8 << 30

func pruneDiskCache(ctx context.Context, container *dagger.Container) error {
	collector := container.WithEnvVariable("INFRA_CACHE_SCAN", strconv.FormatInt(time.Now().UnixNano(), 10))
	output, err := collector.WithExec([]string{"find", diskCachePath, "-path", diskCachePath + "/tmp", "-prune", "-o", "-path", diskCachePath + "/gc", "-prune", "-o", "-type", "f", "-printf", "%T@ %s %p\\0"}).Stdout(ctx)
	if err != nil {
		return fmt.Errorf("scan Bazel action cache: %w", err)
	}
	candidates, err := diskCacheCandidates(output, time.Now(), diskCacheLimit, 7*24*time.Hour)
	if err != nil {
		return err
	}
	for len(candidates) > 0 {
		count := min(len(candidates), 128)
		args := append([]string{"find"}, candidates[:count]...)
		args = append(args, "-maxdepth", "0", "-type", "f", "-mmin", "+30", "-delete")
		if _, err := collector.WithExec(args).Sync(ctx); err != nil {
			return fmt.Errorf("prune Bazel action cache: %w", err)
		}
		candidates = candidates[count:]
	}
	return nil
}

func diskCacheCandidates(output string, now time.Time, maximum int64, age time.Duration) ([]string, error) {
	type entry struct {
		name     string
		size     int64
		modified time.Time
	}
	var entries []entry
	var total int64
	for _, record := range strings.Split(output, "\x00") {
		if record == "" {
			continue
		}
		fields := strings.SplitN(record, " ", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("invalid action cache metadata")
		}
		seconds, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return nil, err
		}
		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || size < 0 || size > 1<<50 {
			return nil, fmt.Errorf("invalid action cache entry size")
		}
		if path.Clean(fields[2]) != fields[2] || !strings.HasPrefix(fields[2], diskCachePath+"/") {
			return nil, fmt.Errorf("action cache entry escaped cache root")
		}
		if total > 1<<60-size {
			return nil, fmt.Errorf("action cache size overflow")
		}
		total += size
		entries = append(entries, entry{fields[2], size, time.Unix(0, int64(seconds*1e9))})
	}
	slices.SortFunc(entries, func(left, right entry) int { return left.modified.Compare(right.modified) })
	var candidates []string
	for _, entry := range entries {
		if now.Sub(entry.modified) <= 30*time.Minute {
			continue
		}
		if total <= maximum && now.Sub(entry.modified) <= age {
			continue
		}
		candidates = append(candidates, entry.name)
		total -= entry.size
	}
	return candidates, nil
}
