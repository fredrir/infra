package packages

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dagger.io/dagger"
	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/release"
)

func TestSignedPackageQualification(t *testing.T) {
	if os.Getenv("INFRA_PACKAGE_QUALIFY") != "1" {
		t.Skip("requires Linux, Go, nfpm, GPG, OpenSSL and a bounded Dagger engine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	work := t.TempDir()
	runner := ci.Runner{Dir: work, Stdout: os.Stdout, Stderr: os.Stderr}
	home := filepath.Join(work, "gnupg")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	runner.Env = []string{"GNUPGHOME=" + home}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = runner.Run(cleanup, "gpgconf", "--kill", "all")
	})
	run := func(name string, args ...string) {
		t.Helper()
		if err := runner.Run(ctx, name, args...); err != nil {
			t.Fatal(err)
		}
	}
	run("gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "", "--quick-generate-key", "Infra Qualification <qualification.invalid@example.invalid>", "rsa2048", "sign", "1d")
	gpgSecret := filepath.Join(work, "gpg.key")
	gpgPublic := filepath.Join(work, "gpg.pub")
	run("gpg", "--batch", "--armor", "--output", gpgSecret, "--export-secret-keys")
	run("gpg", "--batch", "--armor", "--output", gpgPublic, "--export")
	apkSecret := filepath.Join(work, "fredrir.rsa")
	apkPublic := filepath.Join(work, "fredrir.rsa.pub")
	run("openssl", "genrsa", "-out", apkSecret, "2048")
	run("openssl", "rsa", "-in", apkSecret, "-pubout", "-out", apkPublic)
	source := filepath.Join(work, "main.go")
	if err := os.WriteFile(source, []byte("package main\nimport \"fmt\"\nfunc main(){fmt.Println(\"infra-qualification 1.0.0\")}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	releases := filepath.Join(work, "releases")
	if err := os.Mkdir(releases, 0700); err != nil {
		t.Fatal(err)
	}
	for _, arch := range []struct{ goarch, triple string }{{"amd64", "x86_64"}, {"arm64", "aarch64"}} {
		binary := filepath.Join(work, "fixture-"+arch.goarch)
		compiler := runner
		compiler.Env = append(append([]string{}, runner.Env...), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch.goarch)
		if err := compiler.Run(ctx, "go", "build", "-trimpath", "-o", binary, source); err != nil {
			t.Fatal(err)
		}
		for _, flavour := range []string{"gnu", "musl"} {
			out, err := os.Create(filepath.Join(releases, "infra-qualification-"+arch.triple+"-unknown-linux-"+flavour+"-v1.0.0.tar.gz"))
			if err != nil {
				t.Fatal(err)
			}
			gz := gzip.NewWriter(out)
			tw := tar.NewWriter(gz)
			data, err := os.ReadFile(binary)
			if err != nil {
				t.Fatal(err)
			}
			if err := tw.WriteHeader(&tar.Header{Name: "infra-qualification", Mode: 0755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(data); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			if err := out.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	runner.Env = append(runner.Env, "RPM_SIGNING_KEY="+gpgSecret, "APK_SIGNING_KEY="+apkSecret)
	site := filepath.Join(work, "site")
	settings := release.Settings{Name: "infra-qualification", Binary: "infra-qualification", Repository: "fredrir/infra", Maintainer: "Qualification <qualification.invalid@example.invalid>", Description: "Isolated package qualification fixture", License: "MIT", Homepage: "https://example.invalid"}
	if err := (NFPM{Runner: runner}).Build(ctx, settings, "1.0.0", releases, filepath.Join(work, "payloads"), site); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(site, "keys"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(gpgPublic, filepath.Join(site, "keys/fredrir.asc"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(apkPublic, filepath.Join(site, "keys/fredrir.rsa.pub"), 0644); err != nil {
		t.Fatal(err)
	}
	client, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	cli := client.Host().File(os.Getenv("INFRA_QUALIFICATION_BINARY"))
	gpgData, err := os.ReadFile(gpgSecret)
	if err != nil {
		t.Fatal(err)
	}
	apkData, err := os.ReadFile(apkSecret)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"apt", "rpm", "apk"} {
		t.Run(format, func(t *testing.T) {
			directory, image, smokeFormat, key := "deb", aptBuilder, "deb", gpgData
			var install []string
			switch format {
			case "apt":
				install = []string{"apt-get", "install", "-y", "--no-install-recommends", "apt-utils", "gnupg"}
			case "rpm":
				directory, image, smokeFormat = "rpm", rpmBuilder, "rpm"
				install = []string{"dnf", "install", "-y", "--setopt=install_weak_deps=False", "createrepo_c", "gnupg2"}
			case "apk":
				directory, image, smokeFormat, key = "apk", apkBuilder, "apk", apkData
				install = []string{"apk", "add", "--no-cache", "abuild", "openssl"}
			}
			container := client.Container(dagger.ContainerOpts{Platform: "linux/amd64"}).From(image)
			if format == "apt" {
				container = container.WithExec([]string{"sed", "-i", "s|^URIs: http://|URIs: https://|", "/etc/apt/sources.list.d/debian.sources"}).WithExec([]string{"apt-get", "update", "-o", "APT::Update::Error-Mode=any"})
			}
			if format == "apk" {
				container = container.WithExec([]string{"sed", "-i", "s|http://|https://|g", "/etc/apk/repositories"})
			}
			container = container.WithExec(install).WithFile("/usr/local/bin/infra", cli, dagger.ContainerWithFileOpts{Permissions: 0755}).WithDirectory("/index", client.Host().Directory(filepath.Join(site, directory))).WithMountedSecret("/run/secrets/key", client.SetSecret("qualification-"+format, string(key))).WithExec([]string{"infra", "packages", "index", format, "/index", "--key", "/run/secrets/key"})
			if _, err := container.Directory("/index").Export(ctx, filepath.Join(site, directory)); err != nil {
				t.Fatal(err)
			}
			smokeImage := image
			if format == "apt" {
				smokeImage = "public.ecr.aws/docker/library/debian:12@sha256:6ebd97fa83deb272194a2cf015b3d26a4d538e9ad3a7a79d544c8af5b0a01443"
			}
			smoke := client.Container(dagger.ContainerOpts{Platform: "linux/amd64"}).From(smokeImage).WithFile("/usr/local/bin/infra", cli, dagger.ContainerWithFileOpts{Permissions: 0755}).WithDirectory("/repo", client.Host().Directory(site).WithDirectory(directory, container.Directory("/index"))).WithExec([]string{"infra", "packages", "smoke-install", smokeFormat, "infra-qualification", "infra-qualification"})
			result, err := smoke.Stdout(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(os.Stdout, result); err != nil {
				t.Fatal(err)
			}
		})
	}
}
