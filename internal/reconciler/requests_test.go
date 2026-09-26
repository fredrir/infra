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

func TestRequestsAreSharedValidatedAndDiscardedWhenInvalid(t *testing.T) {
	shared := sharedDirectory(t)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	repair := Request{Kind: RequestRepair, Revision: strings.Repeat("d", 40), Full: true, Reason: "1 differences", Requested: now}
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
	if err := WriteRequest(shared, repair); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(shared, "requests", "repair.json"))
	if err != nil || info.Mode().Perm() != 0o660 {
		t.Fatalf("repair request %v: %v", info, err)
	}
	if err := os.WriteFile(filepath.Join(shared, "requests", "apply.json"), []byte(`{"kind":"repair"}`), 0o660); err != nil {
		t.Fatal(err)
	}
	requests, err := readRequests(shared, false)
	if err == nil || !reflect.DeepEqual(requests, map[string]Request{RequestRepair: repair}) {
		t.Fatalf("read %+v, %v", requests, err)
	}
	if _, err := os.Stat(filepath.Join(shared, "requests", "apply.json")); err != nil {
		t.Fatalf("a read-only pass removed the invalid request: %v", err)
	}
	if _, err := readRequests(shared, true); err == nil {
		t.Fatal("the invalid request was not reported")
	}
	if _, err := os.Stat(filepath.Join(shared, "requests", "apply.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the invalid request survived a discarding pass: %v", err)
	}
	if err := os.Symlink(filepath.Join(shared, "requests", "repair.json"), filepath.Join(shared, "requests", "apply.json")); err != nil {
		t.Fatal(err)
	}
	if requests, err := readRequests(shared, true); err == nil || len(requests) != 1 {
		t.Fatalf("a linked request read %+v, %v", requests, err)
	}
	if err := removeRequest(shared, RequestRepair); err != nil {
		t.Fatal(err)
	}
	if requests, err := readRequests(shared, true); err != nil || len(requests) != 0 {
		t.Fatalf("after removal read %+v, %v", requests, err)
	}
}

func TestHostLockSerializesUnits(t *testing.T) {
	shared := sharedDirectory(t)
	release, err := acquireHostLock(context.Background(), shared, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(shared, "lock")); err != nil || info.Mode().Perm() != 0o660 {
		t.Fatalf("lock %v: %v", info, err)
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
	linked := sharedDirectory(t)
	if err := os.Symlink(filepath.Join(shared, "lock"), filepath.Join(linked, "lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireHostLock(context.Background(), linked, time.Millisecond); err == nil {
		t.Fatal("a linked lock was accepted")
	}
}
