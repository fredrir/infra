package platformops

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

type CacheConfig struct {
	Admin          API
	Kubernetes     API
	Namespace      string
	KeyID          string
	KeySecret      string
	Capacity       int64
	MainQuota      int64
	ReleaseQuota   int64
	ToolchainQuota int64
	ExpirationDays int
	AlertPercent   int
	WaitAttempts   int
	WaitInterval   time.Duration
	Log            io.Writer
}

type CacheKey struct {
	Project string
	Role    string
	Name    string
	ID      string
	Secret  string
}

type Permission struct {
	Read  bool `json:"read"`
	Write bool `json:"write"`
	Owner bool `json:"owner"`
}

var projectName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,29}$`)
var cacheKeyID = regexp.MustCompile(`^GK[0-9a-f]{24}$`)
var cacheKeySecret = regexp.MustCompile(`^[0-9a-f]{64}$`)

func ProvisionCache(ctx context.Context, c CacheConfig) error {
	if c.Log == nil {
		c.Log = io.Discard
	}
	if c.Capacity <= 0 || c.MainQuota <= 0 || c.ReleaseQuota <= 0 || c.ToolchainQuota <= 0 || c.ExpirationDays <= 0 || c.AlertPercent < 1 || c.AlertPercent > 100 || c.WaitAttempts < 1 || c.WaitInterval < 0 {
		return fmt.Errorf("invalid cache capacity, quota, or retry configuration")
	}
	if !cacheKeyID.MatchString(c.KeyID) || !cacheKeySecret.MatchString(c.KeySecret) {
		return fmt.Errorf("invalid provisioner key")
	}
	keys, err := projectKeys(ctx, c)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if key.ID == c.KeyID {
			return fmt.Errorf("project reuses provisioner key")
		}
	}
	ready := false
	for attempt := 0; attempt < c.WaitAttempts; attempt++ {
		if _, err := c.Admin.Call(ctx, http.MethodGet, "/v2/GetClusterStatus", nil, nil); err == nil {
			ready = true
			break
		}
		if attempt+1 < c.WaitAttempts {
			if err := pause(ctx, c.WaitInterval); err != nil {
				return err
			}
		}
	}
	if !ready {
		return fmt.Errorf("Garage admin API unavailable")
	}
	if err := ensureLayout(ctx, c); err != nil {
		return err
	}
	if err := ensureCacheKey(ctx, c, CacheKey{Name: "build-cache-provisioner", ID: c.KeyID, Secret: c.KeySecret}); err != nil {
		return err
	}
	wanted := make(map[string]bool)
	projects := make(map[string]bool)
	for _, key := range keys {
		if err := ensureCacheKey(ctx, c, key); err != nil {
			return err
		}
		wanted[key.ID] = true
		projects[key.Project] = true
	}
	var existing []struct {
		ID   string
		Name string
	}
	if _, err := c.Admin.Call(ctx, http.MethodGet, "/v2/ListKeys", nil, &existing); err != nil {
		return err
	}
	for _, key := range existing {
		if strings.HasPrefix(key.Name, "ci-") && !wanted[key.ID] {
			if _, err := c.Admin.Call(ctx, http.MethodPost, "/v2/DeleteKey?id="+url.QueryEscape(key.ID), nil, nil); err != nil {
				return err
			}
			fmt.Fprintln(c.Log, "Deleted key", key.Name)
		}
	}
	names := make([]string, 0, len(projects))
	for project := range projects {
		names = append(names, project)
	}
	sort.Strings(names)
	var full []string
	for _, project := range names {
		for _, pool := range []string{"main", "release"} {
			quota := c.MainQuota
			if pool == "release" {
				quota = c.ReleaseQuota
			}
			name := "ci-" + project + "-" + pool
			near, err := ensureBucket(ctx, c, name, quota, true, CacheGrants(keys, c.KeyID, project, pool))
			if err != nil {
				return err
			}
			if near {
				full = append(full, name)
			}
		}
	}
	near, err := ensureBucket(ctx, c, "toolchains", c.ToolchainQuota, false, CacheGrants(keys, c.KeyID, "", "toolchains"))
	if err != nil {
		return err
	}
	if near {
		full = append(full, "toolchains")
	}
	fmt.Fprintf(c.Log, "Build cache is provisioned for %d projects\n", len(projects))
	if len(full) > 0 {
		return fmt.Errorf("buckets above %d%% of their quota: %s", c.AlertPercent, strings.Join(full, " "))
	}
	return nil
}

func projectKeys(ctx context.Context, c CacheConfig) ([]CacheKey, error) {
	if !resourceName.MatchString(c.Namespace) {
		return nil, fmt.Errorf("invalid cache namespace")
	}
	var document struct {
		Items []struct {
			Metadata struct {
				Name   string
				Labels map[string]string
			}
			Data map[string]string
		}
	}
	path := "/api/v1/namespaces/" + url.PathEscape(c.Namespace) + "/secrets?labelSelector=" + url.QueryEscape("infra.fredrir.com/build-cache-project")
	if _, err := c.Kubernetes.Call(ctx, http.MethodGet, path, nil, &document); err != nil {
		return nil, err
	}
	keys := make([]CacheKey, 0, len(document.Items)*3)
	seen := make(map[string]bool)
	for _, item := range document.Items {
		project := item.Metadata.Labels["infra.fredrir.com/build-cache-project"]
		if !projectName.MatchString(project) || item.Metadata.Name != "build-cache-"+project {
			return nil, fmt.Errorf("invalid project secret %s", item.Metadata.Name)
		}
		for _, role := range []string{"rw", "ro", "release"} {
			id, e1 := base64.StdEncoding.DecodeString(item.Data[role+"_id"])
			secret, e2 := base64.StdEncoding.DecodeString(item.Data[role+"_secret"])
			if e1 != nil || e2 != nil || !cacheKeyID.Match(id) || !cacheKeySecret.Match(secret) {
				return nil, fmt.Errorf("invalid %s key in %s", role, item.Metadata.Name)
			}
			if seen[string(id)] {
				return nil, fmt.Errorf("duplicate key ids")
			}
			seen[string(id)] = true
			keys = append(keys, CacheKey{Project: project, Role: role, Name: "ci-" + project + "-" + role, ID: string(id), Secret: string(secret)})
		}
	}
	return keys, nil
}

func ensureLayout(ctx context.Context, c CacheConfig) error {
	var layout struct {
		Version int
		Roles   []any
	}
	if _, err := c.Admin.Call(ctx, http.MethodGet, "/v2/GetClusterLayout", nil, &layout); err != nil {
		return err
	}
	if len(layout.Roles) > 0 {
		return nil
	}
	var cluster struct {
		Nodes []struct {
			ID       string
			IsUp     bool
			Draining bool
		}
	}
	if _, err := c.Admin.Call(ctx, http.MethodGet, "/v2/GetClusterStatus", nil, &cluster); err != nil {
		return err
	}
	var live []string
	for _, node := range cluster.Nodes {
		if node.IsUp && !node.Draining {
			live = append(live, node.ID)
		}
	}
	if len(live) != 1 {
		return fmt.Errorf("expected exactly one live Garage node")
	}
	roles := map[string]any{"roles": []map[string]any{{"id": live[0], "zone": "dc1", "capacity": c.Capacity, "tags": []string{}}}}
	if _, err := c.Admin.Call(ctx, http.MethodPost, "/v2/UpdateClusterLayout", roles, nil); err != nil {
		return err
	}
	if _, err := c.Admin.Call(ctx, http.MethodPost, "/v2/ApplyClusterLayout", map[string]int{"version": layout.Version + 1}, nil); err != nil {
		return err
	}
	fmt.Fprintln(c.Log, "Applied the single-node cluster layout")
	return nil
}

func ensureCacheKey(ctx context.Context, c CacheConfig, key CacheKey) error {
	var current struct {
		Name            string
		SecretAccessKey string
	}
	status, err := c.Admin.Call(ctx, http.MethodGet, "/v2/GetKeyInfo?id="+url.QueryEscape(key.ID)+"&showSecretKey=true", nil, &current, 200, 404)
	if err != nil {
		return err
	}
	if status == 200 {
		if current.SecretAccessKey != key.Secret {
			return fmt.Errorf("key %s changed its secret; rotate with a new key id", key.Name)
		}
		if current.Name != key.Name {
			_, err = c.Admin.Call(ctx, http.MethodPost, "/v2/UpdateKey?id="+url.QueryEscape(key.ID), map[string]string{"name": key.Name}, nil)
		}
		return err
	}
	status, err = c.Admin.Call(ctx, http.MethodPost, "/v2/ImportKey", map[string]string{"accessKeyId": key.ID, "secretAccessKey": key.Secret, "name": key.Name}, nil, 200, 409)
	if err != nil {
		return err
	}
	if status == 409 {
		return fmt.Errorf("key %s reuses a deleted key id; generate a new key id", key.Name)
	}
	fmt.Fprintln(c.Log, "Imported key", key.Name)
	return nil
}

func CacheGrants(keys []CacheKey, provisioner, project, pool string) map[string]Permission {
	grants := map[string]Permission{provisioner: {Read: pool == "toolchains", Write: pool == "toolchains", Owner: true}}
	for _, key := range keys {
		if pool == "toolchains" {
			if key.Role == "release" {
				grants[key.ID] = Permission{Read: true}
			}
		} else if key.Project == project && ((pool == "main" && key.Role != "release") || (pool == "release" && key.Role == "release")) {
			grants[key.ID] = Permission{Read: true, Write: key.Role != "ro"}
		}
	}
	return grants
}

func ensureBucket(ctx context.Context, c CacheConfig, name string, quota int64, expire bool, want map[string]Permission) (bool, error) {
	var bucket struct {
		ID    string
		Bytes int64
		Keys  []struct {
			AccessKeyID string
			Permissions Permission
		}
	}
	status, err := c.Admin.Call(ctx, http.MethodGet, "/v2/GetBucketInfo?globalAlias="+url.QueryEscape(name), nil, &bucket, 200, 404)
	if err != nil {
		return false, err
	}
	if status == 404 {
		if _, err = c.Admin.Call(ctx, http.MethodPost, "/v2/CreateBucket", map[string]string{"globalAlias": name}, &bucket); err != nil {
			return false, err
		}
		fmt.Fprintln(c.Log, "Created bucket", name)
	}
	if bucket.ID == "" {
		return false, fmt.Errorf("bucket %s has no id", name)
	}
	rules := []map[string]any{}
	if expire {
		rules = append(rules, map[string]any{"ID": "expire-build-cache", "Status": "Enabled", "Expiration": map[string]int{"Days": c.ExpirationDays}, "AbortIncompleteMultipartUpload": map[string]int{"DaysAfterInitiation": 1}})
	}
	if _, err = c.Admin.Call(ctx, http.MethodPost, "/v2/UpdateBucket?id="+url.QueryEscape(bucket.ID), map[string]any{"quotas": map[string]any{"maxSize": quota, "maxObjects": nil}, "lifecycleRules": rules}, nil); err != nil {
		return false, err
	}
	have := make(map[string]Permission)
	all := make(map[string]bool)
	for _, key := range bucket.Keys {
		have[key.AccessKeyID] = key.Permissions
		all[key.AccessKeyID] = true
	}
	for id := range want {
		all[id] = true
	}
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		w, h := want[id], have[id]
		allow := Permission{Read: w.Read && !h.Read, Write: w.Write && !h.Write, Owner: w.Owner && !h.Owner}
		deny := Permission{Read: h.Read && !w.Read, Write: h.Write && !w.Write, Owner: h.Owner && !w.Owner}
		for _, change := range []struct {
			operation   string
			permissions Permission
		}{{"Allow", allow}, {"Deny", deny}} {
			if change.permissions == (Permission{}) {
				continue
			}
			body := map[string]any{"bucketId": bucket.ID, "accessKeyId": id, "permissions": change.permissions}
			if _, err = c.Admin.Call(ctx, http.MethodPost, "/v2/"+change.operation+"BucketKey", body, nil); err != nil {
				return false, err
			}
		}
	}
	threshold := (quota/100)*int64(c.AlertPercent) + (quota%100*int64(c.AlertPercent)+99)/100
	return bucket.Bytes >= threshold, nil
}
