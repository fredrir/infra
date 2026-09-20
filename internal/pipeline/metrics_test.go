package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildMetricsPreserveObservedCountsWithoutInventingMissingCounters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	data := `{"id":{"started":{}},"started":{}}
{"buildMetrics":{"actionSummary":{"actionsExecuted":"208","runnerCount":[{"name":"remote cache hit","count":6}],"actionCacheStatistics":{"misses":208}},"networkMetrics":{"systemNetworkStats":{"bytesSent":"330840099","bytesRecv":"17500916"}}}}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	metrics, err := readMetrics(path)
	if err != nil {
		t.Fatal(err)
	}
	if metrics == nil || metrics.ActionsExecuted == nil || *metrics.ActionsExecuted != 208 || metrics.RemoteCacheHits == nil || *metrics.RemoteCacheHits != 6 || metrics.SystemBytesSent == nil || *metrics.SystemBytesSent != 330840099 || metrics.SystemBytesReceived == nil || *metrics.SystemBytesReceived != 17500916 {
		t.Fatalf("unexpected metrics: %+v", metrics)
	}
	encoded, err := json.Marshal(metrics)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "local_cache_hits") {
		t.Fatalf("invented absent cache metric: %s", encoded)
	}
	if err := os.WriteFile(path, []byte(`{"buildMetrics":{"actionSummary":{"actionsExecuted":"0"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	metrics, err = readMetrics(path)
	if err != nil || metrics.ActionsExecuted == nil || *metrics.ActionsExecuted != 0 {
		t.Fatalf("lost observed zero: %+v %v", metrics, err)
	}
	if value, err := readMetrics(filepath.Join(t.TempDir(), "absent")); value != nil || err != nil {
		t.Fatalf("missing event file: %+v %v", value, err)
	}
}
