package reconcile

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/google/go-github/v88/github"
	"github.com/hmarr/codeowners"
)

const (
	webFlowKey    = "keys/github-web-flow.asc"
	codeOwners    = ".github/CODEOWNERS"
	reviewedBase  = "main"
	personAccount = "User"
)

type PullRequests interface {
	WithCommit(ctx context.Context, commit string) ([]*github.PullRequest, error)
	Get(ctx context.Context, number int) (*github.PullRequest, error)
	Reviews(ctx context.Context, number int) ([]*github.PullRequestReview, error)
	Commits(ctx context.Context, number int) ([]*github.RepositoryCommit, error)
}

type GitHubPullRequests struct {
	Client      *github.Client
	Owner, Name string
}

func (p GitHubPullRequests) WithCommit(ctx context.Context, commit string) ([]*github.PullRequest, error) {
	return collect(p.Client.PullRequests.ListPullRequestsWithCommitIter(ctx, p.Owner, p.Name, commit, &github.ListOptions{PerPage: 100}))
}

func (p GitHubPullRequests) Get(ctx context.Context, number int) (*github.PullRequest, error) {
	pull, _, err := p.Client.PullRequests.Get(ctx, p.Owner, p.Name, number)
	return pull, err
}

func (p GitHubPullRequests) Reviews(ctx context.Context, number int) ([]*github.PullRequestReview, error) {
	return collect(p.Client.PullRequests.ListReviewsIter(ctx, p.Owner, p.Name, number, &github.ListOptions{PerPage: 100}))
}

func (p GitHubPullRequests) Commits(ctx context.Context, number int) ([]*github.RepositoryCommit, error) {
	return collect(p.Client.PullRequests.ListCommitsIter(ctx, p.Owner, p.Name, number, &github.ListOptions{PerPage: 100}))
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
	if g.commands.PullRequests == nil {
		return nil, errors.New("no pull request API")
	}
	key, err := g.commands.git(ctx, nil, "show", g.base+":"+webFlowKey)
	if err != nil {
		return nil, fmt.Errorf("read %s at %s: %w", webFlowKey, g.base, err)
	}
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(key.Stdout))
	if err != nil {
		return nil, fmt.Errorf("%s at %s: %w", webFlowKey, g.base, err)
	}
	owners, err := g.commands.git(ctx, nil, "show", g.base+":"+codeOwners)
	if err != nil {
		return nil, fmt.Errorf("read %s at %s: %w", codeOwners, g.base, err)
	}
	ruleset, err := codeowners.ParseFile(bytes.NewReader(owners.Stdout))
	if err != nil {
		return nil, fmt.Errorf("%s at %s: %w", codeOwners, g.base, err)
	}
	review := &reviewGate{provenanceGate: g, api: g.commands.PullRequests, keyring: keyring, owners: ruleset, commits: map[string]provenanceCommit{}, merges: map[int]reviewedMerge{}}
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
	paths, err := r.commands.git(ctx, nil, "diff-tree", "-r", "--no-renames", "--name-only", "-z", before, merge)
	if err != nil {
		return nil, err
	}
	if err := r.approved(ctx, pull, strings.FieldsFunc(string(paths.Stdout), func(character rune) bool { return character == 0 })); err != nil {
		return nil, err
	}
	return covered, nil
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
		merged, err := r.commands.git(ctx, nil, "rev-list", head, "^"+commit.parents[0])
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
		parents, err := r.commands.git(ctx, nil, "rev-list", "--no-walk", "--parents", commit.GetSHA())
		if err != nil {
			return 0, err
		}
		revisions := strings.Fields(string(parents.Stdout))
		if len(revisions) != 2 {
			continue
		}
		changed, err := r.commands.git(ctx, nil, "diff-tree", "--name-only", "-r", revisions[1], revisions[0])
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
	if _, err := r.commands.git(ctx, nil, "cat-file", "-e", head+"^{commit}"); err == nil {
		return nil
	}
	if _, err := r.commands.git(ctx, nil, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "origin", head); err != nil {
		return fmt.Errorf("fetch the head %s: %w", head[:12], err)
	}
	return nil
}

func (r *reviewGate) reproduces(ctx context.Context, base, head, landed string) error {
	if err := r.present(ctx, head); err != nil {
		return err
	}
	merged, err := r.commands.git(ctx, nil, "merge-tree", "--write-tree", "--no-messages", base, head)
	if err != nil {
		return fmt.Errorf("merge the head %s onto %s: %w", head[:12], base[:12], err)
	}
	tree, err := r.commands.git(ctx, nil, "rev-parse", landed+"^{tree}")
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
