package packages

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/fredrir/infra/internal/ci"
)

func SmokeInstall(ctx context.Context, runner ci.Runner, format, name, binary string) error {
	if !toolName.MatchString(name) || !binaryName.MatchString(binary) {
		return fmt.Errorf("invalid smoke package identity")
	}
	switch format {
	case "deb":
		if err := os.WriteFile("/etc/apt/sources.list.d/fredrir.list", []byte("deb [signed-by=/repo/keys/fredrir.asc] file:/repo/deb stable main\n"), 0644); err != nil {
			return err
		}
		if err := runner.Run(ctx, "apt-get", "update", "-o", "Dir::Etc::sourcelist=/etc/apt/sources.list.d/fredrir.list", "-o", "Dir::Etc::sourceparts=-"); err != nil {
			return err
		}
		runner.Env = append(runner.Env, "DEBIAN_FRONTEND=noninteractive")
		if err := runner.Run(ctx, "apt-get", "install", "-y", "--no-install-recommends", "-o", "Dir::Etc::sourcelist=/etc/apt/sources.list.d/fredrir.list", "-o", "Dir::Etc::sourceparts=-", name); err != nil {
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
			if err := runner.Run(ctx, "zypper", "--non-interactive", "install", "--repo", "fredrir", name); err != nil {
				return err
			}
		} else {
			if err := os.WriteFile("/etc/yum.repos.d/fredrir.repo", []byte("[fredrir]\nname=fredrir\nbaseurl=file:///repo/rpm/$basearch\ngpgcheck=1\nrepo_gpgcheck=1\ngpgkey=file:///repo/keys/fredrir.asc\n"), 0644); err != nil {
				return err
			}
			if err := runner.Run(ctx, "dnf", "install", "-y", "--repo=fredrir", name); err != nil {
				return err
			}
		}
	case "apk":
		if err := copyFile("/repo/keys/fredrir.rsa.pub", "/etc/apk/keys/fredrir.rsa.pub", 0644); err != nil {
			return err
		}
		if err := runner.Run(ctx, "apk", "add", "--no-cache", "--no-network", "--repository", "/repo/apk", name); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown package format %q", format)
	}
	return runner.Run(ctx, binary, "--version")
}
