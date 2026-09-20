package platformops

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type RunnerJob struct {
	Pool       string
	Event      string
	Ref        string
	Protected  bool
	Repository string
	OwnerID    string
}

func CheckRunnerJob(job RunnerJob, event io.Reader) error {
	deny := func(reason string) error {
		return fmt.Errorf("::error::Runner pool %s refuses %s on %s: %s", job.Pool, job.Event, job.Ref, reason)
	}
	if job.OwnerID != "114402558" {
		return deny("foreign repository owner")
	}
	main := job.Ref == "refs/heads/main" && job.Protected
	switch job.Pool {
	case "pr":
		if job.Event != "pull_request" {
			return deny("only pull_request events")
		}
		var payload struct {
			PullRequest struct {
				Head struct {
					Repo struct {
						FullName string `json:"full_name"`
					}
				}
			} `json:"pull_request"`
		}
		if event == nil || json.NewDecoder(io.LimitReader(event, 8<<20)).Decode(&payload) != nil {
			return deny("unreadable pull request head")
		}
		if job.Repository == "" || payload.PullRequest.Head.Repo.FullName != job.Repository {
			return deny("fork pull requests never run")
		}
	case "main":
		if job.Event != "push" && job.Event != "workflow_dispatch" {
			return deny("only push or workflow_dispatch")
		}
		if !main {
			return deny("only the protected main branch")
		}
	case "release":
		if job.Event == "push" && strings.HasPrefix(job.Ref, "refs/tags/v") {
			if !job.Protected {
				return deny("only protected release tags")
			}
		} else if job.Event == "workflow_dispatch" {
			if !main {
				return deny("dry runs only from the protected main branch")
			}
		} else {
			return deny("only release tags or main dry runs")
		}
	default:
		return deny("unknown pool")
	}
	return nil
}
