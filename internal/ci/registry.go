package ci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var imageReferencePattern = regexp.MustCompile(`^ghcr\.io/(fredrir/[a-z0-9][a-z0-9._/-]*)@(sha256:[a-f0-9]{64})$`)
var imageTagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,127}$`)

type TagOptions struct{ Image, Tag, Registry, Actor, Token string }

func TagImage(ctx context.Context, client *http.Client, options TagOptions) error {
	match := imageReferencePattern.FindStringSubmatch(options.Image)
	if match == nil || !imageTagPattern.MatchString(options.Tag) || options.Actor == "" || options.Token == "" {
		return fmt.Errorf("a verified image, valid tag, actor and registry token are required")
	}
	base, err := url.Parse(options.Registry)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return fmt.Errorf("invalid registry URL")
	}
	registry := strings.TrimRight(options.Registry, "/")
	query := url.Values{"service": {"ghcr.io"}, "scope": {"repository:" + match[1] + ":pull,push"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, registry+"/token?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	request.SetBasicAuth(options.Actor, options.Token)
	body, err := registryRequest(client, request)
	if err != nil {
		return fmt.Errorf("registry authentication: %w", err)
	}
	var credentials struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &credentials); err != nil {
		return fmt.Errorf("decode registry token: %w", err)
	}
	if credentials.Token == "" {
		return fmt.Errorf("registry returned an empty token")
	}
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, registry+"/v2/"+match[1]+"/manifests/"+match[2], nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+credentials.Token)
	request.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json")
	body, err = registryRequest(client, request)
	if err != nil {
		return fmt.Errorf("read verified manifest: %w", err)
	}
	digest := sha256.Sum256(body)
	if "sha256:"+hex.EncodeToString(digest[:]) != match[2] {
		return fmt.Errorf("registry manifest checksum mismatch")
	}
	var manifest struct {
		MediaType string `json:"mediaType"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	if manifest.MediaType != "application/vnd.oci.image.index.v1+json" && manifest.MediaType != "application/vnd.oci.image.manifest.v1+json" {
		return fmt.Errorf("unsupported manifest media type")
	}
	request, err = http.NewRequestWithContext(ctx, http.MethodPut, registry+"/v2/"+match[1]+"/manifests/"+options.Tag, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+credentials.Token)
	request.Header.Set("Content-Type", manifest.MediaType)
	_, err = registryRequest(client, request)
	return err
}

func registryRequest(client *http.Client, request *http.Request) ([]byte, error) {
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("registry request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("registry returned HTTP %d", response.StatusCode)
	}
	const limit = 32 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if len(body) > limit {
		return nil, fmt.Errorf("registry response exceeds limit")
	}
	return body, err
}
