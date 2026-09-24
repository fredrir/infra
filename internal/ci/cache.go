package ci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/fredrir/infra/internal/objectstore"
)

func CacheClient() objectstore.Client {
	return objectstore.Client{Endpoint: os.Getenv("SCCACHE_ENDPOINT"), Region: environmentDefault("SCCACHE_REGION", "garage"), AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"), SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY")}
}

func RustCache(ctx context.Context, runner Runner, temporary, action string) error {
	if action != "restore" && action != "save" {
		return fmt.Errorf("cache action must be restore or save")
	}
	client := CacheClient()
	bucket := os.Getenv("SCCACHE_BUCKET")
	if client.Endpoint == "" || bucket == "" || client.AccessKey == "" || client.SecretKey == "" {
		_, err := fmt.Fprintln(runner.Stdout, "Build output cache is not configured")
		return err
	}
	if !regexp.MustCompile(`^ci-[a-z][a-z0-9-]{0,29}-(main|release)$`).MatchString(bucket) {
		return fmt.Errorf("invalid build cache bucket")
	}
	if temporary == "" {
		return fmt.Errorf("runner temporary directory is required")
	}
	toolchain, err := runner.Output(ctx, "rustc", "-vV")
	if err != nil {
		return err
	}
	toolchainHash := sha256.Sum256(toolchain)
	object := "target/" + hex.EncodeToString(toolchainHash[:8])
	limit, err := strconv.ParseInt(environmentDefault("RUST_TARGET_CACHE_LIMIT_KIB", "8388608"), 10, 64)
	if err != nil || limit < 1 || limit > 1<<40 {
		return fmt.Errorf("invalid build cache size limit")
	}
	limit *= 1024
	work, err := os.MkdirTemp(temporary, "target-cache-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	if action == "restore" {
		archive, err := os.Create(filepath.Join(work, "target.tar.zst"))
		if err != nil {
			return err
		}
		err = client.Download(ctx, bucket, object+".tar.zst", &limitedWriter{Writer: archive, remaining: limit})
		closeErr := archive.Close()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			_, err := fmt.Fprintln(runner.Stdout, "No cached build outputs")
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		archive, err = os.Open(filepath.Join(work, "target.tar.zst"))
		if err != nil {
			return err
		}
		err = ExtractZstd(archive, work, "target", limit)
		archive.Close()
		if err != nil {
			_, err := fmt.Fprintln(runner.Stderr, "::warning::Cached build outputs are unreadable")
			return err
		}
		if err := replaceDirectory(filepath.Join(work, "target"), filepath.Join(runner.Dir, "target")); err != nil {
			return err
		}
		_, err = fmt.Fprintln(runner.Stdout, "Restored build outputs")
		return err
	}
	if environmentDefault("SCCACHE_S3_RW_MODE", "READ_WRITE") != "READ_WRITE" {
		_, err := fmt.Fprintln(runner.Stdout, "Build output cache is read-only")
		return err
	}
	if _, err := os.Stat(filepath.Join(runner.Dir, "target")); os.IsNotExist(err) {
		_, err := fmt.Fprintln(runner.Stdout, "No build outputs")
		return err
	} else if err != nil {
		return err
	}
	fingerprint := sha256.New()
	files, err := runner.Output(ctx, "git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return fmt.Errorf("identify Rust source inputs: %w", err)
	}
	paths := strings.Split(strings.TrimSuffix(string(files), "\x00"), "\x00")
	slices.Sort(paths)
	for _, path := range slices.Compact(paths) {
		if path == "" || path == "target" || strings.HasPrefix(path, "target/") || strings.HasPrefix(path, ".infra-build-recipe/") {
			continue
		}
		if !filepath.IsLocal(path) {
			return fmt.Errorf("invalid Rust source path")
		}
		data, err := os.ReadFile(filepath.Join(runner.Dir, path))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		fmt.Fprintf(fingerprint, "%s\x00%x\x00", path, digest)
	}
	for _, path := range []string{filepath.Join(runner.Dir, "Cargo.lock"), filepath.Join(temporary, "rust-args.json")} {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(fingerprint, "%x\x00", sha256.Sum256(data))
	}
	identity := hex.EncodeToString(fingerprint.Sum(nil))
	var previous bytes.Buffer
	if err := client.Download(ctx, bucket, object+".fingerprint", &limitedWriter{Writer: &previous, remaining: 1024}); err == nil && previous.String() == identity {
		_, err := fmt.Fprintln(runner.Stdout, "Cached build outputs are current")
		return err
	}
	var size int64
	err = filepath.WalkDir(filepath.Join(runner.Dir, "target"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if size > limit {
		client.Delete(ctx, bucket, object+".tar.zst")
		client.Delete(ctx, bucket, object+".fingerprint")
		_, err := fmt.Fprintln(runner.Stdout, "Build outputs outgrew the cache; the next run repopulates it")
		return err
	}
	archive, err := os.Create(filepath.Join(work, "target.tar.zst"))
	if err != nil {
		return err
	}
	err = WriteZstd(archive, runner.Dir, "target")
	closeErr := archive.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	archive, err = os.Open(filepath.Join(work, "target.tar.zst"))
	if err != nil {
		return err
	}
	defer archive.Close()
	if err := client.Upload(ctx, bucket, object+".tar.zst", archive); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, err := fmt.Fprintln(runner.Stderr, "::warning::Build outputs were not cached")
		return err
	}
	if err := client.Upload(ctx, bucket, object+".fingerprint", strings.NewReader(identity)); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, err := fmt.Fprintln(runner.Stderr, "::warning::Build output fingerprint was not cached")
		return err
	}
	_, err = fmt.Fprintln(runner.Stdout, "Cached build outputs")
	return err
}

func FetchSDK(ctx context.Context, temporary string, output io.Writer) error {
	object, digest := os.Getenv("MACOS_SDK_OBJECT"), os.Getenv("MACOS_SDK_SHA256")
	if temporary == "" || !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(object) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(digest) {
		return fmt.Errorf("invalid SDK object, digest or temporary directory")
	}
	work, err := os.MkdirTemp(temporary, "sdk-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	archive, err := os.Create(filepath.Join(work, "sdk.tar.zst"))
	if err != nil {
		return err
	}
	hash := sha256.New()
	err = CacheClient().Download(ctx, "toolchains", object, &limitedWriter{Writer: io.MultiWriter(archive, hash), remaining: 16 << 30})
	closeErr := archive.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return fmt.Errorf("macOS SDK checksum mismatch")
	}
	archive, err = os.Open(filepath.Join(work, "sdk.tar.zst"))
	if err != nil {
		return err
	}
	defer archive.Close()
	extracted := filepath.Join(work, "extracted")
	if err := os.Mkdir(extracted, 0755); err != nil {
		return err
	}
	if err := ExtractZstd(archive, extracted, "MacOSX.sdk", 32<<30); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(extracted, "MacOSX.sdk/SDKSettings.json"))
	if err != nil {
		return err
	}
	var settings struct{ Version string }
	if err := json.Unmarshal(data, &settings); err != nil {
		return err
	}
	if err := replaceDirectory(extracted, filepath.Join(temporary, "macos-sdk")); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "macOS SDK %s ready\n", settings.Version)
	return err
}

type limitedWriter struct {
	io.Writer
	remaining int64
}

func (writer *limitedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > writer.remaining {
		return 0, fmt.Errorf("download exceeds size limit")
	}
	count, err := writer.Writer.Write(data)
	writer.remaining -= int64(count)
	return count, err
}

func replaceDirectory(source, destination string) error {
	if _, err := os.Stat(source); err != nil {
		return err
	}
	if _, err := os.Lstat(destination); os.IsNotExist(err) {
		return os.Rename(source, destination)
	} else if err != nil {
		return err
	}
	backup, err := os.MkdirTemp(filepath.Dir(destination), ".previous-")
	if err != nil {
		return err
	}
	if err := os.Remove(backup); err != nil {
		return err
	}
	if err := os.Rename(destination, backup); err != nil {
		return err
	}
	if err := os.Rename(source, destination); err != nil {
		if restoreErr := os.Rename(backup, destination); restoreErr != nil {
			return fmt.Errorf("replace directory: %w; restore failed: %v", err, restoreErr)
		}
		return err
	}
	return os.RemoveAll(backup)
}
