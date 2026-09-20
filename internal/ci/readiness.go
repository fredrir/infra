package ci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func WaitRevision(ctx context.Context, client *http.Client, address, revision string, interval time.Duration) error {
	u, err := url.Parse(address)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || !revisionPattern.MatchString(revision) || interval <= 0 {
		return errors.New("readiness URL, revision and positive interval required")
	}
	var last error
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Cache-Control", "no-cache, no-store")
		response, err := client.Do(request)
		if err == nil {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, 1024))
			response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK && strings.TrimSpace(string(data)) == revision {
				return nil
			}
			last = fmt.Errorf("revision not ready (HTTP %d)", response.StatusCode)
		} else {
			last = err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(ctx.Err(), last)
		case <-timer.C:
		}
	}
}
