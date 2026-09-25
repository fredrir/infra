package reconcile

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/objectstore"
)

type Status struct {
	Evaluated         string             `json:"evaluated_revision,omitempty"`
	ArtifactsVerified string             `json:"artifacts_verified_revision,omitempty"`
	Selection         Selection          `json:"selection"`
	HostScope         string             `json:"host_scope,omitempty"`
	LastFullRevision  string             `json:"last_full_revision,omitempty"`
	LastFullVerified  time.Time          `json:"last_full_verified_at,omitzero"`
	HostsReusedFrom   string             `json:"hosts_reused_from,omitempty"`
	Desired           string             `json:"desired_revision"`
	Applied           string             `json:"applied_revision"`
	Stage             string             `json:"stage"`
	Failure           string             `json:"failure,omitempty"`
	Updated           time.Time          `json:"updated_at"`
	Durations         map[string]float64 `json:"stage_seconds,omitempty"`
}

func (s Status) NeedsRecovery() bool {
	return s.Desired != s.Applied || s.Failure != "" || (s.Stage != "" && s.Stage != "complete" && s.Stage != "evaluated")
}

type Store interface {
	Read(context.Context) (Status, error)
	Write(context.Context, Status) error
	Lock(context.Context) (func() error, error)
}

type S3Store struct {
	Runner         ci.Runner
	Bucket, Prefix string
	Client         *objectstore.Client
}

type lease struct {
	Owner   string    `json:"owner"`
	Expires time.Time `json:"expires"`
}

func (s S3Store) object(ctx context.Context, key string) ([]byte, string, error) {
	if s.Client != nil {
		response, err := s.Client.Request(ctx, http.MethodGet, s.Bucket, s.Prefix+"/"+key, nil)
		if err != nil {
			var status *objectstore.StatusError
			if errors.As(err, &status) && status.Code == http.StatusNotFound {
				return nil, "", nil
			}
			return nil, "", err
		}
		defer response.Body.Close()
		etag := response.Header.Get("ETag")
		if etag == "" {
			return nil, "", fmt.Errorf("state object has no ETag")
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		if len(body) > 1<<20 {
			return nil, "", fmt.Errorf("state object exceeds size limit")
		}
		return body, etag, err
	}
	dir, err := os.MkdirTemp("", "infra-state-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "object")
	data, err := s.Runner.Output(ctx, "aws", "s3api", "get-object", "--bucket", s.Bucket, "--key", s.Prefix+"/"+key, path, "--output", "json")
	if err != nil {
		listing, listErr := s.Runner.Output(ctx, "aws", "s3api", "list-objects-v2", "--bucket", s.Bucket, "--prefix", s.Prefix+"/"+key, "--output", "json")
		if listErr != nil {
			return nil, "", errors.Join(err, listErr)
		}
		var objects struct{ Contents []struct{ Key string } }
		if decodeErr := json.Unmarshal(listing, &objects); decodeErr != nil {
			return nil, "", decodeErr
		}
		for _, object := range objects.Contents {
			if object.Key == s.Prefix+"/"+key {
				return nil, "", err
			}
		}
		return nil, "", nil
	}
	var metadata struct{ ETag string }
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, "", err
	}
	if metadata.ETag == "" {
		return nil, "", fmt.Errorf("state object has no ETag")
	}
	body, err := os.ReadFile(path)
	return body, metadata.ETag, err
}

func (s S3Store) put(ctx context.Context, key string, value any, match string) (string, error) {
	if s.Client != nil {
		body, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		headers := http.Header{"X-Amz-Server-Side-Encryption": {"AES256"}, "Content-Type": {"application/json"}}
		if match == "*" {
			headers.Set("If-None-Match", "*")
		} else if match != "" {
			headers.Set("If-Match", match)
		}
		response, err := s.Client.RequestHeaders(ctx, http.MethodPut, s.Bucket, s.Prefix+"/"+key, bytes.NewReader(body), headers)
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20)); err != nil {
			return "", err
		}
		etag := response.Header.Get("ETag")
		if etag == "" {
			return "", fmt.Errorf("state write has no ETag")
		}
		return etag, nil
	}
	dir, err := os.MkdirTemp("", "infra-state-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "object")
	body, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		return "", err
	}
	args := []string{"s3api", "put-object", "--bucket", s.Bucket, "--key", s.Prefix + "/" + key, "--body", path, "--server-side-encryption", "AES256", "--output", "json"}
	if match == "*" {
		args = append(args, "--if-none-match", "*")
	} else if match != "" {
		args = append(args, "--if-match", match)
	}
	data, err := s.Runner.Output(ctx, "aws", args...)
	if err != nil {
		return "", err
	}
	var metadata struct{ ETag string }
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", err
	}
	if metadata.ETag == "" {
		return "", fmt.Errorf("state write has no ETag")
	}
	return metadata.ETag, nil
}

func (s S3Store) Read(ctx context.Context) (Status, error) {
	data, _, err := s.object(ctx, "status.json")
	if err != nil || data == nil {
		return Status{}, err
	}
	var status Status
	err = json.Unmarshal(data, &status)
	return status, err
}

func (s S3Store) Write(ctx context.Context, status Status) error {
	_, err := s.put(ctx, "status.json", status, "")
	return err
}

func (s S3Store) Lock(ctx context.Context) (func() error, error) {
	if s.Bucket == "" || s.Prefix == "" || strings.HasPrefix(s.Prefix, "/") {
		return nil, fmt.Errorf("state bucket and prefix required")
	}
	body, etag, err := s.object(ctx, "lock.json")
	if err != nil {
		return nil, err
	}
	if body != nil {
		var existing lease
		if err := json.Unmarshal(body, &existing); err != nil {
			return nil, err
		}
		if existing.Expires.IsZero() || time.Now().Before(existing.Expires) {
			return nil, fmt.Errorf("reconciliation locked by %s until %s", existing.Owner, existing.Expires)
		}
	} else {
		etag = "*"
	}
	var owner [16]byte
	if _, err := rand.Read(owner[:]); err != nil {
		return nil, err
	}
	token, err := s.put(ctx, "lock.json", lease{hex.EncodeToString(owner[:]), time.Now().Add(2 * time.Hour)}, etag)
	if err != nil {
		return nil, fmt.Errorf("acquire reconciliation lock: %w", err)
	}
	return func() error {
		release, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if s.Client != nil {
			response, err := s.Client.RequestHeaders(release, http.MethodDelete, s.Bucket, s.Prefix+"/lock.json", nil, http.Header{"If-Match": {token}})
			if err != nil {
				return err
			}
			defer response.Body.Close()
			_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
			return err
		}
		_, err := s.Runner.Output(release, "aws", "s3api", "delete-object", "--bucket", s.Bucket, "--key", s.Prefix+"/lock.json", "--if-match", token)
		return err
	}, nil
}
