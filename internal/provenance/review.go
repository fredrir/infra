package provenance

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/deployment"
	"github.com/google/go-github/v88/github"
	"github.com/hmarr/codeowners"
)

const (
	webFlowKey    = "keys/github-web-flow.asc"
	codeOwners    = ".github/CODEOWNERS"
	reviewedBase  = "main"
	personAccount = "User"
	botAccount    = "Bot"
	renovateBot   = "renovate[bot]"
	promotionBot  = "octo-sts[bot]"
	planCheck     = "reconcile / plan"
	checksApp     = "github-actions"
)

var digestPattern = regexp.MustCompile(`[0-9a-f]{64}`)

type PullRequests interface {
	WithCommit(ctx context.Context, commit string) ([]*github.PullRequest, error)
	Get(ctx context.Context, number int) (*github.PullRequest, error)
	Reviews(ctx context.Context, number int) ([]*github.PullRequestReview, error)
	Commits(ctx context.Context, number int) ([]*github.RepositoryCommit, error)
	CheckRuns(ctx context.Context, ref string) ([]*github.CheckRun, error)
	TagRevision(ctx context.Context, tag string) (string, error)
}

type GitHubPullRequests struct {
	Client      *github.Client
	Owner, Name string
}

func (p GitHubPullRequests) WithCommit(ctx context.Context, commit string) ([]*github.PullRequest, error) {
	pulls, err := collect(p.Client.PullRequests.ListPullRequestsWithCommitIter(ctx, p.Owner, p.Name, commit, &github.ListOptions{PerPage: 100}))
	return pulls, apiUnavailable(err)
}

func (p GitHubPullRequests) Get(ctx context.Context, number int) (*github.PullRequest, error) {
	pull, _, err := p.Client.PullRequests.Get(ctx, p.Owner, p.Name, number)
	return pull, apiUnavailable(err)
}

func (p GitHubPullRequests) Reviews(ctx context.Context, number int) ([]*github.PullRequestReview, error) {
	reviews, err := collect(p.Client.PullRequests.ListReviewsIter(ctx, p.Owner, p.Name, number, &github.ListOptions{PerPage: 100}))
	return reviews, apiUnavailable(err)
}

func (p GitHubPullRequests) Commits(ctx context.Context, number int) ([]*github.RepositoryCommit, error) {
	commits, err := collect(p.Client.PullRequests.ListCommitsIter(ctx, p.Owner, p.Name, number, &github.ListOptions{PerPage: 100}))
	return commits, apiUnavailable(err)
}

func (p GitHubPullRequests) CheckRuns(ctx context.Context, ref string) ([]*github.CheckRun, error) {
	results, _, err := p.Client.Checks.ListCheckRunsForRef(ctx, p.Owner, p.Name, ref, &github.ListCheckRunsOptions{Filter: github.Ptr("latest"), ListOptions: github.ListOptions{PerPage: 100}})
	if err != nil {
		return nil, apiUnavailable(err)
	}
	return results.CheckRuns, nil
}

func (p GitHubPullRequests) TagRevision(ctx context.Context, tag string) (string, error) {
	reference, _, err := p.Client.Git.GetRef(ctx, p.Owner, p.Name, "tags/"+tag)
	if err != nil {
		return "", apiUnavailable(err)
	}
	object := reference.GetObject()
	if object.GetType() != "tag" {
		return object.GetSHA(), nil
	}
	annotated, _, err := p.Client.Git.GetTag(ctx, p.Owner, p.Name, object.GetSHA())
	if err != nil {
		return "", apiUnavailable(err)
	}
	return annotated.GetObject().GetSHA(), nil
}

func apiUnavailable(err error) error {
	var response *github.ErrorResponse
	var rate *github.RateLimitError
	var abuse *github.AbuseRateLimitError
	var network net.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &rate), errors.As(err, &abuse), errors.As(err, &network):
	case errors.As(err, &response) && response.Response != nil && (response.Response.StatusCode >= http.StatusInternalServerError || response.Response.StatusCode == http.StatusTooManyRequests):
	default:
		return err
	}
	return deployment.Unavailable(err)
}

func collect[T any](items iter.Seq2[T, error]) ([]T, error) {
	var collected []T
	for item, err := range items {
		if err != nil {
			return nil, err
		}
		collected = append(collected, item)
	}
	return collected, nil
}

type reviewGate struct {
	provenanceGate
	api     PullRequests
	keyring openpgp.EntityList
	owners  codeowners.Ruleset
	commits map[string]provenanceCommit
	merges  map[int]reviewedMerge
}

type reviewedMerge struct {
	covered []string
	err     error
}

func (g provenanceGate) reviewed(ctx context.Context, commits []provenanceCommit, pending map[string]error) {
	if len(pending) == 0 {
		return
	}
	review, err := g.reviewGate(ctx, commits)
	if err != nil {
		for hash, reason := range pending {
			pending[hash] = errors.Join(reason, fmt.Errorf("not a reviewed merge: %w", err))
		}
		return
	}
	for _, commit := range slices.Backward(commits) {
		reason, ok := pending[commit.hash]
		if !ok {
			continue
		}
		pulls, err := review.api.WithCommit(ctx, commit.hash)
		if err != nil {
			pending[commit.hash] = errors.Join(reason, fmt.Errorf("not a reviewed merge: list its pull requests: %w", err))
			continue
		}
		if len(pulls) == 0 {
			pending[commit.hash] = errors.Join(reason, errors.New("not a reviewed merge: no pull request merged it"))
			continue
		}
		for _, pull := range pulls {
			merge := review.merge(ctx, pull.GetNumber())
			for _, covered := range merge.covered {
				delete(pending, covered)
			}
			if _, ok := pending[commit.hash]; !ok {
				break
			}
			reason = errors.Join(reason, fmt.Errorf("not a reviewed merge: pull request #%d: %w", pull.GetNumber(), cmp.Or(merge.err, errors.New("does not include it"))))
			pending[commit.hash] = reason
		}
	}
}

func (g provenanceGate) reviewGate(ctx context.Context, commits []provenanceCommit) (*reviewGate, error) {
	if g.verifier.PullRequests == nil {
		return nil, errors.New("no pull request API")
	}
	key, err := g.verifier.git(ctx, nil, "show", g.base+":"+webFlowKey)
	if err != nil {
		return nil, fmt.Errorf("read %s at %s: %w", webFlowKey, g.base, err)
	}
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(key.Stdout))
	if err != nil {
		return nil, fmt.Errorf("%s at %s: %w", webFlowKey, g.base, err)
	}
	owners, err := g.verifier.git(ctx, nil, "show", g.base+":"+codeOwners)
	if err != nil {
		return nil, fmt.Errorf("read %s at %s: %w", codeOwners, g.base, err)
	}
	ruleset, err := codeowners.ParseFile(bytes.NewReader(owners.Stdout))
	if err != nil {
		return nil, fmt.Errorf("%s at %s: %w", codeOwners, g.base, err)
	}
	review := &reviewGate{provenanceGate: g, api: g.verifier.PullRequests, keyring: keyring, owners: ruleset, commits: map[string]provenanceCommit{}, merges: map[int]reviewedMerge{}}
	for _, commit := range commits {
		review.commits[commit.hash] = commit
	}
	return review, nil
}

func (r *reviewGate) merge(ctx context.Context, number int) reviewedMerge {
	if merge, ok := r.merges[number]; ok {
		return merge
	}
	covered, err := r.approvedMerge(ctx, number)
	r.merges[number] = reviewedMerge{covered: covered, err: err}
	return r.merges[number]
}

func (r *reviewGate) approvedMerge(ctx context.Context, number int) ([]string, error) {
	pull, err := r.api.Get(ctx, number)
	if err != nil {
		return nil, err
	}
	head, merge := pull.GetHead().GetSHA(), pull.GetMergeCommitSHA()
	switch {
	case !pull.GetMerged():
		return nil, errors.New("not merged")
	case pull.GetBase().GetRef() != reviewedBase:
		return nil, fmt.Errorf("merged into %s, not %s", pull.GetBase().GetRef(), reviewedBase)
	case !revisionPattern.MatchString(head) || !revisionPattern.MatchString(merge):
		return nil, fmt.Errorf("head %q or merge commit %q is not a revision", head, merge)
	}
	before, covered, err := r.landed(ctx, pull)
	if err != nil {
		return nil, err
	}
	paths, err := r.verifier.git(ctx, nil, "diff-tree", "-r", "--no-renames", "--name-only", "-z", before, merge)
	if err != nil {
		return nil, err
	}
	if err := r.approved(ctx, pull, strings.FieldsFunc(string(paths.Stdout), func(character rune) bool { return character == 0 })); err != nil {
		if automated := r.automated(ctx, pull, before, merge); automated != nil {
			return nil, errors.Join(err, automated)
		}
	}
	return covered, nil
}

func (r *reviewGate) automated(ctx context.Context, pull *github.PullRequest, before, merge string) error {
	author := pull.GetUser()
	var content func(context.Context, *github.PullRequest, string, string) error
	switch {
	case author.GetType() != botAccount:
		return fmt.Errorf("not an automated pull request: %s is a %s account", author.GetLogin(), author.GetType())
	case strings.EqualFold(author.GetLogin(), renovateBot):
		content = r.digestsOnly
	case strings.EqualFold(author.GetLogin(), promotionBot):
		content = r.promotion
	default:
		return fmt.Errorf("not an automated pull request: %s is not a trusted bot", author.GetLogin())
	}
	if err := r.checksPassed(ctx, pull); err != nil {
		return fmt.Errorf("not an automated pull request: %w", err)
	}
	if err := content(ctx, pull, before, merge); err != nil {
		return fmt.Errorf("not an automated pull request: %w", err)
	}
	return nil
}

func (r *reviewGate) checksPassed(ctx context.Context, pull *github.PullRequest) error {
	head := pull.GetHead().GetSHA()
	runs, err := r.api.CheckRuns(ctx, head)
	if err != nil {
		return fmt.Errorf("list the checks of the head %s: %w", head[:12], err)
	}
	planned := false
	for _, run := range runs {
		switch {
		case run.GetStatus() != "completed" || !slices.Contains([]string{"success", "skipped", "neutral"}, run.GetConclusion()):
			return fmt.Errorf("check %q on the head %s is %s/%s", run.GetName(), head[:12], run.GetStatus(), run.GetConclusion())
		case run.CompletedAt == nil || pull.MergedAt == nil || run.GetCompletedAt().After(pull.GetMergedAt().Time):
			return fmt.Errorf("check %q on the head %s did not complete before the merge", run.GetName(), head[:12])
		}
		planned = planned || (run.GetName() == planCheck && run.GetConclusion() == "success" && run.GetApp().GetSlug() == checksApp)
	}
	if !planned {
		return fmt.Errorf("no successful %q check on the head %s before the merge", planCheck, head[:12])
	}
	return nil
}

func (r *reviewGate) digestsOnly(ctx context.Context, pull *github.PullRequest, before, merge string) error {
	listed, err := r.api.Commits(ctx, pull.GetNumber())
	if err != nil {
		return fmt.Errorf("list commits: %w", err)
	}
	if len(listed) == 0 || len(listed) != pull.GetCommits() {
		return fmt.Errorf("lists %d of %d commits", len(listed), pull.GetCommits())
	}
	for _, commit := range listed {
		if !strings.EqualFold(commit.GetAuthor().GetLogin(), renovateBot) {
			return fmt.Errorf("commit %.12s is not authored by %s", commit.GetSHA(), renovateBot)
		}
	}
	changed, err := r.verifier.changedFiles(ctx, before, merge)
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		return errors.New("changes no file")
	}
	for _, name := range slices.Sorted(maps.Keys(changed)) {
		previous, err := r.verifier.git(ctx, nil, "show", before+":"+name)
		if err != nil {
			return fmt.Errorf("adds %s", name)
		}
		updated, err := r.verifier.git(ctx, nil, "show", merge+":"+name)
		if err != nil {
			return err
		}
		if !bytes.Equal(digestPattern.ReplaceAll(previous.Stdout, nil), digestPattern.ReplaceAll(updated.Stdout, nil)) {
			return fmt.Errorf("%s changes more than digests", name)
		}
	}
	return nil
}

func (r *reviewGate) promotion(ctx context.Context, pull *github.PullRequest, before, merge string) error {
	changed, err := r.verifier.changedFiles(ctx, before, merge)
	if err != nil {
		return err
	}
	data, err := r.verifier.git(ctx, nil, "show", merge+":"+ci.ChannelFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", ci.ChannelFile, err)
	}
	var channel ci.CIChannel
	if err := json.Unmarshal(data.Stdout, &channel); err != nil {
		return fmt.Errorf("%s: %w", ci.ChannelFile, err)
	}
	tree, err := r.verifier.tree(ctx, before, deploymentTrust, ci.ChannelFile)
	if err != nil {
		return err
	}
	files, err := ci.ChannelFiles(tree, channel)
	if err != nil {
		return err
	}
	expected := map[string][]byte{}
	for name, content := range files {
		if tree[name] == nil || !bytes.Equal(tree[name].Data, content) {
			expected[name] = content
		}
	}
	if err := r.verifier.matches(ctx, merge, changed, expected); err != nil {
		return fmt.Errorf("not a promotion of %s: %w", channel.Release, err)
	}
	if contained, err := r.verifier.contains(ctx, before, channel.Revision); err != nil || !contained {
		return errors.Join(fmt.Errorf("promoted revision %.12s is not on %s before the merge", channel.Revision, reviewedBase), err)
	}
	revision, err := r.api.TagRevision(ctx, channel.Release)
	if err != nil {
		return fmt.Errorf("release %s: %w", channel.Release, err)
	}
	if revision != channel.Revision {
		return fmt.Errorf("release %s is at %.12s, not the promoted %.12s", channel.Release, revision, channel.Revision)
	}
	return nil
}

func (r *reviewGate) landed(ctx context.Context, pull *github.PullRequest) (string, []string, error) {
	head, merge := pull.GetHead().GetSHA(), pull.GetMergeCommitSHA()
	commit, ok := r.commits[merge]
	if !ok {
		return "", nil, fmt.Errorf("merge commit %s is not in the verified range", merge[:12])
	}
	signature := r.webFlowSigned(ctx, merge)
	switch {
	case signature == nil && len(commit.parents) == 2:
		if commit.parents[1] != head {
			return "", nil, fmt.Errorf("merge commit %s merges %s, not the head %s", merge[:12], commit.parents[1][:12], head[:12])
		}
		if err := r.reproduces(ctx, commit.parents[0], head, merge); err != nil {
			return "", nil, err
		}
		merged, err := r.verifier.git(ctx, nil, "rev-list", head, "^"+commit.parents[0])
		return commit.parents[0], append([]string{merge}, strings.Fields(string(merged.Stdout))...), err
	case signature == nil && len(commit.parents) == 1:
		return commit.parents[0], []string{merge}, r.reproduces(ctx, commit.parents[0], head, merge)
	case signature == nil:
		return "", nil, fmt.Errorf("merge commit %s has %d parents", merge[:12], len(commit.parents))
	case errors.Is(signature, errUnsigned) && len(commit.parents) == 1:
		return r.rebased(ctx, pull)
	}
	return "", nil, fmt.Errorf("merge commit %s: %w", merge[:12], signature)
}

func (r *reviewGate) rebased(ctx context.Context, pull *github.PullRequest) (string, []string, error) {
	head, merge := pull.GetHead().GetSHA(), pull.GetMergeCommitSHA()
	if err := r.present(ctx, head); err != nil {
		return "", nil, err
	}
	count, err := r.rebasedCommits(ctx, pull)
	if err != nil {
		return "", nil, err
	}
	chain, cursor := []string{}, merge
	for len(chain) < count {
		commit := r.commits[cursor]
		if len(commit.parents) != 1 {
			return "", nil, fmt.Errorf("rebase merge of %d commits is not a linear chain in the verified range ending at %s", count, merge[:12])
		}
		chain, cursor = append(chain, cursor), commit.parents[0]
	}
	return cursor, chain, r.reproduces(ctx, cursor, head, merge)
}

func (r *reviewGate) rebasedCommits(ctx context.Context, pull *github.PullRequest) (int, error) {
	listed, err := r.api.Commits(ctx, pull.GetNumber())
	if err != nil {
		return 0, fmt.Errorf("list commits: %w", err)
	}
	if len(listed) != pull.GetCommits() {
		return 0, fmt.Errorf("lists %d of %d commits", len(listed), pull.GetCommits())
	}
	count := 0
	for _, commit := range listed {
		if !revisionPattern.MatchString(commit.GetSHA()) {
			return 0, fmt.Errorf("commit %q is not a revision", commit.GetSHA())
		}
		parents, err := r.verifier.git(ctx, nil, "rev-list", "--no-walk", "--parents", commit.GetSHA())
		if err != nil {
			return 0, err
		}
		revisions := strings.Fields(string(parents.Stdout))
		if len(revisions) != 2 {
			continue
		}
		changed, err := r.verifier.git(ctx, nil, "diff-tree", "--name-only", "-r", revisions[1], revisions[0])
		if err != nil {
			return 0, err
		}
		if len(changed.Stdout) > 0 {
			count++
		}
	}
	return count, nil
}

func (r *reviewGate) present(ctx context.Context, head string) error {
	if _, err := r.verifier.git(ctx, nil, "cat-file", "-e", head+"^{commit}"); err == nil {
		return nil
	}
	if _, err := r.verifier.git(ctx, nil, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "origin", head); err != nil {
		err = fmt.Errorf("fetch the head %s: %w", head[:12], err)
		if deployment.UnavailableOutput(err.Error()) {
			return deployment.Unavailable(err)
		}
		return err
	}
	return nil
}

func (r *reviewGate) reproduces(ctx context.Context, base, head, landed string) error {
	if err := r.present(ctx, head); err != nil {
		return err
	}
	merged, err := r.verifier.git(ctx, nil, "merge-tree", "--write-tree", "--no-messages", base, head)
	if err != nil {
		return fmt.Errorf("merge the head %s onto %s: %w", head[:12], base[:12], err)
	}
	tree, err := r.verifier.git(ctx, nil, "rev-parse", landed+"^{tree}")
	if err != nil {
		return err
	}
	if !bytes.Equal(bytes.TrimSpace(merged.Stdout), bytes.TrimSpace(tree.Stdout)) {
		return fmt.Errorf("%s differs from the head %s merged onto %s", landed[:12], head[:12], base[:12])
	}
	return nil
}

func (r *reviewGate) webFlowSigned(ctx context.Context, commit string) error {
	signature, err := r.signature(ctx, commit)
	if err != nil {
		return err
	}
	if _, err := openpgp.CheckArmoredDetachedSignature(r.keyring, bytes.NewReader(signature.payload), bytes.NewReader(signature.armor), nil); err != nil {
		return fmt.Errorf("not signed by the key in %s at the base revision: %w", webFlowKey, err)
	}
	return nil
}

func (r *reviewGate) approved(ctx context.Context, pull *github.PullRequest, paths []string) error {
	reviews, err := r.api.Reviews(ctx, pull.GetNumber())
	if err != nil {
		return fmt.Errorf("list reviews: %w", err)
	}
	decisions := map[string]*github.PullRequestReview{}
	for _, review := range reviews {
		switch review.GetState() {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
			decisions[strings.ToLower(review.GetUser().GetLogin())] = review
		}
	}
	head, author := pull.GetHead().GetSHA(), pull.GetUser().GetLogin()
	approvers, reasons := map[string]bool{}, []error{fmt.Errorf("no code owner approved the head %s before the merge", head[:12])}
	for _, login := range slices.Sorted(maps.Keys(decisions)) {
		review := decisions[login]
		switch {
		case review.GetState() != "APPROVED":
			reasons = append(reasons, fmt.Errorf("the latest review by %s is %s", login, review.GetState()))
		case review.GetCommitID() != head:
			reasons = append(reasons, fmt.Errorf("%s approved %.12s, not the head", login, review.GetCommitID()))
		case review.GetUser().GetType() != personAccount:
			reasons = append(reasons, fmt.Errorf("%s is a %s account", login, review.GetUser().GetType()))
		case strings.EqualFold(login, author):
			reasons = append(reasons, fmt.Errorf("%s authored the pull request", login))
		case review.SubmittedAt == nil || review.GetSubmittedAt().After(pull.GetMergedAt().Time):
			reasons = append(reasons, fmt.Errorf("%s approved after the merge", login))
		case !slices.ContainsFunc(r.owners, func(rule codeowners.Rule) bool { return ownedBy(rule, login) }):
			reasons = append(reasons, fmt.Errorf("%s is not a code owner in %s at %s", login, codeOwners, r.base))
		default:
			approvers[login] = true
		}
	}
	if len(approvers) == 0 {
		return errors.Join(reasons...)
	}
	for _, path := range paths {
		rule, err := r.owners.Match(path)
		if err != nil {
			return err
		}
		if rule == nil || !slices.ContainsFunc(slices.Collect(maps.Keys(approvers)), func(login string) bool { return ownedBy(*rule, login) }) {
			return fmt.Errorf("no owner of %s in %s at %s approved it", path, codeOwners, r.base)
		}
	}
	return nil
}

func ownedBy(rule codeowners.Rule, login string) bool {
	return slices.ContainsFunc(rule.Owners, func(owner codeowners.Owner) bool {
		return strings.EqualFold(owner.Value, login)
	})
}
