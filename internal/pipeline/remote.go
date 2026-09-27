package pipeline

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	remoteCacheProbeTimeout = 2 * time.Second
	remoteCacheTimeout      = "10s"
	remoteCacheJobs         = "100"
	capabilitiesMethod      = "/build.bazel.remote.execution.v2.Capabilities/GetCapabilities"
)

func probeRemoteCache(ctx context.Context, endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("invalid remote cache URL")
	}
	protocols := new(http.Protocols)
	var request func(context.Context) (*http.Request, error)
	switch parsed.Scheme {
	case "grpc", "grpcs":
		scheme := "https"
		if parsed.Scheme == "grpc" {
			scheme = "http"
			protocols.SetUnencryptedHTTP2(true)
		} else {
			protocols.SetHTTP2(true)
		}
		request = func(ctx context.Context) (*http.Request, error) {
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, scheme+"://"+parsed.Host+capabilitiesMethod, bytes.NewReader(make([]byte, 5)))
			if err == nil {
				request.Header.Set("Content-Type", "application/grpc")
				request.Header.Set("TE", "trailers")
			}
			return request, err
		}
	case "http", "https":
		protocols.SetHTTP1(true)
		protocols.SetHTTP2(true)
		request = func(ctx context.Context) (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodHead, strings.TrimSuffix(parsed.String(), "/")+"/ac/"+strings.Repeat("0", 64), nil)
		}
	default:
		return fmt.Errorf("unsupported remote cache scheme %q", parsed.Scheme)
	}
	ctx, cancel := context.WithTimeout(ctx, remoteCacheProbeTimeout)
	defer cancel()
	probe, err := request(ctx)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{Protocols: protocols, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
	defer client.CloseIdleConnections()
	response, err := client.Do(probe)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		return err
	}
	if parsed.Scheme == "http" || parsed.Scheme == "https" {
		if response.StatusCode >= http.StatusInternalServerError || response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return fmt.Errorf("remote cache answered HTTP %d", response.StatusCode)
		}
		return nil
	}
	status := response.Trailer.Get("Grpc-Status")
	if status == "" {
		status = response.Header.Get("Grpc-Status")
	}
	if response.StatusCode != http.StatusOK || status != "0" {
		return fmt.Errorf("remote cache answered HTTP %d with gRPC status %q", response.StatusCode, status)
	}
	return nil
}
