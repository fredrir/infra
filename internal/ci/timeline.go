package ci

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type TimelineStep struct {
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	Conclusion string     `json:"conclusion"`
	Started    *time.Time `json:"started_at"`
	Completed  *time.Time `json:"completed_at"`
}

type TimelineJob struct {
	TimelineStep
	Steps []TimelineStep `json:"steps"`
}

type WorkflowTimeline struct {
	Repository          string        `json:"repository_name"`
	RepositoryID        uint64        `json:"repository_id"`
	HeadSHA             string        `json:"head_sha"`
	DisplayTitle        string        `json:"display_title"`
	ID                  uint64        `json:"id"`
	Created             time.Time     `json:"created_at"`
	Status              string        `json:"status"`
	Conclusion          string        `json:"conclusion"`
	Jobs                []TimelineJob `json:"jobs"`
	InitialQueueSeconds *float64      `json:"initial_queue_seconds,omitempty"`
	ElapsedSeconds      *float64      `json:"elapsed_seconds,omitempty"`
}

type DeploymentTimeline struct {
	Schema                   int                `json:"schema"`
	Runs                     []WorkflowTimeline `json:"runs"`
	BudgetSeconds            float64            `json:"budget_seconds"`
	WorkflowToHealthySeconds *float64           `json:"workflow_to_healthy_seconds,omitempty"`
	BudgetExceeded           *bool              `json:"budget_exceeded,omitempty"`
}

func ReadWorkflowTimeline(ctx context.Context, runner Runner, repository string, id uint64) (WorkflowTimeline, error) {
	var result WorkflowTimeline
	if !repositoryPattern.MatchString(repository) || id == 0 {
		return result, fmt.Errorf("repository and positive workflow run ID required")
	}
	endpoint := "repos/" + repository + "/actions/runs/" + strconv.FormatUint(id, 10)
	data, err := runner.Output(ctx, "gh", "api", endpoint)
	if err != nil {
		return result, err
	}
	result, err = decodeWorkflowTimeline(data)
	if err != nil {
		return result, err
	}
	if result.ID != id || result.Created.IsZero() || result.Repository != repository || result.RepositoryID == 0 {
		return result, fmt.Errorf("invalid workflow run identity")
	}
	result.Repository = repository
	for page := 1; page <= 100; page++ {
		data, err := runner.Output(ctx, "gh", "api", endpoint+"/jobs?per_page=100&page="+strconv.Itoa(page))
		if err != nil {
			return result, err
		}
		var batch struct {
			Total int           `json:"total_count"`
			Jobs  []TimelineJob `json:"jobs"`
		}
		if err := json.Unmarshal(data, &batch); err != nil {
			return result, err
		}
		result.Jobs = append(result.Jobs, batch.Jobs...)
		if len(result.Jobs) >= batch.Total {
			break
		}
		if len(batch.Jobs) == 0 || page == 100 {
			return result, fmt.Errorf("incomplete workflow job timeline")
		}
	}
	var first, last time.Time
	for _, job := range result.Jobs {
		if job.Started != nil && (first.IsZero() || job.Started.Before(first)) {
			first = *job.Started
		}
		if job.Completed != nil && job.Completed.After(last) {
			last = *job.Completed
		}
	}
	if !first.IsZero() {
		seconds := first.Sub(result.Created).Seconds()
		result.InitialQueueSeconds = &seconds
	}
	if result.Status == "completed" && !last.IsZero() {
		seconds := last.Sub(result.Created).Seconds()
		result.ElapsedSeconds = &seconds
	}
	return result, nil
}

func decodeWorkflowTimeline(data []byte) (WorkflowTimeline, error) {
	var response struct {
		WorkflowTimeline
		Repository struct {
			ID       uint64 `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return WorkflowTimeline{}, err
	}
	response.WorkflowTimeline.Repository = response.Repository.FullName
	response.WorkflowTimeline.RepositoryID = response.Repository.ID
	return response.WorkflowTimeline, nil
}

func JoinDeploymentTimeline(build, deployment WorkflowTimeline, budget time.Duration) DeploymentTimeline {
	report := DeploymentTimeline{Schema: 1, Runs: []WorkflowTimeline{build}, BudgetSeconds: budget.Seconds()}
	if deployment.ID == 0 {
		return report
	}
	report.Runs = append(report.Runs, deployment)
	identity := strings.Fields(deployment.DisplayTitle)
	if deployment.Repository != "fredrir/infra" || build.RepositoryID == 0 || len(identity) != 4 || identity[0] != "Deploy" || identity[1] != strconv.FormatUint(build.RepositoryID, 10) || identity[3] != build.HeadSHA || len(build.HeadSHA) != 40 {
		return report
	}
	for _, job := range deployment.Jobs {
		for _, step := range job.Steps {
			if step.Name == "Verify served frontend revision" && step.Conclusion == "success" && step.Completed != nil && !step.Completed.Before(build.Created) {
				seconds := step.Completed.Sub(build.Created).Seconds()
				exceeded := seconds >= budget.Seconds()
				report.WorkflowToHealthySeconds, report.BudgetExceeded = &seconds, &exceeded
			}
		}
	}
	return report
}
