package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v88/github"
)

type VerificationRequest struct {
	AppID          int64
	InstallationID int64
	PrivateKey     []byte
	Repository     string
	Workflow       string
	Ref            string
	API            string
}

const verificationRequestTimeout = time.Minute

func RequestVerification(ctx context.Context, request VerificationRequest) (string, error) {
	owner, name, ok := strings.Cut(request.Repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("repository %q is not OWNER/NAME", request.Repository)
	}
	if request.AppID <= 0 || request.InstallationID <= 0 {
		return "", errors.New("GitHub App and installation IDs are required")
	}
	if request.Workflow == "" || request.Ref == "" {
		return "", errors.New("workflow and ref are required")
	}
	transport, err := ghinstallation.New(http.DefaultTransport, request.AppID, request.InstallationID, request.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("GitHub App private key: %w", err)
	}
	transport.BaseURL = request.API
	transport.InstallationTokenOptions = &github.InstallationTokenOptions{
		Repositories: []string{name},
		Permissions:  &github.InstallationPermissions{Actions: github.Ptr("write")},
	}
	body, err := json.Marshal(github.CreateWorkflowDispatchEventRequest{
		Ref:              request.Ref,
		Inputs:           map[string]any{"verify": "true", "repair": "true"},
		ReturnRunDetails: github.Ptr(true),
	})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, verificationRequestTimeout)
	defer cancel()
	endpoint := strings.TrimSuffix(request.API, "/") + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/actions/workflows/" + url.PathEscape(request.Workflow) + "/dispatches"
	dispatch, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	dispatch.Header.Set("Accept", "application/vnd.github+json")
	dispatch.Header.Set("Content-Type", "application/json")
	dispatch.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	failure := fmt.Sprintf("dispatch %s in %s at %s", request.Workflow, request.Repository, request.Ref)
	response, err := (&http.Client{Transport: transport}).Do(dispatch)
	if err != nil {
		return "", fmt.Errorf("%s: %w", failure, err)
	}
	defer response.Body.Close()
	var result struct {
		HTMLURL string `json:"html_url"`
		Message string `json:"message"`
	}
	decoded := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result)
	if response.StatusCode/100 != 2 {
		return "", fmt.Errorf("%s: %s %s", failure, response.Status, result.Message)
	}
	if decoded != nil && decoded != io.EOF {
		return "", fmt.Errorf("%s: %w", failure, decoded)
	}
	return result.HTMLURL, nil
}
