package reconciler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	RequestApply    = "apply"
	RequestRepair   = "repair"
	requestLimit    = 64 << 10
	requestSkew     = 5 * time.Minute
	requestLifetime = 24 * time.Hour
)

var requestDirectories = map[string]string{RequestApply: "requests", RequestRepair: "repairs"}

type Request struct {
	Kind      string    `json:"kind"`
	Revision  string    `json:"revision,omitempty"`
	Full      bool      `json:"full"`
	Reason    string    `json:"reason,omitempty"`
	Requested time.Time `json:"requested_at"`
}

func (r Request) validate() error {
	switch {
	case r.Kind == RequestRepair && (!revisionPattern.MatchString(r.Revision) || !r.Full):
		return errors.New("a repair names a main revision and is full")
	case r.Kind == RequestApply && r.Revision != "":
		return errors.New("an apply request names no revision")
	case requestDirectories[r.Kind] == "":
		return fmt.Errorf("unknown request kind %q", r.Kind)
	case r.Requested.IsZero():
		return errors.New("requested_at is required")
	}
	return nil
}

func requestPath(shared, kind string) string {
	return filepath.Join(shared, requestDirectories[kind], kind+".json")
}

func WriteRequest(shared string, request Request) error {
	if err := request.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	path := requestPath(shared, request.Kind)
	file, err := os.CreateTemp(filepath.Dir(path), "."+request.Kind+"-")
	if err != nil {
		return fmt.Errorf("request %s: %w", request.Kind, err)
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	if err = errors.Join(err, file.Chmod(0o640), file.Close()); err != nil {
		return fmt.Errorf("request %s: %w", request.Kind, err)
	}
	return os.Rename(file.Name(), path)
}

type invalidRequest struct {
	Fingerprint string
	Reason      string
}

func readRequests(shared string, now time.Time) (map[string]Request, map[string]invalidRequest) {
	requests, invalid := map[string]Request{}, map[string]invalidRequest{}
	for _, kind := range []string{RequestApply, RequestRepair} {
		request, fingerprint, err := readRequest(requestPath(shared, kind))
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err == nil && request.Kind != kind:
			err = fmt.Errorf("kind %q", request.Kind)
		case err == nil && request.Requested.After(now.Add(requestSkew)):
			err = fmt.Errorf("requested_at %s is in the future", request.Requested.Format(time.RFC3339))
		}
		if err != nil {
			invalid[kind] = invalidRequest{Fingerprint: fingerprint, Reason: fmt.Sprintf("%s request: %v", kind, err)}
			continue
		}
		requests[kind] = request
	}
	return requests, invalid
}

func readRequest(path string) (Request, string, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Request{}, fingerprint([]byte(err.Error())), err
	}
	defer file.Close()
	if err := singleRegularFile(file); err != nil {
		return Request{}, fingerprint([]byte(err.Error())), err
	}
	data, err := io.ReadAll(io.LimitReader(file, requestLimit+1))
	if err != nil {
		return Request{}, fingerprint([]byte(err.Error())), err
	}
	if len(data) > requestLimit {
		return Request{}, fingerprint(data), errors.New("request exceeds 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		return Request{}, fingerprint(data), err
	}
	return request, fingerprint(data), request.validate()
}

func fingerprint(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
