package enrollment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Request struct {
	Method, Path, Token string
	Document            map[string]any
	Form                url.Values
}

type API func(context.Context, Request) (map[string]any, error)

func HTTPAPI(base string, client *http.Client) API {
	if base == "" {
		base = "https://api.tailscale.com/api/v2"
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return fmt.Errorf("API redirect refused") }
	return func(ctx context.Context, input Request) (map[string]any, error) {
		var data []byte
		contentType := "application/json"
		if input.Document != nil {
			var err error
			data, err = json.Marshal(input.Document)
			if err != nil {
				return nil, fmt.Errorf("invalid API request")
			}
		}
		if input.Form != nil {
			data = []byte(input.Form.Encode())
			contentType = "application/x-www-form-urlencoded"
		}
		req, err := http.NewRequestWithContext(ctx, input.Method, base+input.Path, bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("invalid API request")
		}
		req.Header.Set("Accept", "application/json")
		if data != nil {
			req.Header.Set("Content-Type", contentType)
		}
		if input.Token != "" {
			req.Header.Set("Authorization", "Bearer "+input.Token)
		}
		response, err := copyClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("API %s failed; sensitive details withheld", input.Method)
		}
		defer response.Body.Close()
		keyPath := regexp.MustCompile(`^/tailnet/-/keys/[A-Za-z0-9_-]{4,128}$`).MatchString(input.Path)
		if response.StatusCode == 404 && keyPath && (input.Method == "GET" || input.Method == "DELETE") {
			return nil, nil
		}
		if response.StatusCode != 200 && response.StatusCode != 201 && response.StatusCode != 204 {
			return nil, fmt.Errorf("API %s failed with HTTP %d; response withheld", input.Method, response.StatusCode)
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, 65537))
		if err != nil || len(raw) > 65536 {
			return nil, fmt.Errorf("unexpected API response")
		}
		if len(raw) == 0 {
			return map[string]any{}, nil
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && input.Method == "DELETE" && keyPath && (response.StatusCode == 200 || response.StatusCode == 204) {
			return map[string]any{}, nil
		}
		var result map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&result) != nil || result == nil {
			return nil, fmt.Errorf("unexpected API response structure")
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return nil, fmt.Errorf("unexpected trailing API response")
		}
		return result, nil
	}
}

func accessToken(ctx context.Context, api API, role string, environment map[string]string) (string, error) {
	prefix := "TAILSCALE_ENROLL"
	if environment["TS_API_CLIENT_ID"] != "" || environment["TS_API_CLIENT_SECRET"] != "" {
		prefix = "TS_API"
	}
	id, secret := environment[prefix+"_CLIENT_ID"], environment[prefix+"_CLIENT_SECRET"]
	if id == "" || secret == "" {
		return "", fmt.Errorf("complete enrollment OAuth credentials required")
	}
	result, err := api(ctx, Request{Method: "POST", Path: "/oauth/token", Form: url.Values{"grant_type": {"client_credentials"}, "client_id": {id}, "client_secret": {secret}, "scope": {"auth_keys"}, "tags": {roles[role]}}})
	if err != nil {
		return "", err
	}
	token, ok := result["access_token"].(string)
	tokenType, _ := result["token_type"].(string)
	scope, _ := result["scope"].(string)
	expiry, expiryOK := result["expires_in"].(json.Number)
	seconds, err := expiry.Int64()
	if !ok || token == "" || !strings.EqualFold(tokenType, "bearer") || strings.Join(strings.Fields(scope), " ") != "auth_keys" || !expiryOK || err != nil || seconds < 60 || seconds > 3600 {
		return "", fmt.Errorf("OAuth token did not return requested narrow scope")
	}
	return token, nil
}
