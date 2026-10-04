package deployment

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/fredrir/infra/internal/process"
	"go.yaml.in/yaml/v3"
)

type Options struct{ Root, RepositoryID, Revision, Image, Digest, Token string }
type Target struct{ Path, Mode, Workload string }
type Mapping struct {
	Repository, Visibility string
	Images                 map[string]Target
}

var revisionPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var imageReferencePattern = regexp.MustCompile(`^ghcr\.io/(fredrir/[a-z0-9][a-z0-9._/-]*)@(sha256:[a-f0-9]{64})$`)
var repositoryPattern = regexp.MustCompile(`^fredrir/[A-Za-z0-9_.-]+$`)
var projectPattern = regexp.MustCompile(`^platform/projects/[a-z][a-z0-9-]*$`)
var workloadPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
var repositoryIDPattern = regexp.MustCompile(`^[0-9]+$`)

func Deploy(ctx context.Context, runner process.Runner, options Options, render func(directory string) error) error {
	if !repositoryIDPattern.MatchString(options.RepositoryID) || !revisionPattern.MatchString(options.Revision) || !imageReferencePattern.MatchString(options.Image+"@"+options.Digest) || options.Token == "" {
		return fmt.Errorf("invalid deployment identity or credentials")
	}
	root, err := filepath.EvalSymlinks(options.Root)
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	runner.Dir = root
	origin, err := runner.Output(ctx, "git", "config", "--get", "remote.origin.url")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(origin)) != "https://github.com/fredrir/infra" {
		return fmt.Errorf("deployment requires the infra origin")
	}
	status, err := runner.Output(ctx, "git", "status", "--porcelain")
	if err != nil {
		return err
	}
	if len(status) != 0 {
		return fmt.Errorf("deployment requires a clean checkout")
	}
	var mapping Mapping
	if err := readYAML(filepath.Join(root, ".github/deployments", options.RepositoryID+".yaml"), &mapping); err != nil {
		return err
	}
	target, ok := mapping.Images[options.Image]
	if !ok || !repositoryPattern.MatchString(mapping.Repository) || !projectPattern.MatchString(target.Path) {
		return fmt.Errorf("image has no valid deployment mapping")
	}
	project := filepath.Join(root, filepath.FromSlash(target.Path))
	resolved, err := filepath.EvalSymlinks(project)
	if err != nil {
		return err
	}
	if project != resolved {
		return fmt.Errorf("deployment project must not traverse symlinks")
	}
	attested, err := VerifyProvenance(ctx, runner, os.DirFS(root), options.RepositoryID, mapping, options.Image, options.Digest, options.Revision)
	if err != nil {
		return err
	}
	order := attested[len(attested)-1]
	files, err := Files(os.DirFS(root), target, order)
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(path, files[name], 0644); err != nil {
			return err
		}
		if filepath.Base(path) == "kustomization.yaml" {
			if err := render(filepath.Dir(path)); err != nil {
				return err
			}
		}
	}
	if err := render(project); err != nil {
		return err
	}
	relativeReceipt := deploymentReceiptPath(target.Path, options.Image)
	if err := runner.Run(ctx, "git", "add", "--intent-to-add", "--", relativeReceipt); err != nil {
		return err
	}
	if err := runner.Run(ctx, "git", "diff", "--check"); err != nil {
		return err
	}
	changed, err := runner.Output(ctx, "git", "diff", "--name-only")
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		_, err := fmt.Fprintf(runner.Stdout, "%s@%s is already deployed\n", options.Image, options.Digest)
		return err
	}
	for _, path := range strings.Split(strings.TrimSpace(string(changed)), "\n") {
		if _, allowed := files[path]; !allowed {
			return fmt.Errorf("unexpected deployment change %q", path)
		}
	}
	if err := runner.Run(ctx, "git", "add", "--", target.Path); err != nil {
		return err
	}
	message := fmt.Sprintf("Deploy %s %s", options.Image[strings.LastIndex(options.Image, "/")+1:], options.Revision[:12])
	body := fmt.Sprintf("Deploy %s@%s from https://github.com/%s/commit/%s with verified provenance.", options.Image, options.Digest, mapping.Repository, options.Revision)
	if err := runner.Run(ctx, "git", "-c", "user.name=infra-release", "-c", "user.email=infra-release@users.noreply.github.com", "commit", "--quiet", "-m", message, "-m", body); err != nil {
		return err
	}
	publisher := runner
	publisher.Env = append(slices.Clone(runner.Env), "GH_TOKEN="+options.Token)
	for attempt := 0; attempt < 3; attempt++ {
		if err := publisher.Run(ctx, "git", "-c", "credential.helper=!gh auth git-credential", "push", "--quiet", "origin", "HEAD:main"); err == nil {
			_, err := fmt.Fprintf(runner.Stdout, "Deployed %s@%s\n", options.Image, options.Digest)
			return err
		}
		if err := publisher.Run(ctx, "git", "-c", "credential.helper=!gh auth git-credential", "fetch", "--quiet", "origin", "main"); err != nil {
			return err
		}
		if err := checkFetchedDeploymentOrder(ctx, runner, relativeReceipt, order); err != nil {
			return err
		}
		if err := runner.Run(ctx, "git", "-c", "user.name=infra-release", "-c", "user.email=infra-release@users.noreply.github.com", "rebase", "--quiet", "FETCH_HEAD"); err != nil {
			return err
		}
	}
	return fmt.Errorf("deployment push failed after three attempts")
}

func VerifyProvenance(ctx context.Context, runner process.Runner, root fs.FS, repositoryID string, mapping Mapping, image, digest, revision string) ([]Order, error) {
	if !repositoryIDPattern.MatchString(repositoryID) {
		return nil, fmt.Errorf("invalid deployment repository ID")
	}
	var identity struct {
		ClaimPattern struct {
			WorkflowSHA string `yaml:"job_workflow_sha"`
			Event       string `yaml:"event_name"`
		} `yaml:"claim_pattern"`
	}
	trust := path.Join(".github/chainguard", "deploy-"+repositoryID+".sts.yaml")
	data, err := fs.ReadFile(root, trust)
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, &identity); err != nil {
		return nil, fmt.Errorf("decode %s: %w", trust, err)
	}
	revisions, err := WorkflowRevisions(identity.ClaimPattern.WorkflowSHA)
	if err != nil {
		return nil, err
	}
	environment := append(os.Environ(), runner.Env...)
	checks := make([]provenanceCheck, len(revisions))
	for index, workflowRevision := range revisions {
		name, arguments, err := ProvenanceCommand(mapping.Visibility, mapping.Repository, workflowRevision, revision, image+"@"+digest)
		if err != nil {
			return nil, err
		}
		if mapping.Visibility == "public" {
			arguments = append(arguments, "--format", "json")
		}
		checks[index] = provenanceCheck{revision: workflowRevision, name: name, arguments: arguments}
	}
	retryTrigger := mapping.Visibility == "private" && identity.ClaimPattern.Event == "^(push|workflow_dispatch)$"
	// Approved revisions grow with every CI promotion; checking them one at a time outlives the deployment budget.
	var group sync.WaitGroup
	slots := make(chan struct{}, provenanceConcurrency)
	for index := range checks {
		group.Add(1)
		go func(check *provenanceCheck) {
			defer group.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			check.run(ctx, runner, environment, retryTrigger)
		}(&checks[index])
	}
	group.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var attested []Order
	failure := errors.New("no attestation names a deployment run")
	unavailable := false
	for index := range checks {
		check := &checks[index]
		if runner.Stderr != nil && check.stderr.Len() > 0 {
			_, _ = runner.Stderr.Write(check.stderr.Bytes())
		}
		if check.err != nil {
			failure = fmt.Errorf("%s at workflow %s: %s", check.name, check.revision[:12], redacted(cmp.Or(lastLine(check.result.Stderr), check.err.Error()), environment))
			unavailable = unavailable || UnavailableOutput(string(check.result.Stderr)) || check.result.ExitCode < 0
			continue
		}
		orders, err := attestedDeploymentOrders(check.result.Stdout, mapping.Visibility, mapping.Repository, image, digest, revision)
		if err != nil {
			return nil, err
		}
		attested = append(attested, orders...)
	}
	if len(attested) == 0 {
		failure = fmt.Errorf("image provenance did not match an approved workflow revision: %w", failure)
		if unavailable {
			return nil, Unavailable(failure)
		}
		return nil, failure
	}
	slices.SortFunc(attested, compareDeploymentRuns)
	return slices.Compact(attested), nil
}

const provenanceConcurrency = 4

type provenanceCheck struct {
	revision, name string
	arguments      []string
	stderr         bytes.Buffer
	result         process.Result
	err            error
}

func (check *provenanceCheck) run(ctx context.Context, runner process.Runner, environment []string, retryTrigger bool) {
	check.result, check.err = runner.Invoke(ctx, process.Options{Name: check.name, Args: check.arguments, Dir: runner.Dir, Env: environment, Stderr: &check.stderr})
	if check.err == nil || !retryTrigger || ctx.Err() != nil {
		return
	}
	// Only a trigger mismatch deserves a second attempt; a wrong revision fails the same way under either trigger.
	output := strings.ToLower(string(check.result.Stderr) + check.stderr.String() + check.err.Error())
	if !strings.Contains(output, "trigger") {
		return
	}
	arguments := slices.Clone(check.arguments)
	for index, argument := range arguments {
		if argument == "--certificate-github-workflow-trigger" {
			arguments[index+1] = "workflow_dispatch"
			break
		}
	}
	check.result, check.err = runner.Invoke(ctx, process.Options{Name: check.name, Args: arguments, Dir: runner.Dir, Env: environment, Stderr: &check.stderr})
}

func lastLine(output []byte) string {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func redacted(text string, environment []string) string {
	for _, entry := range environment {
		name, value, _ := strings.Cut(entry, "=")
		if len(value) >= 8 && (strings.Contains(name, "TOKEN") || strings.Contains(name, "PASSWORD") || strings.Contains(name, "SECRET")) {
			text = strings.ReplaceAll(text, value, "[redacted]")
		}
	}
	return text
}

func ProvenanceCommand(visibility, repository, workflowRevision, revision, image string) (string, []string, error) {
	const workflow = "fredrir/infra/.github/workflows/build-image.yml"
	if !repositoryPattern.MatchString(repository) || !revisionPattern.MatchString(workflowRevision) || !revisionPattern.MatchString(revision) || !imageReferencePattern.MatchString(image) {
		return "", nil, fmt.Errorf("invalid provenance identity")
	}
	switch visibility {
	case "public":
		return "gh", []string{"attestation", "verify", "oci://" + image, "--repo", repository, "--signer-workflow", workflow, "--signer-digest", workflowRevision, "--source-ref", "refs/heads/main", "--source-digest", revision}, nil
	case "private":
		identity := "^https://github.com/" + regexp.QuoteMeta(workflow) + "@(" + workflowRevision + "|refs/tags/ci-v1)$"
		return "cosign", []string{"verify", "--certificate-oidc-issuer", "https://token.actions.githubusercontent.com", "--certificate-identity-regexp", identity, "--certificate-github-workflow-repository", repository, "--certificate-github-workflow-sha", revision, "--certificate-github-workflow-ref", "refs/heads/main", "--certificate-github-workflow-trigger", "push", "--annotations", "source-repository=" + repository, "--annotations", "source-revision=" + revision, "--annotations", "workflow-revision=" + workflowRevision, image}, nil
	default:
		return "", nil, fmt.Errorf("unsupported repository visibility")
	}
}

func Files(root fs.FS, target Target, order Order) (map[string][]byte, error) {
	if !projectPattern.MatchString(target.Path) {
		return nil, fmt.Errorf("image has no valid deployment mapping")
	}
	receipt := deploymentReceiptPath(target.Path, order.Image)
	if err := checkLocalDeploymentOrder(root, receipt, order); err != nil {
		return nil, err
	}
	encoded, err := encodeDeploymentOrder(order)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{receipt: encoded}
	switch target.Mode {
	case "kustomize":
		err := fs.WalkDir(root, target.Path, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("deployment project contains symlink %s", name)
			}
			if entry.IsDir() || entry.Name() != "kustomization.yaml" {
				return nil
			}
			data, err := fs.ReadFile(root, name)
			if err != nil {
				return err
			}
			pinned, err := pinImage(data, order.Image, order.Digest)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if pinned != nil {
				files[name] = pinned
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if len(files) == 1 {
			return nil, fmt.Errorf("project has no matching image pin")
		}
	case "helmrelease":
		if !workloadPattern.MatchString(target.Workload) {
			return nil, fmt.Errorf("invalid deployment workload")
		}
		name := path.Join(target.Path, "release.yaml")
		info, err := fs.Lstat(root, name)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("HelmRelease must be a regular file")
		}
		data, err := fs.ReadFile(root, name)
		if err != nil {
			return nil, err
		}
		if files[name], err = pinWorkload(data, target.Workload, order.Image+"@"+order.Digest, order.Revision); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported deployment mode %q", target.Mode)
	}
	return files, nil
}

func pinWorkload(data []byte, workload, image, revision string) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("decode HelmRelease: %w", err)
	}
	if len(document.Content) != 1 {
		return nil, fmt.Errorf("expected one HelmRelease")
	}
	resource := document.Content[0]
	kind := yamlValue(resource, "kind")
	if kind == nil || kind.Value != "HelmRelease" {
		return nil, fmt.Errorf("expected HelmRelease")
	}
	target := resource
	for _, name := range []string{"spec", "values", "workloads", workload} {
		target = yamlValue(target, name)
		if target == nil {
			return nil, fmt.Errorf("HelmRelease has no workload %q", workload)
		}
	}
	if target.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("invalid workload mapping")
	}
	setYAMLValue(target, "image", image)
	setYAMLValue(target, "sourceRevision", revision)
	return yaml.Marshal(&document)
}

func yamlValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}
func setYAMLValue(node *yaml.Node, key, value string) {
	existing := yamlValue(node, key)
	if existing != nil {
		existing.Kind, existing.Tag, existing.Value = yaml.ScalarNode, "!!str", value
		return
	}
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}
func readYAML(path string, destination any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(data, destination); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func WorkflowRevisions(pattern string) ([]string, error) {
	if len(pattern) > 32768 || !strings.HasPrefix(pattern, "^") || !strings.HasSuffix(pattern, "$") {
		return nil, fmt.Errorf("workflow trust requires anchored exact revisions")
	}
	body := strings.TrimSuffix(strings.TrimPrefix(pattern, "^"), "$")
	if strings.HasPrefix(body, "(") && strings.HasSuffix(body, ")") {
		body = body[1 : len(body)-1]
	}
	revisions := strings.Split(body, "|")
	seen := map[string]bool{}
	for _, revision := range revisions {
		if !revisionPattern.MatchString(revision) || seen[revision] {
			return nil, fmt.Errorf("workflow trust requires unique exact revisions")
		}
		seen[revision] = true
	}
	if len(revisions) > 1 && !strings.HasPrefix(pattern, "^(") {
		return nil, fmt.Errorf("workflow alternatives require grouping")
	}
	return revisions, nil
}
func pinImage(data []byte, image, digest string) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if len(document.Content) != 1 {
		return nil, nil
	}
	images := yamlValue(document.Content[0], "images")
	if images == nil {
		return nil, nil
	}
	if images.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("image pins are not a list")
	}
	found := false
	for _, entry := range images.Content {
		name := yamlValue(entry, "name")
		if name == nil || name.Value != image {
			continue
		}
		found = true
		setYAMLValue(entry, "newName", image)
		setYAMLValue(entry, "digest", digest)
		for i := 0; i < len(entry.Content); i += 2 {
			if entry.Content[i].Value == "newTag" {
				entry.Content = append(entry.Content[:i], entry.Content[i+2:]...)
				break
			}
		}
	}
	if !found {
		return nil, nil
	}
	return yaml.Marshal(&document)
}
