package provenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/fredrir/infra/internal/deployment"
	"github.com/fredrir/infra/internal/process"
	"github.com/google/go-github/v88/github"
)

const (
	deployedImage = "ghcr.io/fredrir/example"
	releasedImage = "ghcr.io/fredrir/web"
)

type provenanceFixture struct {
	t                            *testing.T
	root, remote, owner, visitor string
	base                         string
	attested                     map[string][]int
	attestationOutage            string
	verifications                []process.Options
	verificationsMutex           sync.Mutex
	webFlow, impostor            *openpgp.Entity
	api                          *pullRequestAPI
	pullRequests                 PullRequests
}

type provenanceTemplate struct {
	area, base        string
	webFlow, impostor *openpgp.Entity
}

var provenanceTemplates struct {
	once     sync.Once
	template *provenanceTemplate
}

func TestMain(m *testing.M) {
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	code := m.Run()
	if template := provenanceTemplates.template; template != nil {
		os.RemoveAll(template.area)
	}
	os.Exit(code)
}

func sharedProvenanceTemplate(t *testing.T) *provenanceTemplate {
	t.Helper()
	provenanceTemplates.once.Do(func() { provenanceTemplates.template = buildProvenanceTemplate(t) })
	if provenanceTemplates.template == nil {
		t.Fatal("provenance fixture template unavailable")
	}
	return provenanceTemplates.template
}

func newProvenanceFixture(t *testing.T) *provenanceFixture {
	t.Helper()
	template := sharedProvenanceTemplate(t)
	area := t.TempDir()
	copyTree(t, template.area, area)
	configuration := filepath.Join(area, "infra", ".git", "config")
	data, err := os.ReadFile(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configuration, []byte(strings.ReplaceAll(string(data), template.area, area)), 0o644); err != nil {
		t.Fatal(err)
	}
	f := provenanceFixtureIn(t, area)
	f.base, f.webFlow, f.impostor = template.base, template.webFlow, template.impostor
	return f
}

func provenanceFixtureIn(t *testing.T, area string) *provenanceFixture {
	t.Helper()
	f := &provenanceFixture{t: t, root: filepath.Join(area, "infra"), remote: filepath.Join(area, "origin.git"), owner: filepath.Join(area, "owner"), visitor: filepath.Join(area, "visitor"), attested: map[string][]int{}, api: &pullRequestAPI{}}
	server := httptest.NewServer(f.api)
	t.Cleanup(server.Close)
	client, err := github.NewClient(github.WithURLs(&server.URL, nil), github.WithAuthToken("provenance-token"))
	if err != nil {
		t.Fatal(err)
	}
	f.pullRequests = GitHubPullRequests{Client: client, Owner: "fredrir", Name: "infra"}
	return f
}

func copyTree(t *testing.T, source, destination string) {
	t.Helper()
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		target := filepath.Join(destination, strings.TrimPrefix(path, source))
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestProvenanceTemplateStartsNoBackgroundGitMaintenance(t *testing.T) {
	events := filepath.Join(t.TempDir(), "trace2.json")
	t.Setenv("GIT_TRACE2_EVENT", events)
	template := buildProvenanceTemplate(t)
	t.Cleanup(func() { os.RemoveAll(template.area) })
	data, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event struct {
			Event string   `json:"event"`
			Argv  []string `json:"argv"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Event == "child_start" && len(event.Argv) > 1 && (event.Argv[1] == "maintenance" || event.Argv[1] == "gc") {
			t.Errorf("building the provenance template started %q", strings.Join(event.Argv, " "))
		}
	}
}

func buildProvenanceTemplate(t *testing.T) *provenanceTemplate {
	t.Helper()
	area, err := os.MkdirTemp("", "provenance-template-")
	if err != nil {
		t.Fatal(err)
	}
	f := provenanceFixtureIn(t, area)
	for _, key := range []string{f.owner, f.visitor} {
		if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", filepath.Base(key), "-f", key).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v\n%s", err, output)
		}
	}
	f.run(area, "init", "--quiet", "--bare", "--initial-branch=main", f.remote)
	f.run(area, "init", "--quiet", "--initial-branch=main", f.root)
	for _, setting := range [][]string{{"maintenance.auto", "false"}, {"gc.auto", "0"}} {
		f.run(f.remote, append([]string{"config"}, setting...)...)
	}
	for _, setting := range [][]string{{"maintenance.auto", "false"}, {"gc.auto", "0"}, {"user.name", "Owner"}, {"user.email", "owner@example.invalid"}, {"gpg.format", "ssh"}, {"url." + f.remote + ".insteadOf", "https://github.com/fredrir/infra"}} {
		f.git(append([]string{"config"}, setting...)...)
	}
	f.git("remote", "add", "origin", "https://github.com/fredrir/infra")
	f.webFlow, f.impostor = openPGPKey(t, "GitHub"), openPGPKey(t, "Impostor")
	deployment := func(stage string) string {
		return fmt.Sprintf("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: %s\nspec:\n  replicas: 1\n  template:\n    spec:\n      containers:\n      - name: web\n        image: %s:latest\n", stage, deployedImage)
	}
	pinned := fmt.Sprintf("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- application.yaml\nimages:\n- name: %[1]s\n  newName: %[1]s\n  digest: sha256:%[2]s\n", deployedImage, strings.Repeat("c", 64))
	f.base = f.commit(f.owner, "Declare deployments", map[string]string{
		adminKeys:                                                  f.publicKey(f.owner),
		webFlowKey:                                                 armoredPublicKey(t, f.webFlow),
		codeOwners:                                                 "/platform/ @fredrir @helper\n/tofu/ @fredrir\n",
		".github/deployments/1.yaml":                               fmt.Sprintf("repository: fredrir/example\nvisibility: public\nimages:\n  %s:\n    path: platform/projects/example\n    mode: kustomize\n", deployedImage),
		".github/deployments/2.yaml":                               fmt.Sprintf("repository: fredrir/web\nvisibility: private\nimages:\n  %s:\n    path: platform/projects/web\n    mode: helmrelease\n    workload: web\n", releasedImage),
		".github/chainguard/deploy-1.sts.yaml":                     "claim_pattern:\n  job_workflow_sha: '^" + strings.Repeat("d", 40) + "$'\n",
		".github/chainguard/deploy-2.sts.yaml":                     "claim_pattern:\n  job_workflow_sha: '^(" + strings.Repeat("d", 40) + "|" + strings.Repeat("e", 40) + ")$'\n",
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
	if err := os.WriteFile(filepath.Join(f.remote, "objects", "info", "alternates"), []byte("../../infra/.git/objects\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.run(f.remote, "update-ref", "refs/heads/main", f.base)
	f.git("update-ref", "refs/remotes/origin/main", f.base)
	return &provenanceTemplate{area: area, base: f.base, webFlow: f.webFlow, impostor: f.impostor}
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

func (f *provenanceFixture) head() string {
	f.t.Helper()
	directory := filepath.Join(f.root, ".git")
	data, err := os.ReadFile(filepath.Join(directory, "HEAD"))
	if err != nil {
		f.t.Fatal(err)
	}
	reference, symbolic := strings.CutPrefix(strings.TrimSpace(string(data)), "ref: ")
	if !symbolic {
		return reference
	}
	if data, err := os.ReadFile(filepath.Join(directory, reference)); err == nil {
		return strings.TrimSpace(string(data))
	}
	packed, err := os.ReadFile(filepath.Join(directory, "packed-refs"))
	if err != nil {
		f.t.Fatal(err)
	}
	for line := range strings.Lines(string(packed)) {
		if hash, name, _ := strings.Cut(strings.TrimSpace(line), " "); name == reference {
			return hash
		}
	}
	f.t.Fatalf("%s does not resolve", reference)
	return ""
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
	return f.head()
}

func (f *provenanceFixture) forge(commit string, header func(string) string) string {
	f.t.Helper()
	original, message, _ := strings.Cut(f.git("cat-file", "commit", commit), "\n\n")
	command := exec.Command("git", "hash-object", "-t", "commit", "-w", "--literally", "--stdin")
	command.Dir, command.Stdin = f.root, strings.NewReader(header(original)+"\n\n"+message+"\n")
	output, err := command.Output()
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}

func (f *provenanceFixture) amend(files map[string]string) string {
	f.t.Helper()
	f.write(files)
	f.git("add", "--all")
	f.git("commit", "--quiet", "--amend", "--no-edit", "--no-gpg-sign")
	return f.head()
}

func (f *provenanceFixture) read(name string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, name))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

func (f *provenanceFixture) makeExecutable(name string) {
	f.t.Helper()
	if err := os.Chmod(filepath.Join(f.root, name), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *provenanceFixture) symlinkKeepingContent(name string) {
	f.t.Helper()
	content, path := f.read(name), filepath.Join(f.root, name)
	if err := os.Remove(path); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(content, path); err != nil {
		f.t.Fatal(err)
	}
}

func (f *provenanceFixture) attestation(ctx context.Context, options process.Options) (process.Result, error) {
	if options.Name != "gh" && options.Name != "cosign" {
		return process.Run(ctx, options)
	}
	f.verificationsMutex.Lock()
	f.verifications = append(f.verifications, options)
	f.verificationsMutex.Unlock()
	subject := strings.TrimPrefix(options.Args[slices.IndexFunc(options.Args, func(arg string) bool { return strings.Contains(arg, "ghcr.io/") })], "oci://")
	runs := f.attested[subject]
	if f.attestationOutage != "" {
		return process.Result{ExitCode: 1, Stderr: []byte(f.attestationOutage + "\n")}, errors.New(options.Name + " failed: exit status 1")
	}
	if len(runs) == 0 || (options.Name == "cosign" && !strings.Contains(strings.Join(options.Args, " "), "workflow-revision="+strings.Repeat("e", 40))) {
		return process.Result{ExitCode: 1, Stderr: []byte("Error: no matching attestations found\n")}, errors.New(options.Name + " failed: exit status 1")
	}
	var results []string
	for _, run := range runs {
		if options.Name == "cosign" {
			results = append(results, fmt.Sprintf(`{"optional":{"source-run-id":"%d","source-run-attempt":"1"}}`, run))
		} else {
			results = append(results, fmt.Sprintf(`{"verificationResult":{"signature":{"certificate":{"runInvocationURI":"https://github.com/%s/actions/runs/%d/attempts/1"}}}}`, options.Args[slices.Index(options.Args, "--repo")+1], run))
		}
	}
	return process.Result{Stdout: []byte("[" + strings.Join(results, ",") + "]")}, nil
}

func (f *provenanceFixture) deploy(image string, run int) string {
	f.t.Helper()
	repository := map[string]string{deployedImage: "1", releasedImage: "2"}[image]
	options := deployment.Options{Root: f.root, RepositoryID: repository, Revision: fmt.Sprintf("%040x", run), Image: image, Digest: fmt.Sprintf("sha256:%064x", run), Token: "token"}
	f.attested[image+"@"+options.Digest] = []int{run}
	if err := deployment.Deploy(context.Background(), process.Runner{Dir: f.root, Stdout: io.Discard, Stderr: io.Discard, Execute: f.attestation}, options, func(string) error { return nil }); err != nil {
		f.t.Fatal(err)
	}
	return f.head()
}

func (f *provenanceFixture) verify(base, head string) error {
	f.t.Helper()
	f.verifications = nil
	return (&Verifier{Runner: process.Runner{Dir: f.root, Stderr: io.Discard, Execute: f.attestation}, Work: f.t.TempDir(), PullRequests: f.pullRequests}).Verify(context.Background(), ProvenanceRange{Base: base, Revision: head})
}

type provenanceGateCase struct {
	name        string
	build       func(f *provenanceFixture) string
	unverified  string
	unavailable bool
}

func TestProvenanceGateFirstQuarter(t *testing.T) { testProvenanceGateQuarter(t, 0) }

func TestProvenanceGateSecondQuarter(t *testing.T) { testProvenanceGateQuarter(t, 1) }

func TestProvenanceGateThirdQuarter(t *testing.T) { testProvenanceGateQuarter(t, 2) }

func TestProvenanceGateFourthQuarter(t *testing.T) { testProvenanceGateQuarter(t, 3) }

func testProvenanceGateQuarter(t *testing.T, quarter int) {
	for index, test := range provenanceGateCases() {
		if index%4 != quarter {
			continue
		}
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newProvenanceFixture(t)
			err := f.verify(f.base, test.build(f))
			if test.unverified == "" && err != nil {
				t.Fatal(err)
			}
			if test.unverified != "" && (err == nil || !strings.Contains(err.Error(), "unverified commits: ") || !strings.Contains(err.Error(), test.unverified)) {
				t.Fatalf("got %v, want an unverified commit with %q", err, test.unverified)
			}
			if test.unverified != "" && ProvenanceUnavailable(err) != test.unavailable {
				t.Fatalf("unavailable = %v for %v, want %v", ProvenanceUnavailable(err), err, test.unavailable)
			}
		})
	}
}

func provenanceGateCases() []provenanceGateCase {
	return []provenanceGateCase{
		{name: "owner-signed", build: func(f *provenanceFixture) string {
			return f.commit(f.owner, "Change infrastructure", map[string]string{"tofu/main.tf": "# owner\n"})
		}},
		{name: "unsigned change", build: func(f *provenanceFixture) string {
			return f.commit("", "Change infrastructure", map[string]string{"tofu/main.tf": "# visitor\n"})
		}, unverified: "unsigned; not a deployment"},
		{name: "unknown signer", build: func(f *provenanceFixture) string {
			return f.commit(f.visitor, "Change infrastructure", map[string]string{"tofu/main.tf": "# visitor\n"})
		}, unverified: "not by a key in keys/admin_keys"},
		{name: "signer added in the range", build: func(f *provenanceFixture) string {
			f.commit(f.owner, "Trust visitor", map[string]string{adminKeys: f.publicKey(f.owner) + f.publicKey(f.visitor)})
			return f.commit(f.visitor, "Change infrastructure", map[string]string{"tofu/main.tf": "# visitor\n"})
		}, unverified: "not by a key in keys/admin_keys"},
		{name: "OpenPGP signature", build: func(f *provenanceFixture) string {
			return f.forge(f.commit("", "Change infrastructure", map[string]string{"tofu/main.tf": "# visitor\n"}), func(header string) string {
				return header + "\ngpgsig -----BEGIN PGP SIGNATURE-----\n \n -----END PGP SIGNATURE-----"
			})
		}, unverified: "not an SSH signature"},
		{name: "second signature header", build: func(f *provenanceFixture) string {
			return f.forge(f.commit(f.owner, "Change infrastructure", map[string]string{"tofu/main.tf": "# owner\n"}), func(header string) string {
				_, signature, _ := strings.Cut(header, "\ngpgsig ")
				return header + "\ngpgsig-sha256 " + signature
			})
		}, unverified: "2 signature headers"},
		{name: "replaced object", build: func(f *provenanceFixture) string {
			unsigned := f.commit("", "Change infrastructure", map[string]string{"tofu/main.tf": "# visitor\n"})
			f.git("reset", "--quiet", "--hard", "HEAD^")
			f.git("replace", unsigned, f.commit(f.owner, "Change infrastructure", map[string]string{"tofu/main.tf": "# owner\n"}))
			return unsigned
		}, unverified: `"Change infrastructure": unsigned`},
		{name: "kustomize deployment", build: func(f *provenanceFixture) string { return f.deploy(deployedImage, 101) }},
		{name: "HelmRelease deployment", build: func(f *provenanceFixture) string { return f.deploy(releasedImage, 101) }},
		{name: "consecutive deployments", build: func(f *provenanceFixture) string {
			f.deploy(deployedImage, 101)
			f.deploy(releasedImage, 101)
			return f.deploy(deployedImage, 102)
		}},
		{name: "deployment that also changes resources", build: func(f *provenanceFixture) string {
			f.deploy(deployedImage, 101)
			return f.amend(map[string]string{"platform/projects/example/application/kustomization.yaml": strings.Replace(f.read("platform/projects/example/application/kustomization.yaml"), "- application.yaml", "- application.yaml\n    - https://example.invalid/remote.yaml", 1)})
		}, unverified: "application/kustomization.yaml differs from the deployment rewrite"},
		{name: "deployment with another file", build: func(f *provenanceFixture) string {
			f.deploy(deployedImage, 101)
			return f.amend(map[string]string{"platform/projects/example/namespace.yaml": "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: other\n"})
		}, unverified: "changes platform/projects/example/.deployments/example.json, platform/projects/example/application/kustomization.yaml, platform/projects/example/migration/kustomization.yaml, platform/projects/example/namespace.yaml"},
		{name: "receipt without pins", build: func(f *provenanceFixture) string {
			return f.commit("", "Deploy example", map[string]string{"platform/projects/example/.deployments/example.json": strings.ReplaceAll(f.read("platform/projects/example/.deployments/example.json"), `"run_id": 100`, `"run_id": 101`)})
		}, unverified: "a deployment changes"},
		{name: "deployment with two receipts", build: func(f *provenanceFixture) string {
			f.deploy(deployedImage, 101)
			return f.amend(map[string]string{"platform/projects/example/.deployments/second.json": f.read("platform/projects/example/.deployments/example.json")})
		}, unverified: "not a deployment: changes 2 deployment receipts"},
		{name: "deployment that makes its receipt executable", build: func(f *provenanceFixture) string {
			f.deploy(deployedImage, 101)
			f.makeExecutable("platform/projects/example/.deployments/example.json")
			return f.amend(nil)
		}, unverified: "platform/projects/example/.deployments/example.json is not a regular file addition or content change"},
		{name: "first deployment with an executable receipt", build: func(f *provenanceFixture) string {
			f.deploy(releasedImage, 101)
			f.makeExecutable("platform/projects/web/.deployments/web.json")
			return f.amend(nil)
		}, unverified: "platform/projects/web/.deployments/web.json is not a regular file addition or content change"},
		{name: "first deployment with a symlinked receipt", build: func(f *provenanceFixture) string {
			f.deploy(releasedImage, 101)
			f.symlinkKeepingContent("platform/projects/web/.deployments/web.json")
			return f.amend(nil)
		}, unverified: "platform/projects/web/.deployments/web.json is not a regular file addition or content change"},
		{name: "deployment that turns a pin into a symlink", build: func(f *provenanceFixture) string {
			f.deploy(deployedImage, 101)
			f.symlinkKeepingContent("platform/projects/example/application/kustomization.yaml")
			return f.amend(nil)
		}, unverified: "platform/projects/example/application/kustomization.yaml is not a regular file addition or content change"},
		{name: "unattested deployment", build: func(f *provenanceFixture) string {
			deployed := f.deploy(deployedImage, 101)
			clear(f.attested)
			return deployed
		}, unverified: "image provenance did not match an approved workflow revision: gh at workflow dddddddddddd: Error: no matching attestations found"},
		{name: "attestation registry outage", build: func(f *provenanceFixture) string {
			deployed := f.deploy(deployedImage, 101)
			f.attestationOutage = "Error: failed to fetch attestations: HTTP 502: Bad Gateway (https://api.github.com/repos/fredrir/example/attestations)"
			return deployed
		}, unverified: "source unavailable: image provenance did not match an approved workflow revision", unavailable: true},
		{name: "outage beside a genuinely unsigned change", build: func(f *provenanceFixture) string {
			f.commit("", "Change infrastructure", map[string]string{"tofu/main.tf": "# visitor\n"})
			deployed := f.deploy(deployedImage, 101)
			f.attestationOutage = "Error: GET https://ghcr.io/v2/fredrir/example/manifests/sha256-0: unexpected status code 503 Service Unavailable"
			return deployed
		}, unverified: "unsigned; not a deployment"},
		{name: "deployment of another run's attestation", build: func(f *provenanceFixture) string {
			deployed := f.deploy(releasedImage, 101)
			f.attested[fmt.Sprintf("%s@sha256:%064x", releasedImage, 101)] = []int{102}
			return deployed
		}, unverified: fmt.Sprintf("no attestation names run 101 attempt 1 of %040x", 101)},
		{name: "deployment attested again by a later run", build: func(f *provenanceFixture) string {
			deployed := f.deploy(deployedImage, 101)
			f.attested[fmt.Sprintf("%s@sha256:%064x", deployedImage, 101)] = []int{102, 101}
			return deployed
		}},
		{name: "rolled back deployment", build: func(f *provenanceFixture) string {
			f.deploy(deployedImage, 101)
			return f.amend(map[string]string{"platform/projects/example/.deployments/example.json": strings.ReplaceAll(f.read("platform/projects/example/.deployments/example.json"), `"run_id": 101`, `"run_id": 99`)})
		}, unverified: "stale deployment run 99/1"},
		{name: "acknowledged", build: func(f *provenanceFixture) string {
			failing := f.commit("", "Change infrastructure", map[string]string{"tofu/main.tf": "# visitor\n"})
			return f.commit(f.owner, "Accept the change\n\n"+acknowledgementTrailer+": "+failing, nil)
		}},
		{name: "acknowledged by an unsigned commit", build: func(f *provenanceFixture) string {
			failing := f.commit("", "Change infrastructure", map[string]string{"tofu/main.tf": "# visitor\n"})
			return f.commit("", "Accept the change\n\n"+acknowledgementTrailer+": "+failing, nil)
		}, unverified: `"Change infrastructure": unsigned`},
		{name: "acknowledged from a parallel branch", build: func(f *provenanceFixture) string {
			f.git("checkout", "--quiet", "-b", "side")
			failing := f.commit("", "Change infrastructure", map[string]string{"tofu/main.tf": "# visitor\n"})
			f.git("checkout", "--quiet", "main")
			f.commit(f.owner, "Accept the change\n\n"+acknowledgementTrailer+": "+failing, nil)
			f.git("-c", "user.signingkey="+f.owner, "merge", "--quiet", "--no-ff", "--gpg-sign", "--message", "Merge side", "side")
			return f.head()
		}, unverified: `"Change infrastructure": unsigned`},
		{name: "signed merge", build: func(f *provenanceFixture) string {
			f.git("checkout", "--quiet", "-b", "signed")
			f.commit(f.owner, "Change infrastructure", map[string]string{"tofu/main.tf": "# side\n"})
			f.git("checkout", "--quiet", "main")
			f.commit(f.owner, "Change platform", map[string]string{"platform/projects/example/namespace.yaml": "# main\n"})
			f.git("-c", "user.signingkey="+f.owner, "merge", "--quiet", "--no-ff", "--gpg-sign", "--message", "Merge side", "signed")
			return f.head()
		}},
		{name: "unsigned merge", build: func(f *provenanceFixture) string {
			f.git("checkout", "--quiet", "-b", "unsigned")
			f.commit(f.owner, "Change infrastructure", map[string]string{"tofu/main.tf": "# side\n"})
			f.git("checkout", "--quiet", "main")
			f.commit(f.owner, "Change platform", map[string]string{"platform/projects/example/namespace.yaml": "# main\n"})
			f.git("merge", "--quiet", "--no-ff", "--no-gpg-sign", "--message", "Merge side", "unsigned")
			return f.head()
		}, unverified: `"Merge side": unsigned; not a deployment: merge commit`},
	}
}

func TestDeploymentAttestationNamesTheMappedSource(t *testing.T) {
	f := newProvenanceFixture(t)
	f.deploy(deployedImage, 101)
	head := f.deploy(releasedImage, 101)
	if err := f.verify(f.base, head); err != nil {
		t.Fatal(err)
	}
	var commands []string
	for _, verification := range f.verifications {
		commands = append(commands, verification.Name+" "+strings.Join(verification.Args, " "))
	}
	// Approved revisions are verified concurrently; compare the private candidates in revision order.
	if len(commands) == 3 {
		slices.Sort(commands[1:])
	}
	public := fmt.Sprintf("gh attestation verify oci://%s@sha256:%064x --repo fredrir/example --signer-workflow fredrir/infra/.github/workflows/build-image.yml --signer-digest %s --source-ref refs/heads/main --source-digest %040x --format json", deployedImage, 101, strings.Repeat("d", 40), 101)
	private := fmt.Sprintf("--certificate-github-workflow-repository fredrir/web --certificate-github-workflow-sha %040x", 101)
	if len(commands) != 3 || commands[0] != public || !strings.HasPrefix(commands[1], "cosign verify") || !strings.Contains(commands[1], strings.Repeat("d", 40)) || !strings.Contains(commands[2], strings.Repeat("e", 40)) || !strings.Contains(commands[2], private) {
		t.Fatalf("attestations verified with:\n%s", strings.Join(commands, "\n"))
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

func TestProvenanceRange(t *testing.T) {
	for _, test := range []struct {
		name, applied, override string
		want                    ProvenanceRange
		err                     string
	}{
		{name: "applied", applied: "old", want: ProvenanceRange{Base: "old", Revision: "new"}},
		{name: "override", applied: "old", override: "admin", want: ProvenanceRange{Base: "admin", Revision: "new", Override: true}},
		{name: "first reconciliation", override: "admin", want: ProvenanceRange{Base: "admin", Revision: "new", Override: true}},
		{name: "no base", err: ErrNoProvenanceBase.Error()},
		{name: "override at the revision", applied: "old", override: "new", err: "must be an ancestor of new"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := NewProvenanceRange(test.applied, test.override, "new")
			if got != test.want || (test.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), test.err)) {
				t.Fatalf("range %+v, %v; want %+v, %q", got, err, test.want, test.err)
			}
		})
	}
}

func TestProvenanceOutcomeReportsOutagesSeparately(t *testing.T) {
	checked := ProvenanceRange{Base: strings.Repeat("a", 40), Revision: strings.Repeat("b", 40)}
	for name, test := range map[string]struct {
		err  error
		want string
	}{
		"verified": {want: `{"base":"` + checked.Base + `","revision":"` + checked.Revision + `"}`},
		"rejected": {err: unverifiedCommits{message: "unverified commits: unsigned"}, want: `{"base":"` + checked.Base + `","revision":"` + checked.Revision + `","error":"unverified commits: unsigned"}`},
		"outage":   {err: unverifiedCommits{message: "unverified commits: HTTP 502", unavailable: true}, want: `{"base":"` + checked.Base + `","revision":"` + checked.Revision + `","error":"unverified commits: HTTP 502","unavailable":true}`},
	} {
		data, err := json.Marshal(NewProvenanceOutcome(checked, test.err))
		if err != nil || string(data) != test.want {
			t.Errorf("%s: %s, %v; want %s", name, data, err, test.want)
		}
	}
}
