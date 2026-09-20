package ci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

func DeployAuthenticated(ctx context.Context, runner Runner, options DeployOptions, actor, token string) error {
	if actor == "" || token == "" {
		return fmt.Errorf("registry actor and token are required")
	}
	directory, err := os.MkdirTemp("", "infra-registry-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	data, err := json.Marshal(map[string]any{"auths": map[string]any{"ghcr.io": map[string]string{"username": actor, "password": token}}})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, "config.json"), data, 0600); err != nil {
		return err
	}
	runner.Env = append(runner.Env, "DOCKER_CONFIG="+directory, "REGISTRY_TOKEN=")
	return Deploy(ctx, runner, options)
}

func NotifyReconciler(ctx context.Context, address string, output io.Writer) error {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("invalid reconciler URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err == nil {
		response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			err = fmt.Errorf("HTTP %d", response.StatusCode)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		_, writeErr := fmt.Fprintln(output, "::warning::Flux was not notified; it reconciles on its next poll")
		return writeErr
	}
	return nil
}
