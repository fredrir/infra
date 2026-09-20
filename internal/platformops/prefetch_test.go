package platformops

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func prefetchFixture() PrefetchOptions {
	return PrefetchOptions{Name: "infra-prefetch-test", Namespace: "llunde", Node: "fredrir-09", UtilityImage: "ghcr.io/fredrir/platform-backup-tools@sha256:" + strings.Repeat("a", 64), Images: []string{"ghcr.io/fredrir/llunde-frontend@sha256:" + strings.Repeat("b", 64)}, PullSecrets: []string{"ghcr"}, Timeout: 10 * time.Second}
}

func TestPrefetchMountsImagesWithoutExecutingTheirContents(t *testing.T) {
	o := prefetchFixture()
	data, err := PrefetchManifest(o)
	if err != nil {
		t.Fatal(err)
	}
	var job map[string]any
	if err := json.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	spec := job["spec"].(map[string]any)
	pod := spec["template"].(map[string]any)["spec"].(map[string]any)
	containers := pod["containers"].([]any)
	container := containers[0].(map[string]any)
	volume := pod["volumes"].([]any)[0].(map[string]any)
	if len(containers) != 1 || container["image"] != o.UtilityImage || volume["image"].(map[string]any)["reference"] != o.Images[0] {
		t.Fatal("application image must only be mounted as a volume")
	}
	if pod["automountServiceAccountToken"] != false || pod["nodeName"] != nil || spec["ttlSecondsAfterFinished"] != float64(60) {
		t.Fatal("prefetch must retain isolation and automatic cleanup")
	}
	if container["volumeMounts"].([]any)[0].(map[string]any)["readOnly"] != true || pod["imagePullSecrets"].([]any)[0].(map[string]any)["name"] != "ghcr" {
		t.Fatal("prefetch must use read-only image mounts and namespace credentials")
	}
}

func TestPrefetchRejectsMutableAndForeignImagesBeforeCallingKubernetes(t *testing.T) {
	for _, image := range []string{"ghcr.io/fredrir/app:main", "ghcr.io/foreign/app@sha256:" + strings.Repeat("a", 64)} {
		o := prefetchFixture()
		o.Images = []string{image}
		if err := Prefetch(context.Background(), o, func(context.Context, []string, []byte) error {
			t.Fatal("invalid input reached Kubernetes")
			return nil
		}); err == nil {
			t.Fatal("invalid image accepted")
		}
	}
}

func TestPrefetchCleansUpAfterCanceledWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var commands []string
	err := Prefetch(ctx, prefetchFixture(), func(ctx context.Context, args []string, _ []byte) error {
		commands = append(commands, args[0])
		if args[0] == "wait" {
			cancel()
			return context.Canceled
		}
		if args[0] == "delete" && ctx.Err() != nil {
			t.Fatal("cleanup inherited cancellation")
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || strings.Join(commands, ",") != "create,wait,delete" {
		t.Fatalf("commands %v, error %v", commands, err)
	}
}

func TestPrefetchDoesNotDeleteAnExistingJobAfterCreateFails(t *testing.T) {
	calls := 0
	err := Prefetch(context.Background(), prefetchFixture(), func(context.Context, []string, []byte) error {
		calls++
		return errors.New("already exists")
	})
	if err == nil || calls != 1 {
		t.Fatalf("calls %d, error %v", calls, err)
	}
}
