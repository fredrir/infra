package platformops

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"time"
)

var heartbeatToken = regexp.MustCompile(`^[A-Za-z0-9_-]{32,}$`)

func Heartbeat(ctx context.Context, endpoint, project, token string) error {
	if token == "" {
		return nil
	}
	if !slices.Contains([]string{"parser", "y", "portfolio", "attic", "control"}, project) || !heartbeatToken.MatchString(token) {
		return fmt.Errorf("invalid backup heartbeat")
	}
	if endpoint == "" {
		endpoint = "http://100.86.241.75:8080"
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/api/v1/endpoints/backups_"+project+"/external?success=true", nil)
	if err != nil {
		return fmt.Errorf("invalid heartbeat URL")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("heartbeat request failed")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("heartbeat HTTP %d", resp.StatusCode)
	}
	return nil
}
