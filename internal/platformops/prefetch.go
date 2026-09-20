package platformops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/process"
)

type PrefetchOptions struct {
	Name         string
	Namespace    string
	Node         string
	UtilityImage string
	Images       []string
	PullSecrets  []string
	Timeout      time.Duration
}

type PrefetchRunner func(context.Context, []string, []byte) error

var prefetchImage = regexp.MustCompile(`^ghcr[.]io/fredrir/[a-z0-9][a-z0-9_./-]*@sha256:[a-f0-9]{64}$`)

func PrefetchManifest(o PrefetchOptions) ([]byte, error) {
	if !strings.HasPrefix(o.Name, "infra-prefetch-") || len(o.Name) > 63 || !resourceName.MatchString(o.Name) || len(o.Namespace) > 63 || !resourceName.MatchString(o.Namespace) || len(o.Node) > 253 || !resourceName.MatchString(o.Node) {
		return nil, errors.New("valid prefetch name, namespace and node required")
	}
	if o.Timeout < time.Second || o.Timeout > time.Minute {
		return nil, errors.New("prefetch timeout must be within one minute")
	}
	if !strings.HasPrefix(o.UtilityImage, "ghcr.io/fredrir/platform-backup-tools@sha256:") || !prefetchImage.MatchString(o.UtilityImage) || len(o.Images) == 0 || len(o.Images) > 8 {
		return nil, errors.New("pinned platform tools and one to eight pinned images required")
	}
	var volumes, mounts, secrets []map[string]any
	for i, image := range o.Images {
		if !prefetchImage.MatchString(image) {
			return nil, errors.New("prefetch images require an owned repository and SHA256 digest")
		}
		name := fmt.Sprintf("image-%d", i)
		volumes = append(volumes, map[string]any{"name": name, "image": map[string]any{"reference": image, "pullPolicy": "IfNotPresent"}})
		mounts = append(mounts, map[string]any{"name": name, "mountPath": fmt.Sprintf("/images/%d", i), "readOnly": true})
	}
	for _, secret := range o.PullSecrets {
		if len(secret) > 63 || !resourceName.MatchString(secret) {
			return nil, errors.New("invalid image pull secret name")
		}
		secrets = append(secrets, map[string]any{"name": secret})
	}
	pod := map[string]any{
		"restartPolicy": "Never", "runtimeClassName": "gvisor", "priorityClassName": "batch",
		"automountServiceAccountToken": false, "terminationGracePeriodSeconds": 1,
		"nodeSelector":    map[string]string{"kubernetes.io/hostname": o.Node, "kubernetes.io/arch": "amd64"},
		"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 10001, "runAsGroup": 10001, "seccompProfile": map[string]string{"type": "RuntimeDefault"}},
		"volumes":         volumes,
		"containers": []map[string]any{{
			"name": "prefetch", "image": o.UtilityImage, "imagePullPolicy": "IfNotPresent",
			"command": []string{"/usr/local/bin/infra", "version"}, "volumeMounts": mounts,
			"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}},
			"resources":       map[string]any{"requests": map[string]string{"cpu": "10m", "memory": "32Mi"}, "limits": map[string]string{"cpu": "100m", "memory": "64Mi"}},
		}},
	}
	if len(secrets) > 0 {
		pod["imagePullSecrets"] = secrets
	}
	return json.MarshalIndent(map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": o.Name, "namespace": o.Namespace, "labels": map[string]string{"app.kubernetes.io/managed-by": "infra-prefetch"}},
		"spec": map[string]any{
			"backoffLimit": 0, "activeDeadlineSeconds": int64(o.Timeout.Seconds()), "ttlSecondsAfterFinished": 60,
			"template": map[string]any{"spec": pod},
		},
	}, "", "  ")
}

func Prefetch(ctx context.Context, o PrefetchOptions, run PrefetchRunner) (err error) {
	manifest, err := PrefetchManifest(o)
	if err != nil {
		return err
	}
	if run == nil {
		run = func(ctx context.Context, args []string, input []byte) error {
			_, err := process.Run(ctx, process.Options{Name: "kubectl", Args: args, Stdin: strings.NewReader(string(input))})
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	if err := run(ctx, []string{"create", "--request-timeout=5s", "-f", "-"}, manifest); err != nil {
		return fmt.Errorf("create prefetch job: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err = errors.Join(err, run(cleanup, []string{"delete", "job", o.Name, "--namespace", o.Namespace, "--wait=false", "--ignore-not-found=true", "--request-timeout=5s"}, nil))
	}()
	if err := run(ctx, []string{"wait", "job/" + o.Name, "--namespace", o.Namespace, "--for=condition=Complete", "--timeout=" + o.Timeout.String()}, nil); err != nil {
		return fmt.Errorf("prefetch incomplete: %w", err)
	}
	return nil
}
