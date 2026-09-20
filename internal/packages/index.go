package packages

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/fredrir/infra/internal/ci"
)

func Index(ctx context.Context, runner ci.Runner, format, directory, keyPath string) error {
	root, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	keyPath, err = filepath.Abs(keyPath)
	if err != nil {
		return err
	}
	runner.Dir = root
	if format == "apk" {
		return indexAPK(ctx, runner, keyPath)
	}
	if format != "apt" && format != "rpm" {
		return fmt.Errorf("unsupported package index %q", format)
	}
	home, err := os.MkdirTemp("", "infra-gpg-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	runner.Env = append(runner.Env, "GNUPGHOME="+home)
	if err := runner.Run(ctx, "gpg", "--batch", "--quiet", "--import", keyPath); err != nil {
		return err
	}
	listing, err := runner.Output(ctx, "gpg", "--batch", "--with-colons", "--list-secret-keys")
	if err != nil {
		return err
	}
	key := ""
	for _, line := range strings.Split(string(listing), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) > 9 && fields[0] == "fpr" {
			key = fields[9]
			break
		}
	}
	if key == "" {
		return fmt.Errorf("no GPG signing key imported")
	}
	if format == "rpm" {
		for _, architecture := range []string{"x86_64", "aarch64"} {
			if err := runner.Run(ctx, "createrepo_c", "--quiet", "--general-compress-type=gz", architecture); err != nil {
				return err
			}
			path := filepath.Join(architecture, "repodata/repomd.xml")
			if err := runner.Run(ctx, "gpg", "--batch", "--yes", "--local-user", key, "--digest-algo", "SHA512", "--armor", "--detach-sign", "--output", path+".asc", path); err != nil {
				return err
			}
		}
		return nil
	}
	for _, architecture := range []string{"amd64", "arm64"} {
		directory := filepath.Join(root, "dists/stable/main/binary-"+architecture)
		if err := os.MkdirAll(filepath.Join(directory, "by-hash/SHA256"), 0755); err != nil {
			return err
		}
		path := filepath.Join(directory, "Packages")
		if err := runToFile(ctx, runner, path, "apt-ftparchive", "--arch", architecture, "packages", "pool"); err != nil {
			return err
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		output, err := os.Create(path + ".gz")
		if err != nil {
			input.Close()
			return err
		}
		compressor, err := gzip.NewWriterLevel(output, gzip.BestCompression)
		if err != nil {
			input.Close()
			output.Close()
			return err
		}
		_, copyErr := io.Copy(compressor, input)
		input.Close()
		compressErr := compressor.Close()
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if compressErr != nil {
			return compressErr
		}
		if closeErr != nil {
			return closeErr
		}
		for _, name := range []string{"Packages", "Packages.gz"} {
			data, err := os.ReadFile(filepath.Join(directory, name))
			if err != nil {
				return err
			}
			digest := fmt.Sprintf("%x", sha256.Sum256(data))
			if err := os.WriteFile(filepath.Join(directory, "by-hash/SHA256", digest), data, 0644); err != nil {
				return err
			}
		}
	}
	arguments := []string{}
	for _, setting := range []string{"Origin=fredrir", "Label=fredrir", "Suite=stable", "Codename=stable", "Architectures=amd64 arm64", "Components=main", "Description=fredrir packages", "Acquire-By-Hash=yes"} {
		arguments = append(arguments, "-o", "APT::FTPArchive::Release::"+setting)
	}
	arguments = append(arguments, "release", "dists/stable")
	if err := runToFile(ctx, runner, filepath.Join(home, "Release"), "apt-ftparchive", arguments...); err != nil {
		return err
	}
	if err := copyFile(filepath.Join(home, "Release"), filepath.Join(root, "dists/stable/Release"), 0644); err != nil {
		return err
	}
	if err := runner.Run(ctx, "gpg", "--batch", "--yes", "--local-user", key, "--digest-algo", "SHA512", "--clearsign", "--output", "dists/stable/InRelease", "dists/stable/Release"); err != nil {
		return err
	}
	return runner.Run(ctx, "gpg", "--batch", "--yes", "--local-user", key, "--digest-algo", "SHA512", "--armor", "--detach-sign", "--output", "dists/stable/Release.gpg", "dists/stable/Release")
}

func indexAPK(ctx context.Context, runner ci.Runner, keyPath string) error {
	keys, err := os.MkdirTemp("", "infra-apk-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(keys)
	private := filepath.Join(keys, "fredrir.rsa")
	if err := copyFile(keyPath, private, 0600); err != nil {
		return err
	}
	if err := runner.Run(ctx, "openssl", "rsa", "-in", private, "-pubout", "-out", "/etc/apk/keys/fredrir.rsa.pub"); err != nil {
		return err
	}
	for _, architecture := range []string{"x86_64", "aarch64"} {
		local := runner
		local.Dir = filepath.Join(runner.Dir, architecture)
		packages, err := filepath.Glob(filepath.Join(local.Dir, "*.apk"))
		if err != nil {
			return err
		}
		if len(packages) == 0 {
			continue
		}
		for index, path := range packages {
			packages[index] = filepath.Base(path)
		}
		args := []string{"index", "--quiet", "--rewrite-arch", architecture, "--description", "fredrir packages", "--output", "APKINDEX.tar.gz", "--"}
		if err := local.Run(ctx, "apk", append(args, packages...)...); err != nil {
			return err
		}
		if err := local.Run(ctx, "abuild-sign", "-q", "-k", private, "-p", "fredrir.rsa.pub", "APKINDEX.tar.gz"); err != nil {
			return err
		}
	}
	return nil
}

func runToFile(ctx context.Context, runner ci.Runner, path, name string, arguments ...string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	runner.Stdout = file
	err = runner.Run(ctx, name, arguments...)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
