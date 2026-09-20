package artifact

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type InstallOptions struct {
	URL         string
	SHA256      string
	Revision    string
	Platform    string
	CacheDir    string
	Destination string
	Client      *http.Client
}

type Receipt struct {
	Schema          int    `json:"schema"`
	Revision        string `json:"revision"`
	Platform        string `json:"platform"`
	SHA256          string `json:"sha256"`
	Path            string `json:"path"`
	CacheHit        bool   `json:"cache_hit"`
	DownloadedBytes int64  `json:"downloaded_bytes"`
}

func Install(ctx context.Context, o InstallOptions) (Receipt, error) {
	result := Receipt{Schema: 1, Revision: o.Revision, Platform: o.Platform, SHA256: o.SHA256}
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(o.Revision) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(o.SHA256) {
		return result, fmt.Errorf("full revision and SHA-256 required")
	}
	if o.Platform != "linux/amd64" && o.Platform != "linux/arm64" && o.Platform != "darwin/amd64" && o.Platform != "darwin/arm64" {
		return result, fmt.Errorf("unsupported artifact platform")
	}
	if o.CacheDir == "" {
		return result, fmt.Errorf("artifact cache directory required")
	}
	directory := filepath.Join(o.CacheDir, o.Revision, strings.ReplaceAll(o.Platform, "/", "-"), o.SHA256)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return result, err
	}
	path := filepath.Join(directory, "infra")
	if err := verify(path, o.SHA256, o.Platform); err == nil {
		result.CacheHit = true
	} else {
		u, err := url.Parse(o.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return result, fmt.Errorf("artifact URL must use HTTPS")
		}
		client := o.Client
		if client == nil {
			client = &http.Client{Timeout: 2 * time.Minute}
		}
		copyClient := *client
		originalRedirect := client.CheckRedirect
		copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" || len(via) >= 10 {
				return fmt.Errorf("unsafe artifact redirect")
			}
			if originalRedirect != nil {
				return originalRedirect(req, via)
			}
			return nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.URL, nil)
		if err != nil {
			return result, fmt.Errorf("invalid artifact request")
		}
		response, err := copyClient.Do(req)
		if err != nil {
			return result, fmt.Errorf("artifact download failed")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return result, fmt.Errorf("artifact download HTTP %d", response.StatusCode)
		}
		file, err := os.CreateTemp(directory, ".download-*")
		if err != nil {
			return result, err
		}
		defer os.Remove(file.Name())
		result.DownloadedBytes, err = io.Copy(file, io.LimitReader(response.Body, (256<<20)+1))
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return result, err
		}
		if result.DownloadedBytes > 256<<20 {
			return result, fmt.Errorf("artifact exceeds size limit")
		}
		if err := verify(file.Name(), o.SHA256, o.Platform); err != nil {
			return result, err
		}
		if err := os.Chmod(file.Name(), 0o555); err != nil {
			return result, err
		}
		if err := os.Rename(file.Name(), path); err != nil {
			return result, err
		}
	}
	result.Path = path
	if o.Destination != "" {
		if err := installCopy(path, o.Destination); err != nil {
			return result, err
		}
		result.Path = o.Destination
	}
	return result, nil
}

func verify(path, digest, platform string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("artifact is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return fmt.Errorf("artifact checksum mismatch")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if strings.HasPrefix(platform, "linux/") {
		binary, err := elf.NewFile(file)
		if err != nil {
			return fmt.Errorf("invalid Linux binary")
		}
		defer binary.Close()
		want := elf.EM_X86_64
		if strings.HasSuffix(platform, "/arm64") {
			want = elf.EM_AARCH64
		}
		if binary.Machine != want || binary.Class != elf.ELFCLASS64 {
			return fmt.Errorf("artifact architecture mismatch")
		}
		for _, program := range binary.Progs {
			if program.Type == elf.PT_INTERP {
				return fmt.Errorf("Linux artifact must be static")
			}
		}
	} else {
		binary, err := macho.NewFile(file)
		if err != nil {
			return fmt.Errorf("invalid Darwin binary")
		}
		defer binary.Close()
		want := macho.CpuAmd64
		if strings.HasSuffix(platform, "/arm64") {
			want = macho.CpuArm64
		}
		if binary.Cpu != want {
			return fmt.Errorf("artifact architecture mismatch")
		}
	}
	return nil
}

func installCopy(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.CreateTemp(filepath.Dir(destination), ".infra-install-*")
	if err != nil {
		return err
	}
	defer os.Remove(output.Name())
	_, copyErr := io.Copy(output, input)
	err = errors.Join(copyErr, output.Chmod(0o755), output.Sync(), output.Close())
	if err != nil {
		return err
	}
	return os.Rename(output.Name(), destination)
}
