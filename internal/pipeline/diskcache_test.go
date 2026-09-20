package pipeline

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestDiskCacheEvictsOldEntriesAndPreservesActiveEntries(t *testing.T) {
	now := time.Unix(1800000000, 0)
	record := func(age time.Duration, size int64, name string) string {
		return fmt.Sprintf("%d %d %s/%s\x00", now.Add(-age).Unix(), size, diskCachePath, name)
	}
	data := record(time.Minute, 500, "cas/active") + record(8*24*time.Hour, 100, "ac/expired") + record(time.Hour, 200, "cas/older")
	selected, err := diskCacheCandidates(data, now, 600, 7*24*time.Hour)
	expected := []string{diskCachePath + "/ac/expired", diskCachePath + "/cas/older"}
	if err != nil || !reflect.DeepEqual(selected, expected) {
		t.Fatalf("evictions=%v, error=%v", selected, err)
	}
	selected, err = diskCacheCandidates(record(time.Minute, 900, "cas/active"), now, 100, time.Hour)
	if err != nil || len(selected) != 0 {
		t.Fatalf("active build entries selected: %v %v", selected, err)
	}
	for _, data := range []string{"bad", "1 2 /etc/passwd\x00", "1 -2 " + diskCachePath + "/cas/key\x00"} {
		if _, err := diskCacheCandidates(data, now, 100, time.Hour); err == nil {
			t.Fatal("invalid metadata accepted")
		}
	}
}
