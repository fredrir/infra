package ci

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/sync/errgroup"
)

type ToolAsset struct{ URL, Digest, Member string }

var ToolAssets = map[string]ToolAsset{
	"goreleaser": {"https://github.com/goreleaser/goreleaser/releases/download/v2.18.2/goreleaser_Linux_x86_64.tar.gz", "0a96edc9d9bc594e4a41cc4d59467c182062910ab24d9d1f6dd7b667d32606d3", "goreleaser"},
	"nfpm":       {"https://github.com/goreleaser/nfpm/releases/download/v2.47.0/nfpm_2.47.0_Linux_x86_64.tar.gz", "0660ca602b2d2d2ae4781a06c692b3eeb9d437ffea05b831d76e41f4a3188783", "nfpm"},
	"git-cliff":  {"https://github.com/orhun/git-cliff/releases/download/v2.14.1/git-cliff-2.14.1-x86_64-unknown-linux-musl.tar.gz", "cba6ae86f0a4205784eed8ef049fe53c904138806e33a1bda2e25026b17198eb", "git-cliff-2.14.1/git-cliff"},
	"gh":         {"https://github.com/cli/cli/releases/download/v2.101.0/gh_2.101.0_linux_amd64.tar.gz", "9bca2d1c16825f109907a23307628a2f0698fbf99662b73a5cf0b020293072b8", "gh_2.101.0_linux_amd64/bin/gh"},
	"kustomize":  {"https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize/v5.8.1/kustomize_v5.8.1_linux_amd64.tar.gz", "029a7f0f4e1932c52a0476cf02a0fd855c0bb85694b82c338fc648dcb53a819d", "kustomize"},
	"trivy":      {"https://github.com/aquasecurity/trivy/releases/download/v0.74.0/trivy_0.74.0_Linux-64bit.tar.gz", "2ae6fe3ee734b7fdf11335663e18c75ea12dccc76062f09f164a3b0f8be4371a", "trivy"},
	"yq":         {"https://github.com/mikefarah/yq/releases/download/v4.53.6/yq_linux_amd64", "c5f056448f973ae7d39b5401949648a78f2dc1947d6a8eb65be60d5c504b9385", ""},
	"cosign":     {"https://github.com/sigstore/cosign/releases/download/v3.1.3/cosign-linux-amd64", "4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71", ""},
}

func InstallTools(ctx context.Context, temporary, pathOutput string, names []string) error {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return fmt.Errorf("tool assets require linux/amd64")
	}
	if temporary == "" {
		return fmt.Errorf("runner temporary directory is required")
	}
	for _, name := range names {
		if _, ok := toolAsset(name); !ok {
			return fmt.Errorf("unknown tool %q", name)
		}
	}
	directory := filepath.Join(temporary, "tools")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	group, installContext := errgroup.WithContext(ctx)
	group.SetLimit(4)
	for _, name := range names {
		asset, _ := toolAsset(name)
		group.Go(func() error {
			if err := InstallTool(installContext, client, asset, filepath.Join(directory, name)); err != nil {
				return fmt.Errorf("install %s: %w", name, err)
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	if pathOutput != "" {
		file, err := os.OpenFile(pathOutput, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(file, directory)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	return nil
}

func InstallTool(ctx context.Context, client *http.Client, asset ToolAsset, destination string) error {
	work, err := os.MkdirTemp(filepath.Dir(destination), ".tool-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("tool server returned HTTP %d", response.StatusCode)
	}
	download, err := os.Create(filepath.Join(work, "download"))
	if err != nil {
		return err
	}
	digest := sha256.New()
	_, err = io.Copy(&limitedWriter{Writer: io.MultiWriter(download, digest), remaining: 512 << 20}, response.Body)
	closeErr := download.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if hex.EncodeToString(digest.Sum(nil)) != asset.Digest {
		return fmt.Errorf("tool checksum mismatch")
	}
	path := filepath.Join(work, "download")
	if asset.Member != "" {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		compressed, err := gzip.NewReader(file)
		if err != nil {
			return err
		}
		defer compressed.Close()
		archive := tar.NewReader(compressed)
		path = filepath.Join(work, "binary")
		found := false
		for {
			header, err := archive.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if header.Name != asset.Member && header.Name != "./"+asset.Member {
				continue
			}
			if found || header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > 512<<20 {
				return fmt.Errorf("invalid tool archive member")
			}
			found = true
			output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
			if err != nil {
				return err
			}
			_, err = io.Copy(output, archive)
			closeErr := output.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
		if !found {
			return fmt.Errorf("tool archive has no %s", asset.Member)
		}
	}
	if err := os.Chmod(path, 0755); err != nil {
		return err
	}
	return os.Rename(path, destination)
}
