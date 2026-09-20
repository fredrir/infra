package platformops

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

var resourceName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
var slotCount = regexp.MustCompile(`^[0-9]+$`)

const slotLabel = "infra.fredrir.com/ci-slots"
const slotResource = "infra.fredrir.com/ci-slot"

func ReconcileSlots(ctx context.Context, api API, log io.Writer) error {
	var nodes struct {
		Items []struct {
			Metadata struct {
				Name   string
				Labels map[string]string
			}
			Status struct{ Capacity map[string]string }
		}
	}
	if _, err := api.Call(ctx, http.MethodGet, "/api/v1/nodes?labelSelector="+url.QueryEscape(slotLabel), nil, &nodes); err != nil {
		return err
	}
	for _, node := range nodes.Items {
		wanted, advertised := node.Metadata.Labels[slotLabel], node.Status.Capacity[slotResource]
		if !slotCount.MatchString(wanted) {
			fmt.Fprintf(log, "Ignoring %s: slot label %s is not a count\n", node.Metadata.Name, wanted)
			continue
		}
		if wanted == advertised {
			continue
		}
		if !resourceName.MatchString(node.Metadata.Name) {
			return fmt.Errorf("invalid node name")
		}
		patch := map[string]any{"status": map[string]any{"capacity": map[string]string{slotResource: wanted}}}
		patchAPI := api
		patchAPI.ContentType = "application/merge-patch+json"
		if _, err := patchAPI.Call(ctx, http.MethodPatch, "/api/v1/nodes/"+url.PathEscape(node.Metadata.Name)+"/status", patch, nil); err != nil {
			return err
		}
		if advertised == "" {
			advertised = "none"
		}
		fmt.Fprintf(log, "Advertised %s CI slots on %s (was %s)\n", wanted, node.Metadata.Name, advertised)
	}
	return nil
}

func RunSlots(ctx context.Context, api API, interval time.Duration, once bool, log io.Writer) error {
	if interval <= 0 {
		return fmt.Errorf("reconcile interval must be positive")
	}
	for {
		if err := ReconcileSlots(ctx, api, log); err != nil {
			return err
		}
		if once {
			return nil
		}
		if err := pause(ctx, interval); err != nil {
			return err
		}
	}
}
