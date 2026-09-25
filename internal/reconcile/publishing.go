package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/fredrir/infra/internal/ci"
	"github.com/google/go-github/v88/github"
)

const (
	publishedBranch = "production"
	gitToken        = "INFRA_GIT_TOKEN"
	GitHubAPI       = "https://api.github.com"
)

type Publisher struct {
	Repository     string `json:"repository"`
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKey     []byte `json:"-"`
	API            string `json:"-"`
	Remote         string `json:"-"`
}

func ReadPublisher(root string) (Publisher, error) {
	data, err := os.ReadFile(filepath.Join(root, "build", "publisher.json"))
	if err != nil {
		return Publisher{}, err
	}
	var publisher Publisher
	if err := json.Unmarshal(data, &publisher); err != nil {
		return Publisher{}, fmt.Errorf("build/publisher.json: %w", err)
	}
	if _, _, err := publisher.repository(); err != nil || publisher.AppID <= 0 || publisher.InstallationID <= 0 {
		return Publisher{}, errors.New("build/publisher.json requires repository OWNER/NAME, app_id and installation_id")
	}
	publisher.API = GitHubAPI
	publisher.Remote = "https://github.com/" + publisher.Repository + ".git"
	return publisher, nil
}

func (p Publisher) repository() (string, string, error) {
	owner, name, ok := strings.Cut(p.Repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("repository %q is not OWNER/NAME", p.Repository)
	}
	return owner, name, nil
}

func (p Publisher) Token(ctx context.Context) (string, func(context.Context) error, error) {
	_, name, err := p.repository()
	if err != nil {
		return "", nil, err
	}
	transport, err := ghinstallation.New(http.DefaultTransport, p.AppID, p.InstallationID, p.PrivateKey)
	if err != nil {
		return "", nil, fmt.Errorf("publisher App private key: %w", err)
	}
	transport.BaseURL = p.API
	transport.InstallationTokenOptions = &github.InstallationTokenOptions{
		Repositories: []string{name},
		Permissions:  &github.InstallationPermissions{Contents: github.Ptr("write")},
	}
	token, err := transport.Token(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("publisher installation token: %w", err)
	}
	revoke := func(ctx context.Context) error {
		client, err := GitHubClient(p.API, token)
		if err != nil {
			return err
		}
		_, err = client.Apps.RevokeInstallationToken(ctx)
		return err
	}
	return token, revoke, nil
}

func GitHubClient(api, token string) (*github.Client, error) {
	options := []github.ClientOptionsFunc{github.WithTimeout(30 * time.Second), github.WithURLs(&api, nil)}
	if token != "" {
		options = append(options, github.WithAuthToken(token))
	}
	return github.NewClient(options...)
}

func tokenGit(runner ci.Runner, token string) ci.Runner {
	helper := `!f() { if [ "$1" = get ]; then printf 'username=x-access-token\npassword=%s\n' "$` + gitToken + `"; fi; }; f`
	runner.Env = append(slices.Clone(runner.Env),
		gitToken+"="+token,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=credential.helper",
		"GIT_CONFIG_VALUE_1="+helper,
	)
	return runner
}

func (c *Commands) push(ctx context.Context, revision string) error {
	if c.Publisher == nil || len(c.Publisher.PrivateKey) == 0 {
		return errors.New("publisher App private key required")
	}
	token, revoke, err := c.Publisher.Token(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if revoked := revoke(context.WithoutCancel(ctx)); revoked != nil && c.Runner.Stderr != nil {
			fmt.Fprintln(c.Runner.Stderr, "revoke publisher token:", revoked)
		}
	}()
	return tokenGit(c.Runner, token).Run(ctx, "git", "push", "--no-verify", c.Publisher.Remote, revision+":refs/heads/"+publishedBranch)
}
