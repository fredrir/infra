package ci

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type RustOptions struct {
	Clippy   []string `json:"clippy"`
	Test     []string `json:"test"`
	FastTest []string `json:"fast_test,omitempty"`
}

func PrepareRust(ctx context.Context, runner Runner, temporary, clippy, test string) error {
	clippyFlags, err := RustArguments(clippy)
	if err != nil {
		return err
	}
	testFlags, err := RustArguments(test)
	if err != nil {
		return err
	}
	if temporary == "" {
		return fmt.Errorf("runner temporary directory is required")
	}
	fastFlags, err := RustArguments(os.Getenv("FAST_TEST_ARGS"))
	if err != nil {
		return err
	}
	data, err := json.Marshal(RustOptions{Clippy: clippyFlags, Test: testFlags, FastTest: fastFlags})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(temporary, "rust-args.json"), append(data, '\n'), 0600); err != nil {
		return err
	}
	if os.Getenv("RUSTC_WRAPPER") != "" {
		endpoint, err := url.Parse(os.Getenv("SCCACHE_ENDPOINT"))
		available := false
		if err == nil && endpoint.Hostname() != "" {
			port := endpoint.Port()
			if port == "" {
				port = "443"
				if endpoint.Scheme == "http" {
					port = "80"
				}
			}
			connection, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(endpoint.Hostname(), port))
			if err == nil {
				connection.Close()
				_, err = runner.Output(ctx, "sccache", "--start-server")
				available = err == nil
			}
		}
		if available {
			fmt.Fprintf(runner.Stdout, "Compilation cache: %s (%s)\n", os.Getenv("SCCACHE_BUCKET"), environmentDefault("SCCACHE_S3_RW_MODE", "READ_WRITE"))
		} else {
			fmt.Fprintln(runner.Stderr, "::warning::Build cache unavailable; compiling without it")
			if err := AppendEnvironment(os.Getenv("GITHUB_ENV"), "RUSTC_WRAPPER", ""); err != nil {
				return err
			}
		}
	}
	return runner.Run(ctx, "rustup", "show", "active-toolchain")
}

func CheckRust(ctx context.Context, runner Runner, temporary, stage string) error {
	if stage == "fast" || stage == "deep" {
		stages := []string{"format", "unit"}
		if stage == "fast" {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
		} else {
			stages = []string{"format", "lint", "test", "docs", "minimal", "msrv", "audit"}
		}
		for _, part := range stages {
			if err := CheckRust(ctx, runner, temporary, part); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	var options RustOptions
	if stage == "lint" || stage == "test" || stage == "docs" || stage == "unit" || stage == "prepare-fast" {
		data, err := os.ReadFile(filepath.Join(temporary, "rust-args.json"))
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &options); err != nil {
			return err
		}
		if _, err := RustArguments(strings.Join(options.Clippy, " ")); err != nil {
			return err
		}
		if _, err := RustArguments(strings.Join(options.Test, " ")); err != nil {
			return err
		}
		if _, err := RustArguments(strings.Join(options.FastTest, " ")); err != nil {
			return err
		}
	}
	switch stage {
	case "format":
		return runner.Run(ctx, "cargo", "fmt", "--all", "--", "--check")
	case "lint":
		args := append([]string{"clippy", "--all-targets", "--locked"}, options.Clippy...)
		return runner.Run(ctx, "cargo", append(args, "--", "-D", "warnings")...)
	case "test":
		return runner.Run(ctx, "cargo", append([]string{"nextest", "run", "--all-targets", "--locked", "--no-tests=pass"}, options.Test...)...)
	case "unit":
		args := append([]string{"nextest", "run", "--bins", "--locked", "--no-tests=fail"}, options.Test...)
		return runner.Run(ctx, "cargo", append(args, options.FastTest...)...)
	case "prepare-fast":
		return runner.Run(ctx, "cargo", append([]string{"nextest", "list", "--bins", "--locked", "--list-type", "binaries-only"}, options.Test...)...)
	case "docs":
		metadata, err := cargoMetadata(ctx, runner)
		if err != nil {
			return err
		}
		for _, pkg := range metadata.Packages {
			for _, target := range pkg.Targets {
				if slices.Contains(target.Kind, "lib") {
					return runner.Run(ctx, "cargo", append([]string{"test", "--doc", "--locked"}, options.Test...)...)
				}
			}
		}
		return nil
	case "minimal":
		return runner.Run(ctx, "cargo", "build", "--locked", "--no-default-features")
	case "msrv":
		return CheckMSRV(ctx, runner)
	case "audit":
		return runner.Run(ctx, "cargo", "audit")
	default:
		return fmt.Errorf("unknown Rust check %q", stage)
	}
}

type rustMetadata struct {
	Packages []struct {
		RustVersion string `json:"rust_version"`
		Targets     []struct{ Kind []string }
	}
}

func cargoMetadata(ctx context.Context, runner Runner) (rustMetadata, error) {
	data, err := runner.Output(ctx, "cargo", "metadata", "--no-deps", "--format-version", "1")
	if err != nil {
		return rustMetadata{}, err
	}
	var metadata rustMetadata
	err = json.Unmarshal(data, &metadata)
	return metadata, err
}

var rustVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)

func MaximumRustVersion(versions []string) (string, error) {
	var maximum [3]uint64
	selected := ""
	for _, version := range versions {
		if version == "" {
			continue
		}
		if !rustVersionPattern.MatchString(version) {
			return "", fmt.Errorf("unsupported rust-version %q", version)
		}
		var parsed [3]uint64
		for index, component := range strings.Split(version, ".") {
			value, err := strconv.ParseUint(component, 10, 64)
			if err != nil {
				return "", err
			}
			parsed[index] = value
		}
		if selected == "" || slices.Compare(parsed[:], maximum[:]) > 0 {
			selected, maximum = version, parsed
		}
	}
	return selected, nil
}

func CheckMSRV(ctx context.Context, runner Runner) error {
	metadata, err := cargoMetadata(ctx, runner)
	if err != nil {
		return err
	}
	var versions []string
	for _, pkg := range metadata.Packages {
		versions = append(versions, pkg.RustVersion)
	}
	version, err := MaximumRustVersion(versions)
	if err != nil {
		return err
	}
	if version == "" {
		_, err := fmt.Fprintln(runner.Stdout, "No rust-version declared; skipping the MSRV check")
		return err
	}
	installed, err := runner.Output(ctx, "rustup", "toolchain", "list")
	if err != nil {
		return err
	}
	pattern := regexp.MustCompile("^" + regexp.QuoteMeta(version) + `(\.[0-9]+)?-`)
	var candidates []string
	for _, line := range strings.Split(string(installed), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 0 && pattern.MatchString(fields[0]) {
			candidates = append(candidates, fields[0])
		}
	}
	toolchain := version
	if len(candidates) == 0 {
		if err := runner.Run(ctx, "rustup", "toolchain", "install", version, "--profile", "minimal", "--no-self-update"); err != nil {
			return err
		}
	} else {
		var selected []string
		for _, candidate := range candidates {
			selected = append(selected, strings.SplitN(candidate, "-", 2)[0])
		}
		newest, err := MaximumRustVersion(selected)
		if err != nil {
			return err
		}
		for _, candidate := range candidates {
			if strings.HasPrefix(candidate, newest+"-") {
				toolchain = candidate
				break
			}
		}
	}
	return runner.Run(ctx, "cargo", "+"+toolchain, "check", "--all-targets", "--locked")
}

func AppendEnvironment(path, name, value string) error {
	if path == "" || !regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`).MatchString(name) || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("invalid environment output")
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(file, "%s=%s\n", name, value)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func environmentDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
