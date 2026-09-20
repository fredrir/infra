package packages

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/fredrir/infra/internal/ci"
)

func SmokeInstall(ctx context.Context, runner ci.Runner, format, name, binary string) error {
	return SmokeInstallBatch(ctx, runner, format, []string{name, binary})
}

func SmokeInstallBatch(ctx context.Context, runner ci.Runner, format string, identities []string) error {
	return smokeInstallBatch(ctx, runner, format, identities, "/")
}

func smokeInstallBatch(ctx context.Context, runner ci.Runner, format string, identities []string, root string) error {
	if len(identities) == 0 || len(identities)%2 != 0 {
		return fmt.Errorf("package names and binaries must be nonempty pairs")
	}
	var names, binaries []string
	seen := map[string]bool{}
	for index := 0; index < len(identities); index += 2 {
		name, binary := identities[index], identities[index+1]
		if !toolName.MatchString(name) || !binaryName.MatchString(binary) || seen[name] {
			return fmt.Errorf("invalid or duplicate smoke package identity")
		}
		seen[name] = true
		names, binaries = append(names, name), append(binaries, binary)
	}
	switch format {
	case "deb":
		if err := os.WriteFile(filepath.Join(root, "etc/apt/sources.list.d/fredrir.list"), []byte("deb [signed-by=/repo/keys/fredrir.asc] file:/repo/deb stable main\n"), 0644); err != nil {
			return err
		}
		if err := runner.Run(ctx, "apt-get", "update", "-o", "Dir::Etc::sourcelist=/etc/apt/sources.list.d/fredrir.list", "-o", "Dir::Etc::sourceparts=-"); err != nil {
			return err
		}
		runner.Env = append(runner.Env, "DEBIAN_FRONTEND=noninteractive")
		if err := runner.Run(ctx, "apt-get", append([]string{"install", "-y", "--no-install-recommends", "-o", "Dir::Etc::sourcelist=/etc/apt/sources.list.d/fredrir.list", "-o", "Dir::Etc::sourceparts=-"}, names...)...); err != nil {
			return err
		}
	case "rpm":
		if err := runner.Run(ctx, "rpm", "--import", "/repo/keys/fredrir.asc"); err != nil {
			return err
		}
		if _, err := exec.LookPath("zypper"); err == nil {
			if err := runner.Run(ctx, "zypper", "--non-interactive", "addrepo", "--check", "--gpgcheck", "--refresh", "file:///repo/rpm/$basearch", "fredrir"); err != nil {
				return err
			}
			if err := runner.Run(ctx, "zypper", append([]string{"--non-interactive", "install", "--repo", "fredrir"}, names...)...); err != nil {
				return err
			}
		} else {
			if err := os.WriteFile(filepath.Join(root, "etc/yum.repos.d/fredrir.repo"), []byte("[fredrir]\nname=fredrir\nbaseurl=file:///repo/rpm/$basearch\ngpgcheck=1\nrepo_gpgcheck=1\ngpgkey=file:///repo/keys/fredrir.asc\n"), 0644); err != nil {
				return err
			}
			if err := runner.Run(ctx, "dnf", append([]string{"install", "-y", "--repo=fredrir"}, names...)...); err != nil {
				return err
			}
		}
	case "apk":
		if err := copyFile(filepath.Join(root, "repo/keys/fredrir.rsa.pub"), filepath.Join(root, "etc/apk/keys/fredrir.rsa.pub"), 0644); err != nil {
			return err
		}
		if err := runner.Run(ctx, "apk", append([]string{"add", "--no-cache", "--no-network", "--repository", "/repo/apk"}, names...)...); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown package format %q", format)
	}
	for _, binary := range binaries {
		if err := runner.Run(ctx, binary, "--version"); err != nil {
			return err
		}
	}
	return nil
}
