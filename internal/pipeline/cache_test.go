package pipeline

import "testing"

func TestCacheForwardingRetainsInstancePathAndRejectsNonlocalEndpoints(t *testing.T) {
	forwarded, host, port, err := localCache("http://127.0.0.1:9092/trusted")
	if err != nil || forwarded != "http://infra-cache:9092/trusted" || host != "127.0.0.1" || port != 9092 {
		t.Fatalf("invalid tunnel: %s %s %d %v", forwarded, host, port, err)
	}
	for _, raw := range []string{"http://cache.example:9092", "http://user:secret@localhost:9092", "https://localhost:9092", "http://localhost", "http://localhost:70000"} {
		if _, _, _, err := localCache(raw); err == nil {
			t.Errorf("accepted unsafe or incomplete endpoint %s", raw)
		}
	}
}
