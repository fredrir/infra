package kata

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

type cpioRecord struct {
	fields [13]uint64
	data   []byte
}

func readInitrd(t *testing.T, data []byte) map[string]cpioRecord {
	t.Helper()
	gz, e := gzip.NewReader(bytes.NewReader(data))
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(gz)
	if e != nil {
		t.Fatal(e)
	}
	gz.Close()
	records := map[string]cpioRecord{}
	for len(b) > 0 {
		if len(b) < 110 || string(b[:6]) != "070701" {
			t.Fatal("invalid cpio header")
		}
		var r cpioRecord
		for i := range r.fields {
			r.fields[i], e = strconv.ParseUint(string(b[6+8*i:14+8*i]), 16, 32)
			if e != nil {
				t.Fatal(e)
			}
		}
		n := int(r.fields[11])
		name := string(b[110 : 110+n-1])
		offset := (110 + n + 3) &^ 3
		b = b[offset:]
		size := int(r.fields[6])
		r.data = append([]byte(nil), b[:size]...)
		b = b[(size+3)&^3:]
		if name == "TRAILER!!!" {
			break
		}
		if _, ok := records[name]; ok {
			t.Fatal("duplicate cpio entry")
		}
		records[name] = r
	}
	return records
}

func TestInitrdPreservesFilesLinksAndDeviceMetadata(t *testing.T) {
	root := t.TempDir()
	if e := os.MkdirAll(filepath.Join(root, "dev"), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "dev/unwanted"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "program"), []byte("program"), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.Link(filepath.Join(root, "program"), filepath.Join(root, "other")); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink("/lib/systemd/systemd", filepath.Join(root, "init")); e != nil {
		t.Fatal(e)
	}
	var a, b bytes.Buffer
	if e := WriteInitrd(context.Background(), root, &a); e != nil {
		t.Fatal(e)
	}
	if e := os.Chtimes(filepath.Join(root, "program"), time.Now(), time.Now()); e != nil {
		t.Fatal(e)
	}
	if e := WriteInitrd(context.Background(), root, &b); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("timestamps changed initrd bytes")
	}
	r := readInitrd(t, a.Bytes())
	if _, ok := r["dev/unwanted"]; ok {
		t.Fatal("unexpected device retained")
	}
	if r["dev/console"].fields[1] != 0020600 || r["dev/console"].fields[3] != 5 || r["dev/console"].fields[9] != 5 || r["dev/console"].fields[10] != 1 {
		t.Fatalf("console metadata: %+v", r["dev/console"])
	}
	if r["dev/ttyS3"].fields[3] != 20 || r["dev/ttyS3"].fields[10] != 67 {
		t.Fatal("serial metadata differs")
	}
	if string(r["init"].data) != "/lib/systemd/systemd" || string(r["dev/fd"].data) != "/proc/self/fd" {
		t.Fatal("symlink changed")
	}
	if r["program"].fields[0] != r["other"].fields[0] || r["program"].fields[4] != 2 || string(r["program"].data) != "program" || len(r["other"].data) != 0 {
		t.Fatal("hardlink semantics changed")
	}
}

func TestInitrdHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := WriteInitrd(ctx, t.TempDir(), io.Discard); e == nil {
		t.Fatal("cancelled initrd succeeded")
	}
}
