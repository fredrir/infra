package ci

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/fredrir/infra/internal/process"
)

const candidateWorkflow = `on:
  push:
    branches: [main]
    paths:
      - internal/**
      - build/projects/**
      - .github/workflows/**
`

func candidateRepository(t *testing.T) (string, func(map[string]string) string) {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) string {
		command := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		command.Dir = root
		data, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, data)
		}
		return string(data)
	}
	commit := func(files map[string]string) string {
		for path, content := range files {
			path = filepath.Join(root, path)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		git("add", "--all")
		git("commit", "--quiet", "--allow-empty", "--message", "change")
		return git("rev-parse", "HEAD")[:40]
	}
	git("init", "--quiet")
	commit(map[string]string{
		".github/workflows/ci-candidate.yml": candidateWorkflow,
		"build/projects/alpha.json":          "{}\n",
		"build/projects/alpha/checks":        "alpha\n",
		"build/projects/beta.json":           "{}\n",
		"internal/ci/ci.go":                  "package ci\n",
		"docs/guide.md":                      "guide\n",
	})
	return root, commit
}

func affectedCandidates(t *testing.T, root, base string) []string {
	t.Helper()
	projects, err := AffectedCandidates(context.Background(), process.Runner{Dir: root}, ".github/workflows/ci-candidate.yml", base)
	if err != nil {
		t.Fatal(err)
	}
	return projects
}

func TestCandidatesWithoutBaseQualifyEveryProject(t *testing.T) {
	root, _ := candidateRepository(t)
	if got := affectedCandidates(t, root, ""); !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Fatalf("got %v", got)
	}
}

func TestCandidatesFollowProjectOwnedChanges(t *testing.T) {
	for name, test := range map[string]struct {
		files map[string]string
		want  []string
	}{
		"project recipe":         {map[string]string{"build/projects/alpha/checks": "changed\n"}, []string{"alpha"}},
		"project profile":        {map[string]string{"build/projects/beta.json": "{\"changed\": true}\n"}, []string{"beta"}},
		"recipe and profile":     {map[string]string{"build/projects/alpha/checks": "changed\n", "build/projects/beta.json": "[]\n"}, []string{"alpha", "beta"}},
		"untriggered paths":      {map[string]string{"docs/guide.md": "changed\n", "platform/values.yaml": "new\n"}, []string{}},
		"untriggered and recipe": {map[string]string{"docs/guide.md": "changed\n", "build/projects/alpha/checks": "changed\n"}, []string{"alpha"}},
		"shared code":            {map[string]string{"internal/ci/ci.go": "package ci\n\nconst changed = true\n", "build/projects/alpha/checks": "changed\n"}, []string{"alpha", "beta"}},
		"shared workflow":        {map[string]string{".github/workflows/ci-candidate.yml": candidateWorkflow + "      - go.mod\n"}, []string{"alpha", "beta"}},
		"unknown project":        {map[string]string{"build/projects/gamma/checks": "gamma\n"}, []string{"alpha", "beta"}},
	} {
		t.Run(name, func(t *testing.T) {
			root, commit := candidateRepository(t)
			base := commit(nil)
			commit(test.files)
			if got := affectedCandidates(t, root, base); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
}

func TestCandidatesAccumulateEveryChangeSinceTheReleasedRevision(t *testing.T) {
	root, commit := candidateRepository(t)
	released := commit(nil)
	commit(map[string]string{"internal/ci/ci.go": "package ci\n\nconst shared = true\n"})
	commit(map[string]string{"build/projects/alpha/checks": "changed\n"})
	if got := affectedCandidates(t, root, released); !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Fatalf("an unqualified shared change was narrowed to %v", got)
	}
}

func TestCandidatesRejectUnknownBase(t *testing.T) {
	root, _ := candidateRepository(t)
	if _, err := AffectedCandidates(context.Background(), process.Runner{Dir: root}, ".github/workflows/ci-candidate.yml", "0000000000000000000000000000000000000000"); err == nil {
		t.Fatal("accepted an unknown base revision")
	}
}
