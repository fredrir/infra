package platformops

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestSlotsPatchOnlyValidDriftedNodes(t *testing.T) {
	var mu sync.Mutex
	patched := map[string]string{}
	node := func(name, want, have string) any {
		return map[string]any{"metadata": map[string]any{"name": name, "labels": map[string]string{slotLabel: want}}, "status": map[string]any{"capacity": map[string]string{slotResource: have}}}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet {
			if r.URL.Query().Get("labelSelector") != slotLabel {
				t.Error("missing node selector")
			}
			json.NewEncoder(w).Encode(map[string]any{"items": []any{node("same", "1", "1"), node("drifted", "3", "0"), node("new", "2", ""), node("odd", `3"}}`, ""), node("empty", "", "")}})
			return
		}
		if r.Header.Get("Content-Type") != "application/merge-patch+json" {
			t.Error("incorrect patch type")
		}
		var body struct {
			Status struct{ Capacity map[string]string }
		}
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			t.Error(e)
		}
		patched[r.URL.Path] = body.Status.Capacity[slotResource]
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	var log bytes.Buffer
	if e := ReconcileSlots(context.Background(), API{URL: server.URL}, &log); e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	defer mu.Unlock()
	want := map[string]string{"/api/v1/nodes/drifted/status": "3", "/api/v1/nodes/new/status": "2"}
	if !reflect.DeepEqual(patched, want) {
		t.Fatalf("patches: %v", patched)
	}
	if !strings.Contains(log.String(), "Ignoring odd") || !strings.Contains(log.String(), "Advertised 3 CI slots on drifted (was 0)") {
		t.Fatal(log.String())
	}
}
