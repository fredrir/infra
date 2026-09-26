package reconciler

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
)

func moduleProxy(cache string) string {
	return (&url.URL{Scheme: "file", Path: filepath.Join(cache, "go")}).String()
}

func moduleProxyFile(name string) bool {
	switch filepath.Ext(name) {
	case ".info", ".mod", ".zip":
		return true
	}
	return name == "list"
}

func mirrorModuleDownloads(downloaded, cache string) error {
	mirror := filepath.Join(cache, "go")
	if err := os.MkdirAll(mirror, 0o700); err != nil {
		return err
	}
	kept := map[string]bool{}
	err := filepath.WalkDir(downloaded, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(downloaded, path)
		switch {
		case err != nil:
			return err
		case entry.IsDir() && relative == "sumdb":
			return filepath.SkipDir
		case !entry.Type().IsRegular() || !moduleProxyFile(entry.Name()):
			return nil
		}
		kept[relative] = true
		if _, err := os.Lstat(filepath.Join(mirror, relative)); err == nil {
			return nil
		}
		return copyAtomically(path, filepath.Join(mirror, relative))
	})
	if err != nil {
		return err
	}
	var directories []string
	err = filepath.WalkDir(mirror, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(mirror, path)
		switch {
		case err != nil:
			return err
		case entry.IsDir():
			directories = append(directories, path)
		case !kept[relative]:
			return os.Remove(path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, directory := range slices.Backward(directories[1:]) {
		if entries, err := os.ReadDir(directory); err == nil && len(entries) == 0 {
			if err := os.Remove(directory); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyAtomically(source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.CreateTemp(filepath.Dir(destination), ".mirror-")
	if err != nil {
		return err
	}
	defer os.Remove(output.Name())
	_, err = io.Copy(output, input)
	if err = errors.Join(err, output.Close()); err != nil {
		return err
	}
	return os.Rename(output.Name(), destination)
}

func pluginCache(cache, source string) (string, error) {
	lock, err := os.ReadFile(filepath.Join(source, "tofu", ".terraform.lock.hcl"))
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(lock)
	root, current := filepath.Join(cache, "tofu"), hex.EncodeToString(digest[:])
	if err := os.MkdirAll(filepath.Join(root, current), 0o700); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.Name() != current {
			if err := removeTree(filepath.Join(root, entry.Name())); err != nil {
				return "", err
			}
		}
	}
	return filepath.Join(root, current), nil
}
