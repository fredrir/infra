package reconcile

import (
	"bytes"
	"context"
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
	Evaluated        string             `json:"evaluated_revision,omitempty"`
	Selection        Selection          `json:"selection"`
	HostScope        string             `json:"host_scope,omitempty"`
	LastFullRevision string             `json:"last_full_revision,omitempty"`
	LastFullVerified time.Time          `json:"last_full_verified_at,omitzero"`
	HostsReusedFrom  string             `json:"hosts_reused_from,omitempty"`
	Desired          string             `json:"desired_revision"`
	Applied          string             `json:"applied_revision"`
	Stage            string             `json:"stage"`
	Failure          string             `json:"failure,omitempty"`
	Updated          time.Time          `json:"updated_at"`
	Durations        map[string]float64 `json:"stage_seconds,omitempty"`
}

func (s Status) NeedsRecovery() bool {
	return s.Desired != s.Applied || s.Failure != "" || (s.Stage != "" && s.Stage != "complete" && s.Stage != "evaluated")
}

type Store interface {
	Read(context.Context) (Status, error)
	Write(context.Context, Status) error
	Lock(context.Context) (context.Context, func() error, error)
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

const (
	leaseTTL     = 10 * time.Minute
	leaseRenewal = 3 * time.Minute
)

var errLeaseLost = errors.New("reconciliation lease lost")

type ErrLocked struct {
	Owner   string
	Expires time.Time
}

func (e ErrLocked) Error() string {
	return fmt.Sprintf("reconciliation locked by %s until %s", e.Owner, e.Expires)
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

func (s S3Store) Unlocked(ctx context.Context) error {
	_, err := s.replaceableLease(ctx)
	return err
}

func (s S3Store) replaceableLease(ctx context.Context) (string, error) {
	if s.Bucket == "" || s.Prefix == "" || strings.HasPrefix(s.Prefix, "/") {
		return "", fmt.Errorf("state bucket and prefix required")
	}
	body, etag, err := s.object(ctx, "lock.json")
	if err != nil {
		return "", fmt.Errorf("read reconciliation lock: %w", err)
	}
	if body == nil {
		return "*", nil
	}
	var existing lease
	if err := json.Unmarshal(body, &existing); err != nil {
		return "", fmt.Errorf("read reconciliation lock: %w", err)
	}
	if existing.Expires.IsZero() || time.Now().Before(existing.Expires) {
		return "", ErrLocked(existing)
	}
	return etag, nil
}

func (s S3Store) Lock(ctx context.Context) (context.Context, func() error, error) {
	etag, err := s.replaceableLease(ctx)
	if err != nil {
		return nil, nil, err
	}
	current := &heldLease{store: s, owner: leaseOwner(), expires: time.Now().Add(leaseTTL)}
	current.token, err = s.put(ctx, "lock.json", lease{current.owner, current.expires}, etag)
	if err != nil {
		if _, occupied := s.replaceableLease(ctx); errors.As(occupied, new(ErrLocked)) {
			return nil, nil, occupied
		}
		return nil, nil, fmt.Errorf("acquire reconciliation lock: %w", err)
	}
	held, lose := context.WithCancelCause(ctx)
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		current.renew(held, lose, stop)
	}()
	return held, func() error {
		close(stop)
		<-stopped
		lose(nil)
		return current.release()
	}, nil
}

type heldLease struct {
	store        S3Store
	owner, token string
	expires      time.Time
}

func (l *heldLease) renew(ctx context.Context, lose context.CancelCauseFunc, stop <-chan struct{}) {
	ticker := time.NewTicker(leaseRenewal)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		expires := time.Now().Add(leaseTTL)
		token, err := l.store.put(ctx, "lock.json", lease{l.owner, expires}, l.token)
		if err == nil {
			l.token, l.expires = token, expires
			continue
		}
		if leaseTaken(err) || time.Until(l.expires) < leaseRenewal {
			lose(fmt.Errorf("%w: %w", errLeaseLost, err))
			return
		}
	}
}

func (l *heldLease) release() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := l.store
	if s.Client != nil {
		response, err := s.Client.RequestHeaders(ctx, http.MethodDelete, s.Bucket, s.Prefix+"/lock.json", nil, http.Header{"If-Match": {l.token}})
		if err != nil {
			return err
		}
		defer response.Body.Close()
		_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return err
	}
	_, err := s.Runner.Output(ctx, "aws", "s3api", "delete-object", "--bucket", s.Bucket, "--key", s.Prefix+"/lock.json", "--if-match", l.token)
	return err
}

func leaseTaken(err error) bool {
	var status *objectstore.StatusError
	return errors.As(err, &status) && status.Code == http.StatusPreconditionFailed
}

func leaseOwner() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown host"
	}
	return fmt.Sprintf("%s pid %d", host, os.Getpid())
}
