package reconciler

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/fredrir/infra/internal/reconcile"
	"github.com/google/go-github/v88/github"
)

func installationToken(ctx context.Context, app App, key []byte, repositories []string, permissions *github.InstallationPermissions) (string, func(context.Context) error, error) {
	transport, err := ghinstallation.New(http.DefaultTransport, app.AppID, app.InstallationID, key)
	if err != nil {
		return "", nil, fmt.Errorf("App %d private key: %w", app.AppID, err)
	}
	transport.BaseURL = app.API
	transport.InstallationTokenOptions = &github.InstallationTokenOptions{Repositories: repositories, Permissions: permissions}
	token, err := transport.Token(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("App %d installation token: %w", app.AppID, err)
	}
	revoke := func(ctx context.Context) error {
		client, err := reconcile.GitHubClient(app.API, token)
		if err != nil {
			return err
		}
		_, err = client.Apps.RevokeInstallationToken(ctx)
		return err
	}
	return token, revoke, nil
}

func runnerFleetToken(ctx context.Context, app App, key []byte, source, access string) (string, func(context.Context) error, error) {
	fleet, err := reconcile.LoadRunnerFleet(source)
	if err != nil {
		return "", nil, err
	}
	return installationToken(ctx, app, key, fleet.RepositoryNames(), &github.InstallationPermissions{Administration: github.Ptr(access), Metadata: github.Ptr("read")})
}
