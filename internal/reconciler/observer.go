package reconciler

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/fredrir/infra/internal/reconcile"
	"github.com/google/go-github/v88/github"
)

func observerToken(ctx context.Context, observer Observer, key []byte, source string) (string, func(context.Context) error, error) {
	fleet, err := reconcile.LoadRunnerFleet(source)
	if err != nil {
		return "", nil, err
	}
	transport, err := ghinstallation.New(http.DefaultTransport, observer.AppID, observer.InstallationID, key)
	if err != nil {
		return "", nil, fmt.Errorf("observer App private key: %w", err)
	}
	transport.BaseURL = observer.API
	transport.InstallationTokenOptions = &github.InstallationTokenOptions{
		Repositories: fleet.RepositoryNames(),
		Permissions:  &github.InstallationPermissions{Administration: github.Ptr("read"), Metadata: github.Ptr("read")},
	}
	token, err := transport.Token(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("observer installation token: %w", err)
	}
	revoke := func(ctx context.Context) error {
		client, err := reconcile.GitHubClient(observer.API, token)
		if err != nil {
			return err
		}
		_, err = client.Apps.RevokeInstallationToken(ctx)
		return err
	}
	return token, revoke, nil
}
