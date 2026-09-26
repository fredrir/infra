package reconciler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRequestsLiveInTheirOwnersDirectoriesAndAreValidated(t *testing.T) {
	shared := sharedDirectory(t)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	repair := Request{Kind: RequestRepair, Revision: strings.Repeat("d", 40), Full: true, Reason: "1 differences", Requested: now}
	operator := Request{Kind: RequestApply, Full: true, Reason: "operator", Requested: now}
	for name, invalid := range map[string]Request{
		"repair without revision": {Kind: RequestRepair, Full: true, Requested: now},
		"partial repair":          {Kind: RequestRepair, Revision: strings.Repeat("d", 40), Requested: now},
		"apply with revision":     {Kind: RequestApply, Revision: strings.Repeat("d", 40), Requested: now},
		"unknown kind":            {Kind: "destroy", Requested: now},
		"undated":                 {Kind: RequestApply},
	} {
		if err := WriteRequest(shared, invalid); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, request := range []Request{repair, operator} {
		if err := WriteRequest(shared, request); err != nil {
			t.Fatal(err)
		}
	}
	for path, directory := range map[string]string{requestPath(shared, RequestRepair): "repairs", requestPath(shared, RequestApply): "requests"} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o640 || filepath.Base(filepath.Dir(path)) != directory {
			t.Fatalf("%s: %v %v", path, info, err)
		}
	}
	requests, err := readRequests(shared)
	if err != nil || !reflect.DeepEqual(requests, map[string]Request{RequestRepair: repair, RequestApply: operator}) {
		t.Fatalf("read %+v, %v", requests, err)
	}
	if err := os.WriteFile(requestPath(shared, RequestApply), []byte(`{"kind":"repair","revision":"`+strings.Repeat("d", 40)+`","full":true,"requested_at":"2026-09-26T12:00:00Z"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if requests, err := readRequests(shared); err == nil || !reflect.DeepEqual(requests, map[string]Request{RequestRepair: repair}) {
		t.Fatalf("a misplaced request read %+v, %v", requests, err)
	}
	if err := os.Remove(requestPath(shared, RequestApply)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(requestPath(shared, RequestRepair), requestPath(shared, RequestApply)); err != nil {
		t.Fatal(err)
	}
	if requests, err := readRequests(shared); err == nil || len(requests) != 1 {
		t.Fatalf("a linked request read %+v, %v", requests, err)
	}
}

func TestHostLockSerializesUnitsAndIsNeverCreated(t *testing.T) {
	shared := sharedDirectory(t)
	release, err := acquireHostLock(context.Background(), shared, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	waiting, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := acquireHostLock(waiting, shared, time.Millisecond); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second holder acquired the lock: %v", err)
	}
	acquired := make(chan error, 1)
	go func() {
		second, err := acquireHostLock(context.Background(), shared, time.Millisecond)
		if err == nil {
			err = second()
		}
		acquired <- err
	}()
	select {
	case err := <-acquired:
		t.Fatalf("acquired while held: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := <-acquired; err != nil {
		t.Fatalf("waiting holder: %v", err)
	}
	if info, err := os.Stat(filepath.Join(shared, "lock")); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("the lock changed: %v %v", info, err)
	}
	missing := t.TempDir()
	if _, err := acquireHostLock(context.Background(), missing, time.Millisecond); err == nil {
		t.Fatal("a missing lock was created")
	}
	if _, err := os.Stat(filepath.Join(missing, "lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the lock was created: %v", err)
	}
	linked := t.TempDir()
	if err := os.Symlink(filepath.Join(shared, "lock"), filepath.Join(linked, "lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireHostLock(context.Background(), linked, time.Millisecond); err == nil {
		t.Fatal("a linked lock was accepted")
	}
}
