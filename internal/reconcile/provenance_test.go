package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

const (
	deployedImage = "ghcr.io/fredrir/example"
	releasedImage = "ghcr.io/fredrir/web"
)

type provenanceFixture struct {
	t                            *testing.T
	root, remote, owner, visitor string
	base                         string
}

func newProvenanceFixture(t *testing.T) *provenanceFixture {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	area := t.TempDir()
	f := &provenanceFixture{t: t, root: filepath.Join(area, "infra"), remote: filepath.Join(area, "origin.git"), owner: filepath.Join(area, "owner"), visitor: filepath.Join(area, "visitor")}
	for _, key := range []string{f.owner, f.visitor} {
		if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", filepath.Base(key), "-f", key).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v\n%s", err, output)
		}
	}
	f.run(area, "init", "--quiet", "--bare", "--initial-branch=main", f.remote)
	f.run(area, "init", "--quiet", "--initial-branch=main", f.root)
	for _, setting := range [][]string{{"user.name", "Owner"}, {"user.email", "owner@example.invalid"}, {"gpg.format", "ssh"}, {"url." + f.remote + ".insteadOf", "https://github.com/fredrir/infra"}} {
		f.git(append([]string{"config"}, setting...)...)
	}
	f.git("remote", "add", "origin", "https://github.com/fredrir/infra")
	deployment := func(stage string) string {
		return fmt.Sprintf("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: %s\nspec:\n  replicas: 1\n  template:\n    spec:\n      containers:\n      - name: web\n        image: %s:latest\n", stage, deployedImage)
	}
	pinned := fmt.Sprintf("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- application.yaml\nimages:\n- name: %[1]s\n  newName: %[1]s\n  digest: sha256:%[2]s\n", deployedImage, strings.Repeat("c", 64))
	f.base = f.commit(f.owner, "Declare deployments", map[string]string{
		adminKeys:                                                  f.publicKey(f.owner),
		".github/deployments/1.yaml":                               fmt.Sprintf("repository: fredrir/example\nvisibility: public\nimages:\n  %s:\n    path: platform/projects/example\n    mode: kustomize\n  %s:\n    path: platform/projects/web\n    mode: helmrelease\n    workload: web\n", deployedImage, releasedImage),
		".github/chainguard/deploy-1.sts.yaml":                     "claim_pattern:\n  job_workflow_sha: '^" + strings.Repeat("d", 40) + "$'\n",
		"platform/projects/example/kustomization.yaml":             "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- namespace.yaml\n",
		"platform/projects/example/namespace.yaml":                 "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: example\n",
		"platform/projects/example/application/application.yaml":   deployment("application"),
		"platform/projects/example/application/kustomization.yaml": pinned,
		"platform/projects/example/migration/application.yaml":     deployment("migration"),
		"platform/projects/example/migration/kustomization.yaml":   pinned,
		"platform/projects/example/.deployments/example.json":      fmt.Sprintf("{\n  \"schema\": 1,\n  \"image\": %q,\n  \"revision\": %q,\n  \"digest\": \"sha256:%s\",\n  \"run_id\": 100,\n  \"attempt\": 1\n}\n", deployedImage, strings.Repeat("a", 40), strings.Repeat("c", 64)),
		"platform/projects/web/kustomization.yaml":                 "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- release.yaml\n",
		"platform/projects/web/release.yaml":                       "apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: web\nspec:\n  values:\n    workloads:\n      web:\n        image: old\n        replicas: 0\n",
		"tofu/main.tf":                                             "\n",
	})
	return f
}

func (f *provenanceFixture) run(dir string, args ...string) string {
	f.t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func (f *provenanceFixture) git(args ...string) string {
	f.t.Helper()
	return f.run(f.root, args...)
}

func (f *provenanceFixture) publicKey(key string) string {
	f.t.Helper()
	data, err := os.ReadFile(key + ".pub")
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

func (f *provenanceFixture) write(files map[string]string) {
	f.t.Helper()
	for name, content := range files {
		path := filepath.Join(f.root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *provenanceFixture) commit(key, message string, files map[string]string) string {
	f.t.Helper()
	f.write(files)
	f.git("add", "--all")
	args := []string{"commit", "--quiet", "--allow-empty", "--message", message}
	if key != "" {
		args = append([]string{"-c", "user.signingkey=" + key}, append(args, "--gpg-sign")...)
	}
	f.git(args...)
	return f.git("rev-parse", "HEAD")
}

func (f *provenanceFixture) amend(files map[string]string) string {
	f.t.Helper()
	f.write(files)
	f.git("add", "--all")
	f.git("commit", "--quiet", "--amend", "--no-edit", "--no-gpg-sign")
	return f.git("rev-parse", "HEAD")
}

func (f *provenanceFixture) read(name string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, name))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

func (f *provenanceFixture) deploy(image string, run int) string {
	f.t.Helper()
	runner := ci.Runner{Dir: f.root, Stdout: io.Discard, Stderr: io.Discard, Execute: func(ctx context.Context, options process.Options) (process.Result, error) {
		if options.Name == "gh" {
			return process.Result{Stdout: fmt.Appendf(nil, `[{"verificationResult":{"signature":{"certificate":{"runInvocationURI":"https://github.com/fredrir/example/actions/runs/%d/attempts/1"}}}}]`, run)}, nil
		}
		return process.Run(ctx, options)
	}}
	options := ci.DeployOptions{Root: f.root, RepositoryID: "1", Revision: fmt.Sprintf("%040x", run), Image: image, Digest: fmt.Sprintf("sha256:%064x", run), Token: "token"}
	if err := ci.Deploy(context.Background(), runner, options); err != nil {
		f.t.Fatal(err)
	}
	return f.git("rev-parse", "HEAD")
}

func (f *provenanceFixture) verify(base, head string) error {
	f.t.Helper()
	return (&Commands{Runner: ci.Runner{Dir: f.root}, Work: f.t.TempDir()}).Provenance(context.Background(), base, head)
}

func TestProvenanceGate(t *testing.T) {
	f := newProvenanceFixture(t)
	for _, test := range []struct {
		name       string
		build      func() string
		unverified string
	}{
		{name: "owner-signed", build: func() string {
			return f.commit(f.owner, "Change infrastructure", map[string]string{"tofu/main.tf": "# owner\n"})
		}},
		{name: "unsigned change", build: func() string {
			return f.commit("", "Change infrastructure", map[string]string{"tofu/main.tf": "# visitor\n"})
		}, unverified: "unsigned; not a deployment"},
		{name: "unknown signer", build: func() string {
			return f.commit(f.visitor, "Change infrastructure", map[string]string{"tofu/main.tf": "# visitor\n"})
		}, unverified: "not by a key in keys/admin_keys"},
		{name: "signer added in the range", build: func() string {
			f.commit(f.owner, "Trust visitor", map[string]string{adminKeys: f.publicKey(f.owner) + f.publicKey(f.visitor)})
			return f.commit(f.visitor, "Change infrastructure", map[string]string{"tofu/main.tf": "# visitor\n"})
		}, unverified: "not by a key in keys/admin_keys"},
		{name: "OpenPGP signature", build: func() string {
			object := f.git("cat-file", "commit", f.commit("", "Change infrastructure", map[string]string{"tofu/main.tf": "# visitor\n"}))
			header, message, _ := strings.Cut(object, "\n\n")
			forged := header + "\ngpgsig -----BEGIN PGP SIGNATURE-----\n \n -----END PGP SIGNATURE-----\n\n" + message + "\n"
			command := exec.Command("git", "hash-object", "-t", "commit", "-w", "--stdin")
			command.Dir, command.Stdin = f.root, strings.NewReader(forged)
			output, err := command.Output()
			if err != nil {
				t.Fatal(err)
			}
			return strings.TrimSpace(string(output))
		}, unverified: "not an SSH signature"},
		{name: "kustomize deployment", build: func() string { return f.deploy(deployedImage, 101) }},
		{name: "HelmRelease deployment", build: func() string { return f.deploy(releasedImage, 101) }},
		{name: "consecutive deployments", build: func() string {
			f.deploy(deployedImage, 101)
			f.deploy(releasedImage, 101)
			return f.deploy(deployedImage, 102)
		}},
		{name: "deployment that also changes resources", build: func() string {
			f.deploy(deployedImage, 101)
			return f.amend(map[string]string{"platform/projects/example/application/kustomization.yaml": strings.Replace(f.read("platform/projects/example/application/kustomization.yaml"), "- application.yaml", "- application.yaml\n    - https://example.invalid/remote.yaml", 1)})
		}, unverified: "application/kustomization.yaml differs from the deployment rewrite"},
		{name: "deployment with another file", build: func() string {
			f.deploy(deployedImage, 101)
			return f.amend(map[string]string{"platform/projects/example/namespace.yaml": "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: other\n"})
		}, unverified: "changes platform/projects/example/.deployments/example.json, platform/projects/example/application/kustomization.yaml, platform/projects/example/migration/kustomization.yaml, platform/projects/example/namespace.yaml"},
		{name: "receipt without pins", build: func() string {
			return f.commit("", "Deploy example", map[string]string{"platform/projects/example/.deployments/example.json": strings.ReplaceAll(f.read("platform/projects/example/.deployments/example.json"), `"run_id": 100`, `"run_id": 101`)})
		}, unverified: "a deployment changes"},
		{name: "rolled back deployment", build: func() string {
			f.deploy(deployedImage, 101)
			return f.amend(map[string]string{"platform/projects/example/.deployments/example.json": strings.ReplaceAll(f.read("platform/projects/example/.deployments/example.json"), `"run_id": 101`, `"run_id": 99`)})
		}, unverified: "stale deployment run 99/1"},
		{name: "acknowledged", build: func() string {
			failing := f.commit("", "Change infrastructure", map[string]string{"tofu/main.tf": "# visitor\n"})
			return f.commit(f.owner, "Accept the change\n\n"+acknowledgementTrailer+": "+failing, nil)
		}},
		{name: "acknowledged by an unsigned commit", build: func() string {
			failing := f.commit("", "Change infrastructure", map[string]string{"tofu/main.tf": "# visitor\n"})
			return f.commit("", "Accept the change\n\n"+acknowledgementTrailer+": "+failing, nil)
		}, unverified: `"Change infrastructure": unsigned`},
		{name: "signed merge", build: func() string {
			f.git("checkout", "--quiet", "-b", "signed")
			f.commit(f.owner, "Change infrastructure", map[string]string{"tofu/main.tf": "# side\n"})
			f.git("checkout", "--quiet", "main")
			f.commit(f.owner, "Change platform", map[string]string{"platform/projects/example/namespace.yaml": "# main\n"})
			f.git("-c", "user.signingkey="+f.owner, "merge", "--quiet", "--no-ff", "--gpg-sign", "--message", "Merge side", "signed")
			return f.git("rev-parse", "HEAD")
		}},
		{name: "unsigned merge", build: func() string {
			f.git("checkout", "--quiet", "-b", "unsigned")
			f.commit(f.owner, "Change infrastructure", map[string]string{"tofu/main.tf": "# side\n"})
			f.git("checkout", "--quiet", "main")
			f.commit(f.owner, "Change platform", map[string]string{"platform/projects/example/namespace.yaml": "# main\n"})
			f.git("merge", "--quiet", "--no-ff", "--no-gpg-sign", "--message", "Merge side", "unsigned")
			return f.git("rev-parse", "HEAD")
		}, unverified: `"Merge side": unsigned; not a deployment: merge commit`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.git("checkout", "--quiet", "main")
			f.git("reset", "--quiet", "--hard", f.base)
			f.git("push", "--quiet", "--force", "origin", "HEAD:main")
			err := f.verify(f.base, test.build())
			if test.unverified == "" && err != nil {
				t.Fatal(err)
			}
			if test.unverified != "" && (err == nil || !strings.Contains(err.Error(), "unverified commits: ") || !strings.Contains(err.Error(), test.unverified)) {
				t.Fatalf("got %v, want an unverified commit with %q", err, test.unverified)
			}
		})
	}
}

func TestProvenanceBase(t *testing.T) {
	f := newProvenanceFixture(t)
	head := f.commit(f.owner, "Change infrastructure", map[string]string{"tofu/main.tf": "# owner\n"})
	f.git("checkout", "--quiet", "--orphan", "unrelated")
	unrelated := f.commit(f.owner, "Unrelated", nil)
	for _, test := range []struct {
		name, base, want string
	}{
		{name: "missing", want: ErrNoProvenanceBase.Error()},
		{name: "invalid", base: "HEAD", want: "invalid provenance base"},
		{name: "unknown", base: strings.Repeat("e", 40), want: "is not in the checkout"},
		{name: "not an ancestor", base: unrelated, want: "is not an ancestor of " + head},
		{name: "current", base: head},
		{name: "applied", base: f.base},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := f.verify(test.base, head)
			if (test.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("got %v, want %q", err, test.want)
			}
		})
	}
}

func TestProvenanceGatesApplyBeforePlanning(t *testing.T) {
	for _, test := range []struct {
		name, override, want string
		fail                 bool
	}{
		{name: "applied base", want: "old..new"},
		{name: "override", override: "admin", want: "admin..new"},
		{name: "unverified", want: "old..new", fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &memoryStore{status: Status{Desired: "old", Applied: "old"}}
			ops := &fakeOps{selection: All()}
			if test.fail {
				ops.fail = "provenance"
			}
			err := (Reconciler{Store: store, Ops: ops, ProvenanceBase: test.override}).Apply(context.Background(), false)
			if len(ops.provenance) != 1 || ops.provenance[0] != test.want {
				t.Fatalf("provenance verified %v, want %s", ops.provenance, test.want)
			}
			if !test.fail {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !strings.HasPrefix(fmt.Sprint(err), "provenance: unverified commits") || len(ops.calls) != 0 || store.status.Stage != "provenance" || store.status.Failure == "" || store.status.Applied != "old" {
				t.Fatalf("unverified commits reached %v with %+v: %v", ops.calls, store.status, err)
			}
		})
	}
	ops := &fakeOps{selection: All(), fail: "provenance", failure: ErrNoProvenanceBase}
	if err := (Reconciler{Store: &memoryStore{}, Ops: ops}).Apply(context.Background(), false); !errors.Is(err, ErrNoProvenanceBase) || ops.provenance[0] != "..new" {
		t.Fatalf("first reconciliation without a base returned %v after %v", err, ops.provenance)
	}
}
