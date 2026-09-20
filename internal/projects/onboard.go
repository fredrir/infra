package projects

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fredrir/infra/internal/ci"
	"go.yaml.in/yaml/v3"
)

const OwnerID int64 = 114402558
const imageWorkflow = "fredrir/infra/.github/workflows/build-image.yml"

type Identity struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	Owner    struct {
		ID int64 `json:"id"`
	} `json:"owner"`
}
type RepositoryResolver interface {
	Repository(context.Context, string) (Identity, error)
}
type NativeProvider struct{ Runner ci.Runner }

var repositoryPattern = regexp.MustCompile(`^fredrir/[A-Za-z0-9_.-]+$`)
var projectPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,29}$`)
var rustProjectPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,24}$`)
var revisionPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var imagePattern = regexp.MustCompile(`^ghcr\.io/fredrir/[a-z0-9][a-z0-9._/-]*@sha256:[a-f0-9]{64}$`)

func (provider NativeProvider) Repository(ctx context.Context, repository string) (Identity, error) {
	if !repositoryPattern.MatchString(repository) {
		return Identity{}, fmt.Errorf("invalid repository")
	}
	data, err := provider.Runner.Output(ctx, "gh", "api", "repos/"+repository)
	if err != nil {
		return Identity{}, err
	}
	var identity Identity
	if err := json.Unmarshal(data, &identity); err != nil {
		return identity, err
	}
	if err := validateIdentity(identity, repository); err != nil {
		return identity, err
	}
	return identity, nil
}

func validateIdentity(identity Identity, repository string) error {
	if identity.ID <= 0 || identity.Owner.ID != OwnerID || !strings.EqualFold(identity.FullName, repository) || !repositoryPattern.MatchString(identity.FullName) {
		return fmt.Errorf("repository identity does not match")
	}
	return nil
}

type OnboardOptions struct {
	Repository, Project, Image, SourceRevision, WorkflowRef, Domain, HealthPath, Architecture, TestCommand, Output string
	Port                                                                                                           int
}

func (options OnboardOptions) Validate() error {
	if !repositoryPattern.MatchString(options.Repository) || !projectPattern.MatchString(options.Project) || !imagePattern.MatchString(options.Image) || !revisionPattern.MatchString(options.SourceRevision) || !revisionPattern.MatchString(options.WorkflowRef) {
		return fmt.Errorf("invalid project, repository, immutable image or revision")
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*\.fredrir\.com$`).MatchString(options.Domain) || !regexp.MustCompile(`^/[^\s]{0,255}$`).MatchString(options.HealthPath) {
		return fmt.Errorf("invalid shared gateway domain or health path")
	}
	if options.Port < 1024 || options.Port > 65535 {
		return fmt.Errorf("port must be between 1024 and 65535")
	}
	if options.Architecture != "amd64" {
		return fmt.Errorf("native ARM runners and deployment images are not qualified")
	}
	if options.TestCommand == "" || options.Output == "" {
		return fmt.Errorf("test command and output are required")
	}
	return nil
}

func Onboard(ctx context.Context, resolver RepositoryResolver, options OnboardOptions) error {
	if err := options.Validate(); err != nil {
		return err
	}
	if err := requireAbsent(options.Output); err != nil {
		return err
	}
	identity, err := resolver.Repository(ctx, options.Repository)
	if err != nil {
		return err
	}
	files, err := ProjectFiles(options, identity)
	if err != nil {
		return err
	}
	return writeFreshDirectory(options.Output, files)
}

func ProjectFiles(options OnboardOptions, identity Identity) (map[string][]byte, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if err := validateIdentity(identity, options.Repository); err != nil {
		return nil, err
	}
	namespace := "project-" + options.Project
	workload := map[string]any{"kind": "web", "replicas": 0, "image": options.Image, "sourceRevision": options.SourceRevision, "architectures": []string{"amd64"}, "port": options.Port, "healthPath": options.HealthPath, "domains": []string{options.Domain}, "resources": map[string]any{"requests": map[string]string{"cpu": "100m", "memory": "128Mi", "ephemeral-storage": "256Mi"}, "limits": map[string]string{"cpu": "500m", "memory": "512Mi", "ephemeral-storage": "1Gi"}}}
	release := map[string]any{"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease", "metadata": map[string]string{"name": options.Project, "namespace": namespace}, "spec": map[string]any{"interval": "10m", "releaseName": options.Project, "chart": map[string]any{"spec": map[string]any{"chart": "./charts/project", "reconcileStrategy": "Revision", "sourceRef": map[string]string{"kind": "GitRepository", "name": "flux-system", "namespace": "flux-system"}}}, "values": map[string]any{"project": options.Project, "workloads": map[string]any{"web": workload}}}}
	resources := []map[string]any{{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": namespace, "labels": map[string]string{"infra.fredrir.com/tier": "project", "infra.fredrir.com/managed": "true", "pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/enforce-version": "v1.36"}}},
		{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": map[string]string{"name": "default-deny", "namespace": namespace}, "spec": map[string]any{"podSelector": map[string]any{}, "policyTypes": []string{"Ingress", "Egress"}}},
		{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": map[string]string{"name": "dns", "namespace": namespace}, "spec": map[string]any{"podSelector": map[string]any{}, "policyTypes": []string{"Egress"}, "egress": []any{map[string]any{"to": []any{map[string]any{"namespaceSelector": map[string]any{"matchLabels": map[string]string{"kubernetes.io/metadata.name": "kube-system"}}, "podSelector": map[string]any{"matchLabels": map[string]string{"k8s-app": "kube-dns"}}}}, "ports": []any{map[string]any{"port": 53, "protocol": "UDP"}, map[string]any{"port": 53, "protocol": "TCP"}}}}}},
	}
	caller := map[string]any{"name": "Build", "on": map[string]any{"push": map[string]any{"branches": []string{"main"}}}, "permissions": map[string]string{"contents": "read", "packages": "write", "id-token": "write", "attestations": "write"}, "jobs": map[string]any{"build": map[string]any{"if": fmt.Sprintf("github.event_name == 'push' && github.ref_protected && github.repository_id == '%d' && github.repository_owner_id == '%d'", identity.ID, OwnerID), "uses": imageWorkflow + "@" + options.WorkflowRef, "with": map[string]string{"image": strings.SplitN(options.Image, "@", 2)[0], "test-command": options.TestCommand}}}}
	visibility := "public"
	if identity.Private {
		visibility = "private"
	}
	policy := TrustPolicy(identity, "ref:refs/heads/main", "github-hosted", map[string]string{"event_name": "^push$", "ref": "^refs/heads/main$", "job_workflow_ref": "^" + regexp.QuoteMeta(imageWorkflow+"@"+options.WorkflowRef) + "$", "job_workflow_sha": "^" + options.WorkflowRef + "$"}, map[string]string{"actions": "write"})
	documents := map[string]any{
		"project/release.yaml": release, "project/kustomization.yaml": map[string]any{"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "namespace": namespace, "resources": []string{"baseline.yaml", "release.yaml"}}, "caller/.github/workflows/build.yaml": caller,
		fmt.Sprintf("infrastructure/.github/deployments/%d.yaml", identity.ID): map[string]any{"repository": identity.FullName, "visibility": visibility, "images": map[string]any{strings.SplitN(options.Image, "@", 2)[0]: map[string]string{"path": "platform/projects/" + options.Project, "mode": "helmrelease", "workload": "web"}}}, fmt.Sprintf("infrastructure/.github/chainguard/deploy-%d.sts.yaml", identity.ID): policy,
	}
	files := map[string][]byte{}
	for path, document := range documents {
		data, err := yaml.Marshal(document)
		if err != nil {
			return nil, err
		}
		files[path] = data
	}
	var baseline strings.Builder
	encoder := yaml.NewEncoder(&baseline)
	for _, resource := range resources {
		if err := encoder.Encode(resource); err != nil {
			return nil, err
		}
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	files["project/baseline.yaml"] = []byte(baseline.String())
	return files, nil
}

func TrustPolicy(identity Identity, subject, environment string, claims, permissions map[string]string) map[string]any {
	name := regexp.QuoteMeta(strings.SplitN(identity.FullName, "/", 2)[1])
	identityClaims := map[string]string{"repository_id": fmt.Sprintf("^%d$", identity.ID), "repository_owner_id": fmt.Sprintf("^%d$", OwnerID), "runner_environment": "^" + regexp.QuoteMeta(environment) + "$"}
	for key, value := range claims {
		identityClaims[key] = value
	}
	return map[string]any{"issuer": "https://token.actions.githubusercontent.com", "subject_pattern": fmt.Sprintf("^repo:fredrir(@%d)?/%s(@%d)?:%s$", OwnerID, name, identity.ID, subject), "claim_pattern": identityClaims, "permissions": permissions}
}

func requireAbsent(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("output path already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func writeFreshDirectory(destination string, files map[string][]byte) error {
	if err := requireAbsent(destination); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(destination), ".onboard-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	for name, data := range files {
		if !filepath.IsLocal(name) {
			return fmt.Errorf("generated path escapes output")
		}
		path := filepath.Join(staging, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			return err
		}
	}
	if err := requireAbsent(destination); err != nil {
		return err
	}
	return os.Rename(staging, destination)
}
