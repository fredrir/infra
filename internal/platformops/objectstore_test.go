package platformops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/fredrir/infra/internal/objectstore"
)

type fakeObjectStore struct {
	mu       sync.Mutex
	buckets  map[string]map[string]string
	requests []string
	bodies   map[string]string
	deny     map[string]bool
}

func (f *fakeObjectStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	bucket := strings.Trim(r.URL.Path, "/")
	call := r.Method + " " + bucket + "?" + r.URL.RawQuery
	f.requests = append(f.requests, call)
	f.bodies[call] = string(body)
	if !strings.Contains(r.Header.Get("Authorization"), "Credential=provisioner/") || f.deny[bucket] {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	settings, exists := f.buckets[bucket]
	subresource := strings.TrimSuffix(r.URL.RawQuery, "=")
	switch {
	case r.Method == http.MethodPut && subresource == "":
		f.buckets[bucket] = map[string]string{}
	case !exists:
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodPut:
		settings[subresource] = string(body)
	case r.Method == http.MethodGet && settings[subresource] != "":
		io.WriteString(w, settings[subresource])
	case r.Method == http.MethodGet && subresource == "versioning":
		io.WriteString(w, `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></VersioningConfiguration>`)
	case r.Method == http.MethodGet:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeObjectStore) puts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var puts []string
	for _, request := range f.requests {
		if strings.HasPrefix(request, "PUT ") {
			puts = append(puts, request)
		}
	}
	f.requests = nil
	return puts
}

func newFakeObjectStore(existing ...string) (*fakeObjectStore, *httptest.Server) {
	f := &fakeObjectStore{buckets: map[string]map[string]string{}, bodies: map[string]string{}, deny: map[string]bool{}}
	for _, name := range existing {
		f.buckets[name] = map[string]string{}
	}
	return f, httptest.NewTLSServer(f)
}

func provisionFixture(t *testing.T, hel1, nl *httptest.Server) (ObjectStoreSpec, map[string]objectstore.Client) {
	t.Helper()
	spec, err := ParseObjectStoreSpec([]byte(objectStoreSpecFixture))
	if err != nil {
		t.Fatal(err)
	}
	spec.Cells[0].Endpoint, spec.Cells[1].Endpoint = hel1.URL, nl.URL
	return spec, map[string]objectstore.Client{
		"hel1": {Region: "hel1", AccessKey: "provisioner", SecretKey: "secret", HTTP: hel1.Client()},
		"nl":   {Region: "nl", AccessKey: "provisioner", SecretKey: "secret", HTTP: nl.Client()},
	}
}

const objectStoreSpecFixture = `cells:
- name: hel1
  endpoint: https://seaweedfs-hel1.object-store.svc.cluster.local:8333
  buckets:
  - name: ci-example-main
    quotaGiB: 20
    expireDays: 14
  - name: restic-example
    quotaGiB: 20
    lockDays: 30
    noncurrentDays: 31
- name: nl
  endpoint: https://seaweedfs-nl.object-store.svc.cluster.local:8333
  buckets:
  - name: parser-dataset
    quotaGiB: 100
`

func TestObjectStoreSpecRejectsUnsafeDeclarations(t *testing.T) {
	spec, err := ParseObjectStoreSpec([]byte(objectStoreSpecFixture))
	if err != nil || len(spec.Cells) != 2 || spec.Cells[0].Buckets[1].LockDays != 30 {
		t.Fatalf("valid spec rejected: %v", err)
	}
	for name, change := range map[string][2]string{
		"plaintext endpoint":       {"https://seaweedfs-nl", "http://seaweedfs-nl"},
		"endpoint path":            {"svc.cluster.local:8333\n  buckets:\n  - name: parser", "svc.cluster.local:8333/x\n  buckets:\n  - name: parser"},
		"duplicate cell":           {"name: nl", "name: hel1"},
		"duplicate bucket":         {"name: restic-example", "name: ci-example-main"},
		"unbounded quota":          {"quotaGiB: 100", "quotaGiB: 0"},
		"history outlived by lock": {"noncurrentDays: 31", "noncurrentDays: 30"},
		"expiry of locked objects": {"lockDays: 30", "lockDays: 30\n    expireDays: 1"},
		"invalid bucket name":      {"name: parser-dataset", "name: Parser_Dataset"},
		"unknown field":            {"quotaGiB: 100", "quotaGiB: 100\n    versioned: true"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseObjectStoreSpec([]byte(strings.Replace(objectStoreSpecFixture, change[0], change[1], 1))); err == nil {
				t.Fatal("unsafe spec accepted")
			}
		})
	}
	if _, err := ParseObjectStoreSpec([]byte("cells: []\n")); err == nil {
		t.Fatal("empty spec accepted")
	}
}

func TestProvisionObjectStoreDeclaresEveryBucketSetting(t *testing.T) {
	hel1, hel1Server := newFakeObjectStore("ci-example-main")
	defer hel1Server.Close()
	nl, nlServer := newFakeObjectStore()
	defer nlServer.Close()
	spec, clients := provisionFixture(t, hel1Server, nlServer)
	var log strings.Builder
	if err := ProvisionObjectStore(context.Background(), ObjectStoreConfig{Spec: spec, Clients: clients, WaitAttempts: 1, Log: &log}); err != nil {
		t.Fatal(err)
	}
	for _, request := range hel1.requests {
		if request == "PUT ci-example-main?" {
			t.Fatal("existing bucket recreated")
		}
	}
	for _, want := range []string{"PUT restic-example?", "PUT restic-example?versioning=", "PUT restic-example?object-lock=", "PUT restic-example?lifecycle=", "PUT restic-example?seaweedfs-quota=", "PUT ci-example-main?encryption="} {
		if _, ok := hel1.bodies[want]; !ok {
			t.Fatalf("missing %s in %v", want, hel1.requests)
		}
	}
	if versioning := hel1.bodies["PUT restic-example?versioning="]; !strings.Contains(versioning, "<Status>Enabled</Status>") {
		t.Fatalf("versioning configuration: %s", versioning)
	}
	for _, bucket := range []string{"ci-example-main", "restic-example"} {
		if !strings.Contains(hel1.bodies["PUT "+bucket+"?encryption="], "<SSEAlgorithm>AES256</SSEAlgorithm>") {
			t.Fatalf("%s lacks default encryption", bucket)
		}
	}
	lock := hel1.bodies["PUT restic-example?object-lock="]
	if !strings.Contains(lock, "<Mode>COMPLIANCE</Mode><Days>30</Days>") || !strings.Contains(lock, "<ObjectLockEnabled>Enabled</ObjectLockEnabled>") {
		t.Fatalf("lock configuration: %s", lock)
	}
	versioned := strings.Index(strings.Join(hel1.requests, "\n"), "restic-example?versioning=")
	locked := strings.Index(strings.Join(hel1.requests, "\n"), "restic-example?object-lock=")
	if versioned < 0 || locked < versioned {
		t.Fatal("object lock configured before versioning")
	}
	lifecycle := hel1.bodies["PUT restic-example?lifecycle="]
	if !strings.Contains(lifecycle, "<NoncurrentDays>31</NoncurrentDays>") || !strings.Contains(lifecycle, "<ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker>") || strings.Contains(lifecycle, "<Days>") {
		t.Fatalf("restic lifecycle: %s", lifecycle)
	}
	if cache := hel1.bodies["PUT ci-example-main?lifecycle="]; !strings.Contains(cache, "<Expiration><Days>14</Days></Expiration>") || !strings.Contains(cache, "<DaysAfterInitiation>1</DaysAfterInitiation>") {
		t.Fatalf("cache lifecycle: %s", cache)
	}
	if _, ok := hel1.bodies["PUT ci-example-main?versioning="]; ok {
		t.Fatal("disposable cache versioned")
	}
	var quota map[string]any
	if err := json.Unmarshal([]byte(nl.bodies["PUT parser-dataset?seaweedfs-quota="]), &quota); err != nil || quota["quota_size"] != float64(100<<30) || quota["quota_unit"] != "B" || quota["quota_enabled"] != true {
		t.Fatalf("quota: %v %v", quota, err)
	}
	if _, ok := nl.bodies["PUT parser-dataset?versioning="]; ok {
		t.Fatal("unversioned bucket versioned")
	}
	if !strings.Contains(log.String(), "Created bucket parser-dataset") {
		t.Fatalf("log: %s", log.String())
	}
}

func TestProvisionObjectStoreReportsEveryFailedBucket(t *testing.T) {
	hel1, hel1Server := newFakeObjectStore()
	defer hel1Server.Close()
	hel1.deny["ci-example-main"] = true
	spec, err := ParseObjectStoreSpec([]byte(objectStoreSpecFixture))
	if err != nil {
		t.Fatal(err)
	}
	spec.Cells[0].Endpoint = hel1Server.URL
	spec.Cells[1].Endpoint = "https://127.0.0.1:1"
	clients := map[string]objectstore.Client{
		"hel1": {Region: "hel1", AccessKey: "provisioner", SecretKey: "secret", HTTP: hel1Server.Client()},
		"nl":   {Region: "nl", AccessKey: "provisioner", SecretKey: "secret", Backoff: retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })},
	}
	err = ProvisionObjectStore(context.Background(), ObjectStoreConfig{Spec: spec, Clients: clients, WaitAttempts: 2})
	if err == nil || !strings.Contains(err.Error(), "bucket ci-example-main") || !strings.Contains(err.Error(), "cell nl") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("failures not reported: %v", err)
	}
	if hel1.buckets["restic-example"] == nil {
		t.Fatal("a failing bucket blocked the others")
	}
	delete(clients, "nl")
	if err := ProvisionObjectStore(context.Background(), ObjectStoreConfig{Spec: spec, Clients: clients, WaitAttempts: 1}); err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Fatalf("missing credentials accepted: %v", err)
	}
}

func TestProvisionObjectStoreWritesOnlyDrift(t *testing.T) {
	hel1, hel1Server := newFakeObjectStore()
	defer hel1Server.Close()
	nl, nlServer := newFakeObjectStore()
	defer nlServer.Close()
	spec, clients := provisionFixture(t, hel1Server, nlServer)
	config := ObjectStoreConfig{Spec: spec, Clients: clients, WaitAttempts: 1}
	if err := ProvisionObjectStore(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	hel1.puts()
	nl.puts()
	if err := ProvisionObjectStore(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if puts := append(hel1.puts(), nl.puts()...); len(puts) != 0 {
		t.Fatalf("unchanged buckets rewritten: %v", puts)
	}
	hel1.buckets["ci-example-main"]["lifecycle"] = "<LifecycleConfiguration></LifecycleConfiguration>"
	nl.buckets["parser-dataset"]["seaweedfs-quota"] = `{"quota_size":1,"quota_unit":"B","quota_enabled":true}`
	if err := ProvisionObjectStore(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if puts := append(hel1.puts(), nl.puts()...); strings.Join(puts, ",") != "PUT ci-example-main?lifecycle=,PUT parser-dataset?seaweedfs-quota=" {
		t.Fatalf("drift repair: %v", puts)
	}
}

func TestProvisionObjectStoreRefusesUndeclaredVersioningAndLock(t *testing.T) {
	hel1, hel1Server := newFakeObjectStore("ci-example-main")
	defer hel1Server.Close()
	_, nlServer := newFakeObjectStore()
	defer nlServer.Close()
	hel1.buckets["ci-example-main"]["versioning"] = "<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>"
	hel1.buckets["ci-example-main"]["object-lock"] = "<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>"
	spec, clients := provisionFixture(t, hel1Server, nlServer)
	err := ProvisionObjectStore(context.Background(), ObjectStoreConfig{Spec: spec, Clients: clients, WaitAttempts: 1})
	if err == nil || !strings.Contains(err.Error(), "bucket ci-example-main: versioning is Enabled on an unversioned bucket") || !strings.Contains(err.Error(), "object lock is enabled on an unlocked bucket") {
		t.Fatalf("drift not reported: %v", err)
	}
	if !strings.Contains(hel1.buckets["ci-example-main"]["lifecycle"], "<Days>14</Days>") {
		t.Fatal("drift blocked the remaining settings")
	}
}

func TestProvisionObjectStoreReportsRestoredDriftOnLockedBuckets(t *testing.T) {
	hel1, hel1Server := newFakeObjectStore()
	defer hel1Server.Close()
	_, nlServer := newFakeObjectStore()
	defer nlServer.Close()
	spec, clients := provisionFixture(t, hel1Server, nlServer)
	config := ObjectStoreConfig{Spec: spec, Clients: clients, WaitAttempts: 1}
	if err := ProvisionObjectStore(context.Background(), config); err != nil {
		t.Fatalf("first provisioning reported drift: %v", err)
	}
	for subresource, tampered := range map[string]string{
		"object-lock": "<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Days>1</Days></DefaultRetention></Rule></ObjectLockConfiguration>",
		"lifecycle":   "<LifecycleConfiguration></LifecycleConfiguration>",
		"versioning":  "<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>",
	} {
		original := hel1.buckets["restic-example"][subresource]
		hel1.buckets["restic-example"][subresource] = tampered
		err := ProvisionObjectStore(context.Background(), config)
		if err == nil || !strings.Contains(err.Error(), "bucket restic-example: "+subresource+" had drifted on a locked bucket and was restored") {
			t.Errorf("%s drift on a locked bucket not reported: %v", subresource, err)
		}
		if hel1.buckets["restic-example"][subresource] != original {
			t.Errorf("%s drift not restored", subresource)
		}
		if err := ProvisionObjectStore(context.Background(), config); err != nil {
			t.Errorf("restored %s still reported: %v", subresource, err)
		}
	}
	hel1.buckets["ci-example-main"]["lifecycle"] = "<LifecycleConfiguration></LifecycleConfiguration>"
	if err := ProvisionObjectStore(context.Background(), config); err != nil {
		t.Fatalf("cache lifecycle drift is not healed quietly: %v", err)
	}
}
