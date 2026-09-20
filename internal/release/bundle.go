package release

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func VerifyChecksums(directory string) error {
	file, err := os.Open(filepath.Join(directory, "checksums.txt"))
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	count := 0
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 67 || line[64] != ' ' || (line[65] != ' ' && line[65] != '*') {
			return fmt.Errorf("invalid checksum entry")
		}
		digest, name := line[:64], line[66:]
		if _, err := hex.DecodeString(digest); err != nil {
			return fmt.Errorf("invalid checksum digest")
		}
		if !filepath.IsLocal(name) {
			return fmt.Errorf("checksum path escapes artifact directory")
		}
		path := filepath.Join(directory, name)
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		root, err := filepath.EvalSymlinks(directory)
		if err != nil {
			return err
		}
		root, err = filepath.Abs(root)
		if err != nil {
			return err
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, resolved)
		if err != nil || !filepath.IsLocal(relative) {
			return fmt.Errorf("checksum path escapes artifact directory")
		}
		actual, err := fileDigest(path)
		if err != nil {
			return err
		}
		if actual != digest {
			return fmt.Errorf("checksum mismatch for %s", name)
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("empty checksum manifest")
	}
	return nil
}

func VerifyExecutable(archive, binary, target string) error {
	archiveFile, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer archiveFile.Close()
	compressed, err := gzip.NewReader(archiveFile)
	if err != nil {
		return err
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	var program []byte
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(header.Name, "./")
		if !filepath.IsLocal(name) {
			return fmt.Errorf("archive path escapes root")
		}
		if name != binary {
			continue
		}
		if program != nil || header.Typeflag != tar.TypeReg || header.Mode&0111 == 0 || header.Size > 512<<20 {
			return fmt.Errorf("archive requires one bounded executable %s", binary)
		}
		program, err = io.ReadAll(reader)
		if err != nil {
			return err
		}
	}
	if len(program) == 0 {
		return fmt.Errorf("%s archive has no executable %s", target, binary)
	}
	if strings.Contains(target, "apple-darwin") {
		file, err := macho.NewFile(bytes.NewReader(program))
		if err != nil {
			return err
		}
		defer file.Close()
		cpu := macho.CpuAmd64
		if strings.HasPrefix(target, "aarch64") {
			cpu = macho.CpuArm64
		}
		if file.Magic != macho.Magic64 || file.Cpu != cpu {
			return fmt.Errorf("%s has an incompatible Mach-O binary", target)
		}
		return nil
	}
	file, err := elf.NewFile(bytes.NewReader(program))
	if err != nil {
		return err
	}
	defer file.Close()
	machine := elf.EM_X86_64
	if strings.HasPrefix(target, "aarch64") {
		machine = elf.EM_AARCH64
	}
	if file.Class != elf.ELFCLASS64 || file.Machine != machine {
		return fmt.Errorf("%s has an incompatible ELF binary", target)
	}
	if strings.HasSuffix(target, "musl") {
		for _, segment := range file.Progs {
			if segment.Type == elf.PT_INTERP {
				return fmt.Errorf("%s is not statically linked", target)
			}
		}
		libraries, err := file.ImportedLibraries()
		if err != nil {
			return err
		}
		if len(libraries) != 0 {
			return fmt.Errorf("%s imports dynamic libraries", target)
		}
	} else {
		symbols, err := file.ImportedSymbols()
		if err != nil {
			return err
		}
		for _, symbol := range symbols {
			if !strings.HasPrefix(symbol.Version, "GLIBC_") {
				continue
			}
			version := strings.TrimPrefix(symbol.Version, "GLIBC_")
			if strings.Count(version, ".") == 1 {
				version += ".0"
			}
			parsed, err := parseVersion(version)
			if err != nil {
				return fmt.Errorf("unrecognized glibc version %q", symbol.Version)
			}
			if compareVersion(parsed, [3]uint64{2, 28, 0}) > 0 {
				return fmt.Errorf("%s requires %s", target, symbol.Version)
			}
		}
	}
	return nil
}

func Bundle(dist, summary, notes, destination string) error {
	data, err := os.ReadFile(summary)
	if err != nil {
		return err
	}
	var settings Settings
	if err := json.Unmarshal(data, &settings); err != nil {
		return err
	}
	if !packageName.MatchString(settings.Name) || !packageName.MatchString(settings.Binary) {
		return fmt.Errorf("invalid release name or binary")
	}
	if err := VerifyChecksums(dist); err != nil {
		return err
	}
	for _, target := range []string{"x86_64-unknown-linux-gnu", "aarch64-unknown-linux-gnu", "x86_64-unknown-linux-musl", "aarch64-unknown-linux-musl", "x86_64-apple-darwin", "aarch64-apple-darwin"} {
		matches, err := filepath.Glob(filepath.Join(dist, settings.Name+"-"+target+"-v*.tar.gz"))
		if err != nil {
			return err
		}
		if len(matches) != 1 {
			return fmt.Errorf("expected one %s archive", target)
		}
		if err := VerifyExecutable(matches[0], settings.Binary, target); err != nil {
			return err
		}
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, ".release-bundle-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	assets := filepath.Join(staging, "assets")
	if err := os.Mkdir(assets, 0755); err != nil {
		return err
	}
	archives, err := filepath.Glob(filepath.Join(dist, "*.tar.gz"))
	if err != nil {
		return err
	}
	for _, source := range append(archives, filepath.Join(dist, "checksums.txt")) {
		if err := copyReleaseFile(source, filepath.Join(assets, filepath.Base(source))); err != nil {
			return err
		}
	}
	generated := map[string]string{
		"homebrew/Casks/" + settings.Name + ".rb":    settings.Name + ".rb",
		"aur/" + settings.Name + "-bin.pkgbuild":     settings.Name + "-bin.pkgbuild",
		"aur/" + settings.Name + "-bin.srcinfo":      settings.Name + "-bin.srcinfo",
		"aur/" + settings.Name + ".pkgbuild":         settings.Name + ".pkgbuild",
		"aur/" + settings.Name + ".srcinfo":          settings.Name + ".srcinfo",
		"nix/pkgs/" + settings.Name + "/default.nix": settings.Name + ".nix",
	}
	var checksummed []string
	for source, name := range generated {
		if err := copyReleaseFile(filepath.Join(dist, source), filepath.Join(assets, name)); err != nil {
			return err
		}
		info, err := os.Stat(filepath.Join(assets, name))
		if err != nil {
			return err
		}
		if info.Size() == 0 {
			return fmt.Errorf("empty generated release file %s", source)
		}
		checksummed = append(checksummed, name)
	}
	if err := copyReleaseFile(summary, filepath.Join(assets, "release.json")); err != nil {
		return err
	}
	installer := fmt.Sprintf("#!/bin/sh\nset -eu\ncurl --proto \"=https\" --tlsv1.2 -fsSL https://pkgs.fredrir.com/install.sh | sh -s -- %s \"$@\"\n", settings.Name)
	if err := os.WriteFile(filepath.Join(assets, settings.Name+"-installer.sh"), []byte(installer), 0644); err != nil {
		return err
	}
	checksummed = append(checksummed, "release.json", settings.Name+"-installer.sh")
	slices.Sort(checksummed)
	checksums, err := os.OpenFile(filepath.Join(assets, "checksums.txt"), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	for _, name := range checksummed {
		digest, err := fileDigest(filepath.Join(assets, name))
		if err != nil {
			checksums.Close()
			return err
		}
		if _, err := fmt.Fprintf(checksums, "%s  %s\n", digest, name); err != nil {
			checksums.Close()
			return err
		}
	}
	if err := checksums.Close(); err != nil {
		return err
	}
	if err := copyReleaseFile(notes, filepath.Join(staging, "notes.md")); err != nil {
		return err
	}
	if _, err := os.Stat(destination); err == nil {
		return fmt.Errorf("bundle destination already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(staging, destination)
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
func copyReleaseFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
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
