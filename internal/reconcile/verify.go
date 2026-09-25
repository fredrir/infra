package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

type resource struct {
	APIVersion, Kind string
	Metadata         struct {
		Name       string
		UID        string
		Labels     map[string]string
		Namespace  string
		Generation int64
	}
	Spec struct {
		Ref         struct{ Branch string }
		Suspend     bool
		Path        string
		Replicas    *int64
		Completions *int64
		Paused      bool
		SourceRef   struct{ Kind, Name, Namespace string }
		DependsOn   []struct{ Name, Namespace, ReadyExpr string }
		Artifacts   []struct {
			Name, OriginRevision string
			Copy                 []struct{ From, To string }
		}
		Sources  []struct{ Alias, Kind, Name string }
		Template struct {
			Spec struct {
				Containers, InitContainers []struct{ Name, Image string }
			}
		}
		Values struct {
			Grafana struct {
				INI struct {
					Server struct {
						RootURL string `json:"root_url"`
					}
				} `json:"grafana.ini"`
			}
		}
	}
	Status struct {
		ObservedGeneration                                                                                                                                           int64
		Replicas, UpdatedReplicas, ReadyReplicas, AvailableReplicas, UpdatedNumberScheduled, NumberReady, NumberAvailable, DesiredNumberScheduled, Succeeded, Failed int64
		CurrentRevision, UpdateRevision                                                                                                                              string
		Artifact                                                                                                                                                     struct {
			Revision, Digest string
			Metadata         map[string]string
		}
		Inventory              json.RawMessage
		LastAppliedRevision    string
		LastHandledReconcileAt string
		Conditions             []struct {
			Type, Status, Message string
			ObservedGeneration    int64
		}
	}
}

func ready(item resource) error {
	name := item.Metadata.Namespace + "/" + item.Metadata.Name
	if item.Spec.Suspend {
		return nil
	}
	if item.Status.ObservedGeneration != item.Metadata.Generation {
		return fmt.Errorf("%s has unobserved configuration", name)
	}
	for _, condition := range item.Status.Conditions {
		if (condition.Type == "Reconciling" || condition.Type == "Stalled") && condition.Status == "True" {
			return fmt.Errorf("%s: %s", name, condition.Message)
		}
	}
	for _, condition := range item.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" && condition.ObservedGeneration == item.Metadata.Generation {
			return nil
		}
	}
	return fmt.Errorf("%s is not ready", name)
}

func (c *Commands) resources(ctx context.Context, kind string) ([]resource, error) {
	data, err := c.Runner.Output(ctx, "kubectl", "get", kind, "--all-namespaces", "-o=json", "--request-timeout=30s")
	if err != nil {
		return nil, err
	}
	var result struct{ Items []resource }
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if len(result.Items) == 0 {
		return nil, fmt.Errorf("no %s found", kind)
	}
	return result.Items, nil
}

func (c *Commands) Kubernetes(ctx context.Context, plan Plan) error {
	revision := plan.Revision
	if c.VerifyArtifacts {
		if err := c.loadKubernetes(ctx); err != nil {
			return err
		}
		if len(c.kubernetes.workloads) == 0 {
			if err := c.RenderKubernetes(ctx, plan); err != nil {
				return err
			}
		}
	}
	if err := c.Runner.Run(ctx, "flux", "reconcile", "source", "git", "flux-system", "--timeout=5m"); err != nil {
		return err
	}
	data, err := c.Runner.Output(ctx, "kubectl", "get", "gitrepository", "flux-system", "-n=flux-system", "-o=json", "--request-timeout=30s")
	if err != nil {
		return err
	}
	var source struct {
		Spec   struct{ Ref struct{ Branch string } }
		Status struct{ Artifact struct{ Revision string } }
	}
	if err := json.Unmarshal(data, &source); err != nil {
		return err
	}
	if (source.Spec.Ref.Branch != "production" && source.Spec.Ref.Branch != "main") || !strings.HasSuffix(source.Status.Artifact.Revision, ":"+revision) {
		return fmt.Errorf("Flux source is not at %s", revision)
	}
	if err := c.Runner.Run(ctx, "flux", "reconcile", "kustomization", "flux-system", "--timeout=15m"); err != nil {
		return err
	}
	if source.Spec.Ref.Branch == "main" {
		return c.bootstrapProduction(ctx, plan)
	}
	if c.VerifyArtifacts && len(plan.Affected.Projects) > 0 {
		wait, cancel := context.WithTimeout(ctx, 20*time.Minute)
		defer cancel()
		started := time.Now()
		requested := false
		return poll(wait, func() error {
			err := c.verifyDeployment(wait, plan)
			if err != nil && !requested && time.Since(started) >= 10*time.Second {
				if requestErr := c.requestSelected(wait, plan); requestErr != nil {
					return requestErr
				}
				requested = true
			}
			return err
		})
	}
	token := time.Now().UTC().Format(time.RFC3339Nano)
	if err := c.requestReconcile(ctx, "kustomizations.kustomize.toolkit.fluxcd.io,helmreleases.helm.toolkit.fluxcd.io", token); err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	if err := poll(wait, func() error { return c.verifyKubernetes(wait, revision, token) }); err != nil {
		return err
	}
	// Requests HelmReleases that Kustomizations created while applying the revision.
	if err := c.requestReconcile(ctx, "helmreleases.helm.toolkit.fluxcd.io", token); err != nil {
		return err
	}
	if err := poll(wait, func() error { return c.verifyHelm(wait, token, "") }); err != nil {
		return err
	}
	if c.VerifyArtifacts {
		return poll(wait, func() error { return c.verifyDeployment(wait, plan) })
	}
	return nil
}

func (c *Commands) requestReconcile(ctx context.Context, kinds, token string) error {
	return c.Runner.Run(ctx, "kubectl", "annotate", kinds, "--all", "--all-namespaces", "--overwrite", "--field-manager=flux-client-side-apply", "reconcile.fluxcd.io/requestedAt="+token, "--request-timeout=30s")
}

func (c *Commands) bootstrapProduction(ctx context.Context, plan Plan) error {
	data, err := c.Runner.Output(ctx, "kubectl", "get", "gitrepository", "flux-system", "-n=flux-system", "-o=jsonpath={.spec.ref.branch}", "--request-timeout=30s")
	if err != nil {
		return err
	}
	if string(data) != "production" {
		return fmt.Errorf("Flux bootstrap did not select the production branch")
	}
	return c.Kubernetes(ctx, plan)
}

func kubernetesDifference(format string, args ...any) error {
	return Differences{{System: "kubernetes", Item: fmt.Sprintf(format, args...)}}
}

func (c *Commands) verifyKubernetes(ctx context.Context, revision, token string) error {
	items, err := c.resources(ctx, "kustomizations.kustomize.toolkit.fluxcd.io")
	if err != nil {
		return err
	}
	var problems []error
	root := false
	for _, item := range items {
		name := item.Metadata.Namespace + "/" + item.Metadata.Name
		if item.Spec.Suspend {
			if name == "flux-system/flux-system" {
				root = true
				problems = append(problems, fmt.Errorf("Kustomization %s is suspended", name))
			}
			continue
		}
		problems = append(problems, ready(item))
		if token != "" && item.Status.LastHandledReconcileAt != token {
			problems = append(problems, fmt.Errorf("%s has not reconciled its requested configuration", item.Metadata.Name))
		}
		if item.Spec.SourceRef.Kind == "GitRepository" && item.Spec.SourceRef.Name == "flux-system" {
			if !strings.HasSuffix(item.Status.LastAppliedRevision, ":"+revision) {
				problems = append(problems, kubernetesDifference("Kustomization %s has not applied %s", name, revision))
			}
			root = root || name == "flux-system/flux-system"
		}
	}
	if !root {
		problems = append(problems, fmt.Errorf("active Flux root not found"))
	}
	return errors.Join(problems...)
}

func (c *Commands) verifyHelm(ctx context.Context, token, host string) error {
	items, err := c.resources(ctx, "helmreleases.helm.toolkit.fluxcd.io")
	if err != nil {
		return err
	}
	var problems []error
	grafana := false
	for _, item := range items {
		name := item.Metadata.Namespace + "/" + item.Metadata.Name
		monitoring := name == "observability/monitoring"
		grafana = grafana || monitoring
		if item.Spec.Suspend {
			if monitoring {
				problems = append(problems, fmt.Errorf("HelmRelease %s is suspended", name))
			}
			continue
		}
		problems = append(problems, ready(item))
		if token != "" && item.Status.LastHandledReconcileAt != token {
			problems = append(problems, fmt.Errorf("%s has not reconciled drift", item.Metadata.Name))
		}
		if rootURL := item.Spec.Values.Grafana.INI.Server.RootURL; monitoring && host != "" && rootURL != "https://"+host {
			problems = append(problems, kubernetesDifference("HelmRelease %s serves Grafana at %q, want %q", name, rootURL, "https://"+host))
		}
	}
	if !grafana {
		problems = append(problems, kubernetesDifference("HelmRelease observability/monitoring is missing"))
	}
	return errors.Join(problems...)
}

func (c *Commands) Verify(ctx context.Context, plan Plan) error {
	return c.verifyParts(ctx, plan, c.verifyCluster(ctx, plan))
}

func (c *Commands) verifyCluster(ctx context.Context, plan Plan) error {
	if c.VerifyArtifacts {
		return c.verifyDeployment(ctx, plan)
	}
	return errors.Join(c.verifyKubernetes(ctx, plan.Revision, ""), c.verifyHelm(ctx, "", plan.Host))
}

func (c *Commands) verifyParts(ctx context.Context, plan Plan, cluster error) error {
	return errors.Join(cluster, c.VerifyHosts(ctx, plan), c.verifyServed(ctx, plan))
}

func (c *Commands) verifyServed(ctx context.Context, plan Plan) error {
	wait, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var grafana, frontend error
	var group sync.WaitGroup
	if len(plan.Affected.Projects) == 0 {
		group.Go(func() {
			grafana = poll(wait, func() error { return verifyGrafana(wait, client, "https://"+plan.Host) })
		})
	}
	if len(plan.Affected.Projects) == 0 || slices.Contains(plan.Affected.Projects, "llunde") {
		group.Go(func() { frontend = c.verifyFrontend(wait, client) })
	}
	group.Wait()
	return errors.Join(grafana, frontend)
}

func (c *Commands) verifyFrontend(ctx context.Context, client *http.Client) error {
	data, err := os.ReadFile(filepath.Join(c.Runner.Dir, "platform/projects/llunde/.deployments/llunde-frontend.json"))
	if err != nil {
		return err
	}
	var frontend struct{ Revision string }
	if err = json.Unmarshal(data, &frontend); err != nil {
		return err
	}
	return ci.WaitRevision(ctx, client, "https://llunde.no/.well-known/revision", frontend.Revision, 2*time.Second)
}

func (c *Commands) VerifyDrift(ctx context.Context, plan Plan) error {
	if err := c.checkGenerated(ctx); err != nil {
		return err
	}
	if err := c.tofuInit(ctx); err != nil {
		return err
	}
	if err := c.verifyTofu(ctx); err != nil {
		return err
	}
	return c.VerifyLive(ctx, plan)
}

func (c *Commands) VerifyDeep(ctx context.Context, plan Plan) error {
	var output bytes.Buffer
	log := &lockedWriter{mu: &sync.Mutex{}, writer: &output}
	compare := *c
	compare.Runner.Stdout, compare.Runner.Stderr = log, log
	compared := make(chan error, 1)
	go func() { compared <- compare.compareDeclarations(ctx) }()
	live := c.VerifyLive(ctx, plan)
	declarations := <-compared
	if c.Runner.Stdout != nil {
		_, _ = c.Runner.Stdout.Write(output.Bytes())
	}
	return errors.Join(live, declarations)
}

func (c *Commands) compareDeclarations(ctx context.Context) error {
	var output bytes.Buffer
	log := &lockedWriter{mu: &sync.Mutex{}, writer: &output}
	planned := make(chan error, 1)
	go func() { planned <- c.compareTofu(ctx, log) }()
	hosts := c.compareHosts(ctx)
	infrastructure := <-planned
	if c.Runner.Stdout != nil {
		_, _ = c.Runner.Stdout.Write(output.Bytes())
	}
	return errors.Join(hosts, infrastructure)
}

func (c *Commands) compareTofu(ctx context.Context, log io.Writer) error {
	tofu := Commands{Runner: c.Runner}
	tofu.Runner.Stdout, tofu.Runner.Stderr = log, log
	if err := tofu.tofuInit(ctx); err != nil {
		return fmt.Errorf("OpenTofu comparison: %w", err)
	}
	execute := c.Runner.Execute
	if execute == nil {
		execute = process.Run
	}
	result, err := execute(ctx, process.Options{Name: "tofu", Args: []string{"-chdir=tofu", "plan", "-input=false", "-lock=false", "-json", "-detailed-exitcode", "-var-file=production.tfvars.json"}, Dir: c.Runner.Dir, Env: append(os.Environ(), c.Runner.Env...), Stderr: log})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var differences Differences
	var diagnostics []string
	summary := "plan has changes"
	for line := range strings.Lines(string(result.Stdout)) {
		var event struct {
			Type    string `json:"type"`
			Level   string `json:"@level"`
			Message string `json:"@message"`
			Change  struct {
				Resource struct{ Addr string }
				Action   string
			}
			Outputs map[string]struct{ Action string }
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			fmt.Fprint(log, line)
			continue
		}
		fmt.Fprintln(log, event.Message)
		switch event.Type {
		case "planned_change":
			differences = append(differences, Difference{System: "opentofu", Item: event.Change.Resource.Addr + " " + event.Change.Action})
		case "outputs":
			for _, name := range slices.Sorted(maps.Keys(event.Outputs)) {
				if action := event.Outputs[name].Action; action != "noop" {
					differences = append(differences, Difference{System: "opentofu", Item: "output " + name + " " + action})
				}
			}
		case "change_summary":
			summary = event.Message
		case "diagnostic":
			if event.Level == "error" {
				diagnostics = append(diagnostics, event.Message)
			}
		}
	}
	switch {
	case err == nil && result.ExitCode == 0:
		return nil
	case result.ExitCode == 2:
		if len(differences) == 0 {
			differences = Differences{{System: "opentofu", Item: summary}}
		}
		return differences
	case len(diagnostics) > 0:
		return fmt.Errorf("OpenTofu comparison: %s", strings.Join(diagnostics, "; "))
	case err != nil:
		return fmt.Errorf("OpenTofu comparison: %w", err)
	default:
		return fmt.Errorf("OpenTofu comparison: tofu exited %d", result.ExitCode)
	}
}

func (c *Commands) VerifyLive(ctx context.Context, plan Plan) error {
	return c.verifyParts(ctx, plan, c.verifyRenderedCluster(ctx, plan))
}

func (c *Commands) verifyRenderedCluster(ctx context.Context, plan Plan) error {
	plan, err := c.Preflight(ctx, plan)
	if err != nil {
		return err
	}
	if err := c.RenderKubernetes(ctx, plan); err != nil {
		return err
	}
	return c.verifyCluster(ctx, plan)
}

func (c *Commands) verifyDeployment(ctx context.Context, plan Plan) error {
	if c.kubernetes == nil || len(c.kubernetes.workloads) == 0 {
		return fmt.Errorf("desired Kubernetes workloads have not been rendered")
	}
	source, err := c.getResource(ctx, "gitrepositories.source.toolkit.fluxcd.io", "flux-system", "flux-system")
	if err != nil {
		return err
	}
	if source.Spec.Ref.Branch != "production" || !strings.HasSuffix(source.Status.Artifact.Revision, ":"+plan.Revision) {
		return kubernetesDifference("GitRepository flux-system/flux-system is at %s of branch %s, want production@sha1:%s", source.Status.Artifact.Revision, source.Spec.Ref.Branch, plan.Revision)
	}
	if err = conditionReady(source); err != nil {
		return err
	}
	names := c.selectedOwners(plan)
	for _, name := range names {
		if _, ok := c.kubernetes.workloads[name]; !ok {
			return fmt.Errorf("desired workloads for %s have not been rendered", name)
		}
	}
	if len(plan.Affected.Projects) > 0 {
		names = append(names, "platform-policy", "flux-system")
	}
	snapshot := resourceSnapshot{}
	var problems []error
	owners := make([]resource, 0, len(names))
	for _, name := range names {
		item, err := c.snapshotResource(ctx, snapshot, "kustomizations.kustomize.toolkit.fluxcd.io", "flux-system", name)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if item.Spec.Suspend {
			problems = append(problems, fmt.Errorf("Kustomization flux-system/%s is suspended", name))
			continue
		}
		problems = append(problems, c.verifyOwner(item, plan))
		owners = append(owners, item)
	}
	problems = append(problems, c.verifyArtifacts(ctx, plan, owners))
	for _, owner := range owners {
		problems = append(problems, c.verifyOwnedWorkloads(ctx, owner, snapshot))
	}
	if len(plan.Affected.Projects) == 0 {
		problems = append(problems, c.verifyHelm(ctx, "", plan.Host))
	}
	return errors.Join(problems...)
}

func (c *Commands) verifyOwner(item resource, plan Plan) error {
	name := item.Metadata.Name
	var problems []error
	expected := c.kubernetes.owners[name]
	if item.Spec.Path != expected.Spec.Path || item.Spec.SourceRef != expected.Spec.SourceRef || !reflect.DeepEqual(item.Spec.DependsOn, expected.Spec.DependsOn) {
		problems = append(problems, kubernetesDifference("Kustomization flux-system/%s source topology differs from its declaration", name))
	}
	if token := c.kubernetes.requested["kustomizations.kustomize.toolkit.fluxcd.io/flux-system/"+name]; token != "" && item.Status.LastHandledReconcileAt != token {
		problems = append(problems, fmt.Errorf("%s has not handled its requested reconciliation", name))
	}
	problems = append(problems, ready(item))
	if item.Spec.SourceRef.Kind == "GitRepository" && !strings.HasSuffix(item.Status.LastAppliedRevision, ":"+plan.Revision) {
		problems = append(problems, kubernetesDifference("Kustomization flux-system/%s has not applied %s", name, plan.Revision))
	}
	return errors.Join(problems...)
}

func verifyGrafana(ctx context.Context, client *http.Client, base string) error {
	for _, probe := range []struct {
		Path string
		Code int
	}{{"/api/health", 200}, {"/login", 200}, {"/api/user", 401}} {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+probe.Path, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Cache-Control", "no-cache, no-store")
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		response.Body.Close()
		if readErr != nil {
			return readErr
		}
		if response.StatusCode != probe.Code {
			return fmt.Errorf("Grafana %s returned HTTP %d", probe.Path, response.StatusCode)
		}
		if probe.Path == "/login" {
			match := regexp.MustCompile(`"appUrl"\s*:\s*("(?:[^"\\]|\\.)*")`).FindSubmatch(body)
			var active string
			if len(match) != 2 || json.Unmarshal(match[1], &active) != nil || strings.TrimRight(active, "/") != base {
				return fmt.Errorf("Grafana is not serving the declared root_url")
			}
		}
		if probe.Path == "/api/health" {
			var health struct{ Database string }
			if err := json.Unmarshal(body, &health); err != nil || health.Database != "ok" {
				return fmt.Errorf("Grafana database is not healthy")
			}
		}
	}
	return nil
}

func poll(ctx context.Context, check func() error) error {
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("verification: %v: %w", last, err)
		}
		last = check()
		if last == nil {
			return nil
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("verification: %v: %w", last, ctx.Err())
		case <-timer.C:
		}
	}
}
