package ci

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

func ExtractZstd(source io.Reader, directory, requiredRoot string, limit int64) error {
	resolvedDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return err
	}
	directory, err = filepath.Abs(resolvedDirectory)
	if err != nil {
		return err
	}
	decoder, err := zstd.NewReader(source, zstd.WithDecoderMaxMemory(256<<20))
	if err != nil {
		return err
	}
	defer decoder.Close()
	archive := tar.NewReader(decoder)
	var total int64
	for {
		header, err := archive.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Clean(strings.TrimPrefix(header.Name, "./")))
		if !filepath.IsLocal(name) || (name != requiredRoot && !strings.HasPrefix(name, requiredRoot+"/")) {
			return fmt.Errorf("archive entry escapes %s", requiredRoot)
		}
		if header.Size < 0 || header.Size > limit-total {
			return fmt.Errorf("archive exceeds extraction limit")
		}
		total += header.Size
		path := filepath.Join(directory, filepath.FromSlash(name))
		parent := filepath.Dir(path)
		if err := os.MkdirAll(parent, 0755); err != nil {
			return err
		}
		resolved, err := filepath.EvalSymlinks(parent)
		if err != nil {
			return err
		}
		absolute, err := filepath.Abs(parent)
		if err != nil {
			return err
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return err
		}
		if absolute != resolved {
			return fmt.Errorf("archive entry traverses a symlink")
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, os.FileMode(header.Mode)&0777); err != nil {
				return err
			}
		case tar.TypeReg:
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode)&0777)
			if err != nil {
				return err
			}
			_, err = io.Copy(file, archive)
			closeErr := file.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(header.Linkname) {
				return fmt.Errorf("absolute archive link")
			}
			target := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(name), header.Linkname)))
			if target != requiredRoot && !strings.HasPrefix(target, requiredRoot+"/") {
				return fmt.Errorf("archive link escapes root")
			}
			if err := os.Symlink(header.Linkname, path); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry type %d", header.Typeflag)
		}
	}
}

func WriteZstd(destination io.Writer, directory, root string) error {
	encoder, err := zstd.NewWriter(destination, zstd.WithEncoderConcurrency(2), zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return err
	}
	archive := tar.NewWriter(encoder)
	err = filepath.WalkDir(filepath.Join(directory, root), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		link := ""
		if entry.Type()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name, err = filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(header.Name)
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(archive, file)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
	archiveErr := archive.Close()
	encoderErr := encoder.Close()
	if err != nil {
		return err
	}
	if archiveErr != nil {
		return archiveErr
	}
	return encoderErr
}
