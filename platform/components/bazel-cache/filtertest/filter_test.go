package filtertest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	asset "github.com/buchgr/bazel-remote/v2/genproto/build/bazel/remote/asset/v1"
	remote "github.com/buchgr/bazel-remote/v2/genproto/build/bazel/remote/execution/v2"
	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.yaml.in/yaml/v3"
	"golang.org/x/net/http2"
	"google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	tailnetAddress = "100.87.168.66"
	tailnet        = "100.64.0.0/10"
	hostIP         = "127.0.0.2"
	peerIP         = "127.0.0.1"
	outsiderIP     = "127.200.0.1"
	sockets        = "/run/bazel-remote"
	deadline       = 10 * time.Second
)

var bazelRemote string

func TestMain(m *testing.M) {
	directory, err := os.MkdirTemp("", "bazel-remote-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bazelRemote = filepath.Join(directory, "bazel-remote")
	build := exec.Command("go", "build", "-o", bazelRemote, "github.com/buchgr/bazel-remote/v2")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build bazel-remote:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(directory)
	os.Exit(code)
}

type cache struct {
	reader, writer, health string
}

func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", hostIP+":0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	return port
}

func filterConfig(t *testing.T, ports map[string]string) string {
	t.Helper()
	data, err := os.ReadFile("../nginx.conf")
	if err != nil {
		t.Fatal(err)
	}
	config := string(data)
	for old, count := range map[string]int{tailnetAddress + ":": 3, "deny " + tailnetAddress + ";": 2, tailnet: 2, "unix:" + sockets + "/grpc.sock;": 1} {
		if strings.Count(config, old) != count {
			t.Fatalf("filter no longer names %q exactly %d times", old, count)
		}
	}
	config = strings.ReplaceAll(strings.ReplaceAll(config, tailnetAddress, hostIP), tailnet, "127.0.0.0/9")
	for listener, port := range ports {
		if strings.Count(config, "listen "+hostIP+":"+listener+";") != 1 {
			t.Fatalf("filter no longer has exactly one listener on %s", listener)
		}
		config = strings.Replace(config, "listen "+hostIP+":"+listener+";", "listen "+hostIP+":"+port+";", 1)
	}
	return config
}

func filterImage(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../bazel-cache.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var statefulSet struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct{ Name, Image string } `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &statefulSet); err != nil {
		t.Fatal(err)
	}
	for _, container := range statefulSet.Spec.Template.Spec.Containers {
		if container.Name == "filter" {
			return container.Image
		}
	}
	t.Fatal("the cache ships no filter container")
	return ""
}

func startCache(t *testing.T) cache {
	t.Helper()
	directory := t.TempDir()
	grpcSocket, httpSocket := filepath.Join(directory, "grpc.sock"), filepath.Join(directory, "http.sock")
	server := exec.Command(bazelRemote, "--dir", filepath.Join(directory, "data"), "--max_size", "1", "--grpc_address", "unix://"+grpcSocket, "--http_address", "unix://"+httpSocket, "--access_log_level", "none")
	var log bytes.Buffer
	server.Stdout, server.Stderr = &log, &log
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		server.Process.Kill()
		server.Wait()
		if t.Failed() {
			t.Logf("bazel-remote:\n%s", log.String())
		}
	})
	for start := time.Now(); ; time.Sleep(20 * time.Millisecond) {
		if connection, err := net.Dial("unix", grpcSocket); err == nil {
			connection.Close()
			break
		}
		if time.Since(start) > deadline {
			t.Fatalf("bazel-remote did not listen:\n%s", log.String())
		}
	}
	ports := map[string]string{"9092": freePort(t), "9093": freePort(t), "9095": freePort(t)}
	configuration := t.TempDir()
	if err := os.WriteFile(filepath.Join(configuration, "nginx.conf"), []byte(filterConfig(t, ports)), 0o644); err != nil {
		t.Fatal(err)
	}
	filter, err := testcontainers.Run(context.Background(), filterImage(t),
		testcontainers.WithEntrypoint("nginx"),
		testcontainers.WithCmd("-e", "stderr", "-c", "/etc/nginx/filter/nginx.conf"),
		testcontainers.WithConfigModifier(func(config *container.Config) {
			config.User = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
		}),
		testcontainers.WithHostConfigModifier(func(host *container.HostConfig) {
			host.NetworkMode = "host"
			host.ReadonlyRootfs = true
			host.Tmpfs = map[string]string{"/tmp": ""}
			host.Binds = []string{configuration + ":/etc/nginx/filter:ro", directory + ":" + sockets}
		}),
		testcontainers.WithWaitStrategyAndDeadline(time.Minute, wait.ForLog("start worker process")),
	)
	testcontainers.CleanupContainer(t, filter)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		if logs, err := filter.Logs(context.Background()); err == nil {
			output, _ := io.ReadAll(logs)
			t.Logf("filter:\n%s", output)
		}
	})
	return cache{reader: net.JoinHostPort(hostIP, ports["9092"]), writer: net.JoinHostPort(hostIP, ports["9093"]), health: net.JoinHostPort(hostIP, ports["9095"])}
}

func dialFrom(source string) func(context.Context, string) (net.Conn, error) {
	return func(ctx context.Context, address string) (net.Conn, error) {
		dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(source)}}
		return dialer.DialContext(ctx, "tcp", address)
	}
}

func connect(t *testing.T, address, source string) *grpc.ClientConn {
	t.Helper()
	connection, err := grpc.NewClient("passthrough:///"+address, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(dialFrom(source)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	return connection
}

func digest(data []byte) *remote.Digest {
	sum := sha256.Sum256(data)
	return &remote.Digest{Hash: hex.EncodeToString(sum[:]), SizeBytes: int64(len(data))}
}

func context10(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	t.Cleanup(cancel)
	return ctx
}

func writeStream(ctx context.Context, connection *grpc.ClientConn, blob []byte) error {
	stream, err := bytestream.NewByteStreamClient(connection).Write(ctx)
	if err != nil {
		return err
	}
	d := digest(blob)
	resource := fmt.Sprintf("uploads/%s/blobs/%s/%d", "0b1f4e8a-6f4c-4d7e-9a3b-2c5d8e1f0a4b", d.Hash, d.SizeBytes)
	for offset := 0; ; offset += 64 << 10 {
		end := min(offset+64<<10, len(blob))
		request := &bytestream.WriteRequest{WriteOffset: int64(offset), Data: blob[offset:end], FinishWrite: end == len(blob)}
		if offset == 0 {
			request.ResourceName = resource
		}
		if err := stream.Send(request); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if request.FinishWrite {
			break
		}
	}
	_, err = stream.CloseAndRecv()
	return err
}

func readStream(ctx context.Context, connection *grpc.ClientConn, blob []byte) ([]byte, error) {
	d := digest(blob)
	stream, err := bytestream.NewByteStreamClient(connection).Read(ctx, &bytestream.ReadRequest{ResourceName: fmt.Sprintf("blobs/%s/%d", d.Hash, d.SizeBytes)})
	if err != nil {
		return nil, err
	}
	var data []byte
	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return data, nil
		}
		if err != nil {
			return nil, err
		}
		data = append(data, response.Data...)
	}
}

func actionResult(output []byte) *remote.ActionResult {
	return &remote.ActionResult{OutputFiles: []*remote.OutputFile{{Path: "out", Digest: digest(output)}}}
}

func missing(t *testing.T, connection *grpc.ClientConn, blobs ...[]byte) int {
	t.Helper()
	var digests []*remote.Digest
	for _, blob := range blobs {
		digests = append(digests, digest(blob))
	}
	response, err := remote.NewContentAddressableStorageClient(connection).FindMissingBlobs(context10(t), &remote.FindMissingBlobsRequest{BlobDigests: digests})
	if err != nil {
		t.Fatal(err)
	}
	return len(response.MissingBlobDigests)
}

func cachedAction(connection *grpc.ClientConn, action []byte) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	_, err := remote.NewActionCacheClient(connection).GetActionResult(ctx, &remote.GetActionResultRequest{ActionDigest: digest(action)})
	if status.Code(err) == codes.NotFound {
		return false, nil
	}
	return err == nil, err
}

func TestReaderServesEveryReadAndRefusesEveryWrite(t *testing.T) {
	c := startCache(t)
	writer, reader := connect(t, c.writer, peerIP), connect(t, c.reader, peerIP)
	ctx := context10(t)
	output, streamed, action := []byte("cached output"), []byte("streamed output"), []byte("cached action")
	tree := &remote.Directory{Files: []*remote.FileNode{{Name: "out", Digest: digest(output)}}}
	encodedTree, err := proto.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	cas, ac := remote.NewContentAddressableStorageClient(writer), remote.NewActionCacheClient(writer)
	if _, err := cas.BatchUpdateBlobs(ctx, &remote.BatchUpdateBlobsRequest{Requests: []*remote.BatchUpdateBlobsRequest_Request{{Digest: digest(output), Data: output}, {Digest: digest(encodedTree), Data: encodedTree}}}); err != nil {
		t.Fatalf("writer BatchUpdateBlobs: %v", err)
	}
	if err := writeStream(ctx, writer, streamed); err != nil {
		t.Fatalf("writer ByteStream.Write: %v", err)
	}
	if _, err := ac.UpdateActionResult(ctx, &remote.UpdateActionResultRequest{ActionDigest: digest(action), ActionResult: actionResult(output)}); err != nil {
		t.Fatalf("writer UpdateActionResult: %v", err)
	}

	reads := map[string]func() error{
		"GetCapabilities": func() error {
			_, err := remote.NewCapabilitiesClient(reader).GetCapabilities(ctx, &remote.GetCapabilitiesRequest{})
			return err
		},
		"GetActionResult": func() error {
			result, err := remote.NewActionCacheClient(reader).GetActionResult(ctx, &remote.GetActionResultRequest{ActionDigest: digest(action)})
			if err == nil && result.OutputFiles[0].Digest.Hash != digest(output).Hash {
				err = errors.New("wrong action result")
			}
			return err
		},
		"FindMissingBlobs": func() error {
			if n := missing(t, reader, output, streamed, encodedTree); n != 0 {
				return fmt.Errorf("%d blobs missing", n)
			}
			return nil
		},
		"BatchReadBlobs": func() error {
			response, err := remote.NewContentAddressableStorageClient(reader).BatchReadBlobs(ctx, &remote.BatchReadBlobsRequest{Digests: []*remote.Digest{digest(output)}})
			if err == nil && !bytes.Equal(response.Responses[0].Data, output) {
				err = errors.New("wrong blob")
			}
			return err
		},
		"GetTree": func() error {
			stream, err := remote.NewContentAddressableStorageClient(reader).GetTree(ctx, &remote.GetTreeRequest{RootDigest: digest(encodedTree)})
			if err != nil {
				return err
			}
			response, err := stream.Recv()
			if err == nil && len(response.Directories) != 1 {
				err = errors.New("wrong tree")
			}
			return err
		},
		"ByteStream.Read": func() error {
			data, err := readStream(ctx, reader, streamed)
			if err == nil && !bytes.Equal(data, streamed) {
				err = errors.New("wrong stream")
			}
			return err
		},
	}
	for name, read := range reads {
		if err := read(); err != nil {
			t.Errorf("reader %s: %v", name, err)
		}
	}

	forged, forgedStream, forgedAction := []byte("forged output"), []byte("forged stream"), []byte("forged action")
	writes := map[string]func() error{
		"ActionCache.UpdateActionResult": func() error {
			_, err := remote.NewActionCacheClient(reader).UpdateActionResult(ctx, &remote.UpdateActionResultRequest{ActionDigest: digest(forgedAction), ActionResult: actionResult(output)})
			return err
		},
		"ActionCache.UpdateActionResult over a cached action": func() error {
			_, err := remote.NewActionCacheClient(reader).UpdateActionResult(ctx, &remote.UpdateActionResultRequest{ActionDigest: digest(action), ActionResult: actionResult(forged)})
			return err
		},
		"ContentAddressableStorage.BatchUpdateBlobs": func() error {
			_, err := remote.NewContentAddressableStorageClient(reader).BatchUpdateBlobs(ctx, &remote.BatchUpdateBlobsRequest{Requests: []*remote.BatchUpdateBlobsRequest_Request{{Digest: digest(forged), Data: forged}}})
			return err
		},
		"ByteStream.Write": func() error { return writeStream(ctx, reader, forgedStream) },
		"ByteStream.QueryWriteStatus": func() error {
			_, err := bytestream.NewByteStreamClient(reader).QueryWriteStatus(ctx, &bytestream.QueryWriteStatusRequest{ResourceName: "uploads/0b1f4e8a-6f4c-4d7e-9a3b-2c5d8e1f0a4b/blobs/" + digest(forgedStream).Hash + "/13"})
			return err
		},
		"Execution.Execute": func() error {
			stream, err := remote.NewExecutionClient(reader).Execute(ctx, &remote.ExecuteRequest{ActionDigest: digest(forgedAction)})
			if err == nil {
				_, err = stream.Recv()
			}
			return err
		},
		"Execution.WaitExecution": func() error {
			stream, err := remote.NewExecutionClient(reader).WaitExecution(ctx, &remote.WaitExecutionRequest{Name: "operation"})
			if err == nil {
				_, err = stream.Recv()
			}
			return err
		},
		"Fetch.FetchBlob": func() error {
			_, err := asset.NewFetchClient(reader).FetchBlob(ctx, &asset.FetchBlobRequest{Uris: []string{"https://example.invalid/blob"}})
			return err
		},
		"Push.PushBlob": func() error {
			_, err := asset.NewPushClient(reader).PushBlob(ctx, &asset.PushBlobRequest{Uris: []string{"https://example.invalid/blob"}, BlobDigest: digest(forged)})
			return err
		},
		"Health.Check": func() error {
			_, err := grpc_health_v1.NewHealthClient(reader).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
			return err
		},
	}
	for name, write := range writes {
		if err := write(); status.Code(err) != codes.PermissionDenied {
			t.Errorf("reader %s answered %v, want PermissionDenied", name, err)
		}
	}
	if n := missing(t, writer, forged, forgedStream); n != 2 {
		t.Errorf("%d forged blobs reached the cache", 2-n)
	}
	if written, err := cachedAction(writer, forgedAction); err != nil || written {
		t.Errorf("forged action result written=%v (%v)", written, err)
	}
	result, err := ac.GetActionResult(ctx, &remote.GetActionResultRequest{ActionDigest: digest(action)})
	if err != nil || result.OutputFiles[0].Digest.Hash != digest(output).Hash {
		t.Errorf("cached action changed: %v %v", result, err)
	}
}

func TestWriterRefusesEverythingOutsideTheCacheProtocol(t *testing.T) {
	c := startCache(t)
	writer := connect(t, c.writer, peerIP)
	ctx := context10(t)
	for name, call := range map[string]func() error{
		"Execution.Execute": func() error {
			stream, err := remote.NewExecutionClient(writer).Execute(ctx, &remote.ExecuteRequest{ActionDigest: digest([]byte("action"))})
			if err == nil {
				_, err = stream.Recv()
			}
			return err
		},
		"Push.PushBlob": func() error {
			_, err := asset.NewPushClient(writer).PushBlob(ctx, &asset.PushBlobRequest{Uris: []string{"https://example.invalid/blob"}, BlobDigest: digest([]byte("blob"))})
			return err
		},
		"Health.Check": func() error {
			_, err := grpc_health_v1.NewHealthClient(writer).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
			return err
		},
	} {
		if err := call(); status.Code(err) != codes.PermissionDenied {
			t.Errorf("writer %s answered %v, want PermissionDenied", name, err)
		}
	}
}

func frame(t *testing.T, message proto.Message) []byte {
	t.Helper()
	encoded, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	framed := make([]byte, 5, 5+len(encoded))
	binary.BigEndian.PutUint32(framed[1:], uint32(len(encoded)))
	return append(framed, encoded...)
}

type attempt struct {
	name, method, path, contentType string
	http1, readable                 bool
	headers                         map[string]string
}

func TestFilterRefusesMethodPathAndProtocolConfusion(t *testing.T) {
	c := startCache(t)
	writer := connect(t, c.writer, peerIP)
	update := "/build.bazel.remote.execution.v2.ActionCache/UpdateActionResult"
	read := "/build.bazel.remote.execution.v2.ActionCache/GetActionResult"
	attempts := []attempt{
		{name: "lowercase service", path: "/build.bazel.remote.execution.v2.actioncache/UpdateActionResult"},
		{name: "lowercase method", path: "/build.bazel.remote.execution.v2.ActionCache/updateactionresult"},
		{name: "double leading slash", path: "/" + update},
		{name: "inner double slash", path: strings.Replace(update, "/Update", "//Update", 1)},
		{name: "dot segment", path: "/./build.bazel.remote.execution.v2.ActionCache/UpdateActionResult"},
		{name: "read then parent", path: read + "/../UpdateActionResult"},
		{name: "read with encoded parent", path: read + "/%2e%2e/UpdateActionResult"},
		{name: "encoded separator", path: strings.Replace(update, "ActionCache/", "ActionCache%2F", 1)},
		{name: "encoded method letter", path: strings.Replace(update, "/Update", "/%55pdate", 1)},
		{name: "trailing slash", path: update + "/"},
		{name: "query", path: update + "?x=1"},
		{name: "read path with query", path: read + "?/" + update},
		{name: "read path with fragment", path: read + "#" + update},
		{name: "null byte", path: update + "%00"},
		{name: "semicolon parameter", path: read + ";/UpdateActionResult"},
		{name: "absolute form", path: "http://" + c.reader + update},
		{name: "PUT", method: http.MethodPut, path: update},
		{name: "GET on a read path", method: http.MethodGet, path: read},
		{name: "PUT on a read path", method: http.MethodPut, path: read},
		{name: "grpc-web", path: update, contentType: "application/grpc-web"},
		{name: "text", path: update, contentType: "text/plain"},
		{name: "no content type", path: update, contentType: "-"},
		{name: "grpc-web on a read path", path: read, contentType: "application/grpc-web"},
		{name: "text on a read path", path: read, contentType: "text/plain"},
		{name: "HTTP/1.1", path: update, http1: true},
		{name: "HTTP/1.1 read path", path: read, http1: true},
		{name: "HTTP/1.1 h2c upgrade", path: update, http1: true, headers: map[string]string{"Connection": "Upgrade, HTTP2-Settings", "Upgrade": "h2c", "HTTP2-Settings": "AAMAAABkAARAAAAAAAIAAAAA"}},
		{name: "override header", path: read, readable: true, headers: map[string]string{"X-HTTP-Method-Override": "PUT", "X-Original-URL": update, "X-Rewrite-URL": update}},
		{name: "update body on a read path", path: read, readable: true},
	}
	h2c := &http.Client{Transport: &http2.Transport{AllowHTTP: true, DialTLSContext: func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
		return dialFrom(peerIP)(ctx, address)
	}}}
	h1 := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, address string) (net.Conn, error) { return dialFrom(peerIP)(ctx, address) }}}
	for index, a := range attempts {
		action := []byte(fmt.Sprintf("confused action %d", index))
		body := frame(t, &remote.UpdateActionResultRequest{ActionDigest: digest(action), ActionResult: actionResult([]byte("forged"))})
		method := a.method
		if method == "" {
			method = http.MethodPost
		}
		request, err := http.NewRequest(method, "http://"+c.reader+"/", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.URL.Opaque = a.path
		switch a.contentType {
		case "":
			request.Header.Set("Content-Type", "application/grpc")
		case "-":
		default:
			request.Header.Set("Content-Type", a.contentType)
		}
		request.Header.Set("TE", "trailers")
		for name, value := range a.headers {
			request.Header.Set(name, value)
		}
		client := h2c
		if a.http1 {
			client = h1
		}
		response, err := client.Do(request)
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if !a.readable && response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusBadRequest {
				t.Errorf("%s answered HTTP %d %s", a.name, response.StatusCode, response.Header.Get("Grpc-Status"))
			}
		}
		if written, err := cachedAction(writer, action); err != nil || written {
			t.Errorf("%s: action result written=%v (%v)", a.name, written, err)
		}
	}
}

func TestFilterRefusesTheCacheHostItself(t *testing.T) {
	c := startCache(t)
	for name, address := range map[string]string{"reader": c.reader, "writer": c.writer} {
		_, err := remote.NewCapabilitiesClient(connect(t, address, hostIP)).GetCapabilities(context10(t), &remote.GetCapabilitiesRequest{})
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s answered the cache host with %v", name, err)
		}
	}
}

func TestFilterRefusesSourcesOutsideTheTailnet(t *testing.T) {
	c := startCache(t)
	for name, address := range map[string]string{"reader": c.reader, "writer": c.writer} {
		_, err := remote.NewCapabilitiesClient(connect(t, address, outsiderIP)).GetCapabilities(context10(t), &remote.GetCapabilitiesRequest{})
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s answered a source outside the tailnet with %v", name, err)
		}
	}
}

func TestFilterCarriesLargeBlobsAndLookups(t *testing.T) {
	c := startCache(t)
	writer, reader := connect(t, c.writer, peerIP), connect(t, c.reader, peerIP)
	ctx := context10(t)
	blob := bytes.Repeat([]byte("large cached output "), 8<<20/20)
	if err := writeStream(ctx, writer, blob); err != nil {
		t.Fatalf("writer ByteStream.Write of %d bytes: %v", len(blob), err)
	}
	if data, err := readStream(ctx, reader, blob); err != nil || !bytes.Equal(data, blob) {
		t.Fatalf("reader ByteStream.Read of %d bytes returned %d bytes: %v", len(blob), len(data), err)
	}
	lookups := make([][]byte, 20000)
	for i := range lookups {
		lookups[i] = []byte(fmt.Sprintf("absent blob %d", i))
	}
	if n := missing(t, reader, append(lookups, blob)...); n != len(lookups) {
		t.Errorf("reader FindMissingBlobs reported %d of %d absent blobs", n, len(lookups))
	}
}

func TestHealthListenerServesOnlyTheHealthCheck(t *testing.T) {
	c := startCache(t)
	health := connect(t, c.health, hostIP)
	response, err := grpc_health_v1.NewHealthClient(health).Check(context10(t), &grpc_health_v1.HealthCheckRequest{})
	if err != nil || response.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("health check answered %v %v", response, err)
	}
	if _, err := remote.NewCapabilitiesClient(health).GetCapabilities(context10(t), &remote.GetCapabilitiesRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("health listener answered GetCapabilities with %v", err)
	}
}
