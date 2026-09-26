package reconciler

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/reconcile"
	"github.com/google/go-github/v88/github"
)

const (
	checkName         = "reconcile / apply"
	checkSummaryLimit = 60000
)

type checks struct {
	publisher Publisher
	key       []byte
}

func (c checks) with(ctx context.Context, action func(*github.Client, string, string) error) error {
	owner, name, _ := strings.Cut(c.publisher.Repository, "/")
	token, revoke, err := installationToken(ctx, c.publisher.App, c.key, []string{name}, &github.InstallationPermissions{Checks: github.Ptr("write"), Metadata: github.Ptr("read")})
	if err != nil {
		return err
	}
	client, err := reconcile.GitHubClient(c.publisher.API, token)
	if err == nil {
		err = action(client, owner, name)
	}
	return errors.Join(err, revoke(context.WithoutCancel(ctx)))
}

func (c checks) start(ctx context.Context, revision, external string, started time.Time) (int64, error) {
	var id int64
	err := c.with(ctx, func(client *github.Client, owner, name string) error {
		created, _, err := client.Checks.CreateCheckRun(ctx, owner, name, github.CreateCheckRunOptions{
			Name:       checkName,
			HeadSHA:    revision,
			ExternalID: github.Ptr(external),
			Status:     github.Ptr("in_progress"),
			StartedAt:  &github.Timestamp{Time: started},
		})
		if err == nil {
			id = created.GetID()
		}
		return err
	})
	return id, err
}

func (c checks) complete(ctx context.Context, id int64, conclusion, title, summary string, completed time.Time) error {
	if len(summary) > checkSummaryLimit {
		summary = strings.ToValidUTF8(summary[:checkSummaryLimit], "") + "\n…"
	}
	return c.with(ctx, func(client *github.Client, owner, name string) error {
		_, _, err := client.Checks.UpdateCheckRun(ctx, owner, name, id, github.UpdateCheckRunOptions{
			Name:        checkName,
			Status:      github.Ptr("completed"),
			Conclusion:  github.Ptr(conclusion),
			CompletedAt: &github.Timestamp{Time: completed},
			Output:      &github.CheckRunOutput{Title: github.Ptr(title), Summary: github.Ptr(summary)},
		})
		return err
	})
}
