package platformops

import (
	"strings"
	"testing"
)

func TestRunnerPoolTrustBoundaries(t *testing.T) {
	for _, tc := range []struct {
		pool, event, ref, owner, head string
		protected, allowed            bool
	}{
		{"main", "push", "refs/heads/main", "114402558", "", true, true},
		{"main", "workflow_dispatch", "refs/heads/main", "114402558", "", true, true},
		{"main", "push", "refs/heads/feature", "114402558", "", true, false},
		{"main", "pull_request", "refs/pull/7/merge", "114402558", "", false, false},
		{"main", "push", "refs/tags/v1.0.0", "114402558", "", true, false},
		{"main", "push", "refs/heads/main", "114402558", "", false, false},
		{"main", "pull_request_target", "refs/heads/main", "114402558", "", true, false},
		{"main", "push", "refs/heads/main", "foreign", "", true, false},
		{"pr", "pull_request", "refs/pull/1/merge", "114402558", "fredrir/infra", false, true},
		{"pr", "pull_request", "refs/pull/1/merge", "114402558", "fork/infra", false, false},
		{"pr", "pull_request_target", "refs/heads/main", "114402558", "fredrir/infra", true, false},
		{"pr", "push", "refs/heads/main", "114402558", "fredrir/infra", true, false},
		{"release", "push", "refs/tags/v1", "114402558", "", true, true},
		{"release", "push", "refs/tags/v1", "114402558", "", false, false},
		{"release", "workflow_dispatch", "refs/heads/main", "114402558", "", true, true},
		{"release", "push", "refs/heads/main", "114402558", "", true, false},
		{"release", "push", "refs/tags/nightly", "114402558", "", true, false},
		{"release", "workflow_dispatch", "refs/heads/feature", "114402558", "", true, false},
		{"release", "pull_request", "refs/pull/7/merge", "114402558", "", true, false},
		{"", "push", "refs/heads/main", "114402558", "", true, false},
		{"build", "push", "refs/heads/main", "114402558", "", true, false},
	} {
		err := CheckRunnerJob(RunnerJob{Pool: tc.pool, Event: tc.event, Ref: tc.ref, OwnerID: tc.owner, Protected: tc.protected, Repository: "fredrir/infra"}, strings.NewReader(`{"pull_request":{"head":{"repo":{"full_name":"`+tc.head+`"}}}}`))
		if (err == nil) != tc.allowed {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

func TestCacheGrantsKeepPoolsSeparate(t *testing.T) {
	keys := []CacheKey{{Project: "a", Role: "rw", ID: "a-rw"}, {Project: "a", Role: "ro", ID: "a-ro"}, {Project: "a", Role: "release", ID: "a-release"}, {Project: "b", Role: "rw", ID: "b-rw"}}
	main := CacheGrants(keys, "admin", "a", "main")
	if main["a-rw"] != (Permission{Read: true, Write: true}) || main["a-ro"] != (Permission{Read: true}) || len(main) != 3 {
		t.Fatalf("main grants: %v", main)
	}
	release := CacheGrants(keys, "admin", "a", "release")
	if len(release) != 2 || !release["a-release"].Write {
		t.Fatalf("release grants: %v", release)
	}
	toolchains := CacheGrants(keys, "admin", "", "toolchains")
	if len(toolchains) != 2 || toolchains["a-release"] != (Permission{Read: true}) || toolchains["admin"] != (Permission{Read: true, Write: true, Owner: true}) {
		t.Fatalf("toolchain grants: %v", toolchains)
	}
}
