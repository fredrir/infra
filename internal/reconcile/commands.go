package reconcile

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/fluxartifacts"
	"github.com/fredrir/infra/internal/kustomize"
	"github.com/fredrir/infra/internal/process"
	"github.com/google/go-github/v88/github"
)

type Commands struct {
	Runner        ci.Runner
	Work          string
	Retained      []string
	RequireMain   bool
	ProvenanceEnv []string
	Publisher     *Publisher
	GitHub        *github.Client
	RunnerToken   string
	kubernetes    *kubernetesState
}

func (c *Commands) Revision(ctx context.Context) (string, error) {
	data, err := c.Runner.Output(ctx, "git", "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return "", err
	}
	if len(data) != 0 {
		return "", fmt.Errorf("reconciliation requires a clean checkout")
	}
	data, err = c.Runner.Output(ctx, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	revision := strings.TrimSpace(string(data))
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(revision) {
		return "", fmt.Errorf("invalid revision")
	}
	if c.RequireMain {
		if err := c.currentMain(ctx, revision); err != nil {
			return "", err
		}
	}
	return revision, nil
}

func (c *Commands) Select(ctx context.Context, base string, full bool) (Selection, error) {
	if base == "" || full {
		return All(), nil
	}
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(base) {
		return Selection{}, fmt.Errorf("invalid base revision")
	}
	if _, err := c.Runner.Output(ctx, "git", "cat-file", "-e", base+"^{commit}"); err != nil {
		return All(), nil
	}
	data, err := c.Runner.Output(ctx, "git", "diff", "--name-only", "--no-renames", base, "HEAD")
	if err != nil {
		return Selection{}, err
	}
	selection := Affected(strings.Fields(string(data)))
	if len(selection.Projects) > 0 && !supportedProjects(selection.Projects) {
		selection.Projects = nil
		selection.Reasons = append(selection.Reasons, "full Kubernetes verification required")
	}
	return selection, nil
}

func (c *Commands) Plan(ctx context.Context, plan Plan) error {
	if c.RequireMain {
		if err := c.checkGenerated(); err != nil {
			return err
		}
	}
	if plan.Affected.Tofu {
		if err := c.tofuInit(ctx); err != nil {
			return err
		}
		if err := c.Runner.Run(ctx, "tofu", "-chdir=tofu", "validate"); err != nil {
			return err
		}
		if err := c.Runner.Run(ctx, "tofu", "-chdir=tofu", "test"); err != nil {
			return err
		}
		state, err := c.Runner.Output(ctx, "tofu", "-chdir=tofu", "show", "-json")
		if err != nil {
			return err
		}
		c.Retained, err = retainedHosts(state)
		if err != nil {
			return err
		}
		if err := c.tofuPlan(ctx, "expand", c.Retained); err != nil {
			return err
		}
		if !c.RequireMain {
			if c.Runner.Stdout != nil {
				fmt.Fprintln(c.Runner.Stdout, "Desired OpenTofu state after verification:")
			}
			if err := c.tofuPlan(ctx, "desired", nil); err != nil {
				return err
			}
		}
	}
	if plan.Affected.Kubernetes {
		if err := c.RenderKubernetes(ctx, plan); err != nil {
			return err
		}
	}
	if err := c.PlanHosts(ctx, plan); err != nil {
		return err
	}
	return nil
}

func (c *Commands) checkGenerated() error {
	return fluxartifacts.Check(c.Runner.Dir, kustomize.Build)
}

func (c *Commands) tofuInit(ctx context.Context) error {
	return c.Runner.Run(ctx, "tofu", "-chdir=tofu", "init", "-input=false", "-lockfile=readonly")
}

func retainedHosts(state []byte) ([]string, error) {
	var document struct {
		Values struct {
			Root map[string]any `json:"root_module"`
		} `json:"values"`
	}
	if err := json.Unmarshal(state, &document); err != nil {
		return nil, err
	}
	var hosts []string
	var visit func(map[string]any)
	visit = func(module map[string]any) {
		if resources, ok := module["resources"].([]any); ok {
			for _, entry := range resources {
				resource, ok := entry.(map[string]any)
				if !ok {
					continue
				}
				address, _ := resource["address"].(string)
				if address != `module.platform_dns.cloudflare_dns_record.records["grafana"]` && !strings.HasPrefix(address, `cloudflare_dns_record.grafana[`) {
					continue
				}
				values, _ := resource["values"].(map[string]any)
				host, _ := values["name"].(string)
				if host != "" {
					hosts = append(hosts, host)
				}
			}
		}
		if children, ok := module["child_modules"].([]any); ok {
			for _, child := range children {
				if module, ok := child.(map[string]any); ok {
					visit(module)
				}
			}
		}
	}
	visit(document.Values.Root)
	slices.Sort(hosts)
	return slices.Compact(hosts), nil
}

func (c *Commands) tofuPlan(ctx context.Context, name string, retained []string) error {
	if retained == nil {
		retained = []string{}
	}
	variables, err := json.Marshal(map[string]any{"retained_grafana_hosts": retained})
	if err != nil {
		return err
	}
	path := filepath.Join(c.Work, name+".tfvars.json")
	if err := os.WriteFile(path, variables, 0600); err != nil {
		return err
	}
	return c.Runner.Run(ctx, "tofu", "-chdir=tofu", "plan", "-input=false", "-no-color", "-lock-timeout=5m", "-var-file=production.tfvars.json", "-var-file="+path, "-out="+filepath.Join(c.Work, name+".tfplan"))
}

func (c *Commands) tofuApply(ctx context.Context, name string) error {
	return c.Runner.Run(ctx, "tofu", "-chdir=tofu", "apply", "-input=false", "-no-color", "-lock-timeout=5m", filepath.Join(c.Work, name+".tfplan"))
}

func (c *Commands) Expand(ctx context.Context, _ Plan) error { return c.tofuApply(ctx, "expand") }

func (c *Commands) Retire(ctx context.Context, plan Plan) error {
	if len(c.Retained) == 0 || (len(c.Retained) == 1 && c.Retained[0] == plan.Host) {
		return c.verifyTofu(ctx)
	}
	if err := c.tofuPlan(ctx, "retire", nil); err != nil {
		return err
	}
	if err := c.tofuApply(ctx, "retire"); err != nil {
		return err
	}
	return c.verifyTofu(ctx)
}

func (c *Commands) verifyTofu(ctx context.Context) error {
	return c.allowed(ctx, []int{0}, "tofu", "-chdir=tofu", "plan", "-input=false", "-no-color", "-lock-timeout=5m", "-detailed-exitcode", "-var-file=production.tfvars.json")
}

func (c *Commands) ansible(ctx context.Context, playbook string, extra ...string) error {
	return ansiblePlaybook(ctx, c.Runner, playbook, extra...)
}

func (c *Commands) runnerAPI() ci.Runner {
	runner := c.Runner
	if c.RunnerToken != "" {
		runner.Env = append(slices.Clone(runner.Env), "GH_TOKEN="+c.RunnerToken)
	}
	return runner
}

func ansiblePlaybook(ctx context.Context, runner ci.Runner, playbook string, extra ...string) error {
	runner.Dir = filepath.Join(runner.Dir, "ansible")
	runner.Env = append(slices.Clone(runner.Env), "ANSIBLE_CONFIG="+filepath.Join(runner.Dir, "ansible.cfg"))
	args := append([]string{"-i", "inventory/production.yml", playbook}, extra...)
	if os.Getenv("INFRA_RECONCILE_TAILNET") == "true" {
		args = append(args, "--extra-vars", "infra_reconcile_tailnet=true")
	}
	return runner.Run(ctx, "ansible-playbook", args...)
}

var ErrSuperseded = errors.New("main advanced; retry reconciliation at its current revision")

var pushIgnoredPaths = []string{"*.md", "docs/**/*.md", "build/evidence/*.json"}

func (c *Commands) fetchMain(ctx context.Context) (string, error) {
	if err := c.Runner.Run(ctx, "git", "fetch", "--quiet", "--no-tags", "origin", "refs/heads/main"); err != nil {
		return "", err
	}
	data, err := c.Runner.Output(ctx, "git", "rev-parse", "FETCH_HEAD")
	return strings.TrimSpace(string(data)), err
}

func (c *Commands) OnMain(ctx context.Context, revision string) (bool, error) {
	head, err := c.fetchMain(ctx)
	if err != nil {
		return false, err
	}
	return c.contains(ctx, head, revision)
}

func (c *Commands) currentMain(ctx context.Context, revision string) error {
	head, err := c.fetchMain(ctx)
	if err != nil {
		return err
	}
	if head == revision {
		return nil
	}
	if contained, err := c.contains(ctx, head, revision); err != nil || !contained {
		return cmp.Or(err, ErrSuperseded)
	}
	changed, err := c.Runner.Output(ctx, "git", "diff", "--name-only", "--no-renames", "-z", revision, head)
	if err != nil {
		return err
	}
	for path := range strings.SplitSeq(string(changed), "\x00") {
		if path != "" && !pushIgnored(path) {
			return ErrSuperseded
		}
	}
	return nil
}

func pushIgnored(path string) bool {
	return slices.ContainsFunc(pushIgnoredPaths, func(pattern string) bool { return doublestar.MatchUnvalidated(pattern, path) })
}

func (c *Commands) Publish(ctx context.Context, revision string) error {
	current, err := c.Revision(ctx)
	if err != nil {
		return err
	}
	if current != revision {
		return fmt.Errorf("checkout revision changed")
	}
	if !c.RequireMain {
		if err := c.currentMain(ctx, revision); err != nil {
			return err
		}
	}
	return c.push(ctx, revision)
}

func (c *Commands) allowed(ctx context.Context, codes []int, name string, args ...string) error {
	execute := c.Runner.Execute
	if execute == nil {
		execute = process.Run
	}
	result, err := execute(ctx, process.Options{Name: name, Args: args, Dir: c.Runner.Dir, Env: append(os.Environ(), c.Runner.Env...), Stdout: c.Runner.Stdout, Stderr: c.Runner.Stderr})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if slices.Contains(codes, result.ExitCode) && (err == nil || result.ExitCode > 0) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%s exited %d", name, result.ExitCode)
}
