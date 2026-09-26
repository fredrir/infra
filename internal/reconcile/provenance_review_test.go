package reconcile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/google/go-github/v88/github"
)

const (
	renovate = "renovate[bot]"
	owner    = "fredrir"
	helper   = "helper"
)

var mergedAt = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

type pullRequestAPI struct {
	pulls        map[int]*github.PullRequest
	reviews      map[int][]*github.PullRequestReview
	associations map[string][]int
	unavailable  string
}

func (a *pullRequestAPI) reset() {
	a.pulls, a.reviews, a.associations, a.unavailable = map[int]*github.PullRequest{}, map[int][]*github.PullRequestReview{}, map[string][]int{}, ""
}

func (a *pullRequestAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path, found := strings.CutPrefix(r.URL.Path, "/repos/fredrir/infra/")
	if !found || r.Method != http.MethodGet || (a.unavailable != "" && strings.Contains(r.URL.Path, a.unavailable)) {
		http.Error(w, `{"message":"unavailable"}`, http.StatusBadGateway)
		return
	}
	var body any
	switch parts := strings.Split(path, "/"); {
	case len(parts) == 3 && parts[0] == "commits" && parts[2] == "pulls":
		listed := []*github.PullRequest{}
		for _, number := range a.associations[parts[1]] {
			listed = append(listed, &github.PullRequest{Number: github.Ptr(number), MergeCommitSHA: a.pulls[number].MergeCommitSHA})
		}
		body = listed
	case len(parts) >= 2 && parts[0] == "pulls":
		number, _ := strconv.Atoi(parts[1])
		body = a.pulls[number]
		if len(parts) == 3 && parts[2] == "reviews" {
			body = append([]*github.PullRequestReview{}, a.reviews[number]...)
		}
	}
	if body == nil || body == (*github.PullRequest)(nil) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func openPGPKey(t *testing.T, name string) *openpgp.Entity {
	t.Helper()
	entity, err := openpgp.NewEntity(name, "", "noreply@example.invalid", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	return entity
}

func armoredPublicKey(t *testing.T, entity *openpgp.Entity) string {
	t.Helper()
	var key bytes.Buffer
	writer, err := armor.Encode(&key, openpgp.PublicKeyType, nil)
	if err == nil {
		err = entity.Serialize(writer)
	}
	if err == nil {
		err = writer.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	return key.String() + "\n"
}

func (f *provenanceFixture) object(commit string) []byte {
	f.t.Helper()
	command := exec.Command("git", "cat-file", "commit", commit)
	command.Dir = f.root
	object, err := command.Output()
	if err != nil {
		f.t.Fatal(err)
	}
	return object
}

func (f *provenanceFixture) sign(commit string, key *openpgp.Entity) string {
	f.t.Helper()
	object := f.object(commit)
	var signature bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&signature, key, bytes.NewReader(object), nil); err != nil {
		f.t.Fatal(err)
	}
	header, message, _ := bytes.Cut(object, []byte("\n\n"))
	signed := string(header) + "\ngpgsig " + strings.ReplaceAll(strings.TrimSuffix(signature.String(), "\n"), "\n", "\n ") + "\n\n" + string(message)
	command := exec.Command("git", "hash-object", "-t", "commit", "-w", "--stdin")
	command.Dir, command.Stdin = f.root, strings.NewReader(signed)
	output, err := command.Output()
	if err != nil {
		f.t.Fatal(err)
	}
	hash := strings.TrimSpace(string(output))
	f.git("reset", "--quiet", "--hard", hash)
	return hash
}

func (f *provenanceFixture) pullRequest(number int, changes ...map[string]string) (string, []string) {
	f.t.Helper()
	f.git("checkout", "--quiet", "-b", fmt.Sprintf("pull/%d", number))
	var commits []string
	for index, files := range changes {
		commits = append(commits, f.commit("", fmt.Sprintf("Change %d of #%d", index+1, number), files))
	}
	f.git("push", "--quiet", "origin", fmt.Sprintf("HEAD:refs/pull/%d/head", number))
	f.git("checkout", "--quiet", "main")
	return commits[len(commits)-1], commits
}

func (f *provenanceFixture) mergeCommit(number int, head string) string {
	f.t.Helper()
	f.git("merge", "--quiet", "--no-ff", "--no-gpg-sign", "--message", fmt.Sprintf("Merge pull request #%d", number), head)
	return f.git("rev-parse", "HEAD")
}

func (f *provenanceFixture) squash(number int, head string) string {
	f.t.Helper()
	f.git("merge", "--quiet", "--squash", head)
	f.git("commit", "--quiet", "--no-gpg-sign", "--message", fmt.Sprintf("Squashed pull request #%d", number))
	return f.git("rev-parse", "HEAD")
}

func (f *provenanceFixture) rebase(commits ...string) string {
	f.t.Helper()
	f.git(append([]string{"-c", "user.name=GitHub", "-c", "user.email=noreply@github.com", "cherry-pick", "--allow-empty"}, commits...)...)
	return f.git("rev-parse", "HEAD")
}

func (f *provenanceFixture) forget(number int) {
	f.t.Helper()
	f.git("branch", "--quiet", "-D", fmt.Sprintf("pull/%d", number))
	f.git("reflog", "expire", "--expire=now", "--all")
	f.git("gc", "--quiet", "--prune=now")
}

func (f *provenanceFixture) merged(number int, author, head, merge string, commits int, reviews ...*github.PullRequestReview) *github.PullRequest {
	kind := map[bool]string{true: "Bot", false: personAccount}[strings.HasSuffix(author, "[bot]")]
	pull := &github.PullRequest{
		Number:         github.Ptr(number),
		State:          github.Ptr("closed"),
		Merged:         github.Ptr(true),
		MergedAt:       &github.Timestamp{Time: mergedAt},
		MergeCommitSHA: github.Ptr(merge),
		Commits:        github.Ptr(commits),
		User:           &github.User{Login: github.Ptr(author), Type: github.Ptr(kind)},
		Head:           &github.PullRequestBranch{SHA: github.Ptr(head), Ref: github.Ptr(fmt.Sprintf("pull-%d", number))},
		Base:           &github.PullRequestBranch{Ref: github.Ptr(reviewedBase)},
	}
	f.api.pulls[number], f.api.reviews[number] = pull, reviews
	f.associate(number, merge)
	return pull
}

func (f *provenanceFixture) associate(number int, commits ...string) {
	for _, commit := range commits {
		f.api.associations[commit] = append(f.api.associations[commit], number)
	}
}

func review(login, state, commit string) *github.PullRequestReview {
	return &github.PullRequestReview{
		User:        &github.User{Login: github.Ptr(login), Type: github.Ptr(personAccount)},
		State:       github.Ptr(state),
		CommitID:    github.Ptr(commit),
		SubmittedAt: &github.Timestamp{Time: mergedAt.Add(-time.Hour)},
	}
}

func approval(login, commit string) *github.PullRequestReview {
	return review(login, "APPROVED", commit)
}

func TestReviewedMerges(t *testing.T) {
	f := newProvenanceFixture(t)
	infrastructure := map[string]string{"tofu/main.tf": "# renovate\n"}
	platform := map[string]string{"platform/projects/example/namespace.yaml": "# renovate\n"}
	release := map[string]string{"platform/projects/web/release.yaml": "# renovate\n"}
	advance := func() string {
		return f.commit(f.owner, "Change platform", map[string]string{"platform/projects/web/kustomization.yaml": "# owner\n"})
	}
	for _, test := range []struct {
		name       string
		build      func() string
		unverified []string
	}{
		{name: "approved merge commit with its commits", build: func() string {
			head, _ := f.pullRequest(7, infrastructure, platform)
			advance()
			merge := f.sign(f.mergeCommit(7, head), f.webFlow)
			f.merged(7, renovate, head, merge, 2, approval(owner, head))
			return merge
		}},
		{name: "approved squash merge", build: func() string {
			head, _ := f.pullRequest(8, infrastructure, platform)
			advance()
			merge := f.sign(f.squash(8, head), f.webFlow)
			f.merged(8, renovate, head, merge, 2, approval(owner, head))
			return merge
		}},
		{name: "approved rebase merge of three commits", build: func() string {
			head, commits := f.pullRequest(9, infrastructure, platform, release)
			advance()
			merge := f.rebase(commits...)
			f.forget(9)
			if _, err := f.try("cat-file", "-e", head+"^{commit}"); err == nil {
				t.Fatal("the approved head is still in the checkout")
			}
			f.merged(9, renovate, head, merge, 3, approval(owner, head))
			f.associate(9, f.git("rev-parse", merge+"~1"), f.git("rev-parse", merge+"~2"))
			return merge
		}},
		{name: "approved merges by other code owners of their paths", build: func() string {
			head, _ := f.pullRequest(10, platform)
			f.merged(10, renovate, head, f.sign(f.mergeCommit(10, head), f.webFlow), 1, approval(helper, head))
			head, _ = f.pullRequest(11, infrastructure)
			merge := f.sign(f.squash(11, head), f.webFlow)
			f.merged(11, renovate, head, merge, 1, approval(owner, head))
			return merge
		}},
		{name: "web-flow commit without a pull request", build: func() string {
			return f.sign(f.commit("", "Create tofu/main.tf", infrastructure), f.webFlow)
		}, unverified: []string{"no pull request merged it"}},
		{name: "owner-authored merge without approval", build: func() string {
			head, _ := f.pullRequest(12, infrastructure)
			merge := f.sign(f.mergeCommit(12, head), f.webFlow)
			f.merged(12, owner, head, merge, 1)
			return merge
		}, unverified: []string{"no code owner approved the head", "accept them with a Provenance-Acknowledged trailer in an owner-signed commit, or push the change as owner-signed commits"}},
		{name: "stale approval", build: func() string {
			head, commits := f.pullRequest(13, platform, infrastructure)
			merge := f.sign(f.mergeCommit(13, head), f.webFlow)
			f.merged(13, renovate, head, merge, 2, approval(owner, commits[0]))
			return merge
		}, unverified: []string{"fredrir approved [0-9a-f]{12}, not the head"}},
		{name: "approval by a reviewer outside CODEOWNERS", build: func() string {
			head, _ := f.pullRequest(14, platform)
			merge := f.sign(f.squash(14, head), f.webFlow)
			f.merged(14, renovate, head, merge, 1, approval("outsider", head))
			return merge
		}, unverified: []string{"outsider is not a code owner in .github/CODEOWNERS"}},
		{name: "approval by a code owner of other paths", build: func() string {
			head, _ := f.pullRequest(15, platform, infrastructure)
			merge := f.sign(f.mergeCommit(15, head), f.webFlow)
			f.merged(15, renovate, head, merge, 2, approval(helper, head))
			return merge
		}, unverified: []string{"no owner of tofu/main.tf in .github/CODEOWNERS"}},
		{name: "approval by a bot", build: func() string {
			head, _ := f.pullRequest(16, platform)
			merge := f.sign(f.squash(16, head), f.webFlow)
			bot := approval(helper, head)
			bot.User.Type = github.Ptr("Bot")
			f.merged(16, renovate, head, merge, 1, bot)
			return merge
		}, unverified: []string{"helper is a Bot account"}},
		{name: "approval by the author", build: func() string {
			head, _ := f.pullRequest(17, platform)
			merge := f.sign(f.squash(17, head), f.webFlow)
			f.merged(17, helper, head, merge, 1, approval(helper, head))
			return merge
		}, unverified: []string{"helper authored the pull request"}},
		{name: "approval after an admin-bypass merge", build: func() string {
			head, _ := f.pullRequest(18, platform)
			merge := f.sign(f.squash(18, head), f.webFlow)
			late := approval(owner, head)
			late.SubmittedAt = &github.Timestamp{Time: mergedAt.Add(time.Minute)}
			f.merged(18, renovate, head, merge, 1, late)
			return merge
		}, unverified: []string{"fredrir approved after the merge"}},
		{name: "approval without a submission time", build: func() string {
			head, _ := f.pullRequest(37, platform)
			merge := f.sign(f.squash(37, head), f.webFlow)
			unsubmitted := approval(owner, head)
			unsubmitted.SubmittedAt = nil
			f.merged(37, renovate, head, merge, 1, unsubmitted)
			return merge
		}, unverified: []string{"fredrir approved after the merge"}},
		{name: "change to a path without a code owner", build: func() string {
			head, _ := f.pullRequest(38, platform, map[string]string{"ansible/site.yml": "# renovate\n"})
			merge := f.sign(f.mergeCommit(38, head), f.webFlow)
			f.merged(38, renovate, head, merge, 2, approval(owner, head))
			return merge
		}, unverified: []string{"no owner of ansible/site.yml in .github/CODEOWNERS"}},
		{name: "merged pull request without a merge commit", build: func() string {
			head, _ := f.pullRequest(39, platform)
			merge := f.sign(f.squash(39, head), f.webFlow)
			f.merged(39, renovate, head, merge, 1, approval(owner, head)).MergeCommitSHA = github.Ptr("")
			return merge
		}, unverified: []string{`merge commit "" is not a revision`}},
		{name: "changes requested after the approval", build: func() string {
			head, _ := f.pullRequest(19, platform)
			merge := f.sign(f.squash(19, head), f.webFlow)
			f.merged(19, renovate, head, merge, 1, approval(owner, head), review(owner, "CHANGES_REQUESTED", head))
			return merge
		}, unverified: []string{"the latest review by fredrir is CHANGES_REQUESTED"}},
		{name: "unmerged pull request", build: func() string {
			head, _ := f.pullRequest(20, platform)
			merge := f.sign(f.mergeCommit(20, head), f.webFlow)
			pull := f.merged(20, renovate, head, merge, 1, approval(owner, head))
			pull.Merged, pull.MergedAt, pull.State = github.Ptr(false), nil, github.Ptr("open")
			return merge
		}, unverified: []string{"pull request #20: not merged"}},
		{name: "pull request merged into another base", build: func() string {
			head, _ := f.pullRequest(21, platform)
			merge := f.sign(f.mergeCommit(21, head), f.webFlow)
			f.merged(21, renovate, head, merge, 1, approval(owner, head)).Base.Ref = github.Ptr("develop")
			return merge
		}, unverified: []string{"merged into develop, not main"}},
		{name: "commit pull requests API error", build: func() string {
			head, _ := f.pullRequest(22, platform)
			merge := f.sign(f.mergeCommit(22, head), f.webFlow)
			f.merged(22, renovate, head, merge, 1, approval(owner, head))
			f.api.unavailable = "/commits/"
			return merge
		}, unverified: []string{"list its pull requests", "502"}},
		{name: "pull request API error", build: func() string {
			head, _ := f.pullRequest(23, platform)
			merge := f.sign(f.squash(23, head), f.webFlow)
			f.merged(23, renovate, head, merge, 1, approval(owner, head))
			f.api.unavailable = "/pulls/23"
			return merge
		}, unverified: []string{"pull request #23: GET", "502"}},
		{name: "review API error", build: func() string {
			head, _ := f.pullRequest(24, platform)
			merge := f.sign(f.squash(24, head), f.webFlow)
			f.merged(24, renovate, head, merge, 1, approval(owner, head))
			f.api.unavailable = "/reviews"
			return merge
		}, unverified: []string{"pull request #24: list reviews", "502"}},
		{name: "merge of another head", build: func() string {
			head, commits := f.pullRequest(25, platform, release)
			advance()
			merge := f.sign(f.mergeCommit(25, head), f.webFlow)
			f.merged(25, renovate, commits[0], merge, 1, approval(owner, commits[0]))
			return merge
		}, unverified: []string{"merges [0-9a-f]{12}, not the head [0-9a-f]{12}"}},
		{name: "unsigned merge commit", build: func() string {
			head, _ := f.pullRequest(26, platform)
			advance()
			merge := f.mergeCommit(26, head)
			f.merged(26, renovate, head, merge, 1, approval(owner, head))
			return merge
		}, unverified: []string{"pull request #26: merge commit [0-9a-f]{12}: unsigned"}},
		{name: "merge signed by another OpenPGP key", build: func() string {
			head, _ := f.pullRequest(27, platform)
			merge := f.sign(f.squash(27, head), f.impostor)
			f.merged(27, renovate, head, merge, 1, approval(owner, head))
			return merge
		}, unverified: []string{"not signed by the key in keys/github-web-flow.asc at the base revision"}},
		{name: "web-flow key replaced in the range", build: func() string {
			f.commit(f.owner, "Trust another web-flow key", map[string]string{webFlowKey: armoredPublicKey(t, f.impostor)})
			head, _ := f.pullRequest(28, platform)
			merge := f.sign(f.squash(28, head), f.impostor)
			f.merged(28, renovate, head, merge, 1, approval(owner, head))
			return merge
		}, unverified: []string{"not signed by the key in keys/github-web-flow.asc at the base revision"}},
		{name: "code owner added in the range", build: func() string {
			f.commit(f.owner, "Add a code owner", map[string]string{codeOwners: "* @outsider\n"})
			head, _ := f.pullRequest(29, platform)
			merge := f.sign(f.squash(29, head), f.webFlow)
			f.merged(29, renovate, head, merge, 1, approval("outsider", head))
			return merge
		}, unverified: []string{"outsider is not a code owner in .github/CODEOWNERS at " + f.base}},
		{name: "merge commit outside the range", build: func() string {
			head, _ := f.pullRequest(30, platform)
			merge := f.sign(f.squash(30, head), f.webFlow)
			f.merged(30, renovate, head, f.base, 1, approval(owner, head))
			f.associate(30, merge)
			return merge
		}, unverified: []string{"merge commit " + f.base[:12] + " is not in the verified range"}},
		{name: "unsigned commit attributed to an approved pull request", build: func() string {
			foreign := f.commit("", "Change infrastructure", infrastructure)
			head, _ := f.pullRequest(31, platform)
			merge := f.sign(f.squash(31, head), f.webFlow)
			f.merged(31, renovate, head, merge, 1, approval(owner, head))
			f.associate(31, foreign)
			return merge
		}, unverified: []string{"pull request #31: does not include it"}},
		{name: "rebase merge with an interleaved foreign commit", build: func() string {
			head, commits := f.pullRequest(32, platform, release, map[string]string{"platform/projects/example/application/application.yaml": "# renovate\n"})
			f.rebase(commits[0])
			f.commit("", "Change infrastructure", infrastructure)
			merge := f.rebase(commits[1:]...)
			f.merged(32, renovate, head, merge, 3, approval(owner, head))
			f.associate(32, f.git("rev-parse", merge+"~1"), f.git("rev-parse", merge+"~2"), f.git("rev-parse", merge+"~3"))
			return merge
		}, unverified: []string{"differ from the head", `"Change infrastructure": unsigned`}},
		{name: "rebase chain through a merge commit", build: func() string {
			head, commits := f.pullRequest(33, platform, release)
			first := f.rebase(commits[0])
			f.git("reset", "--quiet", "--hard", f.git("commit-tree", first+"^{tree}", "-p", first, "-p", f.base, "-m", "Merge applied history"))
			merge := f.rebase(commits[1])
			f.merged(33, renovate, head, merge, 3, approval(owner, head))
			return merge
		}, unverified: []string{"rebase merge of 3 commits is not a linear chain in the verified range"}},
		{name: "rebased commits that change the approved content", build: func() string {
			head, commits := f.pullRequest(34, platform, release)
			f.rebase(commits...)
			merge := f.amend(infrastructure)
			f.merged(34, renovate, head, merge, 2, approval(owner, head))
			f.associate(34, f.git("rev-parse", merge+"~1"))
			return merge
		}, unverified: []string{"differ from the head"}},
		{name: "rebase merge with a conflict", build: func() string {
			head, _ := f.pullRequest(35, map[string]string{"platform/projects/web/release.yaml": "# renovate\n"})
			main := f.commit(f.owner, "Change the release", map[string]string{"platform/projects/web/release.yaml": "# owner\n"})
			conflicted, _ := f.try("merge-tree", "--write-tree", "--no-messages", main, head)
			tree, _, _ := strings.Cut(conflicted, "\n")
			f.git("reset", "--quiet", "--hard", f.git("commit-tree", tree, "-p", main, "-m", "Change 1 of #35"))
			merge := f.git("rev-parse", "HEAD")
			f.merged(35, renovate, head, merge, 1, approval(owner, head))
			return merge
		}, unverified: []string{"merge the head"}},
		{name: "rebase merge of a head that cannot be fetched", build: func() string {
			head, commits := f.pullRequest(36, platform)
			merge := f.rebase(commits...)
			f.git("push", "--quiet", "origin", "--delete", "refs/pull/36/head")
			f.run(f.remote, "gc", "--quiet", "--prune=now")
			f.forget(36)
			f.merged(36, renovate, head, merge, 1, approval(owner, head))
			return merge
		}, unverified: []string{"fetch the head"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.git("checkout", "--quiet", "main")
			f.git("reset", "--quiet", "--hard", f.base)
			f.git("push", "--quiet", "--force", "origin", "HEAD:main")
			f.api.reset()
			head := test.build()
			err := f.verify(f.base, head)
			if len(test.unverified) == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), "unverified commits: ") {
				t.Fatalf("got %v, want unverified commits", err)
			}
			for _, want := range test.unverified {
				if !regexp.MustCompile(want).MatchString(err.Error()) {
					t.Errorf("got %v, want a match of %q", err, want)
				}
			}
		})
	}
}

func (f *provenanceFixture) try(args ...string) (string, error) {
	command := exec.Command("git", args...)
	command.Dir = f.root
	output, err := command.Output()
	return strings.TrimSpace(string(output)), err
}

func TestWebFlowKeyVerifiesAGitHubMerge(t *testing.T) {
	key, err := os.ReadFile(filepath.Join("..", "..", webFlowKey))
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(key))
	if err != nil {
		t.Fatal(err)
	}
	if len(keyring) != 1 || fmt.Sprintf("%X", keyring[0].PrimaryKey.Fingerprint) != "968479A1AFF927E37D1A566BB5690EEEBB952194" {
		t.Fatalf("%s holds %d keys, not only GitHub's web-flow key", webFlowKey, len(keyring))
	}
	for _, test := range []struct {
		name, object string
		valid        bool
	}{
		{name: "merge of pull request 149", object: githubMerge, valid: true},
		{name: "altered message", object: strings.Replace(githubMerge, "bump node to 24", "bump node to 25", 1)},
		{name: "altered parent", object: strings.Replace(githubMerge, "parent 70589f4086e15b5743604193f545c534b71bc22d", "parent "+strings.Repeat("0", 40), 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			signature, err := parseCommitSignature([]byte(test.object))
			if err != nil {
				t.Fatal(err)
			}
			_, err = openpgp.CheckArmoredDetachedSignature(keyring, bytes.NewReader(signature.payload), bytes.NewReader(signature.armor), nil)
			if (err == nil) != test.valid {
				t.Fatalf("verification returned %v", err)
			}
		})
	}
}

const githubMerge = "tree 1a1ac29e2a41f304d5812e719432ce26050bca24\n" +
	"parent 70589f4086e15b5743604193f545c534b71bc22d\n" +
	"parent fa823e514c378cc05f5d91278f8e47a48d4a0b01\n" +
	"author Fredrik Hansteen <fhansteen@gmail.com> 1789911075 +0200\n" +
	"committer GitHub <noreply@github.com> 1789911075 +0200\n" +
	"gpgsig -----BEGIN PGP SIGNATURE-----\n" +
	" \n" +
	" wsFcBAABCAAQBQJqr+AjCRC1aQ7uu5UhlAAAB5UQAKvvsFisHBvO2PleogZXCukM\n" +
	" 8g3YxPn1SvrzywPVhN7ujF0XwrsSkhxYT8XFOnwioQiEFu4TGwkWP8os19tDah8P\n" +
	" njMmTZvUrDvB5q37gYhDTjmvO0zN+WmkFU4efNvmzxIxBL5Zt383VGA1aZuLLftf\n" +
	" zz3D37bQQtqvx3JdMxVDEUnj8dpFQQDoEdT3V6B24d5hSC0K8C5/+GuA87FR0Ln9\n" +
	" g875K2fKS7iYuQQDbvtybQ0OYM0McnEzXy5+24ebedGaH0rU9RtoGbKDUIphhuBc\n" +
	" 9sfJkWAvGb8D5wqiH6TMQ6b6z4vMxfsoC1fO3xmahrucqgB5itzyelcfNMcCELtT\n" +
	" 7PZ1MGNatzar8cOkLcb+6DeIBWDHkFfAMOxT7JCZw2f2N5u6mTHeOC1EbiMQAala\n" +
	" QVvkcR01Gj5An3ceLyWtCbw5WSmNOcNI7eEqPSyt4OHJ3ti/O4C2dAdui92la8Bv\n" +
	" X2olMigPxORo+uyCl1U5f0+71xkYe2MX9fiwV6fEuwWs1l8nV8buFqvTBvAVuvWY\n" +
	" FMGLZU12Ej5dxcylws6JI7XwCcAGxKTSYHcGE8ZFhiktL+gCfaLhKpIUh+5fMhu7\n" +
	" dwQR9U7bT0Rdpn65nGOLIaMGy7W8fT1yiHzdYTtJtOEFBtHfLtRPlbz/eDKcvVVb\n" +
	" J0kE+zAdK1yFbyu2oJfe\n" +
	" =HIT9\n" +
	" -----END PGP SIGNATURE-----\n" +
	" \n" +
	"\n" +
	"Merge pull request #149 from fredrir/bump-node\n" +
	"\n" +
	"bump node to 24"
