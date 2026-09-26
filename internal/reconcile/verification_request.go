package reconcile

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/fredrir/infra/internal/platformops"
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
	Gatus          string
	Heartbeat      string
	HeartbeatToken string
	Poll           time.Duration
	Deadline       time.Duration
	Log            io.Writer
}

const dispatchTimeout = time.Minute

var heartbeatEndpoint = regexp.MustCompile(`^[a-z0-9-]+_[a-z0-9-]+$`)

type verificationRun struct {
	ID      int64  `json:"workflow_run_id"`
	HTMLURL string `json:"html_url"`
}

type verificationClient struct {
	http  *http.Client
	runs  string
	etag  string
	state struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
}

func RequestVerification(ctx context.Context, request VerificationRequest) error {
	owner, name, ok := strings.Cut(request.Repository, "/")
	switch {
	case !ok || owner == "" || name == "" || strings.Contains(name, "/"):
		return fmt.Errorf("repository %q is not OWNER/NAME", request.Repository)
	case request.AppID <= 0 || request.InstallationID <= 0:
		return errors.New("GitHub App and installation IDs are required")
	case request.Workflow == "" || request.Ref == "":
		return errors.New("workflow and ref are required")
	case !heartbeatEndpoint.MatchString(request.Heartbeat):
		return fmt.Errorf("heartbeat endpoint %q is not GROUP_NAME", request.Heartbeat)
	case !platformops.ValidHeartbeatToken(request.HeartbeatToken):
		return errors.New("invalid verification heartbeat token")
	case request.Poll <= 0 || request.Deadline <= 0:
		return errors.New("poll interval and deadline must be positive")
	}
	transport, err := ghinstallation.New(http.DefaultTransport, request.AppID, request.InstallationID, request.PrivateKey)
	if err != nil {
		return fmt.Errorf("GitHub App private key: %w", err)
	}
	transport.BaseURL = request.API
	transport.InstallationTokenOptions = &github.InstallationTokenOptions{
		Repositories: []string{name},
		Permissions:  &github.InstallationPermissions{Actions: github.Ptr("write")},
	}
	repository := strings.TrimSuffix(request.API, "/") + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	client := &verificationClient{http: &http.Client{Transport: transport, Timeout: 30 * time.Second}, runs: repository + "/actions/runs/"}
	run, err := client.dispatch(ctx, repository+"/actions/workflows/"+url.PathEscape(request.Workflow)+"/dispatches", request)
	if err != nil {
		return fmt.Errorf("dispatch %s in %s at %s: %w", request.Workflow, request.Repository, request.Ref, err)
	}
	log := cmp.Or(request.Log, io.Discard)
	fmt.Fprintln(log, "Requested verification:", run.HTMLURL)
	conclusion, err := client.wait(ctx, run, request.Poll, request.Deadline)
	var failure error
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		failure = fmt.Errorf("%s did not complete within %s: %w", run.HTMLURL, request.Deadline, err)
	case conclusion == "cancelled":
		fmt.Fprintln(log, "Verification cancelled:", run.HTMLURL)
		return nil
	case conclusion != "success":
		failure = fmt.Errorf("%s concluded %s", run.HTMLURL, conclusion)
	default:
		fmt.Fprintln(log, "Verification succeeded:", run.HTMLURL)
	}
	return errors.Join(failure, platformops.ReportHeartbeat(ctx, request.Gatus, request.Heartbeat, request.HeartbeatToken, failure))
}

func (c *verificationClient) dispatch(ctx context.Context, endpoint string, request VerificationRequest) (verificationRun, error) {
	body, err := json.Marshal(github.CreateWorkflowDispatchEventRequest{
		Ref:              request.Ref,
		Inputs:           map[string]any{"verify": "true", "repair": "true"},
		ReturnRunDetails: github.Ptr(true),
	})
	if err != nil {
		return verificationRun{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, dispatchTimeout)
	defer cancel()
	dispatch, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return verificationRun{}, err
	}
	dispatch.Header.Set("Content-Type", "application/json")
	var run verificationRun
	if _, err := c.do(dispatch, &run); err != nil {
		return verificationRun{}, err
	}
	if run.ID <= 0 || run.HTMLURL == "" {
		return verificationRun{}, errors.New("response identifies no workflow run")
	}
	return run, nil
}

func (c *verificationClient) wait(ctx context.Context, run verificationRun, poll, deadline time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	delay := poll
	var last error
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			if last != nil {
				return "", fmt.Errorf("last poll: %w", last)
			}
			return "", ctx.Err()
		case <-timer.C:
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.runs+strconv.FormatInt(run.ID, 10), nil)
		if err != nil {
			return "", err
		}
		if c.etag != "" {
			request.Header.Set("If-None-Match", c.etag)
		}
		response, err := c.do(request, &c.state)
		if ctx.Err() != nil {
			continue
		}
		delay, last = max(poll, retryAfter(response, time.Now())), err
		if err == nil && c.state.Status == "completed" {
			return c.state.Conclusion, nil
		}
	}
}

func (c *verificationClient) do(request *http.Request, result any) (*http.Response, error) {
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return response, nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return response, err
	}
	if response.StatusCode/100 != 2 {
		var message struct {
			Message string `json:"message"`
		}
		json.Unmarshal(data, &message)
		return response, fmt.Errorf("%s %s", response.Status, message.Message)
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, result); err != nil {
			return response, err
		}
	}
	if etag := response.Header.Get("ETag"); etag != "" && request.Method == http.MethodGet {
		c.etag = etag
	}
	return response, nil
}

func retryAfter(response *http.Response, now time.Time) time.Duration {
	if response == nil || (response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusTooManyRequests) {
		return 0
	}
	if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if reset, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && response.Header.Get("X-RateLimit-Remaining") == "0" {
		return time.Unix(reset, 0).Sub(now)
	}
	return 0
}
