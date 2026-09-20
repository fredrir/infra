package platformops

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type API struct {
	URL         string
	Client      *http.Client
	Token       func() (string, error)
	ContentType string
}

func (a API) Call(ctx context.Context, method, path string, input, output any, accepted ...int) (int, error) {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.URL, "/")+path, body)
	if err != nil {
		return 0, fmt.Errorf("invalid API request")
	}
	if a.Token != nil {
		token, err := a.Token()
		if err != nil {
			return 0, fmt.Errorf("read API credentials: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	if input != nil {
		contentType := a.ContentType
		if contentType == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("%s %s unreachable", method, strings.SplitN(path, "?", 2)[0])
	}
	defer resp.Body.Close()
	if len(accepted) == 0 {
		accepted = []int{http.StatusOK}
	}
	if !slices.Contains(accepted, resp.StatusCode) {
		return resp.StatusCode, fmt.Errorf("%s %s returned HTTP %d", method, strings.SplitN(path, "?", 2)[0], resp.StatusCode)
	}
	if output != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(output); err != nil {
			return resp.StatusCode, fmt.Errorf("invalid API response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func KubernetesAPI(address, account string) (API, string, error) {
	if address == "" {
		address = "https://kubernetes.default.svc"
	}
	if account == "" {
		account = "/var/run/secrets/kubernetes.io/serviceaccount"
	}
	u, err := url.Parse(address)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return API{}, "", fmt.Errorf("invalid Kubernetes API URL")
	}
	namespace, err := os.ReadFile(filepath.Join(account, "namespace"))
	if err != nil {
		return API{}, "", err
	}
	ns := strings.TrimSpace(string(namespace))
	if !resourceName.MatchString(ns) {
		return API{}, "", fmt.Errorf("invalid service account namespace")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if u.Scheme == "https" {
		pem, err := os.ReadFile(filepath.Join(account, "ca.crt"))
		if err != nil {
			return API{}, "", err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return API{}, "", fmt.Errorf("invalid service account CA")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	api := API{URL: address, Client: &http.Client{Transport: transport, Timeout: 30 * time.Second}, Token: func() (string, error) {
		data, err := os.ReadFile(filepath.Join(account, "token"))
		return string(data), err
	}}
	return api, ns, nil
}

func pause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
