package reconcile

import (
	"context"
	"fmt"
	"strings"
)

func (c *Commands) PublishedRevision(ctx context.Context) (string, error) {
	source, err := c.getResource(ctx, "gitrepositories.source.toolkit.fluxcd.io", "flux-system", "flux-system")
	if err != nil {
		return "", err
	}
	if source.Spec.Ref.Branch != "production" {
		return "", fmt.Errorf("Flux source does not follow production")
	}
	if err = conditionReady(source); err != nil {
		return "", err
	}
	revision := strings.TrimPrefix(source.Status.Artifact.Revision, "production@sha1:")
	if revision == source.Status.Artifact.Revision || !revisionPattern.MatchString(revision) {
		return "", fmt.Errorf("Flux production source has an invalid revision")
	}
	return revision, nil
}
