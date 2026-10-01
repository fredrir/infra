package ci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/deployment"
	"github.com/fredrir/infra/internal/process"
	"github.com/pelletier/go-toml/v2"
	"go.yaml.in/yaml/v3"
)

const parserDependencyImage = "ghcr.io/fredrir/pyparser-dependencies"

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type DependencyPlan struct {
	Key        string `json:"key"`
	Tag        string `json:"tag"`
	Repository string `json:"repository"`
	Dockerfile string `json:"dockerfile"`
	BuildArgs  string `json:"build_args"`
	Image      string `json:"image"`
	Build      bool   `json:"build"`
}

func PlanDependencies(ctx context.Context, runner process.Runner, infraRoot string) (DependencyPlan, error) {
	recipe := "build/projects/llunde-pyparser/dependencies.Containerfile"
	inputs := map[string]string{}
	for _, name := range []string{recipe, "internal/ci/dependencies.go"} {
		file := filepath.Join(infraRoot, name)
		info, err := os.Lstat(file)
		if err != nil {
			return DependencyPlan{}, err
		}
		if !info.Mode().IsRegular() {
			return DependencyPlan{}, errors.New("dependency input must be a regular file")
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return DependencyPlan{}, err
		}
		digest := sha256.Sum256(data)
		inputs[name] = hex.EncodeToString(digest[:])
	}
	for _, name := range []string{"pyproject.toml", "uv.lock"} {
		info, err := os.Lstat(filepath.Join(runner.Dir, name))
		if err != nil {
			return DependencyPlan{}, err
		}
		if !info.Mode().IsRegular() {
			return DependencyPlan{}, errors.New("runtime lock inputs must be regular files")
		}
	}
	runner.Env = append(runner.Env, "UV_NO_CONFIG=1")
	lock, err := runner.Output(ctx, "uv", "export", "--frozen", "--extra", "fixtures", "--no-dev", "--no-emit-project", "--format", "pylock.toml", "--no-header", "--quiet")
	if err != nil {
		return DependencyPlan{}, err
	}
	var locked map[string]any
	if err := toml.Unmarshal(lock, &locked); err != nil {
		return DependencyPlan{}, err
	}
	canonical, err := json.Marshal(locked)
	if err != nil {
		return DependencyPlan{}, err
	}
	digest := sha256.Sum256(canonical)
	inputs["runtime-lock"] = hex.EncodeToString(digest[:])
	encoded, err := json.Marshal(inputs)
	if err != nil {
		return DependencyPlan{}, err
	}
	digest = sha256.Sum256(encoded)
	key := hex.EncodeToString(digest[:])
	return DependencyPlan{Key: key, Tag: "inputs-" + key, Repository: parserDependencyImage, Dockerfile: recipe, BuildArgs: "PUBLIC_PARSER_DEPENDENCY_KEY=" + key, Build: true}, nil
}

type DependencyRegistry struct {
	Client *http.Client
	Base   string
	Actor  string
	Token  string
	bearer string
}

func (registry *DependencyRegistry) request(ctx context.Context, kind, reference string) ([]byte, bool, error) {
	if registry.bearer == "" {
		query := url.Values{"service": {"ghcr.io"}, "scope": {"repository:fredrir/pyparser-dependencies:pull"}}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(registry.Base, "/")+"/token?"+query.Encode(), nil)
		if err != nil {
			return nil, false, err
		}
		if registry.Token != "" {
			request.SetBasicAuth(registry.Actor, registry.Token)
		}
		body, _, err := registry.fetch(request, false)
		if err != nil {
			return nil, false, err
		}
		var credentials struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(body, &credentials); err != nil {
			return nil, false, err
		}
		if credentials.Token == "" {
			return nil, false, errors.New("registry token missing")
		}
		registry.bearer = credentials.Token
	}
	if kind != "manifests" && kind != "blobs" {
		return nil, false, errors.New("invalid registry resource")
	}
	if !(strings.HasPrefix(reference, "sha256:") && digestPattern.MatchString(strings.TrimPrefix(reference, "sha256:"))) && !(strings.HasPrefix(reference, "inputs-") && digestPattern.MatchString(strings.TrimPrefix(reference, "inputs-"))) {
		return nil, false, errors.New("invalid dependency reference")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(registry.Base, "/")+"/v2/fredrir/pyparser-dependencies/"+kind+"/"+reference, nil)
	if err != nil {
		return nil, false, err
	}
	request.Header.Set("Authorization", "Bearer "+registry.bearer)
	request.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json")
	body, missing, err := registry.fetch(request, kind == "manifests" && strings.HasPrefix(reference, "inputs-"))
	if err != nil || missing {
		return nil, missing, err
	}
	digest := sha256.Sum256(body)
	if strings.HasPrefix(reference, "sha256:") && reference != "sha256:"+hex.EncodeToString(digest[:]) {
		return nil, false, errors.New("registry metadata checksum mismatch")
	}
	return body, false, nil
}

func (registry *DependencyRegistry) fetch(request *http.Request, allowMissing bool) ([]byte, bool, error) {
	client := *registry.Client
	client.CheckRedirect = func(next *http.Request, previous []*http.Request) error {
		if len(previous) >= 5 {
			return errors.New("registry redirect limit")
		}
		origin := previous[0].URL
		if origin.Scheme == "https" && next.URL.Scheme != "https" {
			return errors.New("registry redirect requires HTTPS")
		}
		if next.URL.Host != origin.Host || next.URL.Scheme != origin.Scheme {
			next.Header.Del("Authorization")
		}
		return nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		response, err := client.Do(request.Clone(request.Context()))
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
			response.Body.Close()
			if len(body) > 2<<20 {
				return nil, false, errors.New("registry metadata exceeds limit")
			}
			if response.StatusCode == 404 && allowMissing {
				return nil, true, nil
			}
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return body, false, readErr
			}
			if response.StatusCode != 429 && response.StatusCode != 500 && response.StatusCode != 502 && response.StatusCode != 503 && response.StatusCode != 504 {
				return nil, false, fmt.Errorf("registry returned HTTP %d", response.StatusCode)
			}
		}
		if attempt == 2 {
			return nil, false, errors.New("registry request failed")
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 500 * time.Millisecond)
		select {
		case <-request.Context().Done():
			timer.Stop()
			return nil, false, request.Context().Err()
		case <-timer.C:
		}
	}
	return nil, false, errors.New("registry retries exhausted")
}

func (registry *DependencyRegistry) Lookup(ctx context.Context, plan DependencyPlan) (DependencyPlan, error) {
	body, missing, err := registry.request(ctx, "manifests", plan.Tag)
	if err != nil {
		return plan, err
	}
	if missing {
		return plan, nil
	}
	digest := sha256.Sum256(body)
	plan.Image = parserDependencyImage + "@sha256:" + hex.EncodeToString(digest[:])
	return plan, nil
}

func (registry *DependencyRegistry) Verify(ctx context.Context, runner process.Runner, infraRoot string, plan DependencyPlan, image string) error {
	match := imageReferencePattern.FindStringSubmatch(image)
	if match == nil || !strings.HasPrefix(image, parserDependencyImage+"@") {
		return errors.New("dependency image digest required")
	}
	body, _, err := registry.request(ctx, "manifests", match[2])
	if err != nil {
		return err
	}
	var manifest struct {
		MediaType string `json:"mediaType"`
		Config    struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return err
	}
	if strings.Contains(manifest.MediaType, "index") || strings.Contains(manifest.MediaType, "manifest.list") {
		var matches []string
		for _, entry := range manifest.Manifests {
			if entry.Platform.OS == "linux" && entry.Platform.Architecture == "amd64" {
				matches = append(matches, entry.Digest)
			}
		}
		if len(matches) != 1 {
			return errors.New("one linux/amd64 dependency manifest required")
		}
		body, _, err = registry.request(ctx, "manifests", matches[0])
		if err != nil {
			return err
		}
		if err := json.Unmarshal(body, &manifest); err != nil {
			return err
		}
	}
	if manifest.MediaType != "application/vnd.oci.image.manifest.v1+json" && manifest.MediaType != "application/vnd.docker.distribution.manifest.v2+json" {
		return errors.New("unsupported dependency manifest")
	}
	body, _, err = registry.request(ctx, "blobs", manifest.Config.Digest)
	if err != nil {
		return err
	}
	var config struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Config       struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &config); err != nil {
		return err
	}
	if config.OS != "linux" || config.Architecture != "amd64" || config.Config.Labels["io.llunde.parser.dependencies.key"] != plan.Key {
		return errors.New("dependency image inputs or platform mismatch")
	}
	revision := config.Config.Labels["org.opencontainers.image.revision"]
	if !revisionPattern.MatchString(revision) {
		return errors.New("dependency source revision missing")
	}
	data, err := os.ReadFile(filepath.Join(infraRoot, ".github/chainguard/deploy-1328252868.sts.yaml"))
	if err != nil {
		return err
	}
	var trust struct {
		Claims struct {
			SHA string `yaml:"job_workflow_sha"`
		} `yaml:"claim_pattern"`
	}
	if err := yaml.Unmarshal(data, &trust); err != nil {
		return err
	}
	revisions, err := deployment.WorkflowRevisions(trust.Claims.SHA)
	if err != nil {
		return err
	}
	if current := os.Getenv("WORKFLOW_SHA"); revisionPattern.MatchString(current) {
		revisions = append(revisions, current)
	}
	directory, err := os.MkdirTemp("", "infra-registry-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	credentials, err := json.Marshal(map[string]any{"auths": map[string]any{"ghcr.io": map[string]string{"username": registry.Actor, "password": registry.Token}}})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, "config.json"), credentials, 0600); err != nil {
		return err
	}
	runner.Env = append(runner.Env, "DOCKER_CONFIG="+directory)
	runner.Stdout = io.Discard
	var failure error
	for _, workflow := range revisions {
		_, arguments, err := deployment.ProvenanceCommand("private", "fredrir/llunde-pyparser", workflow, revision, image)
		if err != nil {
			return err
		}
		arguments = append([]string{"verify", "--timeout", "1m"}, arguments[1:]...)
		if failure = runner.Run(ctx, "cosign", arguments...); failure == nil {
			return nil
		}
		for index, argument := range arguments {
			if argument == "--certificate-github-workflow-trigger" {
				arguments[index+1] = "workflow_dispatch"
				break
			}
		}
		if ctx.Err() == nil {
			if failure = runner.Run(ctx, "cosign", arguments...); failure == nil {
				return nil
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return errors.Join(errors.New("dependency provenance rejected"), failure)
}
