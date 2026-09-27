package filtertest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	_ "github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/standard"
	_ "github.com/caddyserver/caddy/v2/modules/caddytls"
	_ "github.com/caddyserver/caddy/v2/modules/logging"
)

const (
	filterProvisioner = "7F3A9C0B5E1D2468ACE0"
	filterWriter      = "0B1C2D3E4F5061728394"
	filterSignature   = "4d1f0c9e8b7a6f5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e"
)

func filterAuthorization(accessKey string) string {
	return "AWS4-HMAC-SHA256 Credential=" + accessKey + "/20260927/hel1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=" + filterSignature
}

type filterRequest struct {
	name     string
	method   string
	uri      string
	headers  http.Header
	admitted bool
}

func writer(name, method, uri string, admitted bool, headers ...string) filterRequest {
	request := filterRequest{name: name, method: method, uri: uri, headers: http.Header{"Authorization": {filterAuthorization(filterWriter)}}, admitted: admitted}
	for index := 0; index+1 < len(headers); index += 2 {
		request.headers.Add(headers[index], headers[index+1])
	}
	return request
}

func provisioner(name, method, uri string, admitted bool) filterRequest {
	return filterRequest{name: name, method: method, uri: uri, headers: http.Header{"Authorization": {filterAuthorization(filterProvisioner)}}, admitted: admitted}
}

var filterCorpus = []filterRequest{
	writer("sccache put", "PUT", "/ci-nsql-main/sccache/a/b/c/abc", true),
	writer("sccache get", "GET", "/ci-nsql-main/sccache/a/b/c/abc", true),
	writer("sccache check", "HEAD", "/ci-nsql-main/sccache/.sccache_check", true),
	writer("archive put", "PUT", "/ci-nsql-main/target/0123456789abcdef.tar.zst", true),
	writer("archive delete", "DELETE", "/ci-nsql-main/target/0123456789abcdef.tar.zst", true),
	writer("restic list", "GET", "/restic-parser/?delimiter=%2F&encoding-type=url&fetch-owner=true&list-type=2&prefix=parser%2Flocks%2F", true),
	writer("restic data list", "GET", "/restic-parser/?delimiter=&encoding-type=url&fetch-owner=true&list-type=2&prefix=parser%2Fdata%2F", true),
	writer("restic bucket probe", "HEAD", "/restic-parser/", true),
	writer("restic put", "PUT", "/restic-parser/data/00/0011223344", true),
	writer("restic delete", "DELETE", "/restic-parser/locks/0011223344", true),
	writer("restic create multipart", "POST", "/restic-parser/data/00/0011223344?uploads=", true),
	writer("restic upload part", "PUT", "/restic-parser/data/00/0011223344?partNumber=1&uploadId=4ea59805caad34f0_c9353", true),
	writer("restic complete multipart", "POST", "/restic-parser/data/00/0011223344?uploadId=4ea59805caad34f0_c9353", true),
	writer("restic prune delete", "DELETE", "/restic-parser/data/00/0011223344", true),
	writer("internal version header", "PUT", "/restic-parser/data/00/0011223344", true, "Seaweed-X-Amz-Version-Id", "v1"),
	writer("internal legal hold header", "PUT", "/ci-nsql-main/sccache/abc", true, "seaweed-x-amz-legal-hold", "ON"),
	writer("internal filer header", "PUT", "/ci-nsql-main/sccache/abc", true, "X-SeaweedFS-Replication", "001"),
	writer("boto3 put", "PUT", "/parser-dataset/files/abc.pdf", true),
	writer("boto3 ranged get", "GET", "/parser-dataset/files/abc.pdf", true, "X-Amz-Checksum-Mode", "ENABLED", "Range", "bytes=0-9"),
	writer("boto3 list", "GET", "/parser-dataset?continuation-token=abc&encoding-type=url&list-type=2&max-keys=7&prefix=assets%2F", true),
	writer("boto3 batch delete", "POST", "/parser-dataset?delete", true, "Content-Type", "application/xml"),
	writer("boto3 create multipart", "POST", "/parser-dataset/files/abc.pdf?uploads", true, "X-Amz-Checksum-Algorithm", "CRC32"),
	writer("boto3 upload part", "PUT", "/parser-dataset/files/abc.pdf?uploadId=4ea59805caad34f0_c9353&partNumber=3", true),
	writer("boto3 complete multipart", "POST", "/parser-dataset/files/abc.pdf?uploadId=4ea59805caad34f0_c9353", true),
	writer("boto3 delete", "DELETE", "/parser-dataset/files/abc.pdf", true),
	writer("rclone list", "GET", "/parser-dataset?delimiter=%2F&max-keys=1000&prefix=", true),
	writer("rclone put", "PUT", "/parser-dataset/.mirror-seeded?x-id=PutObject", true, "X-Amz-Meta-Mtime", "1790470000.1", "X-Amz-Storage-Class", "STANDARD"),
	writer("rclone get", "GET", "/parser-dataset/files/abc.pdf?x-id=GetObject", true),
	writer("rclone delete", "DELETE", "/llunde-pyparser-bucket/files/abc.pdf?x-id=DeleteObject", true),
	writer("rclone create multipart", "POST", "/parser-dataset/files/big.pdf?uploads=", true, "X-Amz-Meta-Mtime", "1790470000.1", "X-Amz-Meta-Md5chksum", "abc"),
	writer("rclone upload part", "PUT", "/parser-dataset/files/big.pdf?partNumber=2&uploadId=4ea59805caad34f0_c9353&x-id=UploadPart", true),
	writer("rclone complete multipart", "POST", "/parser-dataset/files/big.pdf?uploadId=4ea59805caad34f0_c9353", true),
	writer("rclone abort multipart", "DELETE", "/parser-dataset/files/big.pdf?uploadId=4ea59805caad34f0_c9353&x-id=AbortMultipartUpload", true),
	writer("read bucket configuration", "GET", "/ci-nsql-main?versioning", true),
	writer("presigned read", "GET", "/ci-nsql-main/sccache/abc?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=0B1C%2F20260927%2Fhel1%2Fs3%2Faws4_request&X-Amz-Signature=00", true),
	provisioner("provisioner create", "PUT", "/restic-parser", true),
	provisioner("provisioner versioning", "PUT", "/restic-parser?versioning=", true),
	provisioner("provisioner object lock", "PUT", "/restic-parser?object-lock=", true),
	provisioner("provisioner encryption", "PUT", "/restic-parser?encryption=", true),
	provisioner("provisioner lifecycle", "PUT", "/restic-parser?lifecycle=", true),
	provisioner("provisioner quota", "PUT", "/restic-parser?seaweedfs-quota=", true),
	provisioner("provisioner malformed", "PUT", "/restic-parser?versioning=&%zz", false),
	provisioner("provisioner semicolon", "PUT", "/restic-parser?versioning=&a=;", false),
	provisioner("provisioner trailing percent", "PUT", "/restic-parser?versioning=&a=%", false),

	writer("versioning", "PUT", "/ci-nsql-main?versioning", false),
	writer("versioning with empty value", "PUT", "/ci-nsql-main?versioning=", false),
	writer("encoded versioning key", "PUT", "/ci-nsql-main?%76ersioning", false),
	writer("versioning by prefix", "PUT", "/ci-nsql-main?prefix=x&versioning", false),
	writer("versioning by semicolon", "PUT", "/ci-nsql-main?versioning&prefix=x&z=;", false),
	writer("versioning by bad escape", "PUT", "/ci-nsql-main?prefix=x&versioning=&%zz", false),
	writer("versioning by trailing percent", "PUT", "/ci-nsql-main?prefix=x&versioning=&z=%", false),
	writer("versioning by short escape", "PUT", "/ci-nsql-main?prefix=x&versioning=&z=%4", false),
	writer("encoded prefix", "PUT", "/ci-nsql-main?%70refix=x&versioning", false),
	writer("uppercase versioning", "PUT", "/ci-nsql-main?Versioning&versioning", false),
	writer("lifecycle by bad escape", "PUT", "/ci-nsql-main?lifecycle=&prefix=x&%zz", false),
	writer("lifecycle delete by semicolon", "DELETE", "/ci-nsql-main?lifecycle&prefix=x&b=;", false),
	writer("object lock", "PUT", "/restic-parser?object-lock", false),
	writer("retention by bad escape", "PUT", "/restic-parser/data/obj?retention&%zz", false),
	writer("legal hold by semicolon", "PUT", "/restic-parser/data/obj?legal-hold&a=;", false),
	writer("tagging", "PUT", "/restic-parser/data/obj?tagging", false),
	writer("acl", "PUT", "/restic-parser/data/obj?acl", false),
	writer("version delete", "DELETE", "/restic-parser/data/obj?versionId=abc", false),
	writer("prefix on an object write", "PUT", "/ci-nsql-main/obj?prefix=x", false),
	writer("prefix on a post", "POST", "/ci-nsql-main/obj?prefix=x", false),
	writer("malformed read", "GET", "/ci-nsql-main?list-type=2&%zz", false),
	writer("read with semicolon", "GET", "/ci-nsql-main?list-type=2;prefix=x", false),
	writer("read with trailing percent", "GET", "/ci-nsql-main?list-type=2&prefix=%", false),
	writer("versioning on an object path", "PUT", "/ci-nsql-main/obj?versioning", false),
	writer("retention", "PUT", "/restic-parser/data/obj?retention", false),
	writer("legal hold", "PUT", "/restic-parser/data/obj?legal-hold", false),
	writer("duplicate part number", "PUT", "/ci-nsql-main/obj?partNumber=1&partNumber=2&uploadId=abc", false),
	writer("duplicate uploads", "POST", "/ci-nsql-main/obj?uploads&uploads", false),
	writer("duplicate upload id", "POST", "/ci-nsql-main/obj?uploadId=a&uploadId=b", false),
	writer("duplicate delete", "POST", "/ci-nsql-main?delete&delete=", false),
	writer("part without upload", "PUT", "/ci-nsql-main/obj?partNumber=1", false),
	writer("uploads and upload id", "POST", "/ci-nsql-main/obj?uploads&uploadId=a", false),
	writer("object post without query", "POST", "/ci-nsql-main/obj", false),
	writer("bucket create", "PUT", "/ci-nsql-main", false),
	writer("bucket delete", "DELETE", "/ci-nsql-main", false),
	writer("batch delete on an object path", "POST", "/ci-nsql-main/obj?delete", false),
	writer("batch delete on a locked bucket", "POST", "/restic-parser?delete", false, "Content-Type", "application/xml"),
	writer("batch version delete on a locked bucket", "POST", "/restic-y?delete=", false, "Content-Type", "application/xml"),
	writer("batch delete on a locked bucket with a slash", "POST", "/restic-portfolio/?delete", false),
	writer("batch delete on a locked bucket with x-id", "POST", "/restic-parser?delete&x-id=DeleteObjects", false),
	writer("batch delete on an encoded locked bucket", "POST", "/%72estic-parser?delete", false),
	writer("form upload", "POST", "/ci-nsql-main", false, "Content-Type", "multipart/form-data; boundary=x"),
	writer("form upload with delete", "POST", "/ci-nsql-main?delete", false, "Content-Type", "Multipart/Form-Data; boundary=x"),
	writer("lock mode header", "PUT", "/restic-parser/data/obj", false, "X-Amz-Object-Lock-Mode", "GOVERNANCE"),
	writer("lock date header", "PUT", "/restic-parser/data/obj", false, "X-Amz-Object-Lock-Retain-Until-Date", "2026-09-28T00:00:00Z"),
	writer("legal hold header", "PUT", "/restic-parser/data/obj", false, "X-Amz-Object-Lock-Legal-Hold", "OFF"),
	writer("governance bypass", "DELETE", "/restic-parser/data/obj", false, "X-Amz-Bypass-Governance-Retention", "true"),
	writer("patch", "PATCH", "/ci-nsql-main/obj", false),
	writer("options", "OPTIONS", "/ci-nsql-main/obj", false),
	writer("presigned write", "PUT", "/ci-nsql-main/obj?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=0B1C%2F20260927%2Fhel1%2Fs3%2Faws4_request&X-Amz-Signature=00", false),
	{name: "presigned provisioner write", method: "PUT", uri: "/ci-nsql-main?versioning&X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=" + filterProvisioner + "%2F20260927%2Fhel1%2Fs3%2Faws4_request&X-Amz-Signature=00", headers: http.Header{}, admitted: false},
	{name: "unsigned configuration", method: "PUT", uri: "/ci-nsql-main?versioning", headers: http.Header{}, admitted: false},
	{name: "provisioner header after the writer's", method: "PUT", uri: "/restic-parser/data/obj?retention", headers: http.Header{"Authorization": {filterAuthorization(filterWriter), filterAuthorization(filterProvisioner)}}, admitted: false},
	{name: "provisioner header before the writer's", method: "PUT", uri: "/restic-parser?versioning", headers: http.Header{"Authorization": {filterAuthorization(filterProvisioner), filterAuthorization(filterWriter)}}, admitted: false},
	{name: "provisioner key with a short signature", method: "PUT", uri: "/restic-parser?versioning", headers: http.Header{"Authorization": {strings.TrimSuffix(filterAuthorization(filterProvisioner), "2f1e")}}, admitted: false},
	{name: "provisioner key in a longer credential", method: "PUT", uri: "/restic-parser?versioning", headers: http.Header{"Authorization": {strings.Replace(filterAuthorization(filterProvisioner), "Credential=", "Credential=X", 1)}}, admitted: false},
	{name: "provisioner key with a V2 signature", method: "PUT", uri: "/restic-parser?versioning", headers: http.Header{"Authorization": {"AWS " + filterProvisioner + ":c2lnbmF0dXJl"}}, admitted: false},
}

func filterCertificate(t *testing.T, directory string) *x509.CertPool {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "s3.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "s3.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encoded}), 0o600); err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return roots
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func TestObjectStoreS3FilterAdmitsOnlyItsClients(t *testing.T) {
	data, err := os.ReadFile("../s3-filter.caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	var mutex sync.Mutex
	var reached, leaked []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		reached = append(reached, r.Method+" "+r.RequestURI)
		for name := range r.Header {
			if lower := strings.ToLower(name); strings.HasPrefix(lower, "seaweed-") || strings.HasPrefix(lower, "x-seaweedfs-") {
				leaked = append(leaked, r.Method+" "+r.RequestURI+" "+name)
			}
		}
		mutex.Unlock()
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	directory := t.TempDir()
	roots := filterCertificate(t, directory)
	port := freePort(t)
	config := string(data)
	for old, new := range map[string]string{
		"/etc/seaweedfs/tls/s3.crt": filepath.Join(directory, "s3.crt"),
		"/etc/seaweedfs/tls/s3.key": filepath.Join(directory, "s3.key"),
		"\n:8333 {\n":               fmt.Sprintf("\n:%d {\n", port),
		"127.0.0.1:8334":            strings.TrimPrefix(upstream.URL, "http://"),
	} {
		if strings.Count(config, old) != 1 {
			t.Fatalf("S3 filter no longer has exactly one %q", old)
		}
		config = strings.Replace(config, old, new, 1)
	}
	t.Setenv("PROVISIONER_ACCESS_KEY_ID", filterProvisioner)
	adapted, warnings, err := caddyconfig.GetAdapter("caddyfile").Adapt([]byte(config), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("S3 filter adapts with warnings: %v", warnings)
	}
	if !strings.Contains(string(adapted), `"protocols":["h1","h2"]`) {
		t.Fatalf("S3 filter serves more than HTTP/1.1 and HTTP/2: %s", adapted)
	}
	if err := caddy.Load(adapted, true); err != nil {
		t.Fatal(err)
	}
	defer caddy.Stop()
	clients := map[int]*http.Client{
		1: {Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, NextProtos: []string{"http/1.1"}}}},
		2: {Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}, ForceAttemptHTTP2: true}},
	}
	base := fmt.Sprintf("https://localhost:%d", port)
	for protocol, client := range clients {
		for _, request := range filterCorpus {
			path, query, _ := strings.Cut(request.uri, "?")
			outgoing, err := http.NewRequest(request.method, base+path, strings.NewReader("body"))
			if err != nil {
				t.Fatal(err)
			}
			outgoing.URL.RawQuery = query
			outgoing.Header = request.headers.Clone()
			mutex.Lock()
			before := len(reached)
			mutex.Unlock()
			response, err := client.Do(outgoing)
			if err != nil {
				t.Fatalf("HTTP/%d %s: %v", protocol, request.name, err)
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.ProtoMajor != protocol {
				t.Fatalf("%s spoke HTTP/%d instead of HTTP/%d", request.name, response.ProtoMajor, protocol)
			}
			mutex.Lock()
			forwarded := len(reached) > before
			mutex.Unlock()
			if forwarded != request.admitted || (request.admitted && response.StatusCode != http.StatusOK) || (!request.admitted && response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusBadRequest) {
				t.Errorf("HTTP/%d %s %s %s: forwarded=%v status=%d, want admitted=%v", protocol, request.name, request.method, request.uri, forwarded, response.StatusCode, request.admitted)
			}
		}
	}
	if len(leaked) != 0 {
		t.Errorf("internal SeaweedFS headers reached the object store: %v", leaked)
	}
}
