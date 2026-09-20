package platformops

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

type fakeCacheKey struct {
	Name, Secret string
	Deleted      bool
}
type fakeCacheBucket struct {
	ID, Alias   string
	Quota       int64
	Rules       []any
	Permissions map[string]Permission
	Bytes       int64
}
type fakeCacheProject struct {
	Name, Project string
	Data          map[string]string
}
type fakeCache struct {
	mu            sync.Mutex
	Version       int
	Roles, Staged []any
	Keys          map[string]*fakeCacheKey
	Buckets       map[string]*fakeCacheBucket
	Projects      map[string]fakeCacheProject
	Mutations     []string
	Draining      bool
}

func cacheProject(project string, seed int) fakeCacheProject {
	p := fakeCacheProject{Name: "build-cache-" + project, Project: project, Data: map[string]string{}}
	for i, role := range []string{"rw", "ro", "release"} {
		p.Data[role+"_id"] = fmt.Sprintf("GK%024x", seed*10+i+1)
		p.Data[role+"_secret"] = fmt.Sprintf("%064x", seed*10+i+1)
	}
	return p
}
func newCacheFixture(t *testing.T) (*fakeCache, CacheConfig, *bytes.Buffer) {
	t.Helper()
	state := &fakeCache{Keys: map[string]*fakeCacheKey{}, Buckets: map[string]*fakeCacheBucket{}, Projects: map[string]fakeCacheProject{"nsql": cacheProject("nsql", 1), "ui-box": cacheProject("ui-box", 2)}}
	server := httptest.NewServer(http.HandlerFunc(state.serve))
	t.Cleanup(server.Close)
	var log bytes.Buffer
	c := CacheConfig{Admin: API{URL: server.URL, Token: func() (string, error) { return "admin-token", nil }}, Kubernetes: API{URL: server.URL, Token: func() (string, error) { return "service-token", nil }}, Namespace: "build-cache", KeyID: "GK" + strings.Repeat("0", 24), KeySecret: strings.Repeat("a", 64), Capacity: 150000000000, MainQuota: 21474836480, ReleaseQuota: 10737418240, ToolchainQuota: 5368709120, ExpirationDays: 14, AlertPercent: 90, WaitAttempts: 2, Log: &log}
	return state, c, &log
}
func (f *fakeCache) edit(fn func(*fakeCache)) { f.mu.Lock(); defer f.mu.Unlock(); fn(f) }
func (f *fakeCache) view(b *fakeCacheBucket) any {
	keys := []any{}
	for id, p := range b.Permissions {
		if p != (Permission{}) {
			keys = append(keys, map[string]any{"accessKeyId": id, "permissions": p})
		}
	}
	return map[string]any{"id": b.ID, "bytes": b.Bytes, "keys": keys, "quotas": map[string]any{"maxSize": b.Quota, "maxObjects": nil}, "lifecycleRules": b.Rules}
}
func (f *fakeCache) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	respond := func(status int, v any) { w.WriteHeader(status); json.NewEncoder(w).Encode(v) }
	if strings.HasPrefix(r.URL.Path, "/api/v1/") {
		if r.Header.Get("Authorization") != "Bearer service-token" {
			respond(401, nil)
			return
		}
		if r.URL.Path != "/api/v1/namespaces/build-cache/secrets" || r.URL.Query().Get("labelSelector") != "infra.fredrir.com/build-cache-project" {
			respond(404, nil)
			return
		}
		items := []any{}
		for _, p := range f.Projects {
			data := map[string]string{}
			for k, v := range p.Data {
				data[k] = base64.StdEncoding.EncodeToString([]byte(v))
			}
			items = append(items, map[string]any{"metadata": map[string]any{"name": p.Name, "labels": map[string]string{"infra.fredrir.com/build-cache-project": p.Project}}, "data": data})
		}
		respond(200, map[string]any{"items": items})
		return
	}
	if r.Header.Get("Authorization") != "Bearer admin-token" {
		respond(403, nil)
		return
	}
	if r.Method == http.MethodPost {
		f.Mutations = append(f.Mutations, r.URL.Path)
	}
	q := r.URL.Query()
	var body struct {
		AccessKeyID, SecretAccessKey, Name, GlobalAlias, BucketID string
		Version                                                   int
		Roles                                                     []any
		Quotas                                                    struct{ MaxSize int64 }
		LifecycleRules                                            []any
		Permissions                                               Permission
	}
	if r.Body != nil && r.ContentLength != 0 {
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			respond(400, nil)
			return
		}
	}
	switch r.URL.Path {
	case "/v2/GetClusterStatus":
		respond(200, map[string]any{"nodes": []any{map[string]any{"id": "node-1", "isUp": true, "draining": f.Draining}}})
	case "/v2/GetClusterLayout":
		respond(200, map[string]any{"version": f.Version, "roles": f.Roles})
	case "/v2/UpdateClusterLayout":
		f.Staged = body.Roles
		respond(200, map[string]any{})
	case "/v2/ApplyClusterLayout":
		if body.Version != f.Version+1 {
			respond(400, nil)
			return
		}
		f.Version = body.Version
		f.Roles = f.Staged
		f.Staged = nil
		respond(200, map[string]any{})
	case "/v2/GetKeyInfo":
		k := f.Keys[q.Get("id")]
		if k == nil || k.Deleted {
			respond(404, nil)
			return
		}
		respond(200, map[string]string{"name": k.Name, "secretAccessKey": k.Secret})
	case "/v2/ImportKey":
		if f.Keys[body.AccessKeyID] != nil {
			respond(409, nil)
			return
		}
		f.Keys[body.AccessKeyID] = &fakeCacheKey{Name: body.Name, Secret: body.SecretAccessKey}
		respond(200, map[string]string{"accessKeyId": body.AccessKeyID})
	case "/v2/UpdateKey":
		f.Keys[q.Get("id")].Name = body.Name
		respond(200, map[string]any{})
	case "/v2/ListKeys":
		keys := []any{}
		for id, k := range f.Keys {
			if !k.Deleted {
				keys = append(keys, map[string]string{"id": id, "name": k.Name})
			}
		}
		respond(200, keys)
	case "/v2/DeleteKey":
		id := q.Get("id")
		f.Keys[id].Deleted = true
		for _, b := range f.Buckets {
			delete(b.Permissions, id)
		}
		respond(200, map[string]any{})
	case "/v2/GetBucketInfo":
		b := f.Buckets[q.Get("globalAlias")]
		if b == nil {
			respond(404, nil)
			return
		}
		respond(200, f.view(b))
	case "/v2/CreateBucket":
		b := &fakeCacheBucket{ID: body.GlobalAlias, Alias: body.GlobalAlias, Permissions: map[string]Permission{}}
		f.Buckets[b.Alias] = b
		respond(200, f.view(b))
	case "/v2/UpdateBucket":
		b := f.Buckets[q.Get("id")]
		b.Quota = body.Quotas.MaxSize
		b.Rules = body.LifecycleRules
		respond(200, f.view(b))
	case "/v2/AllowBucketKey", "/v2/DenyBucketKey":
		k := f.Keys[body.AccessKeyID]
		if k == nil || k.Deleted {
			respond(404, nil)
			return
		}
		b := f.Buckets[body.BucketID]
		p := b.Permissions[body.AccessKeyID]
		allow := r.URL.Path == "/v2/AllowBucketKey"
		if body.Permissions.Read {
			p.Read = allow
		}
		if body.Permissions.Write {
			p.Write = allow
		}
		if body.Permissions.Owner {
			p.Owner = allow
		}
		b.Permissions[body.AccessKeyID] = p
		respond(200, f.view(b))
	default:
		respond(404, nil)
	}
}

func TestCacheFreshClusterAndIdempotentReconcile(t *testing.T) {
	f, c, log := newCacheFixture(t)
	if e := ProvisionCache(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	f.edit(func(f *fakeCache) {
		if f.Version != 1 || len(f.Roles) != 1 || len(f.Keys) != 7 || len(f.Buckets) != 5 {
			t.Fatalf("incomplete fresh cluster: %+v", f)
		}
		p := f.Projects["nsql"]
		main := f.Buckets["ci-nsql-main"]
		want := map[string]Permission{c.KeyID: {Owner: true}, p.Data["rw_id"]: {Read: true, Write: true}, p.Data["ro_id"]: {Read: true}}
		if !reflect.DeepEqual(main.Permissions, want) {
			t.Fatalf("main grants: %v", main.Permissions)
		}
		if main.Quota != c.MainQuota || len(main.Rules) != 1 {
			t.Fatal("main bucket quota/lifecycle mismatch")
		}
		release := f.Buckets["ci-nsql-release"]
		if release.Quota != c.ReleaseQuota || !reflect.DeepEqual(release.Permissions, map[string]Permission{c.KeyID: {Owner: true}, p.Data["release_id"]: {Read: true, Write: true}}) {
			t.Fatal("release grants")
		}
		toolchains := f.Buckets["toolchains"]
		if toolchains.Quota != c.ToolchainQuota || len(toolchains.Rules) != 0 || len(toolchains.Permissions) != 3 || toolchains.Permissions[c.KeyID] != (Permission{Read: true, Write: true, Owner: true}) {
			t.Fatal("toolchains contract")
		}
		f.Mutations = nil
	})
	if e := ProvisionCache(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	f.edit(func(f *fakeCache) {
		if len(f.Mutations) != 5 {
			t.Fatal(f.Mutations)
		}
		for _, path := range f.Mutations {
			if path != "/v2/UpdateBucket" {
				t.Fatalf("non-idempotent mutation: %s", path)
			}
		}
	})
	if strings.Contains(log.String(), c.KeySecret) || strings.Contains(log.String(), "admin-token") {
		t.Fatal("secret leaked")
	}
}

func TestCacheNarrowsGrantsAndRevokesRemovedProjects(t *testing.T) {
	f, c, _ := newCacheFixture(t)
	if e := ProvisionCache(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	removed := cacheProject("ui-box", 2)
	f.edit(func(f *fakeCache) {
		p := f.Projects["nsql"]
		f.Buckets["ci-nsql-main"].Permissions[p.Data["ro_id"]] = Permission{Read: true, Write: true}
		f.Buckets["ci-nsql-main"].Permissions[removed.Data["rw_id"]] = Permission{Read: true, Write: true}
		delete(f.Projects, "ui-box")
	})
	if e := ProvisionCache(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	f.edit(func(f *fakeCache) {
		p := f.Projects["nsql"]
		if f.Buckets["ci-nsql-main"].Permissions[p.Data["ro_id"]] != (Permission{Read: true}) {
			t.Fatal("readonly grant retained write")
		}
		for _, role := range []string{"rw", "ro", "release"} {
			if !f.Keys[removed.Data[role+"_id"]].Deleted {
				t.Fatal("removed project key retained")
			}
		}
		if f.Keys[c.KeyID].Deleted {
			t.Fatal("provisioner deleted")
		}
		if _, ok := f.Buckets["toolchains"].Permissions[removed.Data["release_id"]]; ok {
			t.Fatal("removed release key retained")
		}
	})
}

func TestCacheQuotaAlertsAfterFinishingAllProjects(t *testing.T) {
	f, c, _ := newCacheFixture(t)
	if e := ProvisionCache(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	f.edit(func(f *fakeCache) {
		f.Buckets["ci-nsql-main"].Bytes = c.MainQuota * 9 / 10
		f.Buckets["ci-ui-box-main"].Bytes = c.MainQuota*9/10 - 1
		f.Projects["late"] = cacheProject("late", 3)
	})
	e := ProvisionCache(context.Background(), c)
	if e == nil || !strings.Contains(e.Error(), "ci-nsql-main") || strings.Contains(e.Error(), "ci-ui-box-main") {
		t.Fatalf("incorrect quota alert: %v", e)
	}
	f.edit(func(f *fakeCache) {
		if f.Buckets["ci-late-release"] == nil || f.Buckets["toolchains"] == nil {
			t.Fatal("alert aborted provisioning")
		}
	})
}

func TestCacheRejectsChangedSecretsAndDeletedKeyReuse(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprint(deleted), func(t *testing.T) {
			f, c, log := newCacheFixture(t)
			if e := ProvisionCache(context.Background(), c); e != nil {
				t.Fatal(e)
			}
			f.edit(func(f *fakeCache) {
				p := f.Projects["nsql"]
				if deleted {
					f.Keys[p.Data["rw_id"]].Deleted = true
				} else {
					p.Data["rw_secret"] = strings.Repeat("c", 64)
				}
			})
			e := ProvisionCache(context.Background(), c)
			if e == nil {
				t.Fatal("unsafe key replacement accepted")
			}
			if strings.Contains(e.Error()+log.String(), strings.Repeat("c", 64)) {
				t.Fatal("secret leaked")
			}
			if deleted && !strings.Contains(e.Error(), "deleted key id") {
				t.Fatal(e)
			}
		})
	}
}

func TestCacheRejectsMalformedProjectsBeforeMutations(t *testing.T) {
	for _, change := range []func(*fakeCacheProject){func(p *fakeCacheProject) { p.Project = "Bad_Name" }, func(p *fakeCacheProject) { p.Name = "other-name" }, func(p *fakeCacheProject) { p.Data = map[string]string{"rw_id": "GK1"} }, func(p *fakeCacheProject) { p.Data["ro_id"] = "not-a-key" }, func(p *fakeCacheProject) { p.Data["ro_id"] = p.Data["rw_id"] }} {
		f, c, _ := newCacheFixture(t)
		f.edit(func(f *fakeCache) {
			p := cacheProject("nsql", 1)
			change(&p)
			f.Projects = map[string]fakeCacheProject{"nsql": p}
		})
		if e := ProvisionCache(context.Background(), c); e == nil {
			t.Fatal("malformed project accepted")
		}
		f.edit(func(f *fakeCache) {
			if len(f.Mutations) > 0 {
				t.Fatal("mutation before validation")
			}
		})
	}
}

func TestCacheWrongAdminTokenAndDrainingNodeRefuseMutations(t *testing.T) {
	for _, draining := range []bool{false, true} {
		t.Run(fmt.Sprint(draining), func(t *testing.T) {
			f, c, _ := newCacheFixture(t)
			if draining {
				f.edit(func(f *fakeCache) { f.Draining = true })
			} else {
				c.Admin.Token = func() (string, error) { return "wrong-token", nil }
			}
			if e := ProvisionCache(context.Background(), c); e == nil {
				t.Fatal("unsafe cluster accepted")
			}
			f.edit(func(f *fakeCache) {
				sort.Strings(f.Mutations)
				if len(f.Mutations) != 0 {
					t.Fatalf("unexpected mutations: %v", f.Mutations)
				}
			})
		})
	}
}
