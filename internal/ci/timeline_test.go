package ci

import (
	"testing"
	"time"
)

func TestDeploymentTimelineIncludesQueueAndRequiresRevisionReadiness(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ready := start.Add(35 * time.Second)
	build := WorkflowTimeline{ID: 1, RepositoryID: 42, HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Created: start}
	deploy := WorkflowTimeline{ID: 2, Repository: "fredrir/infra", DisplayTitle: "Deploy 42 ghcr.io/fredrir/frontend aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Created: start.Add(20 * time.Second), Jobs: []TimelineJob{{Steps: []TimelineStep{{Name: "Verify served frontend revision", Conclusion: "success", Completed: &ready}}}}}
	report := JoinDeploymentTimeline(build, deploy, 30*time.Second)
	if report.WorkflowToHealthySeconds == nil || *report.WorkflowToHealthySeconds != 35 || !*report.BudgetExceeded {
		t.Fatalf("queue excluded: %+v", report)
	}
	deploy.Jobs[0].Steps[0].Conclusion = "failure"
	report = JoinDeploymentTimeline(build, deploy, time.Minute)
	if report.WorkflowToHealthySeconds != nil {
		t.Fatal("failed readiness reported healthy")
	}
}

func TestTimelineDecodesGitHubRepositoryObject(t *testing.T) {
	run, err := decodeWorkflowTimeline([]byte(`{"id":1,"repository":{"id":42,"full_name":"fredrir/frontend"},"head_sha":"abc","created_at":"2026-01-01T00:00:00Z"}`))
	if err != nil || run.Repository != "fredrir/frontend" || run.RepositoryID != 42 || run.HeadSHA != "abc" {
		t.Fatalf("invalid timeline: %+v, %v", run, err)
	}
}

func TestTimelineRejectsUnrelatedDeployment(t *testing.T) {
	start := time.Now()
	ready := start.Add(time.Second)
	build := WorkflowTimeline{ID: 1, RepositoryID: 42, HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Created: start}
	deploy := WorkflowTimeline{ID: 2, Repository: "fredrir/infra", DisplayTitle: "Deploy 43 ghcr.io/fredrir/frontend aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Jobs: []TimelineJob{{Steps: []TimelineStep{{Name: "Verify served frontend revision", Conclusion: "success", Completed: &ready}}}}}
	if JoinDeploymentTimeline(build, deploy, time.Minute).WorkflowToHealthySeconds != nil {
		t.Fatal("unrelated deployment accepted")
	}
}
