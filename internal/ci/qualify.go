package ci

import (
	"context"
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"
)

func QualifyRustToolchain(ctx context.Context, runner Runner) error {
	directory, err := os.MkdirTemp("", "infra-rust-qualification-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	runner.Dir = directory
	if err := runner.Run(ctx, "cargo", "new", "--vcs", "none", "probe"); err != nil {
		return err
	}
	runner.Dir = filepath.Join(directory, "probe")
	if err := runner.Run(ctx, "cargo", "zigbuild", "--release", "--target", "aarch64-unknown-linux-gnu.2.28", "--target", "x86_64-unknown-linux-musl"); err != nil {
		return err
	}
	for target, architecture := range map[string]elf.Machine{"aarch64-unknown-linux-gnu": elf.EM_AARCH64, "x86_64-unknown-linux-musl": elf.EM_X86_64} {
		binary, err := elf.Open(filepath.Join(runner.Dir, "target", target, "release", "probe"))
		if err != nil {
			return err
		}
		if binary.Machine != architecture {
			binary.Close()
			return fmt.Errorf("wrong architecture for %s", target)
		}
		if target == "x86_64-unknown-linux-musl" {
			for _, program := range binary.Progs {
				if program.Type == elf.PT_INTERP {
					binary.Close()
					return fmt.Errorf("musl binary requires a dynamic interpreter")
				}
			}
		}
		binary.Close()
	}
	return nil
}
