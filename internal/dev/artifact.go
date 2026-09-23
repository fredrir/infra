package dev

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

const encryptedFields = "^(data|stringData)$"

func buildArtifact(ctx context.Context, opts ClusterOptions, recipient string) (string, error) {
	artifact := filepath.Join(opts.State.Cluster(), "artifact")
	if err := os.RemoveAll(artifact); err != nil {
		return "", err
	}
	if err := copyTree(filepath.Join(opts.State.Root, "platform"), filepath.Join(artifact, "platform")); err != nil {
		return "", err
	}
	config, err := filepath.Abs(filepath.Join(opts.State.Cluster(), "sops.yaml"))
	if err != nil {
		return "", err
	}
	rules := fmt.Sprintf("creation_rules:\n- age: %s\n  encrypted_regex: %s\n", recipient, encryptedFields)
	if err := os.WriteFile(config, []byte(rules), 0o600); err != nil {
		return "", err
	}
	return artifact, filepath.WalkDir(artifact, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sops.yaml") {
			return err
		}
		relative, err := filepath.Rel(artifact, path)
		if err != nil {
			return err
		}
		plaintext, err := os.ReadFile(filepath.Join(opts.State.Root, clusterSecretsDir, relative))
		if os.IsNotExist(err) {
			encrypted, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			plaintext, err = synthesizeSecret(encrypted)
			if err != nil {
				return fmt.Errorf("%s: %w", relative, err)
			}
		} else if err != nil {
			return err
		}
		if err := os.WriteFile(path, plaintext, 0o600); err != nil {
			return err
		}
		if _, err := capture(ctx, opts.Runner, opts.tool("sops"), "--config", config, "--encrypt", "--in-place", path); err != nil {
			return fmt.Errorf("encrypt %s: %w", relative, err)
		}
		return nil
	})
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

func synthesizeSecret(encrypted []byte) ([]byte, error) {
	var secret map[string]any
	if err := yaml.Unmarshal(encrypted, &secret); err != nil {
		return nil, err
	}
	if secret["kind"] != "Secret" {
		return nil, fmt.Errorf("only Secrets are synthesized, found %v", secret["kind"])
	}
	secretType := "Opaque"
	if declared, ok := secret["type"].(string); ok && declared != "" {
		secretType = declared
	}
	keys := make(map[string]bool)
	for _, field := range []string{"data", "stringData"} {
		if values, ok := secret[field].(map[string]any); ok {
			for key := range values {
				keys[key] = true
			}
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("secret declares no keys")
	}
	names := make([]string, 0, len(keys))
	for key := range keys {
		names = append(names, key)
	}
	sort.Strings(names)
	values := make(map[string]string, len(names))
	for _, key := range names {
		values[key] = "dev"
		if secretType == "kubernetes.io/dockerconfigjson" && key == ".dockerconfigjson" {
			values[key] = `{"auths":{}}`
		}
	}
	synthesized := map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": secret["metadata"], "type": secretType, "stringData": values}
	if immutable, ok := secret["immutable"]; ok {
		synthesized["immutable"] = immutable
	}
	return yaml.Marshal(synthesized)
}
