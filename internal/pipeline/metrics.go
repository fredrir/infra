package pipeline

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

type Metrics struct {
	ActionsExecuted     *uint64 `json:"actions_executed,omitempty"`
	LocalCacheHits      *uint64 `json:"local_cache_hits,omitempty"`
	RemoteCacheHits     *uint64 `json:"remote_cache_hits,omitempty"`
	SystemBytesSent     *uint64 `json:"system_network_bytes_sent,omitempty"`
	SystemBytesReceived *uint64 `json:"system_network_bytes_received,omitempty"`
}

func readMetrics(path string) (*Metrics, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var result *Metrics
	for scanner.Scan() {
		var event struct {
			BuildMetrics *struct {
				ActionSummary struct {
					ActionsExecuted       json.RawMessage
					ActionCacheStatistics struct{ Hits json.RawMessage }
					RunnerCount           []struct {
						Name  string
						Count json.RawMessage
					}
				}
				NetworkMetrics struct {
					SystemNetworkStats struct{ BytesSent, BytesRecv json.RawMessage }
				}
			}
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, err
		}
		if event.BuildMetrics == nil {
			continue
		}
		metrics := event.BuildMetrics
		result = &Metrics{}
		fields := []struct {
			raw    json.RawMessage
			target **uint64
		}{
			{metrics.ActionSummary.ActionsExecuted, &result.ActionsExecuted},
			{metrics.ActionSummary.ActionCacheStatistics.Hits, &result.LocalCacheHits},
			{metrics.NetworkMetrics.SystemNetworkStats.BytesSent, &result.SystemBytesSent},
			{metrics.NetworkMetrics.SystemNetworkStats.BytesRecv, &result.SystemBytesReceived},
		}
		for _, runner := range metrics.ActionSummary.RunnerCount {
			if runner.Name == "remote cache hit" {
				fields = append(fields, struct {
					raw    json.RawMessage
					target **uint64
				}{runner.Count, &result.RemoteCacheHits})
			}
		}
		for _, field := range fields {
			if len(field.raw) == 0 {
				continue
			}
			value := string(field.raw)
			if value[0] == '"' {
				if err := json.Unmarshal(field.raw, &value); err != nil {
					return nil, err
				}
			}
			count, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid build metric: %w", err)
			}
			*field.target = &count
		}
	}
	return result, scanner.Err()
}
