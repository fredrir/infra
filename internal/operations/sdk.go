package operations

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/fredrir/infra/internal/process"
	"github.com/klauspost/compress/zstd"
)

type SDKArchive struct {
	Path   string `json:"path"`
	Object string `json:"object"`
	SHA256 string `json:"sha256"`
}

func PackageMacOSSDK(ctx context.Context, destination string) (SDKArchive, error) {
	if runtime.GOOS != "darwin" {
		return SDKArchive{}, errors.New("Apple machine required")
	}
	path, e := process.Run(ctx, process.Options{Name: "xcrun", Args: []string{"--sdk", "macosx", "--show-sdk-path"}, Timeout: 30 * time.Second})
	if e != nil {
		return SDKArchive{}, e
	}
	version, e := process.Run(ctx, process.Options{Name: "xcrun", Args: []string{"--sdk", "macosx", "--show-sdk-version"}, Timeout: 30 * time.Second})
	if e != nil {
		return SDKArchive{}, e
	}
	return packageSDK(ctx, strings.TrimSpace(string(path.Stdout)), strings.TrimSpace(string(version.Stdout)), destination)
}

func packageSDK(ctx context.Context, root, version, destination string) (result SDKArchive, err error) {
	if !regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`).MatchString(version) {
		return result, errors.New("invalid SDK version")
	}
	root, e := filepath.EvalSymlinks(root)
	if e != nil {
		return result, e
	}
	root, e = filepath.Abs(root)
	if e != nil {
		return result, e
	}
	st, e := os.Stat(root)
	if e != nil {
		return result, e
	}
	if !st.IsDir() {
		return result, errors.New("SDK directory required")
	}
	if e = os.MkdirAll(destination, 0755); e != nil {
		return result, e
	}
	destination, e = filepath.EvalSymlinks(destination)
	if e != nil {
		return result, e
	}
	destination, e = filepath.Abs(destination)
	if e != nil {
		return result, e
	}
	rel, e := filepath.Rel(root, destination)
	if e != nil {
		return result, e
	}
	if rel == "." || filepath.IsLocal(rel) {
		return result, errors.New("archive destination must be outside SDK")
	}
	result.Object = "MacOSX" + version + ".sdk.tar.zst"
	result.Path = filepath.Join(destination, result.Object)
	if _, e = os.Lstat(result.Path); !os.IsNotExist(e) {
		return result, errors.New("SDK archive already exists")
	}
	f, e := os.CreateTemp(destination, ".sdk-")
	if e != nil {
		return result, e
	}
	defer os.Remove(f.Name())
	zw, e := zstd.NewWriter(f, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithEncoderConcurrency(2))
	if e != nil {
		f.Close()
		return result, e
	}
	tw := tar.NewWriter(zw)
	links := map[[2]uint64]string{}
	e = filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		relative, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		name := "MacOSX.sdk"
		if relative != "." {
			name += "/" + filepath.ToSlash(relative)
		}
		target := ""
		if info.Mode()&os.ModeSymlink != 0 {
			target, e = os.Readlink(path)
			if e != nil {
				return e
			}
		} else if !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("unsupported SDK entry %s", name)
		}
		header, e := tar.FileInfoHeader(info, target)
		if e != nil {
			return e
		}
		header.Name = name
		if info.IsDir() {
			header.Name += "/"
		}
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""
		header.ModTime = time.Unix(0, 0)
		header.AccessTime = time.Time{}
		header.ChangeTime = time.Time{}
		if info.Mode().IsRegular() {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 {
				key := [2]uint64{uint64(stat.Dev), stat.Ino}
				if earlier, ok := links[key]; ok {
					header.Typeflag = tar.TypeLink
					header.Linkname = earlier
					header.Size = 0
				} else {
					links[key] = name
				}
			}
		}
		if e = tw.WriteHeader(header); e != nil {
			return e
		}
		if header.Typeflag != tar.TypeReg {
			return nil
		}
		input, e := os.Open(path)
		if e != nil {
			return e
		}
		_, e = io.Copy(tw, &cancelReader{ctx, input})
		return errors.Join(e, input.Close())
	})
	e = errors.Join(e, tw.Close(), zw.Close(), f.Sync(), f.Close())
	if e != nil {
		return result, e
	}
	f, e = os.Open(f.Name())
	if e != nil {
		return result, e
	}
	hash := sha256.New()
	_, e = io.Copy(hash, &cancelReader{ctx, f})
	e = errors.Join(e, f.Close())
	if e != nil {
		return result, e
	}
	result.SHA256 = hex.EncodeToString(hash.Sum(nil))
	if e = os.Link(f.Name(), result.Path); e != nil {
		return result, e
	}
	return result, nil
}

type cancelReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *cancelReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}
