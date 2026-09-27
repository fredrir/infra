package filtertest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

func TestClientsRecoverFromCancelledWrites(t *testing.T) {
	c := startCache(t)
	const clients, streams = 8, 64
	blob := bytes.Repeat([]byte("x"), 1<<20)
	d := digest(blob)
	var pending sync.WaitGroup
	cancels := make([]context.CancelFunc, clients*streams)
	for client := range clients {
		writer := connect(t, c.writer, peerIP)
		for stream := range streams {
			index := client*streams + stream
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			cancels[index] = cancel
			t.Cleanup(cancel)
			pending.Go(func() {
				upload, err := bytestream.NewByteStreamClient(writer).Write(ctx)
				if err == nil {
					err = upload.Send(&bytestream.WriteRequest{ResourceName: fmt.Sprintf("uploads/cancel-%d/blobs/%s/%d", index, d.Hash, d.SizeBytes), Data: blob[:256<<10]})
				}
				if err == nil {
					err = upload.Send(&bytestream.WriteRequest{WriteOffset: 256 << 10, Data: blob[256<<10 : 512<<10]})
				}
				if err != nil {
					t.Errorf("start incomplete upload %d: %v", index, err)
				}
			})
		}
	}
	pending.Wait()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", c.httpSocket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: deadline}
	received := false
	for started := time.Now(); time.Since(started) < deadline; time.Sleep(10 * time.Millisecond) {
		response, err := client.Get("http://cache/metrics")
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "grpc_server_msg_received_total{") && strings.Contains(line, `grpc_method="Write"`) && strings.Contains(line, `grpc_service="google.bytestream.ByteStream"`) {
				fields := strings.Fields(line)
				count, err := strconv.Atoi(fields[len(fields)-1])
				if err == nil && count >= 2*len(cancels) {
					received = true
				}
			}
		}
		if received {
			break
		}
	}
	if !received {
		t.Fatal("incomplete uploads did not reach the backend")
	}
	if err := writeStream(context10(t), connect(t, c.writer, peerIP), []byte("while uploads are incomplete")); err != nil {
		t.Fatalf("fresh writer blocked by incomplete uploads: %v", err)
	}
	state, err := c.filter.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !state.Running || state.OOMKilled {
		t.Fatalf("filter stopped under incomplete uploads: %+v", state)
	}
	group, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", state.Pid))
	if err != nil {
		t.Fatal(err)
	}
	measured := false
	for _, line := range strings.Split(string(group), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			directory := filepath.Join("/sys/fs/cgroup", path)
			peak, err := os.ReadFile(filepath.Join(directory, "memory.peak"))
			if err != nil {
				t.Fatal(err)
			}
			value, err := strconv.ParseInt(strings.TrimSpace(string(peak)), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			events, err := os.ReadFile(filepath.Join(directory, "memory.events"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains("\n"+string(events), "\noom_kill 0\n") {
				t.Fatalf("filter worker OOM: %s", events)
			}
			t.Logf("filter peak with %d incomplete writes received by backend: %d bytes; oom_kill=0", len(cancels), value)
			measured = true
		}
	}
	if !measured {
		t.Fatal("filter memory measurement unavailable")
	}
	for _, cancel := range cancels {
		cancel()
	}
	output := []byte("after cancelled writes")
	if err := writeStream(context10(t), connect(t, c.writer, peerIP), output); err != nil {
		t.Fatalf("fresh writer after cancellation: %v", err)
	}
	if got, err := readStream(context10(t), connect(t, c.reader, peerIP), output); err != nil || !bytes.Equal(got, output) {
		t.Fatalf("fresh reader after cancellation: %q, %v", got, err)
	}
}
