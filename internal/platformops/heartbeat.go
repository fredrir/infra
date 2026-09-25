package platformops

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"time"
)

const GatusURL = "http://100.86.241.75:8080"

var heartbeatToken = regexp.MustCompile(`^[A-Za-z0-9_-]{32,}$`)

func ValidHeartbeatToken(token string) bool {
	return heartbeatToken.MatchString(token)
}

func Heartbeat(ctx context.Context, endpoint, project, token string) error {
	if token == "" {
		return nil
	}
	if !slices.Contains([]string{"parser", "y", "portfolio", "attic", "control"}, project) || !ValidHeartbeatToken(token) {
		return fmt.Errorf("invalid backup heartbeat")
	}
	return ReportHeartbeat(ctx, endpoint, "backups_"+project, token, nil)
}

func ReportHeartbeat(ctx context.Context, endpoint, key, token string, failure error) error {
	if !ValidHeartbeatToken(token) {
		return fmt.Errorf("invalid heartbeat token")
	}
	query := url.Values{"success": {strconv.FormatBool(failure == nil)}}
	if failure != nil {
		query.Set("error", failure.Error())
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cmp.Or(endpoint, GatusURL)+"/api/v1/endpoints/"+url.PathEscape(key)+"/external?"+query.Encode(), nil)
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
