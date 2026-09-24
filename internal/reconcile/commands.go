package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/fluxartifacts"
	"github.com/fredrir/infra/internal/process"
)

type Commands struct {
	Runner          ci.Runner
	Work            string
	Retained        []string
	RequireMain     bool
	ScopeHosts      bool
	ScopeProjects   bool
	VerifyArtifacts bool
	kubernetes      *kubernetesState
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
	if !c.ScopeHosts && effectiveHostScope(selection) == HostScopeRunners {
		selection.HostScope = HostScopeFull
		selection.Reasons = append(selection.Reasons, "host scoping disabled")
	}
	if len(selection.Projects) > 0 && (!c.ScopeProjects || !c.VerifyArtifacts || !supportedProjects(selection.Projects)) {
		selection.Projects = nil
		selection.Reasons = append(selection.Reasons, "full Kubernetes verification required")
	}
	return selection, nil
}

func (c *Commands) Plan(ctx context.Context, plan Plan) error {
	if c.RequireMain {
		if err := c.checkGenerated(ctx); err != nil {
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

func (c *Commands) checkGenerated(ctx context.Context) error {
	return fluxartifacts.Check(c.Runner.Dir, func(path string) ([]byte, error) {
		return c.Runner.Output(ctx, "kubectl", "kustomize", path)
	})
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
	runner := c.Runner
	runner.Dir = filepath.Join(c.Runner.Dir, "ansible")
	runner.Env = append(slices.Clone(runner.Env), "ANSIBLE_CONFIG="+filepath.Join(runner.Dir, "ansible.cfg"))
	args := append([]string{"-i", "inventory/production.yml", playbook}, extra...)
	if os.Getenv("INFRA_RECONCILE_TAILNET") == "true" {
		args = append(args, "--extra-vars", "infra_reconcile_tailnet=true")
	}
	return runner.Run(ctx, "ansible-playbook", args...)
}

func (c *Commands) currentMain(ctx context.Context, revision string) error {
	if err := c.Runner.Run(ctx, "git", "fetch", "--quiet", "origin", "refs/heads/main"); err != nil {
		return err
	}
	head, err := c.Runner.Output(ctx, "git", "rev-parse", "FETCH_HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(head)) != revision {
		return fmt.Errorf("main advanced; retry reconciliation at its current revision")
	}
	return nil
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
	return c.Runner.Run(ctx, "git", "push", "origin", revision+":refs/heads/production")
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
