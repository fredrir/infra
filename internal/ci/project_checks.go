package ci

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/process"
)

var regexpEnvironment = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func ProjectCheckCommands(ctx context.Context, runner process.Runner, project, suite, report string) error {
	if path := PostgresPath(); path != "" {
		runner.Env = append(runner.Env, "PATH="+path+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	run := func(ctx context.Context, runner process.Runner, commands ...[]string) error {
		for _, command := range commands {
			if err := runner.Run(ctx, command[0], command[1:]...); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	switch project + "/" + suite {
	case "y/api":
		if err := runner.Run(ctx, "npx", "tsc", "-p", "."); err != nil {
			return err
		}
		return os.CopyFS(filepath.Join(runner.Dir, "dist/schema"), os.DirFS(filepath.Join(runner.Dir, "src/schema")))
	case "y/web":
		return run(ctx, runner, []string{"npm", "run", "test", "--", "--run"}, []string{"npm", "run", "build"})
	case "nsql/rust-deep":
		temporary, err := os.MkdirTemp("", "infra-rust-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(temporary)
		return WithDatabase(ctx, runner, Database{User: "postgres", Name: "postgres", Variables: []string{"NSQL_TEST_PG_URL"}}, func(ctx context.Context, runner process.Runner) error {
			if err := PrepareRust(ctx, runner, temporary, "--all-features", ""); err != nil {
				return err
			}
			return CheckRust(ctx, runner, temporary, "deep")
		})
	case "portfolio/fast":
		return run(ctx, runner, []string{"cargo", "fmt", "--all", "--check"}, []string{"cargo", "metadata", "--locked", "--offline", "--no-deps", "--format-version", "1"})
	case "portfolio/web-fast":
		return run(ctx, runner, []string{"bun", "run", "lint"}, []string{"bun", "run", "--filter", "@portfolio/web", "typecheck"}, []string{"bun", "run", "--filter", "@portfolio/edge", "typecheck"})
	case "llunde-backend/integration":
		return WithDatabase(ctx, runner, Database{User: "llunde", Name: "llunde", Variables: []string{"CI_POSTGRES_URL"}, JDBC: true}, func(ctx context.Context, runner process.Runner) error {
			return WithValkey(ctx, runner, 0, func(ctx context.Context, runner process.Runner) error {
				return runner.Run(ctx, "./gradlew", "--no-daemon", "-Dorg.gradle.jvmargs=-Xmx2g", "-Pkotlin.compiler.execution.strategy=in-process", "-Porg.gradle.java.installations.paths=/opt/java/jdk25,/opt/java/jdk21", "check", "installDist")
			})
		})
	case "llunde-backend/fast":
		return runner.Run(ctx, "java", "-cp", "/fast-tests:/fast-tests/*", "org.junit.platform.console.ConsoleLauncher", "execute", "--disable-banner", "--details=summary", "--fail-if-no-tests", "--select-class=no.llunde.config.TwilioConfigTest", "--select-class=no.llunde.config.AppConfigTest", "--select-class=no.llunde.auth.password.PasswordPolicyTest", "--select-class=no.llunde.notifications.EmailRendererTest")
	case "portfolio/rust":
		return WithDatabase(ctx, runner, Database{User: "portfolio", Name: "portfolio", Variables: []string{"DATABASE_URL"}}, func(ctx context.Context, runner process.Runner) error {
			return run(ctx, runner, []string{"cargo", "fmt", "--all", "--check"}, []string{"cargo", "clippy", "--locked", "--workspace", "--all-targets", "--", "-D", "warnings"}, []string{"cargo", "test", "--locked", "--workspace"})
		})
	case "portfolio/mutation":
		return WithDatabase(ctx, runner, Database{User: "portfolio", Name: "portfolio", Variables: []string{"DATABASE_URL"}}, func(ctx context.Context, runner process.Runner) error {
			return projectReportCommand(ctx, runner, report, "mutants", 40*time.Minute, []string{"cargo", "mutants", "--workspace", "--timeout", "300", "-j", "2"})
		})
	case "portfolio/fuzz-sanitize", "portfolio/fuzz-contact", "portfolio/fuzz-s3":
		crate, target := "apps/api", "sanitize"
		if suite == "fuzz-contact" {
			target = "contact_validate"
		}
		if suite == "fuzz-s3" {
			crate, target = "apps/worker", "s3_event_parse"
		}
		runner.Dir = filepath.Join(runner.Dir, crate)
		artifacts, err := filepath.Abs(filepath.Join(report, "artifacts"))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(artifacts, 0755); err != nil {
			return err
		}
		return projectReportCommand(ctx, runner, report, "fuzz", 30*time.Minute, []string{"cargo", "+nightly-2026-09-01", "fuzz", "run", target, "--", "-max_total_time=180", "-rss_limit_mb=3072", "-artifact_prefix=" + artifacts + string(os.PathSeparator)})
	case "portfolio/quality":
		files := []string{"packages/api-client/openapi.json", "packages/api-client/src/schema.ts"}
		before := make(map[string][]byte)
		for _, file := range files {
			data, err := os.ReadFile(filepath.Join(runner.Dir, file))
			if err != nil {
				return err
			}
			before[file] = data
		}
		generator := runner
		generator.Dir = filepath.Join(runner.Dir, "packages/api-client")
		if err := generator.Run(ctx, "bun", "run", "generate"); err != nil {
			return err
		}
		for _, file := range files {
			data, err := os.ReadFile(filepath.Join(runner.Dir, file))
			if err != nil {
				return err
			}
			if sha256.Sum256(data) != sha256.Sum256(before[file]) {
				return fmt.Errorf("generated file differs: %s", file)
			}
		}
		return runner.Run(ctx, "cargo", "audit")
	case "portfolio/web":
		return run(ctx, runner, []string{"bun", "run", "lint"}, []string{"bun", "run", "--filter", "@portfolio/web", "typecheck"}, []string{"bun", "run", "--filter", "@portfolio/edge", "typecheck"}, []string{"bun", "run", "--filter", "@portfolio/web", "build"})
	case "llunde-frontend/fast":
		hash := sha256.New()
		for _, file := range []string{"bun.lock", "tsconfig.json"} {
			data, err := os.ReadFile(filepath.Join(runner.Dir, file))
			if err != nil {
				return err
			}
			hash.Write(data)
		}
		cache := filepath.Join("/var/cache/typescript", hex.EncodeToString(hash.Sum(nil))+".tsbuildinfo")
		return runChecks(ctx, runner, []concurrentCheck{
			func(ctx context.Context, runner process.Runner) error {
				return runner.Run(ctx, "bun", "run", "--bun", "lint:ci", "--threads=2")
			},
			func(ctx context.Context, runner process.Runner) error {
				return runner.Run(ctx, "bun", "run", "--bun", "typecheck", "--incremental", "--tsBuildInfoFile", cache)
			},
		})
	case "llunde-frontend/web":
		return run(ctx, runner, []string{"bun", "run", "check:local"})
	case "llunde-pyparser/ui":
		runner.Dir = filepath.Join(runner.Dir, "review-ui")
		return run(ctx, runner, []string{"bun", "install", "--frozen-lockfile"}, []string{"bun", "run", "check:local"})
	case "portfolio/security":
		return CheckWebsiteSecurity(ctx, &http.Client{Timeout: 60 * time.Second}, "hansteen.dev")
	default:
		return fmt.Errorf("unknown project check %s/%s", project, suite)
	}
}

func projectReportCommand(ctx context.Context, runner process.Runner, directory, name string, timeout time.Duration, command []string) error {
	if directory == "" {
		return errors.New("report directory required")
	}
	if err := os.MkdirAll(directory, 0755); err != nil {
		return err
	}
	file, err := os.Create(filepath.Join(directory, name+".log"))
	if err != nil {
		return err
	}
	defer file.Close()
	if runner.Stdout == nil {
		runner.Stdout = io.Discard
	}
	if runner.Stderr == nil {
		runner.Stderr = io.Discard
	}
	result, runErr := runner.Invoke(ctx, process.Options{Name: command[0], Args: command[1:], Dir: runner.Dir, Env: append(os.Environ(), runner.Env...), Timeout: timeout, KillGrace: 30 * time.Second, Stdout: io.MultiWriter(file, runner.Stdout), Stderr: io.MultiWriter(file, runner.Stderr)})
	var artifactErr error
	if name == "mutants" {
		if info, err := os.Stat(filepath.Join(runner.Dir, "mutants.out")); err == nil && info.IsDir() {
			artifactErr = os.CopyFS(filepath.Join(directory, "mutants.out"), os.DirFS(filepath.Join(runner.Dir, "mutants.out")))
		}
	}
	code := result.ExitCode
	if code == 0 && (runErr != nil || artifactErr != nil) {
		code = 1
	}
	writeErr := os.WriteFile(filepath.Join(directory, "exit-code"), []byte(fmt.Sprintf("%d\n", code)), 0600)
	return errors.Join(runErr, writeErr, artifactErr)
}

func CheckWebsiteSecurity(ctx context.Context, client *http.Client, host string) error {
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9.-]+$`).MatchString(host) {
		return errors.New("invalid website host")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://observatory-api.mdn.mozilla.net/api/v2/scan?host="+host, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	var scan struct {
		Grade string `json:"grade"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&scan)
	response.Body.Close()
	if response.StatusCode != 200 || decodeErr != nil {
		return errors.New("website security scan failed")
	}
	if !strings.Contains("|B|B+|A-|A|A+|", "|"+scan.Grade+"|") {
		return fmt.Errorf("website security grade %s", scan.Grade)
	}
	request, err = http.NewRequestWithContext(ctx, http.MethodHead, "https://"+host+"/en", nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "synthetic")
	response, err = client.Do(request)
	if err != nil {
		return err
	}
	response.Body.Close()
	for _, name := range []string{"Strict-Transport-Security", "Content-Security-Policy"} {
		if response.Header.Get(name) == "" {
			return fmt.Errorf("website header missing: %s", name)
		}
	}
	if !strings.EqualFold(response.Header.Get("X-Content-Type-Options"), "nosniff") || !strings.Contains(response.Header.Get("Alt-Svc"), "h3") {
		return errors.New("website transport headers missing")
	}
	connection, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: 15 * time.Second}, Config: &tls.Config{MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}}).DialContext(ctx, "tcp", net.JoinHostPort(host, "443"))
	if err == nil {
		connection.Close()
		return errors.New("obsolete TLS accepted")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}
