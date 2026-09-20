package ci

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

type DeployOptions struct{ Root, RepositoryID, Revision, Image, Digest, Token string }
type DeploymentTarget struct{ Path, Mode, Workload string }
type DeploymentMapping struct {
	Repository, Visibility string
	Images                 map[string]DeploymentTarget
}

var repositoryPattern = regexp.MustCompile(`^fredrir/[A-Za-z0-9_.-]+$`)
var projectPattern = regexp.MustCompile(`^platform/projects/[a-z][a-z0-9-]*$`)
var workloadPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
var repositoryIDPattern = regexp.MustCompile(`^[0-9]+$`)

func Deploy(ctx context.Context, runner Runner, options DeployOptions) error {
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
	var mapping DeploymentMapping
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
	var identity struct {
		ClaimPattern struct {
			WorkflowSHA string `yaml:"job_workflow_sha"`
		} `yaml:"claim_pattern"`
	}
	if err := readYAML(filepath.Join(root, ".github/chainguard", "deploy-"+options.RepositoryID+".sts.yaml"), &identity); err != nil {
		return err
	}
	workflowRevision := strings.TrimSuffix(strings.TrimPrefix(identity.ClaimPattern.WorkflowSHA, "^"), "$")
	if !revisionPattern.MatchString(workflowRevision) {
		return fmt.Errorf("invalid deployment workflow revision")
	}
	name, arguments, err := ProvenanceCommand(mapping.Visibility, mapping.Repository, workflowRevision, options.Revision, options.Image+"@"+options.Digest)
	if err != nil {
		return err
	}
	if err := runner.Run(ctx, name, arguments...); err != nil {
		return err
	}
	allowed := map[string]bool{}
	switch target.Mode {
	case "kustomize":
		var pins []string
		err := filepath.WalkDir(project, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("deployment project contains symlink %s", path)
			}
			if entry.IsDir() || entry.Name() != "kustomization.yaml" {
				return nil
			}
			var resource struct{ Images []struct{ Name string } }
			if err := readYAML(path, &resource); err != nil {
				return err
			}
			for _, image := range resource.Images {
				if image.Name == options.Image {
					pins = append(pins, path)
					break
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(pins) == 0 {
			return fmt.Errorf("project has no matching image pin")
		}
		slices.Sort(pins)
		for _, path := range pins {
			local := runner
			local.Dir = filepath.Dir(path)
			if err := local.Run(ctx, "kustomize", "edit", "set", "image", options.Image+"="+options.Image+"@"+options.Digest); err != nil {
				return err
			}
			if _, err := local.Output(ctx, "kustomize", "build", "."); err != nil {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			allowed[filepath.ToSlash(relative)] = true
		}
	case "helmrelease":
		if !workloadPattern.MatchString(target.Workload) {
			return fmt.Errorf("invalid deployment workload")
		}
		path := filepath.Join(project, "release.yaml")
		if err := UpdateWorkload(path, target.Workload, options.Image+"@"+options.Digest, options.Revision); err != nil {
			return err
		}
		allowed[target.Path+"/release.yaml"] = true
	default:
		return fmt.Errorf("unsupported deployment mode %q", target.Mode)
	}
	if _, err := runner.Output(ctx, "kustomize", "build", project); err != nil {
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
		if !allowed[path] {
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
		if err := runner.Run(ctx, "git", "-c", "user.name=infra-release", "-c", "user.email=infra-release@users.noreply.github.com", "rebase", "--quiet", "FETCH_HEAD"); err != nil {
			return err
		}
	}
	return fmt.Errorf("deployment push failed after three attempts")
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
		return "cosign", []string{"verify", "--certificate-oidc-issuer", "https://token.actions.githubusercontent.com", "--certificate-identity", "https://github.com/" + workflow + "@" + workflowRevision, "--certificate-github-workflow-repository", repository, "--certificate-github-workflow-sha", revision, "--certificate-github-workflow-ref", "refs/heads/main", "--certificate-github-workflow-trigger", "push", "--annotations", "source-repository=" + repository, "--annotations", "source-revision=" + revision, "--annotations", "workflow-revision=" + workflowRevision, image}, nil
	default:
		return "", nil, fmt.Errorf("unsupported repository visibility")
	}
}

func UpdateWorkload(path, workload, image, revision string) error {
	var document yaml.Node
	if err := readYAML(path, &document); err != nil {
		return err
	}
	if len(document.Content) != 1 {
		return fmt.Errorf("expected one HelmRelease")
	}
	resource := document.Content[0]
	kind := yamlValue(resource, "kind")
	if kind == nil || kind.Value != "HelmRelease" {
		return fmt.Errorf("expected HelmRelease")
	}
	target := resource
	for _, name := range []string{"spec", "values", "workloads", workload} {
		target = yamlValue(target, name)
		if target == nil {
			return fmt.Errorf("HelmRelease has no workload %q", workload)
		}
	}
	if target.Kind != yaml.MappingNode {
		return fmt.Errorf("invalid workload mapping")
	}
	setYAMLValue(target, "image", image)
	setYAMLValue(target, "sourceRevision", revision)
	data, err := yaml.Marshal(&document)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, info.Mode().Perm())
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
