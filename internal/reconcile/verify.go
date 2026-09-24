package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/fredrir/infra/internal/ci"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
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
	if err := c.Runner.Run(ctx, "kubectl", "annotate", "kustomizations.kustomize.toolkit.fluxcd.io", "--all", "--all-namespaces", "--overwrite", "--field-manager=flux-client-side-apply", "reconcile.fluxcd.io/requestedAt="+token, "--request-timeout=30s"); err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	if err := poll(wait, func() error { return c.verifyKubernetes(wait, revision, token) }); err != nil {
		return err
	}
	if err := c.Runner.Run(ctx, "kubectl", "annotate", "helmreleases.helm.toolkit.fluxcd.io", "--all", "--all-namespaces", "--overwrite", "--field-manager=flux-client-side-apply", "reconcile.fluxcd.io/requestedAt="+token, "--request-timeout=30s"); err != nil {
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

func (c *Commands) verifyKubernetes(ctx context.Context, revision, token string) error {
	items, err := c.resources(ctx, "kustomizations.kustomize.toolkit.fluxcd.io")
	if err != nil {
		return err
	}
	root := false
	for _, item := range items {
		if item.Spec.Suspend {
			continue
		}
		if err := ready(item); err != nil {
			return err
		}
		if token != "" && item.Status.LastHandledReconcileAt != token {
			return fmt.Errorf("%s has not reconciled its requested configuration", item.Metadata.Name)
		}
		if item.Spec.SourceRef.Kind == "GitRepository" && item.Spec.SourceRef.Name == "flux-system" {
			if !strings.HasSuffix(item.Status.LastAppliedRevision, ":"+revision) {
				return fmt.Errorf("%s has not applied %s", item.Metadata.Name, revision)
			}
			if item.Metadata.Name == "flux-system" && item.Metadata.Namespace == "flux-system" {
				root = true
			}
		}
	}
	if !root {
		return fmt.Errorf("active Flux root not found")
	}
	return nil
}

func (c *Commands) verifyHelm(ctx context.Context, token, host string) error {
	items, err := c.resources(ctx, "helmreleases.helm.toolkit.fluxcd.io")
	if err != nil {
		return err
	}
	grafana := false
	for _, item := range items {
		if item.Spec.Suspend {
			continue
		}
		if err := ready(item); err != nil {
			return err
		}
		if token != "" && item.Status.LastHandledReconcileAt != token {
			return fmt.Errorf("%s has not reconciled drift", item.Metadata.Name)
		}
		if item.Metadata.Namespace == "observability" && item.Metadata.Name == "monitoring" {
			grafana = true
			if host != "" && item.Spec.Values.Grafana.INI.Server.RootURL != "https://"+host {
				return fmt.Errorf("Grafana root_url does not match %s", host)
			}
		}
	}
	if !grafana {
		return fmt.Errorf("active Grafana release not found")
	}
	return nil
}

func (c *Commands) Verify(ctx context.Context, plan Plan) error {
	if c.VerifyArtifacts {
		if err := c.verifyDeployment(ctx, plan); err != nil {
			return err
		}
	} else {
		if err := c.verifyKubernetes(ctx, plan.Revision, ""); err != nil {
			return err
		}
		if err := c.verifyHelm(ctx, "", plan.Host); err != nil {
			return err
		}
	}
	if err := c.VerifyHosts(ctx, plan); err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if len(plan.Affected.Projects) == 0 {
		if err := poll(wait, func() error { return verifyGrafana(wait, client, "https://"+plan.Host) }); err != nil {
			return err
		}
	}
	if len(plan.Affected.Projects) > 0 && !slices.Contains(plan.Affected.Projects, "llunde") {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(c.Runner.Dir, "platform/projects/llunde/.deployments/llunde-frontend.json"))
	if err != nil {
		return err
	}
	var frontend struct{ Revision string }
	if err = json.Unmarshal(data, &frontend); err != nil {
		return err
	}
	return ci.WaitRevision(wait, client, "https://llunde.no/.well-known/revision", frontend.Revision, 2*time.Second)
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

func (c *Commands) VerifyLive(ctx context.Context, plan Plan) error {
	plan, err := c.Preflight(ctx, plan)
	if err != nil {
		return err
	}
	if err := c.RenderKubernetes(ctx, plan); err != nil {
		return err
	}
	return c.Verify(ctx, plan)
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
		return fmt.Errorf("Flux source has changed from %s", plan.Revision)
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
	owners := make([]resource, 0, len(names))
	for _, name := range names {
		item, err := c.snapshotResource(ctx, snapshot, "kustomizations.kustomize.toolkit.fluxcd.io", "flux-system", name)
		if err != nil {
			return err
		}
		if item.Spec.Suspend {
			return fmt.Errorf("selected Kustomization %s is suspended", name)
		}
		expected := c.kubernetes.owners[name]
		if item.Spec.Path != expected.Spec.Path || item.Spec.SourceRef != expected.Spec.SourceRef || !reflect.DeepEqual(item.Spec.DependsOn, expected.Spec.DependsOn) {
			return fmt.Errorf("%s source topology differs", name)
		}
		if token := c.kubernetes.requested["kustomizations.kustomize.toolkit.fluxcd.io/flux-system/"+name]; token != "" && item.Status.LastHandledReconcileAt != token {
			return fmt.Errorf("%s has not handled its requested reconciliation", name)
		}
		if err = ready(item); err != nil {
			return err
		}
		if item.Spec.SourceRef.Kind == "GitRepository" && !strings.HasSuffix(item.Status.LastAppliedRevision, ":"+plan.Revision) {
			return fmt.Errorf("%s has not applied %s", name, plan.Revision)
		}
		owners = append(owners, item)
	}
	if err = c.verifyArtifacts(ctx, plan, owners); err != nil {
		return err
	}
	for _, owner := range owners {
		if err = c.verifyOwnedWorkloads(ctx, owner, snapshot); err != nil {
			return err
		}
	}
	if len(plan.Affected.Projects) == 0 {
		return c.verifyHelm(ctx, "", plan.Host)
	}
	return nil
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
