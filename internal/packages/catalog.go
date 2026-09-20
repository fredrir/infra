package packages

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/fredrir/infra/internal/release"
	"go.yaml.in/yaml/v3"
)

type Project struct {
	Project, Repository, Visibility string
	Taps                            []string
}
type Registry struct{ Projects []Project }
type Tool struct {
	Repository string   `json:"repository"`
	Version    string   `json:"version"`
	Binary     string   `json:"binary"`
	Taps       []string `json:"taps"`
}
type Tools map[string]Tool
type PublishedRelease struct {
	Tag               string `json:"tag_name"`
	Draft, Prerelease bool
	Assets            []struct{ Name string }
}

var stableTag = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)$`)
var toolName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
var repositoryName = regexp.MustCompile(`^fredrir/[A-Za-z0-9_.-]+$`)
var binaryName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var channelSuffixes = []string{".rb", ".nix", "-bin.pkgbuild", "-bin.srcinfo", ".pkgbuild", ".srcinfo"}

func StableReleases(releases []PublishedRelease, keep int) []string {
	type candidate struct {
		tag     string
		version [3]uint64
	}
	var candidates []candidate
	for _, published := range releases {
		match := stableTag.FindStringSubmatch(published.Tag)
		if published.Draft || published.Prerelease || match == nil {
			continue
		}
		valid := false
		for _, asset := range published.Assets {
			valid = valid || asset.Name == "release.json"
		}
		if !valid {
			continue
		}
		version := candidate{tag: published.Tag}
		for index := range version.version {
			number, err := strconv.ParseUint(match[index+1], 10, 64)
			if err != nil {
				valid = false
				break
			}
			version.version[index] = number
		}
		if valid {
			candidates = append(candidates, version)
		}
	}
	slices.SortFunc(candidates, func(left, right candidate) int { return slices.Compare(right.version[:], left.version[:]) })
	if keep < 0 {
		keep = 0
	}
	if keep < len(candidates) {
		candidates = candidates[:keep]
	}
	tags := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		tags = append(tags, candidate.tag)
	}
	return tags
}

func Checksums(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 67 || line[64] != ' ' || (line[65] != ' ' && line[65] != '*') {
			return nil, fmt.Errorf("invalid checksum entry")
		}
		name, digest := line[66:], line[:64]
		if !filepath.IsLocal(name) {
			return nil, fmt.Errorf("unsafe checksum path")
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return nil, err
		}
		if _, ok := entries[name]; ok {
			return nil, fmt.Errorf("duplicate checksum entry")
		}
		entries[name] = digest
	}
	return entries, scanner.Err()
}

func VerifyChecksum(path, expected string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return err
	}
	if hex.EncodeToString(digest.Sum(nil)) != expected {
		return fmt.Errorf("checksum mismatch for %s", filepath.Base(path))
	}
	return nil
}

type ReleaseSource interface {
	Releases(context.Context, string) ([]PublishedRelease, error)
	Download(context.Context, string, string, string) error
	Verify(context.Context, string, string, string) error
}

type Commands interface {
	Run(context.Context, string, ...string) error
	Output(context.Context, string, ...string) ([]byte, error)
}

type GitHub struct {
	Runner  Commands
	trusted map[string]bool
}

func (source *GitHub) Releases(ctx context.Context, repository string) ([]PublishedRelease, error) {
	data, err := source.Runner.Output(ctx, "gh", "api", "repos/"+repository+"/releases?per_page=100")
	if err != nil {
		return nil, err
	}
	var releases []PublishedRelease
	err = json.Unmarshal(data, &releases)
	return releases, err
}

func (source *GitHub) Download(ctx context.Context, repository, tag, destination string) error {
	return source.Runner.Run(ctx, "gh", "release", "download", tag, "--repo", repository, "--dir", destination, "--pattern", "*-unknown-linux-*.tar.gz", "--pattern", "checksums.txt", "--pattern", "release.json", "--pattern", "*.rb", "--pattern", "*.nix", "--pattern", "*.pkgbuild", "--pattern", "*.srcinfo")
}

func (source *GitHub) Verify(ctx context.Context, path, repository, tag string) error {
	data, err := source.Runner.Output(ctx, "gh", "attestation", "verify", path, "--repo", repository, "--signer-workflow", "fredrir/infra/.github/workflows/rust-release.yml", "--source-ref", "refs/tags/"+tag, "--format", "json")
	if err != nil {
		return err
	}
	digests, err := SignerDigests(data)
	if err != nil {
		return err
	}
	if source.trusted == nil {
		source.trusted = map[string]bool{}
	}
	for _, digest := range digests {
		if source.trusted[digest] {
			continue
		}
		data, err := source.Runner.Output(ctx, "gh", "api", "repos/fredrir/infra/compare/"+digest+"...main")
		if err != nil {
			return err
		}
		var comparison struct{ Status string }
		if err := json.Unmarshal(data, &comparison); err != nil {
			return err
		}
		if comparison.Status != "ahead" && comparison.Status != "identical" {
			return fmt.Errorf("asset signer is not an infra revision on main")
		}
		source.trusted[digest] = true
	}
	return nil
}

func SignerDigests(data []byte) ([]string, error) {
	var attestations []struct {
		VerificationResult struct {
			Signature struct {
				Certificate struct {
					Digest string `json:"buildSignerDigest"`
				}
			}
		}
	}
	if err := json.Unmarshal(data, &attestations); err != nil {
		return nil, err
	}
	if len(attestations) == 0 {
		return nil, fmt.Errorf("attestation has no signer")
	}
	var digests []string
	for _, attestation := range attestations {
		digest := attestation.VerificationResult.Signature.Certificate.Digest
		if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(digest) {
			return nil, fmt.Errorf("invalid attestation signer digest")
		}
		if !slices.Contains(digests, digest) {
			digests = append(digests, digest)
		}
	}
	return digests, nil
}

type PackageBuilder interface {
	Build(context.Context, release.Settings, string, string, string, string) error
}
type CollectOptions struct{ Registry, Work, Site, Channels string }

func Collect(ctx context.Context, source ReleaseSource, builder PackageBuilder, options CollectOptions) (Tools, error) {
	data, err := os.ReadFile(options.Registry)
	if err != nil {
		return nil, err
	}
	var registry Registry
	if err := yaml.Unmarshal(data, &registry); err != nil {
		return nil, err
	}
	for _, directory := range []string{options.Work, options.Site, options.Channels} {
		if err := os.MkdirAll(filepath.Dir(directory), 0755); err != nil {
			return nil, err
		}
		if err := os.Mkdir(directory, 0755); err != nil {
			return nil, err
		}
	}
	tools := Tools{}
	for _, project := range registry.Projects {
		if project.Visibility != "public" {
			continue
		}
		if !repositoryName.MatchString(project.Repository) || !toolName.MatchString(project.Project) {
			return nil, fmt.Errorf("invalid package project")
		}
		for _, tap := range project.Taps {
			if !toolName.MatchString(tap) {
				return nil, fmt.Errorf("invalid package tap")
			}
		}
		releases, err := source.Releases(ctx, project.Repository)
		if err != nil {
			return nil, err
		}
		for position, tag := range StableReleases(releases, 3) {
			directory := filepath.Join(options.Work, "releases", project.Project, tag)
			if err := os.MkdirAll(directory, 0755); err != nil {
				return nil, err
			}
			if err := source.Download(ctx, project.Repository, tag, directory); err != nil {
				return nil, err
			}
			sums, err := Checksums(filepath.Join(directory, "checksums.txt"))
			if err != nil {
				return nil, err
			}
			entries, err := os.ReadDir(directory)
			if err != nil {
				return nil, err
			}
			for _, entry := range entries {
				if entry.Name() == "checksums.txt" {
					continue
				}
				if !entry.Type().IsRegular() {
					return nil, fmt.Errorf("release assets must be regular files")
				}
				digest, ok := sums[entry.Name()]
				if !ok {
					return nil, fmt.Errorf("%s is not listed in checksums.txt", entry.Name())
				}
				path := filepath.Join(directory, entry.Name())
				if err := VerifyChecksum(path, digest); err != nil {
					return nil, err
				}
				if err := source.Verify(ctx, path, project.Repository, tag); err != nil {
					return nil, err
				}
			}
			data, err := os.ReadFile(filepath.Join(directory, "release.json"))
			if err != nil {
				return nil, err
			}
			var settings release.Settings
			if err := json.Unmarshal(data, &settings); err != nil {
				return nil, err
			}
			if settings.Repository != project.Repository || settings.Name != project.Project || !binaryName.MatchString(settings.Binary) {
				return nil, fmt.Errorf("release metadata names another project or invalid binary")
			}
			version := strings.TrimPrefix(tag, "v")
			if err := builder.Build(ctx, settings, version, directory, filepath.Join(options.Work, "payloads"), options.Site); err != nil {
				return nil, err
			}
			if position == 0 {
				destination := filepath.Join(options.Channels, settings.Name)
				if err := os.Mkdir(destination, 0755); err != nil {
					return nil, err
				}
				for _, suffix := range channelSuffixes {
					if err := copyFile(filepath.Join(directory, settings.Name+suffix), filepath.Join(destination, settings.Name+suffix), 0644); err != nil {
						return nil, err
					}
				}
				var summary map[string]any
				if err := json.Unmarshal(data, &summary); err != nil {
					return nil, err
				}
				summary["version"], summary["tag"] = version, tag
				if err := writeJSON(filepath.Join(destination, "release.json"), summary); err != nil {
					return nil, err
				}
				taps := project.Taps
				if taps == nil {
					taps = []string{}
				}
				tools[settings.Name] = Tool{Repository: project.Repository, Version: version, Binary: settings.Binary, Taps: taps}
			}
		}
	}
	if err := writeJSON(filepath.Join(options.Channels, "tools.json"), tools); err != nil {
		return nil, err
	}
	return tools, nil
}

func copyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(output, input)
	closeErr := output.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}
