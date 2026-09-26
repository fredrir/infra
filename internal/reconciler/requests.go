package reconciler

import (
	"bytes"
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
	RequestApply  = "apply"
	RequestRepair = "repair"
	requestLimit  = 64 << 10
)

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
	case r.Kind != RequestApply && r.Kind != RequestRepair:
		return fmt.Errorf("unknown request kind %q", r.Kind)
	case r.Requested.IsZero():
		return errors.New("requested_at is required")
	}
	return nil
}

func requestsDirectory(shared string) string {
	return filepath.Join(shared, "requests")
}

func WriteRequest(shared string, request Request) error {
	if err := request.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	directory := requestsDirectory(shared)
	file, err := os.CreateTemp(directory, "."+request.Kind+"-")
	if err != nil {
		return fmt.Errorf("request %s: %w", request.Kind, err)
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	if err = errors.Join(err, file.Chmod(0o660), file.Close()); err != nil {
		return fmt.Errorf("request %s: %w", request.Kind, err)
	}
	return os.Rename(file.Name(), filepath.Join(directory, request.Kind+".json"))
}

func readRequests(shared string, discard bool) (map[string]Request, error) {
	requests := map[string]Request{}
	var problems []error
	for _, kind := range []string{RequestApply, RequestRepair} {
		path := filepath.Join(requestsDirectory(shared), kind+".json")
		request, err := readRequest(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err == nil && request.Kind != kind:
			err = fmt.Errorf("kind %q", request.Kind)
			fallthrough
		case err != nil:
			if discard {
				err = errors.Join(err, removeRequest(shared, kind))
			}
			problems = append(problems, fmt.Errorf("invalid %s request: %w", kind, err))
		default:
			requests[kind] = request
		}
	}
	return requests, errors.Join(problems...)
}

func readRequest(path string) (Request, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Request{}, err
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return Request{}, errors.Join(errors.New("not a regular file"), err)
	}
	data, err := io.ReadAll(io.LimitReader(file, requestLimit+1))
	if err != nil {
		return Request{}, err
	}
	if len(data) > requestLimit {
		return Request{}, errors.New("request exceeds 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		return Request{}, err
	}
	return request, request.validate()
}

func removeRequest(shared, kind string) error {
	if err := os.Remove(filepath.Join(requestsDirectory(shared), kind+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
