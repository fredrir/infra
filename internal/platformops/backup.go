package platformops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Command func(context.Context, string, string, ...string) ([]byte, error)

type Repository struct {
	Name string
	Env  []string
}

type ResticCommand func(ctx context.Context, repository Repository, args ...string) ([]byte, error)

var repositoryName = regexp.MustCompile(`^[a-z]{1,16}$`)

var sharedResticSettings = []string{"RESTIC_CACHE_DIR"}

var repositorySettings = []string{"RESTIC_REPOSITORY", "RESTIC_PASSWORD", "RESTIC_CACERT", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_DEFAULT_REGION"}

func RepositoryPrefix(name string) string {
	return strings.ToUpper(name) + "_"
}

func RepositoriesFromEnvironment(names []string, lookup func(string) (string, bool)) ([]Repository, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("backup repositories required")
	}
	var repositories []Repository
	for _, name := range names {
		if !repositoryName.MatchString(name) || slices.ContainsFunc(repositories, func(repository Repository) bool { return repository.Name == name }) {
			return nil, fmt.Errorf("invalid backup repository %q", name)
		}
		repository := Repository{Name: name}
		for _, key := range repositorySettings {
			if value, ok := lookup(RepositoryPrefix(name) + key); ok && value != "" {
				repository.Env = append(repository.Env, key+"="+value)
			}
		}
		for _, key := range repositorySettings[:2] {
			if !slices.ContainsFunc(repository.Env, func(entry string) bool { return strings.HasPrefix(entry, key+"=") }) {
				return nil, fmt.Errorf("backup repository %s lacks %s", name, key)
			}
		}
		repositories = append(repositories, repository)
	}
	return repositories, nil
}

func ResticEnvironment(base []string, repositories []Repository, repository Repository) []string {
	var environment []string
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		repositoryScoped := (strings.HasPrefix(key, "RESTIC_") || strings.HasPrefix(key, "AWS_")) && !slices.Contains(sharedResticSettings, key)
		if repositoryScoped || slices.ContainsFunc(repositories, func(other Repository) bool { return strings.HasPrefix(key, RepositoryPrefix(other.Name)) }) {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, repository.Env...)
}

func MaintainRepositories(ctx context.Context, restic ResticCommand, repositories []Repository, timeout time.Duration) error {
	if len(repositories) == 0 {
		return fmt.Errorf("backup repositories required")
	}
	if timeout <= 0 {
		return fmt.Errorf("maintenance timeout required")
	}
	var failures []error
	for _, repository := range repositories {
		if err := maintainRepository(ctx, restic, repository, timeout); err != nil {
			failures = append(failures, fmt.Errorf("maintenance of %s: %w", repository.Name, err))
		}
	}
	return errors.Join(failures...)
}

func maintainRepository(ctx context.Context, restic ResticCommand, repository Repository, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, args := range [][]string{{"--retry-lock", "10m", "check", "--read-data-subset=10%"}, {"--retry-lock", "10m", "forget", "--group-by", "host,tags", "--keep-daily", "7", "--keep-weekly", "4", "--keep-monthly", "12", "--prune"}} {
		if _, err := restic(ctx, repository, args...); err != nil {
			return err
		}
	}
	return nil
}

type BackupConfig struct {
	Work               string
	Files              string
	Kind               string
	Project            string
	Writers            []string
	Kubernetes         API
	Namespace          string
	MongoURI           string
	LocalFiles         bool
	LocalFilesMaxBytes int64
	LocalFilesExclude  string
	PreflightTimeout   time.Duration
	ExportTimeout      time.Duration
	UploadTimeout      time.Duration
	QuiesceTimeout     time.Duration
	RecoveryTimeout    time.Duration
	HeartbeatEndpoint  string
	HeartbeatToken     string
	Repositories       []Repository
	Run                Command
	Restic             ResticCommand
	Log                io.Writer
}

type writerState struct {
	Name     string `json:"name"`
	Replicas int    `json:"replicas"`
}

func Backup(ctx context.Context, c BackupConfig, exportOnly bool) error {
	if c.Run == nil {
		return fmt.Errorf("backup command runner required")
	}
	if c.Log == nil {
		c.Log = io.Discard
	}
	if c.Work == "" || c.Files == "" {
		return fmt.Errorf("backup directories required")
	}
	if c.Kind != "postgres" && c.Kind != "mongodb" && c.Kind != "sqlite" {
		return fmt.Errorf("invalid backup kind")
	}
	if c.PreflightTimeout <= 0 || c.ExportTimeout <= 0 || c.UploadTimeout <= 0 || c.QuiesceTimeout <= 0 || c.RecoveryTimeout <= 0 {
		return fmt.Errorf("backup timeouts must be positive")
	}
	if len(c.Writers) > 0 && !resourceName.MatchString(c.Namespace) {
		return fmt.Errorf("backup namespace required")
	}
	for _, name := range c.Writers {
		if !resourceName.MatchString(name) {
			return fmt.Errorf("invalid backup writer")
		}
	}
	if c.LocalFiles && c.LocalFilesMaxBytes <= 0 {
		return fmt.Errorf("local file budget required")
	}
	source := filepath.Join(c.Work, "source")
	if err := os.MkdirAll(c.Work, 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(source); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("backup source must be a private directory")
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return fmt.Errorf("backup source must be empty")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(source, 0o700); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(source, "SHA256SUMS")); err != nil && !os.IsNotExist(err) {
		return err
	}
	var available []Repository
	var failures []error
	if !exportOnly {
		if c.Restic == nil || len(c.Repositories) == 0 {
			return fmt.Errorf("backup repositories required")
		}
		for _, repository := range c.Repositories {
			preflight, cancel := context.WithTimeout(ctx, c.PreflightTimeout)
			_, err := c.Restic(preflight, repository, "cat", "config")
			cancel()
			if err != nil {
				failures = append(failures, fmt.Errorf("backup preflight of %s: %w", repository.Name, err))
				continue
			}
			available = append(available, repository)
		}
		if len(available) == 0 {
			return errors.Join(failures...)
		}
	}
	exportCtx, cancel := context.WithTimeout(ctx, c.ExportTimeout)
	err := exportBackup(exportCtx, c)
	cancel()
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	if exportOnly {
		return nil
	}
	for _, repository := range available {
		upload, cancel := context.WithTimeout(ctx, c.UploadTimeout)
		_, err := c.Restic(upload, repository, "--retry-lock", "10m", "backup", source, "--host", "platform", "--tag", c.Project, "--tag", c.Kind, "--json")
		cancel()
		if err != nil {
			failures = append(failures, fmt.Errorf("backup upload to %s: %w", repository.Name, err))
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	if err := Heartbeat(ctx, c.HeartbeatEndpoint, c.Project, c.HeartbeatToken); err != nil {
		fmt.Fprintln(c.Log, "backup heartbeat failed")
	}
	return nil
}

func exportBackup(ctx context.Context, c BackupConfig) (result error) {
	source := filepath.Join(c.Work, "source")
	journal := filepath.Join(c.Work, "resume.json")
	var writers []writerState
	if previous, err := os.ReadFile(journal); err == nil {
		if err := json.Unmarshal(previous, &writers); err != nil {
			return fmt.Errorf("invalid backup recovery journal")
		}
		if len(writers) > 0 {
			return fmt.Errorf("unfinished backup recovery; run backup-resume first")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := writeJSON(journal, []writerState{}); err != nil {
		return err
	}
	defer func() {
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.RecoveryTimeout)
		defer cancel()
		if err := ResumeBackup(recovery, c); err != nil {
			result = errors.Join(result, fmt.Errorf("writer resume failed; manual recovery required: %w", err))
		}
		if result != nil {
			_ = os.Remove(filepath.Join(source, "SHA256SUMS"))
		}
	}()
	for _, name := range c.Writers {
		replicas, _, err := deployment(ctx, c, name)
		if err != nil {
			return err
		}
		if replicas != 0 && replicas != 1 {
			return fmt.Errorf("writer %s must have zero or one replicas", name)
		}
		if replicas == 1 {
			writers = append(writers, writerState{Name: name, Replicas: replicas})
			if err := writeJSON(journal, writers); err != nil {
				return err
			}
			if err := scaleWriter(ctx, c, name, 0); err != nil {
				return err
			}
		}
	}
	quiesce, cancel := context.WithTimeout(ctx, c.QuiesceTimeout)
	defer cancel()
	for {
		quiet, err := writersQuiet(quiesce, c)
		if err != nil {
			return err
		}
		if quiet {
			break
		}
		if err := pause(quiesce, time.Second); err != nil {
			return fmt.Errorf("writers did not quiesce: %w", err)
		}
	}
	switch c.Kind {
	case "postgres":
		path := filepath.Join(source, "database.dump")
		if _, err := c.Run(ctx, "", "pg_dump", "--format=custom", "--no-owner", "--no-acl", "--file="+path); err != nil {
			return err
		}
		if _, err := c.Run(ctx, "", "pg_restore", "--list", path); err != nil {
			return err
		}
	case "mongodb":
		config := filepath.Join(c.Work, "mongodb.json")
		if c.MongoURI == "" {
			return fmt.Errorf("MongoDB URI required")
		}
		if err := writeJSON(config, map[string]string{"uri": c.MongoURI}); err != nil {
			return err
		}
		defer os.Remove(config)
		args := []string{"--config=" + config, "--archive=" + filepath.Join(source, "database.archive.gz"), "--gzip"}
		output, err := c.Run(ctx, "", "mongodump", args...)
		if err != nil {
			return err
		}
		if !strings.Contains(string(output), "done dumping") {
			return fmt.Errorf("MongoDB export contains no dumped collections")
		}
		if _, err := c.Run(ctx, "", "mongorestore", append(args, "--dryRun")...); err != nil {
			return err
		}
	case "sqlite":
		target := filepath.Join(source, "metadata.db")
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if err = os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
			_, err = c.Run(ctx, "", "sqlite3", filepath.Join(c.Files, "metadata.db"), "VACUUM INTO '"+strings.ReplaceAll(target, "'", "''")+"'")
			if err == nil {
				break
			}
			if attempt < 2 {
				if err := pause(ctx, 10*time.Second); err != nil {
					return err
				}
			}
		}
		if err != nil {
			return fmt.Errorf("sqlite snapshot failed: %w", err)
		}
		output, err := c.Run(ctx, "", "sqlite3", target, "PRAGMA integrity_check;")
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(output)) != "ok" {
			return fmt.Errorf("sqlite integrity check failed")
		}
	}
	if c.LocalFiles {
		output, err := c.Run(ctx, "", "du", "-sb", c.Files)
		if err != nil {
			return err
		}
		fields := strings.Fields(string(output))
		if len(fields) == 0 {
			return fmt.Errorf("missing local file size")
		}
		size, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || size < 0 || size > c.LocalFilesMaxBytes {
			return fmt.Errorf("local files exceed scratch budget")
		}
		args := []string{"--create", "--file=" + filepath.Join(source, "local-files.tar"), "--one-file-system"}
		if c.LocalFilesExclude != "" {
			args = append(args, "--exclude="+c.LocalFilesExclude)
		}
		args = append(args, "--directory="+c.Files, ".")
		if _, err := c.Run(ctx, "", "tar", args...); err != nil {
			return err
		}
	}
	quiet, err := writersQuiet(ctx, c)
	if err != nil {
		return err
	}
	if !quiet {
		return fmt.Errorf("writer resumed during export")
	}
	return WriteChecksums(source)
}

func deployment(ctx context.Context, c BackupConfig, name string) (int, string, error) {
	var object struct {
		Spec struct {
			Replicas *int
			Selector struct{ MatchLabels map[string]string }
		}
	}
	path := "/apis/apps/v1/namespaces/" + url.PathEscape(c.Namespace) + "/deployments/" + url.PathEscape(name)
	if _, err := c.Kubernetes.Call(ctx, http.MethodGet, path, nil, &object); err != nil {
		return 0, "", err
	}
	if object.Spec.Replicas == nil {
		return 0, "", fmt.Errorf("writer %s has no replica count", name)
	}
	labels := make([]string, 0, len(object.Spec.Selector.MatchLabels))
	for k, v := range object.Spec.Selector.MatchLabels {
		labels = append(labels, k+"="+v)
	}
	sort.Strings(labels)
	if len(labels) == 0 {
		return 0, "", fmt.Errorf("writer %s has no pod selector", name)
	}
	return *object.Spec.Replicas, strings.Join(labels, ","), nil
}

func scaleWriter(ctx context.Context, c BackupConfig, name string, replicas int) error {
	api := c.Kubernetes
	api.ContentType = "application/merge-patch+json"
	body := map[string]any{"spec": map[string]any{"replicas": replicas}}
	_, err := api.Call(ctx, http.MethodPatch, "/apis/apps/v1/namespaces/"+url.PathEscape(c.Namespace)+"/deployments/"+url.PathEscape(name)+"/scale", body, nil)
	return err
}

func writersQuiet(ctx context.Context, c BackupConfig) (bool, error) {
	for _, name := range c.Writers {
		replicas, selector, err := deployment(ctx, c, name)
		if err != nil {
			return false, err
		}
		if replicas != 0 {
			return false, fmt.Errorf("writer %s is not stopped", name)
		}
		var pods struct{ Items []json.RawMessage }
		if _, err := c.Kubernetes.Call(ctx, http.MethodGet, "/api/v1/namespaces/"+url.PathEscape(c.Namespace)+"/pods?labelSelector="+url.QueryEscape(selector), nil, &pods); err != nil {
			return false, err
		}
		if len(pods.Items) != 0 {
			return false, nil
		}
	}
	return true, nil
}

func ResumeBackup(ctx context.Context, c BackupConfig) error {
	path := filepath.Join(c.Work, "resume.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var writers []writerState
	if err := json.Unmarshal(data, &writers); err != nil {
		return err
	}
	var failures []error
	var remaining []writerState
	for _, writer := range writers {
		if !resourceName.MatchString(writer.Name) || writer.Replicas != 1 {
			return fmt.Errorf("invalid backup recovery journal")
		}
		current, _, err := deployment(ctx, c, writer.Name)
		if err == nil {
			if current == 0 {
				err = scaleWriter(ctx, c, writer.Name, writer.Replicas)
			} else if current != writer.Replicas {
				err = fmt.Errorf("writer %s changed replicas", writer.Name)
			}
		}
		if err != nil {
			failures = append(failures, err)
			remaining = append(remaining, writer)
		}
	}
	if len(failures) == 0 {
		return os.Remove(path)
	}
	if err := writeJSON(path, remaining); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".infra-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func WriteChecksums(root string) error {
	var output strings.Builder
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if name == "SHA256SUMS" {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("backup source is not a regular file: %s", entry.Name())
		}
		if strings.ContainsAny(name, "\r\n\\") {
			return fmt.Errorf("invalid backup filename")
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, err = io.Copy(hash, file)
		closeErr := file.Close()
		if err = errors.Join(err, closeErr); err != nil {
			return err
		}
		fmt.Fprintf(&output, "%s  ./%s\n", hex.EncodeToString(hash.Sum(nil)), filepath.ToSlash(name))
		return nil
	})
	if err != nil {
		return err
	}
	if output.Len() == 0 {
		return fmt.Errorf("backup export is empty")
	}
	return os.WriteFile(filepath.Join(root, "SHA256SUMS"), []byte(output.String()), 0o600)
}
