package reconciler

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/dev"
	"github.com/fredrir/infra/internal/objectstore"
	"github.com/golang-jwt/jwt/v4"
	"github.com/klauspost/compress/zstd"
	"go.yaml.in/yaml/v3"
)

const (
	qualificationNode   = "dev-reconciler-1"
	qualificationBucket = "qualification"
	qualificationRegion = "eu-north-1"
	minioModule         = "github.com/minio/minio@v0.0.0-20260212201848-7aac2a2c5b7c"
	guestHost           = "10.0.2.2"
)

type qualification struct {
	t        *testing.T
	ctx      context.Context
	root     string
	cache    string
	work     string
	hosts    dev.HostsOptions
	s3       objectstore.Client
	gatus    string
	token    string
	revision string
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

func randomHex(t *testing.T, size int) string {
	t.Helper()
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(value)
}

func (q *qualification) start(name string, env []string, args ...string) {
	q.t.Helper()
	log, err := os.Create(filepath.Join(q.work, filepath.Base(name)+".log"))
	if err != nil {
		q.t.Fatal(err)
	}
	command := exec.Command(name, args...)
	command.Env, command.Stdout, command.Stderr = append(os.Environ(), env...), log, log
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		q.t.Fatalf("start %s: %v", name, err)
	}
	q.t.Cleanup(func() {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Wait()
		_ = log.Close()
	})
}

func (q *qualification) await(what string, timeout time.Duration, ready func() error) {
	q.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := ready()
		if err == nil {
			return
		}
		if time.Now().After(deadline) || q.ctx.Err() != nil {
			q.t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(2 * time.Second)
	}
}

func (q *qualification) run(dir string, name string, args ...string) string {
	q.t.Helper()
	command := exec.CommandContext(q.ctx, name, args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		q.t.Fatalf("%s %q: %v\n%s", name, args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func (q *qualification) guest(args ...string) (string, error) {
	var output bytes.Buffer
	err := dev.HostsSSH(q.ctx, dev.SSHOptions{Hosts: q.hosts, Node: qualificationNode, Args: args, Stdout: &output, Stderr: &output})
	return strings.TrimSpace(output.String()), err
}

func (q *qualification) origin() int {
	q.t.Helper()
	q.revision = q.run(q.root, "git", "rev-parse", "HEAD")
	origin := filepath.Join(q.work, "origin")
	q.run(q.root, "git", "clone", "--quiet", "--bare", "--no-local", q.root, filepath.Join(origin, "infra.git"))
	for _, branch := range []string{"main", "production"} {
		q.run(filepath.Join(origin, "infra.git"), "git", "update-ref", "refs/heads/"+branch, q.revision)
	}
	port := freePort(q.t)
	q.start("git", nil, "daemon", "--reuseaddr", "--export-all", "--base-path="+origin, "--listen=127.0.0.1", "--port="+strconv.Itoa(port), origin)
	q.await("git origin", time.Minute, func() error {
		_, err := exec.Command("git", "ls-remote", fmt.Sprintf("git://127.0.0.1:%d/infra.git", port), "refs/heads/main").Output()
		return err
	})
	return port
}

func (q *qualification) minio() int {
	q.t.Helper()
	binary := filepath.Join(q.cache, "minio")
	if _, err := os.Stat(binary); errors.Is(err, os.ErrNotExist) {
		command := exec.CommandContext(q.ctx, "go", "install", "-trimpath", minioModule)
		command.Env = append(os.Environ(), "GOBIN="+q.cache, "CGO_ENABLED=0")
		if output, err := command.CombinedOutput(); err != nil {
			q.t.Fatalf("build MinIO: %v\n%s", err, output)
		}
	}
	port := freePort(q.t)
	user, password := "qualification", randomHex(q.t, 20)
	encryption := make([]byte, 32)
	if _, err := rand.Read(encryption); err != nil {
		q.t.Fatal(err)
	}
	q.start(binary, []string{"MINIO_ROOT_USER=" + user, "MINIO_ROOT_PASSWORD=" + password, "MINIO_SITE_REGION=" + qualificationRegion, "MINIO_KMS_SECRET_KEY=qualification:" + base64.StdEncoding.EncodeToString(encryption)}, "server", filepath.Join(q.work, "objects"), "--quiet", "--address", fmt.Sprintf("127.0.0.1:%d", port), "--console-address", fmt.Sprintf("127.0.0.1:%d", freePort(q.t)))
	q.s3 = objectstore.Client{Endpoint: fmt.Sprintf("http://127.0.0.1:%d", port), Region: qualificationRegion, AccessKey: user, SecretKey: password}
	q.await("MinIO", time.Minute, func() error {
		response, err := http.Get(q.s3.Endpoint + "/minio/health/live")
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP %d", response.StatusCode)
		}
		return nil
	})
	if response, err := q.signed(http.MethodPut, "/"+qualificationBucket, nil); err != nil || response.StatusCode != http.StatusOK {
		q.t.Fatalf("create bucket: %v %v", response, err)
	}
	return port
}

func (q *qualification) signed(method, path string, query url.Values) (*http.Response, error) {
	request, err := http.NewRequestWithContext(q.ctx, method, q.s3.Endpoint+path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	const empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	request.Header.Set("X-Amz-Content-Sha256", empty)
	if err := v4.NewSigner().SignHTTP(q.ctx, aws.Credentials{AccessKeyID: q.s3.AccessKey, SecretAccessKey: q.s3.SecretKey}, request, empty, "s3", qualificationRegion, time.Now()); err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(request)
}

func (q *qualification) runKeys() []string {
	q.t.Helper()
	response, err := q.signed(http.MethodGet, "/"+qualificationBucket, url.Values{"list-type": {"2"}, "prefix": {"reconciliation/production/runs/"}})
	if err != nil {
		q.t.Fatal(err)
	}
	defer response.Body.Close()
	var listing struct {
		Contents []struct{ Key string }
	}
	if err := xml.NewDecoder(response.Body).Decode(&listing); err != nil {
		q.t.Fatal(err)
	}
	var keys []string
	for _, object := range listing.Contents {
		keys = append(keys, object.Key)
	}
	return keys
}

func (q *qualification) gatusBinary() string {
	q.t.Helper()
	binary := filepath.Join(q.cache, "gatus")
	var pins struct {
		Archive string `yaml:"gatus_archive_sha256"`
		Binary  string `yaml:"gatus_binary_sha256"`
	}
	data, err := os.ReadFile(filepath.Join(q.root, "ansible/roles/gatus/defaults/main.yml"))
	if err != nil {
		q.t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &pins); err != nil {
		q.t.Fatal(err)
	}
	if current, err := os.ReadFile(binary); err == nil && fmt.Sprintf("%x", sha256.Sum256(current)) == pins.Binary {
		return binary
	}
	response, err := http.Get("https://auth.docker.io/token?service=registry.docker.io&scope=repository:twinproduction/gatus:pull")
	if err != nil {
		q.t.Fatal(err)
	}
	var token struct{ Token string }
	err = json.NewDecoder(response.Body).Decode(&token)
	response.Body.Close()
	if err != nil {
		q.t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(q.ctx, http.MethodGet, "https://registry-1.docker.io/v2/twinproduction/gatus/blobs/sha256:"+pins.Archive, nil)
	if err != nil {
		q.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token.Token)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		q.t.Fatal(err)
	}
	defer response.Body.Close()
	layer, err := io.ReadAll(response.Body)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(layer)) != pins.Archive {
		q.t.Fatalf("Gatus layer does not match its pin: %v", err)
	}
	compressed, err := gzip.NewReader(bytes.NewReader(layer))
	if err != nil {
		q.t.Fatal(err)
	}
	archive := tar.NewReader(compressed)
	for {
		header, err := archive.Next()
		if err != nil {
			q.t.Fatalf("Gatus layer has no gatus binary: %v", err)
		}
		if strings.TrimPrefix(header.Name, "./") != "gatus" {
			continue
		}
		executable, err := io.ReadAll(archive)
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(executable)) != pins.Binary {
			q.t.Fatalf("Gatus binary does not match its pin: %v", err)
		}
		if err := os.WriteFile(binary, executable, 0o755); err != nil {
			q.t.Fatal(err)
		}
		return binary
	}
}

func (q *qualification) startGatus() int {
	q.t.Helper()
	port := freePort(q.t)
	q.token = randomHex(q.t, 24)
	config := filepath.Join(q.work, "gatus.yaml")
	content := fmt.Sprintf("web:\n  address: 127.0.0.1\n  port: %[1]d\nstorage:\n  type: memory\nendpoints:\n  - name: gatus\n    url: http://127.0.0.1:%[1]d/health\n    interval: 1h\n    conditions:\n      - '[STATUS] == 200'\nexternal-endpoints:\n  - name: verification\n    group: reconciliation\n    token: %[2]s\n", port, q.token)
	if err := os.WriteFile(config, []byte(content), 0o600); err != nil {
		q.t.Fatal(err)
	}
	q.start(q.gatusBinary(), []string{"GATUS_CONFIG_PATH=" + config})
	q.gatus = fmt.Sprintf("http://127.0.0.1:%d", port)
	q.await("Gatus", time.Minute, func() error {
		response, err := http.Get(q.gatus + "/health")
		if err != nil {
			return err
		}
		response.Body.Close()
		return nil
	})
	return port
}

type qualificationApp struct {
	mu      sync.Mutex
	key     *rsa.PublicKey
	minted  int
	revoked int
}

func (a *qualificationApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/2/access_tokens":
		signed := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		claims := jwt.RegisteredClaims{}
		if _, err := jwt.ParseWithClaims(signed, &claims, func(*jwt.Token) (any, error) { return a.key, nil }, jwt.WithValidMethods([]string{"RS256"})); err != nil || claims.Issuer != "1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		a.minted++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_qualification", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	case r.Method == http.MethodDelete && r.URL.Path == "/installation/token" && r.Header.Get("Authorization") == "Bearer ghs_qualification":
		a.revoked++
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestReconcilerQualification(t *testing.T) {
	if os.Getenv("INFRA_RECONCILER_QUALIFY") != "1" {
		t.Skip("run through infra dev qualify reconciler")
	}
	root, binary := os.Getenv("INFRA_TEST_SOURCE_ROOT"), os.Getenv("INFRA_QUALIFICATION_BINARY")
	if root == "" || binary == "" {
		t.Fatal("INFRA_TEST_SOURCE_ROOT and INFRA_QUALIFICATION_BINARY are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	defer cancel()
	state := dev.NewState(root)
	q := &qualification{t: t, ctx: ctx, root: root, cache: filepath.Join(state.Cache, "reconciler"), hosts: dev.HostsOptions{State: state, Runner: ci.Runner{Dir: root}}}
	if err := os.MkdirAll(q.cache, 0o755); err != nil {
		t.Fatal(err)
	}
	work, err := os.MkdirTemp(q.cache, "run-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(work) })
	q.work = work
	gitPort, minioPort, gatusPort := q.origin(), q.minio(), q.startGatus()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app := &qualificationApp{key: &key.PublicKey}
	github := httptest.NewServer(app)
	defer github.Close()
	githubPort := github.Listener.Addr().(*net.TCPAddr).Port

	executable, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	defer executable.Close()
	var installed bytes.Buffer
	if err := dev.HostsSSH(ctx, dev.SSHOptions{Hosts: q.hosts, Node: qualificationNode, Args: []string{"sudo sh -c 'install -m 0755 /dev/stdin /usr/local/bin/.infra.new && mv -f /usr/local/bin/.infra.new /usr/local/bin/infra'"}, Stdin: executable, Stdout: &installed, Stderr: &installed}); err != nil {
		t.Fatalf("install the supervisor: %v\n%s", err, installed.String())
	}
	authority := filepath.Join(work, "kubernetes-ca.crt")
	if err := os.WriteFile(authority, testAuthority(t), 0o644); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(work, "tree")
	if err := os.Mkdir(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"ansible", "build"} {
		q.run(root, "cp", "-a", filepath.Join(root, directory), filepath.Join(tree, directory))
	}
	if err := os.Symlink(state.Venv(), filepath.Join(tree, ".venv")); err != nil {
		t.Fatal(err)
	}
	playing := dev.HostsOptions{State: dev.State{Root: tree, Cache: state.Cache}, Runner: ci.Runner{Dir: tree}}
	play := func(args ...string) (string, error) {
		var output bytes.Buffer
		err := dev.HostsPlay(ctx, dev.PlayOptions{Hosts: playing, Playbook: "reconciler.yml", Args: args, Stdout: &output, Stderr: &output})
		return output.String(), err
	}
	played := time.Now()
	if output, err := play("--tags=host_key"); err != nil {
		t.Fatalf("reconciler.yml --tags=host_key: %v\n%s", err, output)
	}
	recipient, err := q.guest("sudo", "age-keygen", "-y", "/etc/age/host.key")
	if err != nil || !strings.HasPrefix(recipient, "age1") {
		t.Fatalf("host age recipient %q: %v", recipient, err)
	}
	apply := "apply-" + randomHex(t, 16)
	credentials := map[string]map[string]string{
		"verify": {
			AWSAccessKeyID:        q.s3.AccessKey,
			AWSSecretAccessKey:    q.s3.SecretKey,
			CloudflareAPIToken:    "qualification-" + randomHex(t, 8),
			HcloudToken:           "qualification-" + randomHex(t, 8),
			PlatformMailRecipient: "operator@example.net",
			KubernetesToken:       "qualification-" + randomHex(t, 8),
			ObserverAppKey:        string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
			GatusToken:            q.token,
		},
		"apply": {AWSAccessKeyID: apply},
	}
	plaintext, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(work, "sops.yaml")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	encrypt := exec.CommandContext(ctx, "sops", "--config", empty, "encrypt", "--age", recipient, "--input-type", "json", "--output-type", "yaml", "--output", filepath.Join(tree, "ansible/roles/reconciler/files/credentials.sops.yaml"), "/dev/stdin")
	encrypt.Stdin = bytes.NewReader(plaintext)
	if output, err := encrypt.CombinedOutput(); err != nil {
		t.Fatalf("encrypt the credentials to %s: %v\n%s", recipient, err, output)
	}
	variables := map[string]any{
		"reconciler_kubernetes_ca_file":      authority,
		"reconciler_verify_schedule":         "*:0/2",
		"reconciler_verify_randomized_delay": "0",
		"reconciler_verify": Config{
			Repository: fmt.Sprintf("git://%s:%d/infra.git", guestHost, gitPort),
			State:      "/var/lib/infra-verify",
			Bucket:     qualificationBucket,
			Prefix:     "reconciliation/production",
			Region:     qualificationRegion,
			Endpoint:   fmt.Sprintf("http://%s:%d", guestHost, minioPort),
			Gatus:      fmt.Sprintf("http://%s:%d", guestHost, gatusPort),
			Heartbeat:  "reconciliation_verification",
			Kubernetes: Kubernetes{Server: fmt.Sprintf("https://%s:%d", guestHost, freePort(t)), CertificateAuthority: "/etc/infra-reconcile/kubernetes-ca.crt"},
			Observer:   Observer{AppID: 1, InstallationID: 2, API: fmt.Sprintf("http://%s:%d", guestHost, githubPort)},
		},
	}
	data, err := json.Marshal(variables)
	if err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(work, "variables.json")
	if err := os.WriteFile(extra, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := play("--skip-tags=transport,infra_binary", "--extra-vars=@"+extra); err != nil {
		t.Fatalf("reconciler.yml: %v\n%s", err, output)
	}
	t.Logf("reconciler.yml converged in %s", time.Since(played).Round(time.Second))
	if output, err := play("--skip-tags=transport,infra_binary", "--extra-vars=@"+extra); err != nil || !strings.Contains(output, "changed=0") {
		t.Errorf("reconciler.yml is not idempotent: %v\n%s", err, output)
	}

	suffix := "-verify-" + q.revision[:12] + "/"
	var reportKey string
	q.await("a timer-started verification report", 30*time.Minute, func() error {
		for _, key := range q.runKeys() {
			if strings.Contains(key, suffix) && strings.HasSuffix(key, "/report.json") {
				reportKey = key
				return nil
			}
		}
		status, _ := q.guest("systemctl", "show", "infra-reconcile-verify.service", "-p", "ActiveState", "-p", "Result", "--value")
		return fmt.Errorf("no report yet; service %q", strings.ReplaceAll(status, "\n", " "))
	})
	t.Logf("report %s after %s", reportKey, time.Since(played).Round(time.Second))
	var report bytes.Buffer
	if err := q.s3.Download(ctx, qualificationBucket, reportKey, &report); err != nil {
		t.Fatal(err)
	}
	var run Run
	if err := json.Unmarshal(report.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	t.Logf("run %s", report.String())
	if run.Kind != "verify" || run.Revision != q.revision || run.Stage != "verify" || run.Verification == nil || run.Verification.Scope != "cloud" {
		t.Errorf("report %+v", run)
	}
	var compressed bytes.Buffer
	if err := q.s3.Download(ctx, qualificationBucket, strings.TrimSuffix(reportKey, "report.json")+"log.txt.zst", &compressed); err != nil {
		t.Fatal(err)
	}
	decoder, err := zstd.NewReader(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	log, err := io.ReadAll(decoder)
	decoder.Close()
	if err != nil || !bytes.Contains(log, []byte("Verifying production at "+q.revision)) {
		t.Errorf("run log: %v\n%s", err, log)
	}
	for set, values := range credentials {
		for name, value := range values {
			if name != PlatformMailRecipient && (bytes.Contains(log, []byte(strings.TrimSpace(value))) || bytes.Contains(report.Bytes(), []byte(strings.TrimSpace(value)))) {
				t.Errorf("uploaded run contains %s %s", set, name)
			}
		}
	}

	type heartbeat struct {
		Success bool
		Errors  []string
	}
	var heartbeats []heartbeat
	q.await("a Gatus heartbeat", time.Minute, func() error {
		response, err := http.Get(q.gatus + "/api/v1/endpoints/statuses")
		if err != nil {
			return err
		}
		defer response.Body.Close()
		var statuses []struct {
			Key     string
			Results []heartbeat
		}
		if err := json.NewDecoder(response.Body).Decode(&statuses); err != nil {
			return err
		}
		for _, status := range statuses {
			if status.Key == "reconciliation_verification" && len(status.Results) > 0 {
				heartbeats = status.Results
				return nil
			}
		}
		return errors.New("no heartbeat")
	})
	t.Logf("heartbeats %+v", heartbeats)
	if last := heartbeats[len(heartbeats)-1]; last.Success != (run.Outcome == "matches") {
		t.Errorf("heartbeat success %t for outcome %s", last.Success, run.Outcome)
	}
	app.mu.Lock()
	if app.minted == 0 || app.revoked != app.minted {
		t.Errorf("observer tokens minted %d, revoked %d", app.minted, app.revoked)
	}
	app.mu.Unlock()

	if trigger, err := q.guest("systemctl", "show", "infra-reconcile-verify.timer", "-p", "LastTriggerUSec", "--value"); err != nil || trigger == "" || trigger == "n/a" {
		t.Errorf("timer never fired: %q %v", trigger, err)
	} else {
		t.Logf("timer last fired %s", trigger)
	}
	if triggered, err := q.guest("systemctl", "show", "infra-reconcile-verify.service", "-p", "TriggeredBy", "--value"); err != nil || triggered != "infra-reconcile-verify.timer" {
		t.Errorf("verification triggered by %q: %v", triggered, err)
	}
	for _, path := range []string{"/etc/infra-reconcile/credentials.sops.yaml", "/etc/age/host.key"} {
		output, err := q.guest("sudo", "-u", "infra-verify", "cat", path)
		if err == nil || !strings.Contains(output, "Permission denied") {
			t.Errorf("infra-verify read %s: %v", path, err)
		}
	}
	if kept, err := q.guest("sudo", "find", "/var/lib/infra-verify", "-mindepth", "1"); err != nil || kept != "" {
		t.Errorf("verification state kept across the run: %v %q", err, kept)
	}
	if output, err := q.guest("sudo", "ls", "/run/infra-reconcile-verify"); err == nil {
		t.Errorf("decrypted verify credentials outlived the run: %q", output)
	}
	if journal, err := q.guest("sudo", "journalctl", "--no-pager", "-o", "cat", "-u", "infra-reconcile-verify.service"); err != nil || strings.Contains(journal, apply) || strings.Contains(journal, q.token) {
		t.Errorf("verify journal holds a credential or is unreadable: %v", err)
	}
	if exposure, err := q.guest("sudo", "systemd-analyze", "security", "--no-pager", "infra-reconcile-verify.service"); err == nil {
		lines := strings.Split(exposure, "\n")
		t.Logf("sandbox %s", lines[len(lines)-1])
	}
}
