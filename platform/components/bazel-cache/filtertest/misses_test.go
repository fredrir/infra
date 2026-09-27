package filtertest

import "testing"

func TestCacheMissesKeepTheirStatusThroughTheFilter(t *testing.T) {
	c := startCache(t)
	for name, address := range map[string]string{"reader": c.reader, "writer": c.writer} {
		connection := connect(t, address, peerIP)
		failed := 0
		for i := range 300 {
			if written, err := cachedAction(connection, []byte{byte(i), byte(i >> 8), 7}); err != nil || written {
				failed++
			}
		}
		if failed != 0 {
			t.Errorf("%s lost the status of %d of 300 cache misses", name, failed)
		}
	}
}
