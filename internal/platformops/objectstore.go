package platformops

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"time"

	"github.com/fredrir/infra/internal/objectstore"
	"go.yaml.in/yaml/v3"
)

type ObjectStoreSpec struct {
	Cells []ObjectStoreCell `yaml:"cells"`
}

type ObjectStoreCell struct {
	Name     string              `yaml:"name"`
	Endpoint string              `yaml:"endpoint"`
	Buckets  []ObjectStoreBucket `yaml:"buckets"`
}

type ObjectStoreBucket struct {
	Name           string `yaml:"name"`
	QuotaGiB       int64  `yaml:"quotaGiB"`
	ExpireDays     int    `yaml:"expireDays,omitempty"`
	LockDays       int    `yaml:"lockDays,omitempty"`
	NoncurrentDays int    `yaml:"noncurrentDays,omitempty"`
}

type ObjectStoreConfig struct {
	Spec         ObjectStoreSpec
	Clients      map[string]objectstore.Client
	WaitAttempts int
	WaitInterval time.Duration
	Log          io.Writer
}

var cellName = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)
var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

func ParseObjectStoreSpec(data []byte) (ObjectStoreSpec, error) {
	var spec ObjectStoreSpec
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&spec); err != nil {
		return spec, fmt.Errorf("invalid object store spec: %w", err)
	}
	if len(spec.Cells) == 0 {
		return spec, fmt.Errorf("object store spec declares no cells")
	}
	cells := map[string]bool{}
	for _, cell := range spec.Cells {
		endpoint, err := url.Parse(cell.Endpoint)
		if !cellName.MatchString(cell.Name) || cells[cell.Name] || err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.Path != "" {
			return spec, fmt.Errorf("invalid object store cell %q", cell.Name)
		}
		cells[cell.Name] = true
		buckets := map[string]bool{}
		for _, bucket := range cell.Buckets {
			retained := bucket.LockDays == 0 || bucket.NoncurrentDays > bucket.LockDays
			if !bucketName.MatchString(bucket.Name) || buckets[bucket.Name] || bucket.QuotaGiB <= 0 || bucket.ExpireDays < 0 || bucket.LockDays < 0 || bucket.NoncurrentDays < 0 || !retained || (bucket.ExpireDays > 0 && bucket.LockDays > 0) {
				return spec, fmt.Errorf("invalid bucket %q in cell %s", bucket.Name, cell.Name)
			}
			buckets[bucket.Name] = true
		}
	}
	return spec, nil
}

func ProvisionObjectStore(ctx context.Context, c ObjectStoreConfig) error {
	if c.Log == nil {
		c.Log = io.Discard
	}
	if c.WaitAttempts < 1 || c.WaitInterval < 0 {
		return fmt.Errorf("invalid object store retry configuration")
	}
	var failures []error
	for _, cell := range c.Spec.Cells {
		client, ok := c.Clients[cell.Name]
		if !ok {
			return fmt.Errorf("no credentials for object store cell %s", cell.Name)
		}
		client.Endpoint = cell.Endpoint
		if err := waitForCell(ctx, client, c.WaitAttempts, c.WaitInterval); err != nil {
			failures = append(failures, fmt.Errorf("cell %s: %w", cell.Name, err))
			continue
		}
		for _, bucket := range cell.Buckets {
			if err := ensureObjectStoreBucket(ctx, client, bucket, c.Log); err != nil {
				failures = append(failures, fmt.Errorf("cell %s bucket %s: %w", cell.Name, bucket.Name, err))
			}
		}
		fmt.Fprintf(c.Log, "Object store cell %s holds %d declared buckets\n", cell.Name, len(cell.Buckets))
	}
	return errors.Join(failures...)
}

func waitForCell(ctx context.Context, client objectstore.Client, attempts int, interval time.Duration) error {
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		var response *http.Response
		response, err = client.BucketRequest(ctx, http.MethodHead, "object-store-probe", nil, nil, nil)
		var status *objectstore.StatusError
		if err == nil {
			response.Body.Close()
			return nil
		}
		if errors.As(err, &status) && status.Status == http.StatusNotFound {
			return nil
		}
		if attempt+1 < attempts {
			if err := pause(ctx, interval); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("S3 endpoint unavailable: %w", err)
}

type versioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Status  string   `xml:"Status"`
}

type objectLockConfiguration struct {
	XMLName           xml.Name `xml:"ObjectLockConfiguration"`
	ObjectLockEnabled string   `xml:"ObjectLockEnabled"`
	Mode              string   `xml:"Rule>DefaultRetention>Mode"`
	Days              int      `xml:"Rule>DefaultRetention>Days"`
}

type encryptionConfiguration struct {
	XMLName   xml.Name `xml:"ServerSideEncryptionConfiguration"`
	Algorithm string   `xml:"Rule>ApplyServerSideEncryptionByDefault>SSEAlgorithm"`
}

type lifecycleConfiguration struct {
	XMLName xml.Name        `xml:"LifecycleConfiguration"`
	Rules   []lifecycleRule `xml:"Rule"`
}

type lifecycleRule struct {
	ID                          string                     `xml:"ID"`
	Filter                      struct{}                   `xml:"Filter"`
	Status                      string                     `xml:"Status"`
	Expiration                  *lifecycleExpiration       `xml:"Expiration,omitempty"`
	NoncurrentVersionExpiration *lifecycleNoncurrentExpiry `xml:"NoncurrentVersionExpiration,omitempty"`
	AbortIncompleteMultipart    *lifecycleAbort            `xml:"AbortIncompleteMultipartUpload,omitempty"`
}

type lifecycleExpiration struct {
	Days                      int  `xml:"Days,omitempty"`
	ExpiredObjectDeleteMarker bool `xml:"ExpiredObjectDeleteMarker,omitempty"`
}

type lifecycleNoncurrentExpiry struct {
	NoncurrentDays int `xml:"NoncurrentDays"`
}

type lifecycleAbort struct {
	DaysAfterInitiation int `xml:"DaysAfterInitiation"`
}

func objectStoreLifecycle(bucket ObjectStoreBucket) lifecycleConfiguration {
	rules := []lifecycleRule{{ID: "abort-incomplete-uploads", Status: "Enabled", AbortIncompleteMultipart: &lifecycleAbort{DaysAfterInitiation: 1}}}
	if bucket.ExpireDays > 0 {
		rules = append(rules, lifecycleRule{ID: "expire-objects", Status: "Enabled", Expiration: &lifecycleExpiration{Days: bucket.ExpireDays}})
	}
	if bucket.NoncurrentDays > 0 {
		rules = append(rules,
			lifecycleRule{ID: "expire-noncurrent-versions", Status: "Enabled", NoncurrentVersionExpiration: &lifecycleNoncurrentExpiry{NoncurrentDays: bucket.NoncurrentDays}},
			lifecycleRule{ID: "remove-expired-delete-markers", Status: "Enabled", Expiration: &lifecycleExpiration{ExpiredObjectDeleteMarker: true}})
	}
	return lifecycleConfiguration{Rules: rules}
}

func ensureObjectStoreBucket(ctx context.Context, client objectstore.Client, bucket ObjectStoreBucket, log io.Writer) error {
	response, err := client.BucketRequest(ctx, http.MethodHead, bucket.Name, nil, nil, nil)
	var status *objectstore.StatusError
	switch {
	case err == nil:
		response.Body.Close()
	case errors.As(err, &status) && status.Status == http.StatusNotFound:
		if err := bucketCall(ctx, client, bucket.Name, nil, nil); err != nil {
			return fmt.Errorf("create: %w", err)
		}
		fmt.Fprintln(log, "Created bucket", bucket.Name)
	default:
		return err
	}
	var drift []error
	versioning, _, err := currentSetting[versioningConfiguration](ctx, client, bucket.Name, "versioning")
	if err != nil {
		return fmt.Errorf("versioning: %w", err)
	}
	lock, locked, err := currentSetting[objectLockConfiguration](ctx, client, bucket.Name, "object-lock")
	if err != nil {
		return fmt.Errorf("object-lock: %w", err)
	}
	versioned := bucket.LockDays > 0 || bucket.NoncurrentDays > 0
	if !versioned && versioning.Status != "" {
		drift = append(drift, fmt.Errorf("versioning is %s on an unversioned bucket", versioning.Status))
	}
	if bucket.LockDays == 0 && locked && lock.ObjectLockEnabled != "" {
		drift = append(drift, fmt.Errorf("object lock is enabled on an unlocked bucket"))
	}
	if _, err := applySetting(ctx, client, bucket.Name, "encryption", encryptionConfiguration{Algorithm: "AES256"}, log); err != nil {
		return err
	}
	if versioned {
		if _, err := applySetting(ctx, client, bucket.Name, "versioning", versioningConfiguration{Status: "Enabled"}, log); err != nil {
			return err
		}
	}
	restored := map[string]bool{"versioning": versioned && versioning.Status == "Suspended"}
	if bucket.LockDays > 0 {
		if restored["object-lock"], err = applySetting(ctx, client, bucket.Name, "object-lock", objectLockConfiguration{ObjectLockEnabled: "Enabled", Mode: "COMPLIANCE", Days: bucket.LockDays}, log); err != nil {
			return err
		}
	}
	if restored["lifecycle"], err = applySetting(ctx, client, bucket.Name, "lifecycle", objectStoreLifecycle(bucket), log); err != nil {
		return err
	}
	if bucket.LockDays > 0 {
		for _, subresource := range []string{"versioning", "object-lock", "lifecycle"} {
			if restored[subresource] {
				drift = append(drift, fmt.Errorf("%s had drifted on a locked bucket and was restored", subresource))
			}
		}
	}
	if err := applyQuota(ctx, client, bucket, log); err != nil {
		return err
	}
	return errors.Join(drift...)
}

type bucketQuota struct {
	Size    int64  `json:"quota_size"`
	Unit    string `json:"quota_unit"`
	Enabled bool   `json:"quota_enabled"`
}

func applyQuota(ctx context.Context, client objectstore.Client, bucket ObjectStoreBucket, log io.Writer) error {
	want := bucketQuota{Size: bucket.QuotaGiB << 30, Unit: "B", Enabled: true}
	body, err := bucketRead(ctx, client, bucket.Name, "seaweedfs-quota")
	if err != nil {
		return fmt.Errorf("quota: %w", err)
	}
	var current bucketQuota
	if json.Unmarshal(body, &current) == nil && current == want {
		return nil
	}
	document, err := json.Marshal(want)
	if err != nil {
		return err
	}
	if err := bucketCall(ctx, client, bucket.Name, url.Values{"seaweedfs-quota": {""}}, document); err != nil {
		return fmt.Errorf("quota: %w", err)
	}
	fmt.Fprintln(log, "Set quota on", bucket.Name)
	return nil
}

func applySetting[T any](ctx context.Context, client objectstore.Client, bucket, subresource string, want T, log io.Writer) (bool, error) {
	current, found, err := currentSetting[T](ctx, client, bucket, subresource)
	if err != nil {
		return false, fmt.Errorf("%s: %w", subresource, err)
	}
	if found && reflect.DeepEqual(current, want) {
		return false, nil
	}
	body, err := xml.Marshal(want)
	if err != nil {
		return false, err
	}
	if err := bucketCall(ctx, client, bucket, url.Values{subresource: {""}}, body); err != nil {
		return false, fmt.Errorf("%s: %w", subresource, err)
	}
	fmt.Fprintf(log, "Set %s on %s\n", subresource, bucket)
	return found, nil
}

func currentSetting[T any](ctx context.Context, client objectstore.Client, bucket, subresource string) (T, bool, error) {
	var current T
	body, err := bucketRead(ctx, client, bucket, subresource)
	if err != nil || body == nil {
		return current, false, err
	}
	if err := xml.Unmarshal(body, &current); err != nil {
		return current, false, err
	}
	reflect.ValueOf(&current).Elem().FieldByName("XMLName").Set(reflect.ValueOf(xml.Name{}))
	return current, true, nil
}

func bucketRead(ctx context.Context, client objectstore.Client, bucket, subresource string) ([]byte, error) {
	response, err := client.BucketRequest(ctx, http.MethodGet, bucket, url.Values{subresource: {""}}, nil, nil)
	var status *objectstore.StatusError
	if errors.As(err, &status) && status.Status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	return io.ReadAll(io.LimitReader(response.Body, 1<<20))
}

func bucketCall(ctx context.Context, client objectstore.Client, bucket string, query url.Values, body []byte) error {
	response, err := client.BucketRequest(ctx, http.MethodPut, bucket, query, bytes.NewReader(body), nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return err
}
