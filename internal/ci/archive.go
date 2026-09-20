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
	decoder, err := zstd.NewReader(source, zstd.WithDecoderMaxMemory(256<<20))
	if err != nil {
		return err
	}
	defer decoder.Close()
	return ExtractTar(decoder, directory, requiredRoot, limit)
}

func ExtractTar(source io.Reader, directory, requiredRoot string, limit int64) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	valid := func(name string) bool {
		return filepath.IsLocal(name) && (requiredRoot == "" || name == requiredRoot || strings.HasPrefix(name, requiredRoot+"/"))
	}
	archive := tar.NewReader(source)
	var total int64
	var directories []*tar.Header
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Clean(strings.TrimPrefix(header.Name, "./")))
		if !valid(name) {
			return fmt.Errorf("archive entry escapes root")
		}
		if header.Size < 0 || header.Size > limit-total {
			return fmt.Errorf("archive exceeds extraction limit")
		}
		total += header.Size
		if err := root.MkdirAll(filepath.Dir(name), 0755); err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, os.FileMode(header.Mode)&0777|0700); err != nil {
				return err
			}
			header.Name = name
			directories = append(directories, header)
		case tar.TypeReg:
			file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode)&0777)
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
			if err := root.Chtimes(name, header.ModTime, header.ModTime); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(header.Linkname) {
				return fmt.Errorf("absolute archive link")
			}
			target := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(name), header.Linkname)))
			if !valid(target) {
				return fmt.Errorf("archive link escapes root")
			}
			if err := root.Symlink(header.Linkname, name); err != nil {
				return err
			}
		case tar.TypeLink:
			target := filepath.ToSlash(filepath.Clean(header.Linkname))
			if !valid(target) {
				return fmt.Errorf("archive hard link escapes root")
			}
			if err := root.Link(target, name); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry type %d", header.Typeflag)
		}
	}
	for index := len(directories) - 1; index >= 0; index-- {
		header := directories[index]
		if err := root.Chtimes(header.Name, header.ModTime, header.ModTime); err != nil {
			return err
		}
	}
	return nil
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
