package kata

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

type initrdEntry struct {
	name, source, target         string
	mode, uid, gid, major, minor uint32
	size                         int64
	linkKey                      string
	links                        uint32
}

func WriteInitrd(ctx context.Context, root string, w io.Writer) error {
	entries := []initrdEntry{}
	e := filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		name, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		name = filepath.ToSlash(name)
		if name == "dev" {
			return filepath.SkipDir
		}
		st, e := d.Info()
		if e != nil {
			return e
		}
		v := initrdEntry{name: name, source: p, size: st.Size()}
		if s, ok := st.Sys().(*syscall.Stat_t); ok {
			v.uid = s.Uid
			v.gid = s.Gid
			if st.Mode().IsRegular() && s.Nlink > 1 {
				v.linkKey = fmt.Sprintf("%d:%d", s.Dev, s.Ino)
			}
		}
		v.mode = uint32(st.Mode().Perm())
		if st.Mode()&os.ModeSetuid != 0 {
			v.mode |= 04000
		}
		if st.Mode()&os.ModeSetgid != 0 {
			v.mode |= 02000
		}
		if st.Mode()&os.ModeSticky != 0 {
			v.mode |= 01000
		}
		switch {
		case st.IsDir():
			v.mode |= 0040000
			v.size = 0
		case st.Mode().IsRegular():
			v.mode |= 0100000
		case st.Mode()&os.ModeSymlink != 0:
			v.mode |= 0120000
			v.target, e = os.Readlink(p)
			if e != nil {
				return e
			}
			v.size = int64(len(v.target))
		default:
			return fmt.Errorf("unsupported initrd file %s", name)
		}
		entries = append(entries, v)
		return nil
	})
	if e != nil {
		return e
	}
	entries = append(entries, initrdEntry{name: "dev", mode: 0040755})
	for _, d := range []struct {
		name               string
		major, minor, mode uint32
	}{{"console", 5, 1, 0600}, {"tty", 5, 0, 0666}, {"null", 1, 3, 0666}, {"zero", 1, 5, 0666}, {"ttyS0", 4, 64, 0660}, {"ttyS1", 4, 65, 0660}, {"ttyS2", 4, 66, 0660}, {"ttyS3", 4, 67, 0660}} {
		gid := uint32(0)
		if d.name == "tty" || d.name == "console" {
			gid = 5
		}
		if strings.HasPrefix(d.name, "ttyS") {
			gid = 20
		}
		entries = append(entries, initrdEntry{name: "dev/" + d.name, mode: 0020000 | d.mode, gid: gid, major: d.major, minor: d.minor})
	}
	for _, s := range []struct{ name, target string }{{"fd", "/proc/self/fd"}, {"stdin", "fd/0"}, {"stdout", "fd/1"}, {"stderr", "fd/2"}} {
		entries = append(entries, initrdEntry{name: "dev/" + s.name, mode: 0120777, target: s.target, size: int64(len(s.target))})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	g, e := gzip.NewWriterLevel(w, gzip.BestCompression)
	if e != nil {
		return e
	}
	g.Header.OS = 3
	groups := map[string][]int{}
	for i, v := range entries {
		if v.linkKey != "" {
			groups[v.linkKey] = append(groups[v.linkKey], i)
		}
	}
	for _, group := range groups {
		for _, i := range group {
			entries[i].links = uint32(len(group))
			if i != group[len(group)-1] {
				entries[i].size = 0
				entries[i].source = ""
			}
		}
	}
	inodes := map[string]uint32{}
	next := uint32(1)
	for _, v := range entries {
		inode := next
		if v.linkKey != "" {
			if existing, ok := inodes[v.linkKey]; ok {
				inode = existing
			} else {
				inodes[v.linkKey] = inode
				next++
			}
		} else {
			next++
		}
		if e = writeCPIOEntry(ctx, g, inode, v); e != nil {
			g.Close()
			return e
		}
	}
	if e = writeCPIOEntry(ctx, g, 0, initrdEntry{name: "TRAILER!!!"}); e != nil {
		g.Close()
		return e
	}
	return g.Close()
}

func writeCPIOEntry(ctx context.Context, w io.Writer, inode uint32, v initrdEntry) error {
	if v.size < 0 || v.size > 1<<32-1 {
		return fmt.Errorf("initrd member too large: %s", v.name)
	}
	if v.links == 0 {
		v.links = 1
	}
	header := fmt.Sprintf("070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x", inode, v.mode, v.uid, v.gid, v.links, 0, v.size, 0, 0, v.major, v.minor, len(v.name)+1, 0)
	if _, e := io.WriteString(w, header+v.name+"\x00"); e != nil {
		return e
	}
	if e := padding(w, int64(len(header)+len(v.name)+1)); e != nil {
		return e
	}
	if v.mode&0170000 == 0100000 && v.source != "" {
		f, e := os.Open(v.source)
		if e != nil {
			return e
		}
		n, e := io.Copy(w, &contextReader{ctx, f})
		e = errors.Join(e, f.Close())
		if e != nil {
			return e
		}
		if n != v.size {
			return fmt.Errorf("initrd input changed: %s", v.name)
		}
	} else if v.target != "" {
		if _, e := io.WriteString(w, v.target); e != nil {
			return e
		}
	}
	return padding(w, v.size)
}
func padding(w io.Writer, n int64) error { _, e := w.Write(make([]byte, (4-n%4)%4)); return e }
