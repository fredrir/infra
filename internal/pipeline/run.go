package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dagger.io/dagger"
	"github.com/fredrir/infra/internal/process"
)

type Options struct {
	ForwardLocalCache bool
	Root              string
	Local             bool
	Bazel             string
	Operation         string
	Targets           []string
	Base              string
	DiskCache         string
	RepositoryCache   string
	RemoteCache       string
	RemoteExecutor    string
	ReadOnlyCache     bool
	ReportDir         string
	Log               io.Writer
}

type Report struct {
	Schema          int       `json:"schema"`
	Metrics         *Metrics  `json:"metrics,omitempty"`
	Operation       string    `json:"operation"`
	Targets         []string  `json:"targets"`
	Started         time.Time `json:"started"`
	DurationSeconds float64   `json:"duration_seconds"`
	Success         bool      `json:"success"`
	Error           string    `json:"error,omitempty"`
	TraceURL        string    `json:"trace_url,omitempty"`
}

func Run(ctx context.Context, opts Options) (report Report, err error) {
	report = Report{Schema: 1, Operation: opts.Operation, Targets: opts.Targets, Started: time.Now().UTC(), TraceURL: os.Getenv("DAGGER_TRACE_URL")}
	if opts.Operation != "test" && opts.Operation != "build" && opts.Operation != "generate-check" && opts.Operation != "cache-gc" && opts.Operation != "prepare-check" {
		return report, errors.New("operation must be build, test, generate-check, prepare-check or cache-gc")
	}
	if opts.Operation == "cache-gc" && (opts.Local || len(opts.Targets) != 0 || opts.Base != "") {
		return report, errors.New("cache maintenance requires Dagger and no targets or base")
	}
	if opts.Operation == "generate-check" && len(opts.Targets) != 0 {
		return report, errors.New("generated BUILD checks do not accept targets")
	}
	if opts.ReportDir == "" {
		opts.ReportDir = filepath.Join(opts.Root, "dist", "reports")
	}
	if err := os.MkdirAll(opts.ReportDir, 0o755); err != nil {
		return report, err
	}
	for _, name := range []string{"events.jsonl", "profile.json.gz"} {
		if err := os.Remove(filepath.Join(opts.ReportDir, name)); err != nil && !os.IsNotExist(err) {
			return report, err
		}
	}
	defer func() {
		err = errors.Join(err, ctx.Err())
		if metrics, metricsErr := readMetrics(filepath.Join(opts.ReportDir, "events.jsonl")); metricsErr == nil {
			report.Metrics = metrics
		}
		report.DurationSeconds = time.Since(report.Started).Seconds()
		report.Success = err == nil
		if err != nil {
			report.Error = err.Error()
		}
		data, marshalErr := json.MarshalIndent(report, "", "  ")
		if marshalErr == nil {
			marshalErr = os.WriteFile(filepath.Join(opts.ReportDir, "report.json"), append(data, '\n'), 0o644)
		}
		err = errors.Join(err, marshalErr)
	}()
	if opts.ForwardLocalCache {
		if opts.Local {
			return report, errors.New("cache forwarding requires Dagger execution")
		}
		if _, _, _, err := localCache(opts.RemoteCache); err != nil {
			return report, err
		}
	}
	config, err := ReadToolchain(opts.Root)
	if err != nil {
		return report, err
	}
	expression, err := AffectedExpression(ctx, opts.Root, opts.Base)
	if err != nil {
		return report, err
	}
	if expression == "set()" && len(opts.Targets) == 0 {
		return report, nil
	}
	if opts.Operation == "generate-check" {
		if opts.Base != "" && expression != "//..." && !strings.Contains(expression, "//internal/") && !strings.Contains(expression, "//cmd/") && !strings.Contains(expression, "//integration/") {
			return report, nil
		}
		opts.Targets = []string{"//:gazelle", "--", "-mode=diff"}
	}
	if opts.Local {
		err = runLocal(ctx, opts, config, expression, &report)
	} else {
		err = runDagger(ctx, opts, config, expression, &report)
	}
	return report, err
}

func buildArgs(opts Options, config Toolchain, targets []string, reports string) []string {
	operation := opts.Operation
	if operation == "generate-check" {
		operation = "run"
	}
	if operation == "prepare-check" {
		operation = "build"
	}
	args := []string{operation, "--config=ci", "--action_env=INFRA_BUILD_IMAGE=" + config.Image,
		"--build_event_json_file=" + filepath.Join(reports, "events.jsonl"), "--profile=" + filepath.Join(reports, "profile.json.gz")}
	if !opts.Local {
		args = append([]string{"--batch"}, args...)
	}
	if opts.Operation == "test" {
		args = append(args, "--build_tests_only")
	}
	if opts.Local {
		if opts.DiskCache != "" {
			args = append(args, "--disk_cache="+opts.DiskCache, "--experimental_disk_cache_gc_max_size=4G", "--experimental_disk_cache_gc_max_age=7d")
		}
		if opts.RepositoryCache != "" {
			args = append(args, "--repository_cache="+opts.RepositoryCache)
		}
	}
	if opts.RemoteCache != "" {
		args = append(args, "--remote_cache="+opts.RemoteCache)
	}
	if opts.RemoteExecutor != "" {
		args = append(args, "--remote_executor="+opts.RemoteExecutor)
	}
	if opts.ReadOnlyCache {
		args = append(args, "--remote_upload_local_results=false")
	}
	return append(args, targets...)
}

func runLocal(ctx context.Context, opts Options, config Toolchain, expression string, report *Report) error {
	bazel := opts.Bazel
	if bazel == "" {
		bazel = "bazel"
	}
	version, err := process.Run(ctx, process.Options{Name: bazel, Args: []string{"--version"}, Timeout: time.Minute})
	data := version.Stdout
	if err != nil {
		return fmt.Errorf("read Bazel version: %w", err)
	}
	if strings.TrimSpace(string(data)) != "bazel "+config.Bazel {
		return fmt.Errorf("Bazel %s required, got %s", config.Bazel, strings.TrimSpace(string(data)))
	}
	targets := opts.Targets
	if len(targets) == 0 && expression != "//..." {
		query, err := process.Run(ctx, process.Options{Name: bazel, Args: []string{"query", expression, "--output=label"}, Dir: opts.Root, Stderr: opts.Log})
		data := query.Stdout
		if err != nil {
			return fmt.Errorf("query affected targets: %w", err)
		}
		targets = strings.Fields(string(data))
		if len(targets) == 0 {
			return nil
		}
	}
	if len(targets) == 0 {
		targets = []string{"//..."}
	}
	if opts.Operation == "prepare-check" {
		targets = append(targets, "//:gazelle")
	}
	report.Targets = targets
	reports, err := filepath.Abs(opts.ReportDir)
	if err != nil {
		return err
	}
	_, err = process.Run(ctx, process.Options{Name: bazel, Args: buildArgs(opts, config, targets, reports), Dir: opts.Root, Stdout: opts.Log, Stderr: opts.Log})
	return err
}

func runDagger(ctx context.Context, opts Options, config Toolchain, expression string, report *Report) error {
	client, err := dagger.Connect(ctx, dagger.WithLogOutput(opts.Log))
	if err != nil {
		return err
	}
	defer client.Close()
	if os.Getenv("DAGGER_CLOUD_TOKEN") != "" {
		if trace, traceErr := client.Cloud().TraceURL(ctx); traceErr == nil {
			report.TraceURL = trace
		}
	}
	actual, err := client.Version(ctx)
	if err != nil {
		return err
	}
	if strings.TrimPrefix(actual, "v") != config.Dagger {
		return fmt.Errorf("Dagger engine %s required, got %s", config.Dagger, actual)
	}
	source := client.Host().Directory(opts.Root, dagger.HostDirectoryOpts{Gitignore: true, Exclude: []string{".git", "dist", "bazel-*", ".infra", ".cache", ".direnv", ".venv", "**/.terraform", "**/node_modules", ".env", ".env.*"}})
	url := "https://github.com/bazelbuild/bazel/releases/download/" + config.Bazel + "/bazel-" + config.Bazel + "-linux-x86_64"
	container := client.Container(dagger.ContainerOpts{Platform: "linux/amd64"}).From(config.Image).
		WithFile("/usr/local/bin/bazel", client.HTTP(url), dagger.ContainerWithFileOpts{Permissions: 0o755}).
		WithExec([]string{"sha256sum", "--check", "--strict"}, dagger.ContainerWithExecOpts{Stdin: config.BazelSHA256 + "  /usr/local/bin/bazel\n"}).
		WithDirectory("/src", source).WithWorkdir("/src").
		WithMountedCache("/root/.cache/bazel-repo", client.CacheVolume("infra-bazel-repository-v1")).
		WithMountedCache(diskCachePath, client.CacheVolume("infra-bazel-actions-v1"), dagger.ContainerWithMountedCacheOpts{Sharing: dagger.CacheSharingModeShared}).
		WithExec([]string{"mkdir", "-p", "/reports"})
	if opts.Operation == "cache-gc" {
		return pruneDiskCache(ctx, container)
	}
	if opts.ForwardLocalCache {
		endpoint, host, port, err := localCache(opts.RemoteCache)
		if err != nil {
			return err
		}
		service := client.Host().Service([]dagger.PortForward{{Backend: port, Frontend: port}}, dagger.HostServiceOpts{Host: host})
		container = container.WithServiceBinding("infra-cache", service)
		opts.RemoteCache = endpoint
	}
	targets := opts.Targets
	if len(targets) == 0 && expression != "//..." {
		query := container.WithExec([]string{"bazel", "--batch", "query", expression, "--output=label"})
		result, err := query.Stdout(ctx)
		if err != nil {
			return err
		}
		targets = strings.Fields(result)
		if len(targets) == 0 {
			return nil
		}
	}
	if len(targets) == 0 {
		targets = []string{"//..."}
	}
	if opts.Operation == "prepare-check" {
		targets = append(targets, "//:gazelle")
	}
	report.Targets = targets
	args := buildArgs(opts, config, targets, "/reports")
	args = append(args[:len(args)-len(targets)], append([]string{"--repository_cache=/root/.cache/bazel-repo", "--disk_cache=" + diskCachePath}, targets...)...)
	container = container.WithExec(append([]string{"bazel"}, args...), dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny})
	code, err := container.ExitCode(ctx)
	if err != nil {
		return err
	}
	_, exportErr := container.Directory("/reports").Export(ctx, opts.ReportDir)
	if code != 0 {
		return errors.Join(fmt.Errorf("Bazel %s exited with status %d", opts.Operation, code), exportErr)
	}
	return exportErr
}
