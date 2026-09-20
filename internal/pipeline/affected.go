package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

func AffectedExpression(ctx context.Context, root, base string) (string, error) {
	if base == "" {
		return "//...", nil
	}
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "-z", base, "--")
	cmd.Dir = root
	data, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read changed paths: %w", err)
	}
	cmd = exec.CommandContext(ctx, "git", "ls-files", "--others", "--exclude-standard", "-z")
	cmd.Dir = root
	extra, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read untracked paths: %w", err)
	}
	return ExpressionForPaths(root, strings.Split(string(append(data, extra...)), "\x00")), nil
}

func ExpressionForPaths(root string, paths []string) string {
	var labels []string
	valid := regexp.MustCompile(`^[a-zA-Z0-9_./-]+$`)
	for _, path := range paths {
		if path == "" {
			continue
		}
		if !valid.MatchString(path) || !filepath.IsLocal(path) {
			return "//..."
		}
		if !strings.HasPrefix(path, "internal/") && !strings.HasPrefix(path, "cmd/") && !strings.HasPrefix(path, "integration/") {
			return "//..."
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		for dir != "." {
			if _, err := os.Stat(filepath.Join(root, dir, "BUILD.bazel")); err == nil {
				break
			}
			dir = filepath.ToSlash(filepath.Dir(dir))
		}
		if dir == "." {
			return "//..."
		}
		labels = append(labels, "//"+dir+":all")
	}
	slices.Sort(labels)
	labels = slices.Compact(labels)
	if len(labels) == 0 {
		return "set()"
	}
	return "rdeps(//..., set(" + strings.Join(labels, " ") + "))"
}
