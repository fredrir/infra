package packages

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"dagger.io/dagger"
	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/pipeline"
	"golang.org/x/sync/errgroup"
)

//go:embed assets/fredrir.asc
var PublicGPG []byte

//go:embed assets/fredrir.rsa.pub
var PublicAPK []byte

//go:embed assets/aur_known_hosts
var AURKnownHosts []byte

const aptBuilder = "public.ecr.aws/docker/library/buildpack-deps:trixie-curl@sha256:04907bdd423bdac4bdf6b7ce84562eea9f75916b55be1fa279b271879f116802"
const rpmBuilder = "quay.io/fedora/fedora:44@sha256:a65912511863a7d25139928fb283da04367f2224f37313e1cbb59d44fa02e755"
const apkBuilder = "public.ecr.aws/docker/library/alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b"

type PipelineOptions struct {
	Root, Temporary, Binary string
	Log, Output             io.Writer
}

func Build(ctx context.Context, options PipelineOptions) error {
	if options.Temporary == "" || os.Getenv("PACKAGES_GPG_KEY") == "" || os.Getenv("PACKAGES_APK_KEY") == "" {
		return fmt.Errorf("package build needs signing keys and runner temporary directory")
	}
	binary, err := linuxBinary(options.Binary)
	if err != nil {
		return err
	}
	keys, err := os.MkdirTemp(options.Temporary, "package-keys-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(keys)
	gpgKey, apkKey := filepath.Join(keys, "gpg.key"), filepath.Join(keys, "fredrir.rsa")
	if err := os.WriteFile(gpgKey, []byte(os.Getenv("PACKAGES_GPG_KEY")+"\n"), 0600); err != nil {
		return err
	}
	if err := os.WriteFile(apkKey, []byte(os.Getenv("PACKAGES_APK_KEY")+"\n"), 0600); err != nil {
		return err
	}
	work := filepath.Join(options.Temporary, "packages")
	runner := ci.Runner{Stdout: options.Output, Stderr: options.Log, Env: []string{"RPM_SIGNING_KEY=" + gpgKey, "APK_SIGNING_KEY=" + apkKey}}
	tools, err := Collect(ctx, &GitHub{Runner: runner}, NFPM{Runner: runner}, CollectOptions{Registry: filepath.Join(options.Root, ".github/rust-projects.yaml"), Work: filepath.Join(work, "work"), Site: filepath.Join(work, "staging"), Channels: filepath.Join(work, "channels")})
	if err != nil {
		return err
	}
	for _, directory := range []string{"deb/pool", "rpm/x86_64", "rpm/aarch64", "apk/x86_64", "apk/aarch64"} {
		if err := os.MkdirAll(filepath.Join(work, "staging", directory), 0755); err != nil {
			return err
		}
	}
	client, err := connect(ctx, options.Root, options.Log)
	if err != nil {
		return err
	}
	defer client.Close()
	cli := client.Host().File(binary)
	staging := client.Host().Directory(filepath.Join(work, "staging"))
	gpgSecret := client.SetSecret("packages-gpg-key", os.Getenv("PACKAGES_GPG_KEY"))
	apkSecret := client.SetSecret("packages-apk-key", os.Getenv("PACKAGES_APK_KEY"))
	apt := client.Container(dagger.ContainerOpts{Platform: "linux/amd64"}).From(aptBuilder).
		WithExec([]string{"sed", "-i", "s|^URIs: http://|URIs: https://|", "/etc/apt/sources.list.d/debian.sources"}).
		WithExec([]string{"apt-get", "update", "-o", "APT::Update::Error-Mode=any"}).WithEnvVariable("DEBIAN_FRONTEND", "noninteractive").WithExec([]string{"apt-get", "install", "-y", "--no-install-recommends", "apt-utils", "gnupg"})
	rpm := client.Container(dagger.ContainerOpts{Platform: "linux/amd64"}).From(rpmBuilder).WithExec([]string{"dnf", "install", "-y", "--setopt=install_weak_deps=False", "createrepo_c", "gnupg2"})
	apk := client.Container(dagger.ContainerOpts{Platform: "linux/amd64"}).From(apkBuilder).WithExec([]string{"sed", "-i", "s|http://|https://|g", "/etc/apk/repositories"}).WithExec([]string{"apk", "add", "--no-cache", "abuild", "openssl"})
	index := func(container *dagger.Container, format, directory string, secret *dagger.Secret, public []byte) *dagger.Directory {
		return container.WithFile("/usr/local/bin/infra", cli, dagger.ContainerWithFileOpts{Permissions: 0755}).WithDirectory("/repo", staging.Directory(directory)).WithMountedSecret("/run/secrets/signing-key", secret).WithEnvVariable("SIGNING_PUBLIC_KEY", fmt.Sprintf("%x", sha256.Sum256(public))).WithExec([]string{"infra", "packages", "index", format, "/repo", "--key", "/run/secrets/signing-key"}).Directory("/repo")
	}
	site := client.Directory().WithDirectory("deb", index(apt, "apt", "deb", gpgSecret, PublicGPG)).WithDirectory("rpm", index(rpm, "rpm", "rpm", gpgSecret, PublicGPG)).WithDirectory("apk", index(apk, "apk", "apk", apkSecret, PublicAPK))
	if _, err := site.Export(ctx, filepath.Join(work, "site")); err != nil {
		return err
	}
	return BuildSite(filepath.Join(work, "site"), tools, PublicGPG, PublicAPK)
}

func connect(ctx context.Context, root string, log io.Writer) (*dagger.Client, error) {
	config, err := pipeline.ReadToolchain(root)
	if err != nil {
		return nil, err
	}
	client, err := dagger.Connect(ctx, dagger.WithLogOutput(log))
	if err != nil {
		return nil, err
	}
	version, err := client.Version(ctx)
	if err != nil {
		client.Close()
		return nil, err
	}
	if strings.TrimPrefix(version, "v") != config.Dagger {
		client.Close()
		return nil, fmt.Errorf("Dagger %s required, got %s", config.Dagger, version)
	}
	return client, nil
}

func linuxBinary(path string) (string, error) {
	var err error
	if path == "" {
		if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
			return "", fmt.Errorf("a prebuilt Linux amd64 infra binary is required")
		}
		path, err = os.Executable()
		if err != nil {
			return "", err
		}
	}
	file, err := elf.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if file.Class != elf.ELFCLASS64 || file.Machine != elf.EM_X86_64 {
		return "", fmt.Errorf("infra binary must target Linux amd64")
	}
	for _, segment := range file.Progs {
		if segment.Type == elf.PT_INTERP {
			return "", fmt.Errorf("infra binary must be statically linked")
		}
	}
	return path, nil
}

type SmokeTarget struct{ Image, Format string }

func SmokeTargets(scope string) ([]SmokeTarget, error) {
	targets := []SmokeTarget{{"public.ecr.aws/docker/library/debian:12@sha256:6ebd97fa83deb272194a2cf015b3d26a4d538e9ad3a7a79d544c8af5b0a01443", "deb"}, {rpmBuilder, "rpm"}, {apkBuilder, "apk"}}
	switch scope {
	case "quick":
		return targets, nil
	case "full":
		return append(targets, SmokeTarget{"public.ecr.aws/docker/library/ubuntu:22.04@sha256:829f6df217bcbae2b371026e81711d1a787c61b2967ad09d015063663ebafbf7", "deb"}, SmokeTarget{"public.ecr.aws/docker/library/ubuntu:24.04@sha256:69cecf4bbf72d2d44a9eef1b71fb98c7fb973d78af11399deccef19beb008ad9", "deb"}, SmokeTarget{"quay.io/rockylinux/rockylinux:9@sha256:8101994123cf3d0a8fee517bee7f39e555c7d92bd2d9eb3303cc988a0eeed00f", "rpm"}, SmokeTarget{"registry.opensuse.org/opensuse/leap:16.0@sha256:6a8998a33df6164d29d545c1bb8d9dd5a3595206d993b1a43c54de9aa33d8feb", "rpm"}), nil
	default:
		return nil, fmt.Errorf("unknown smoke scope %q", scope)
	}
}

func Smoke(ctx context.Context, options PipelineOptions, site, channels, scope string) error {
	targets, err := SmokeTargets(scope)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(channels, "tools.json"))
	if err != nil {
		return err
	}
	var tools Tools
	if err := json.Unmarshal(data, &tools); err != nil {
		return err
	}
	if _, err := Installer(tools); err != nil {
		return err
	}
	if len(tools) == 0 {
		_, err := fmt.Fprintln(options.Output, "No packages to test")
		return err
	}
	binary, err := linuxBinary(options.Binary)
	if err != nil {
		return err
	}
	client, err := connect(ctx, options.Root, options.Log)
	if err != nil {
		return err
	}
	defer client.Close()
	cli := client.Host().File(binary)
	repository := client.Host().Directory(site)
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	batches := smokePackageBatches(names, tools, scope)
	if err := smokeDistributions(ctx, targets, func(ctx context.Context, target SmokeTarget) error {
		base := client.Container(dagger.ContainerOpts{Platform: "linux/amd64"}).From(target.Image).WithFile("/usr/local/bin/infra", cli, dagger.ContainerWithFileOpts{Permissions: 0755}).WithDirectory("/repo", repository)
		for _, identities := range batches {
			args := append([]string{"infra", "packages", "smoke-install", target.Format}, identities...)
			if _, err := base.WithExec(args).Sync(ctx); err != nil {
				return fmt.Errorf("packages %v on %s: %w", identities, target.Image, err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, target := range targets {
		for _, name := range names {
			fmt.Fprintf(options.Output, "%s on %s: ok\n", name, target.Image)
		}
	}
	return nil
}

func smokeDistributions(ctx context.Context, targets []SmokeTarget, check func(context.Context, SmokeTarget) error) error {
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(2)
	for _, target := range targets {
		group.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return check(ctx, target)
		})
	}
	return group.Wait()
}

func smokePackageBatches(names []string, tools Tools, scope string) [][]string {
	var batches [][]string
	var combined []string
	for _, name := range names {
		identity := []string{name, tools[name].Binary}
		if scope == "full" {
			batches = append(batches, identity)
		} else {
			combined = append(combined, identity...)
		}
	}
	if len(combined) > 0 {
		batches = append(batches, combined)
	}
	return batches
}
