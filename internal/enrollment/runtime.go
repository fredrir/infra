package enrollment

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
)

func DisableCoreDumps() error {
	return syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0})
}

func RuntimeOperation(mode string, target Target, input io.Reader, runtimeRoot string, owner int) error {
	if err := ValidateTarget(target); err != nil {
		return err
	}
	if os.Geteuid() != owner || (mode != "preflight" && mode != "deliver" && mode != "cleanup") {
		return fmt.Errorf("invalid runtime operation")
	}
	if mode != "preflight" && !keyIDPattern.MatchString(target.KeyID) {
		return fmt.Errorf("key identity required")
	}
	rootInfo, err := os.Lstat(runtimeRoot)
	if err != nil {
		return err
	}
	if err := privateDirectory(rootInfo, owner); err != nil {
		return err
	}
	root, err := os.OpenRoot(runtimeRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	relative := strings.TrimPrefix(target.KeyFile, "/run/")
	parent := path.Dir(relative)
	current := ""
	if parent != "." {
		for _, part := range strings.Split(parent, "/") {
			current = path.Join(current, part)
			info, err := root.Lstat(current)
			if os.IsNotExist(err) {
				if mode != "deliver" {
					continue
				}
				if err := root.Mkdir(current, 0o700); err != nil {
					return err
				}
				info, err = root.Lstat(current)
			}
			if err != nil {
				return err
			}
			if err := privateDirectory(info, owner); err != nil {
				return err
			}
		}
	}
	receipt := relative + ".metadata.json"
	for _, name := range []string{relative, receipt} {
		_, err := root.Lstat(name)
		if mode != "cleanup" && err == nil {
			return fmt.Errorf("runtime key already exists")
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if mode == "preflight" {
		return nil
	}
	if mode == "deliver" {
		raw, err := io.ReadAll(io.LimitReader(input, 8193))
		if err != nil || len(raw) > 8192 {
			return fmt.Errorf("invalid key payload")
		}
		var payload struct {
			Key      string         `json:"key"`
			Metadata map[string]any `json:"metadata"`
		}
		if json.Unmarshal(raw, &payload) != nil || !authKeyPattern.MatchString(payload.Key) || !matchesTarget(payload.Metadata, target) {
			return fmt.Errorf("key payload identity differs")
		}
		digest := sha256.Sum256([]byte(payload.Key))
		payload.Metadata["keySha256"] = hex.EncodeToString(digest[:])
		metadata, err := json.Marshal(payload.Metadata)
		if err != nil {
			return err
		}
		if len(metadata) > 4096 {
			return fmt.Errorf("key receipt exceeds size limit")
		}
		if err := privateWrite(root, receipt, metadata); err != nil {
			return err
		}
		if err := privateWrite(root, relative, []byte(payload.Key+"\n")); err != nil {
			return errors.Join(err, root.Remove(receipt))
		}
		return nil
	}
	info, err := root.Lstat(receipt)
	if os.IsNotExist(err) {
		if _, err := root.Lstat(relative); !os.IsNotExist(err) {
			return fmt.Errorf("key has no ownership receipt")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := privateFile(info, owner, 4096); err != nil {
		return err
	}
	metadataBytes, err := root.ReadFile(receipt)
	if err != nil {
		return err
	}
	var metadata map[string]any
	if json.Unmarshal(metadataBytes, &metadata) != nil || !matchesTarget(metadata, target) {
		return fmt.Errorf("cleanup identity differs")
	}
	info, err = root.Lstat(relative)
	if err == nil {
		if err := privateFile(info, owner, 512); err != nil {
			return err
		}
		key, err := root.ReadFile(relative)
		if err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(strings.TrimSpace(string(key))))
		if metadata["keySha256"] != hex.EncodeToString(digest[:]) {
			return fmt.Errorf("runtime key differs from receipt")
		}
		if err := root.Remove(relative); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return root.Remove(receipt)
}

func matchesTarget(metadata map[string]any, target Target) bool {
	return metadata["id"] == target.KeyID && metadata["node"] == target.Node && metadata["role"] == target.Role
}

func privateDirectory(info os.FileInfo, owner int) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || int(stat.Uid) != owner || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("runtime directory must be owner-controlled")
	}
	return nil
}

func privateFile(info os.FileInfo, owner int, maxSize int64) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || int(stat.Uid) != owner || info.Mode().Perm()&0o077 != 0 || stat.Nlink != 1 || info.Size() > maxSize {
		return fmt.Errorf("runtime key file must be private and regular")
	}
	return nil
}

func privateWrite(root *os.Root, name string, data []byte) error {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	temporary := path.Join(path.Dir(name), "."+path.Base(name)+"."+hex.EncodeToString(random[:]))
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	return root.Link(temporary, name)
}
