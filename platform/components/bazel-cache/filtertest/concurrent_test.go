package filtertest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/bytestream"
)

func TestConcurrentClientsRecoverFromCancelledStreams(t *testing.T) {
	c := startCache(t)
	blob := bytes.Repeat([]byte("concurrent cached output "), 4<<20/25)
	if err := writeStream(context10(t), connect(t, c.writer, peerIP), blob); err != nil {
		t.Fatal(err)
	}
	d := digest(blob)
	resource := fmt.Sprintf("blobs/%s/%d", d.Hash, d.SizeBytes)
	t.Run("clients", func(t *testing.T) {
		for client := range 3 {
			reader, writer := connect(t, c.reader, peerIP), connect(t, c.writer, peerIP)
			for stream := range 8 {
				t.Run(fmt.Sprintf("%d/%d", client, stream), func(t *testing.T) {
					t.Parallel()
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					for round := range 4 {
						output := bytes.Repeat([]byte(fmt.Sprintf("%d/%d/%d ", client, stream, round)), 16<<10)
						if err := writeStream(ctx, writer, output); err != nil {
							t.Fatalf("concurrent write: %v", err)
						}
						interrupted, stop := context.WithCancel(ctx)
						read, err := bytestream.NewByteStreamClient(reader).Read(interrupted, &bytestream.ReadRequest{ResourceName: resource})
						if err == nil {
							_, err = read.Recv()
						}
						stop()
						if err != nil {
							t.Fatalf("start cancelled stream: %v", err)
						}
						read, err = bytestream.NewByteStreamClient(reader).Read(ctx, &bytestream.ReadRequest{ResourceName: resource})
						if err != nil {
							t.Fatal(err)
						}
						hash := sha256.New()
						for {
							response, err := read.Recv()
							if errors.Is(err, io.EOF) {
								break
							}
							if err != nil {
								t.Fatalf("read after cancellation: %v", err)
							}
							hash.Write(response.Data)
						}
						if fmt.Sprintf("%x", hash.Sum(nil)) != d.Hash {
							t.Fatal("concurrent read returned corrupted data")
						}
					}
				})
			}
		}
	})
	fresh := connect(t, c.reader, peerIP)
	if data, err := readStream(context10(t), fresh, blob); err != nil || !bytes.Equal(data, blob) {
		t.Fatalf("fresh client after concurrent traffic: %d bytes, %v", len(data), err)
	}
}
