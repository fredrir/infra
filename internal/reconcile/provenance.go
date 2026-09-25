package reconcile

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing/fstest"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
	"go.yaml.in/yaml/v3"
)

const (
	adminKeys              = "keys/admin_keys"
	acknowledgementTrailer = "Provenance-Acknowledged"
	deploymentMappings     = ".github/deployments"
	deploymentTrust        = ".github/chainguard"
)

var ErrNoProvenanceBase = errors.New("no applied revision to verify commits from; apply once with --provenance-base")

type ProvenanceRange struct {
	Base     string `json:"base"`
	Revision string `json:"revision"`
	Override bool   `json:"override,omitempty"`
}

func NewProvenanceRange(applied, override, revision string) (ProvenanceRange, error) {
	switch {
	case override == "" && applied == "":
		return ProvenanceRange{}, ErrNoProvenanceBase
	case override == revision:
		return ProvenanceRange{}, fmt.Errorf("--provenance-base must be an ancestor of %s, not the revision itself", revision)
	}
	return ProvenanceRange{Base: cmp.Or(override, applied), Revision: revision, Override: override != ""}, nil
}

type provenanceCommit struct {
	hash, subject string
	parents       []string
	acknowledges  []string
}

type provenanceGate struct {
	commands *Commands
	signers  string
}

type provenanceRule func(context.Context, provenanceCommit) error

func (c *Commands) Provenance(ctx context.Context, checked ProvenanceRange) error {
	base, revision := checked.Base, checked.Revision
	switch {
	case !revisionPattern.MatchString(base):
		return fmt.Errorf("invalid provenance base %q", base)
	case base == revision:
		return nil
	}
	if _, err := c.git(ctx, nil, "cat-file", "-e", base+"^{commit}"); err != nil {
		return fmt.Errorf("provenance base %s is not in the checkout: %w", base, err)
	}
	if contained, err := c.contains(ctx, revision, base); err != nil || !contained {
		return errors.Join(fmt.Errorf("provenance base %s is not an ancestor of %s", base, revision), err)
	}
	directory, err := os.MkdirTemp(c.Work, "provenance-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	gate := provenanceGate{commands: c, signers: filepath.Join(directory, "allowed_signers")}
	if err := gate.trust(ctx, base); err != nil {
		return err
	}
	commits, err := c.provenanceCommits(ctx, base, revision)
	if err != nil {
		return err
	}
	return gate.verify(ctx, commits)
}

func (g provenanceGate) trust(ctx context.Context, base string) error {
	keys, err := g.commands.git(ctx, nil, "show", base+":"+adminKeys)
	if err != nil {
		return fmt.Errorf("read %s at %s: %w", adminKeys, base, err)
	}
	var signers strings.Builder
	for line := range strings.Lines(string(keys.Stdout)) {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			fmt.Fprintf(&signers, "admin namespaces=\"git\" %s\n", line)
		}
	}
	if signers.Len() == 0 {
		return fmt.Errorf("%s at %s lists no keys", adminKeys, base)
	}
	return os.WriteFile(g.signers, []byte(signers.String()), 0o600)
}

func (g provenanceGate) verify(ctx context.Context, commits []provenanceCommit) error {
	acknowledgers := map[string][]string{}
	rejected := map[string]error{}
	for _, commit := range commits {
		signature := g.ownerSigned(ctx, commit)
		if signature == nil {
			for _, acknowledged := range commit.acknowledges {
				acknowledgers[acknowledged] = append(acknowledgers[acknowledged], commit.hash)
			}
		} else if reason := g.withoutOwner(ctx, commit, signature); reason != nil {
			rejected[commit.hash] = reason
		}
	}
	var unverified []string
	for _, commit := range commits {
		if reason, ok := rejected[commit.hash]; ok && !g.acknowledged(ctx, commit.hash, acknowledgers[commit.hash]) {
			unverified = append(unverified, fmt.Sprintf("%s %q: %s", commit.hash[:12], commit.subject, strings.ReplaceAll(reason.Error(), "\n", "; ")))
		}
	}
	if len(unverified) > 0 {
		return fmt.Errorf("unverified commits: %s", strings.Join(unverified, " | "))
	}
	return nil
}

func (g provenanceGate) withoutOwner(ctx context.Context, commit provenanceCommit, signature error) error {
	reasons := []error{signature}
	for _, rule := range []provenanceRule{g.deployment} {
		err := rule(ctx, commit)
		if err == nil {
			return nil
		}
		reasons = append(reasons, err)
	}
	return errors.Join(reasons...)
}

func (g provenanceGate) acknowledged(ctx context.Context, commit string, acknowledgers []string) bool {
	for _, acknowledger := range acknowledgers {
		if contained, err := g.commands.contains(ctx, acknowledger, commit); err == nil && contained {
			return true
		}
	}
	return false
}

func (g provenanceGate) ownerSigned(ctx context.Context, commit provenanceCommit) error {
	object, err := g.commands.git(ctx, nil, "cat-file", "commit", commit.hash)
	if err != nil {
		return err
	}
	header, _, _ := bytes.Cut(object.Stdout, []byte("\n\n"))
	var signatures []string
	for line := range strings.Lines(string(header)) {
		if name, value, _ := strings.Cut(line, " "); strings.HasPrefix(name, "gpgsig") {
			signatures = append(signatures, name+" "+strings.TrimSpace(value))
		}
	}
	switch {
	case len(signatures) == 0:
		return errors.New("unsigned")
	case len(signatures) > 1:
		return fmt.Errorf("%d signature headers", len(signatures))
	case signatures[0] != "gpgsig -----BEGIN SSH SIGNATURE-----":
		return errors.New("not an SSH signature")
	}
	if _, err := g.commands.git(ctx, nil, "-c", "gpg.program=false", "-c", "gpg.openpgp.program=false", "-c", "gpg.x509.program=false", "-c", "gpg.ssh.program=ssh-keygen", "-c", "gpg.ssh.allowedSignersFile="+g.signers, "verify-commit", commit.hash); err != nil {
		return fmt.Errorf("SSH signature not by a key in %s at the base revision: %w", adminKeys, err)
	}
	return nil
}

func (g provenanceGate) deployment(ctx context.Context, commit provenanceCommit) error {
	if len(commit.parents) != 1 {
		return errors.New("not a deployment: merge commit")
	}
	parent := commit.parents[0]
	changed, err := g.commands.changedFiles(ctx, parent, commit.hash)
	if err != nil {
		return fmt.Errorf("not a deployment: %w", err)
	}
	var receipts []string
	for name := range changed {
		if matched, _ := path.Match("platform/projects/*/.deployments/*.json", name); matched {
			receipts = append(receipts, name)
		}
	}
	if len(receipts) != 1 {
		return fmt.Errorf("not a deployment: changes %d deployment receipts", len(receipts))
	}
	receipt, err := g.commands.git(ctx, nil, "show", commit.hash+":"+receipts[0])
	if err != nil {
		return err
	}
	order, err := ci.DecodeDeploymentOrder(receipt.Stdout)
	if err != nil {
		return fmt.Errorf("not a deployment: %s: %w", receipts[0], err)
	}
	tree, err := g.commands.tree(ctx, parent, deploymentMappings, deploymentTrust, path.Dir(path.Dir(receipts[0])))
	if err != nil {
		return fmt.Errorf("not a deployment: %w", err)
	}
	sources, err := deploymentSources(tree, order.Image)
	if err != nil {
		return fmt.Errorf("not a deployment: %w", err)
	}
	mismatches := []error{fmt.Errorf("not a deployment of %s@%s", order.Image, order.Digest)}
	for _, source := range sources {
		if err := g.deployedBy(ctx, commit.hash, changed, tree, source, order); err != nil {
			mismatches = append(mismatches, err)
			continue
		}
		return nil
	}
	return errors.Join(mismatches...)
}

func (g provenanceGate) deployedBy(ctx context.Context, commit string, changed map[string]bool, tree fstest.MapFS, source deploymentSource, order ci.DeploymentOrder) error {
	written, err := ci.DeploymentFiles(tree, source.target, order)
	if err != nil {
		return err
	}
	expected := map[string][]byte{}
	for name, data := range written {
		if tree[name] == nil || !bytes.Equal(tree[name].Data, data) {
			expected[name] = data
		}
	}
	if err := g.commands.matches(ctx, commit, changed, expected); err != nil {
		return err
	}
	attestations := g.commands.Runner
	attestations.Env = append(slices.Clone(attestations.Env), g.commands.ProvenanceEnv...)
	attestations.Stdout = nil
	attested, err := ci.VerifyDeploymentProvenance(ctx, attestations, tree, source.repositoryID, source.mapping, order.Image, order.Digest, order.Revision)
	if err != nil {
		return fmt.Errorf("%s at %s: %w", order.Image+"@"+order.Digest, order.Revision, err)
	}
	if attested != order {
		return fmt.Errorf("receipt names run %d attempt %d of %s, attested run %d attempt %d of %s", order.RunID, order.Attempt, order.Revision, attested.RunID, attested.Attempt, attested.Revision)
	}
	return nil
}

type deploymentSource struct {
	repositoryID string
	mapping      ci.DeploymentMapping
	target       ci.DeploymentTarget
}

func deploymentSources(tree fstest.MapFS, image string) ([]deploymentSource, error) {
	mappings, err := fs.Glob(tree, deploymentMappings+"/*.yaml")
	if err != nil {
		return nil, err
	}
	var sources []deploymentSource
	for _, name := range mappings {
		var mapping ci.DeploymentMapping
		if err := yaml.Unmarshal(tree[name].Data, &mapping); err != nil {
			return nil, fmt.Errorf("decode %s: %w", name, err)
		}
		if target, ok := mapping.Images[image]; ok {
			sources = append(sources, deploymentSource{repositoryID: strings.TrimSuffix(path.Base(name), ".yaml"), mapping: mapping, target: target})
		}
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("%s has no deployment mapping", image)
	}
	return sources, nil
}

func (c *Commands) matches(ctx context.Context, commit string, changed map[string]bool, expected map[string][]byte) error {
	if names := slices.Sorted(maps.Keys(changed)); !slices.Equal(names, slices.Sorted(maps.Keys(expected))) {
		return fmt.Errorf("changes %s, a deployment changes %s", strings.Join(names, ", "), strings.Join(slices.Sorted(maps.Keys(expected)), ", "))
	}
	for name, data := range expected {
		committed, err := c.git(ctx, nil, "show", commit+":"+name)
		if err != nil {
			return err
		}
		if !bytes.Equal(committed.Stdout, data) {
			return fmt.Errorf("%s differs from the deployment rewrite", name)
		}
	}
	return nil
}

func (c *Commands) changedFiles(ctx context.Context, parent, commit string) (map[string]bool, error) {
	diff, err := c.git(ctx, nil, "diff-tree", "-r", "--no-renames", "--no-commit-id", "-z", parent, commit)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(string(diff.Stdout), "\x00"), "\x00")
	changed := map[string]bool{}
	for index := 0; index+1 < len(fields); index += 2 {
		status, name := strings.Fields(fields[index]), fields[index+1]
		added := len(status) == 5 && status[4] == "A" && status[1] == "100644"
		modified := len(status) == 5 && status[4] == "M" && status[0] == ":"+status[1]
		if !added && !modified {
			return nil, fmt.Errorf("%s is not a regular file addition or content change", name)
		}
		changed[name] = true
	}
	return changed, nil
}

func (c *Commands) tree(ctx context.Context, revision string, paths ...string) (fstest.MapFS, error) {
	listing, err := c.git(ctx, nil, append([]string{"ls-tree", "-r", "-z", "--full-tree", revision, "--"}, paths...)...)
	if err != nil {
		return nil, err
	}
	var names, objects []string
	for entry := range strings.SplitSeq(strings.TrimSuffix(string(listing.Stdout), "\x00"), "\x00") {
		metadata, name, _ := strings.Cut(entry, "\t")
		fields := strings.Fields(metadata)
		if len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
			return nil, fmt.Errorf("%s at %s is not a regular file", name, revision)
		}
		names, objects = append(names, name), append(objects, fields[2])
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%s has none of %s", revision, strings.Join(paths, ", "))
	}
	blobs, err := c.git(ctx, strings.NewReader(strings.Join(objects, "\n")+"\n"), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	reader := bufio.NewReader(bytes.NewReader(blobs.Stdout))
	tree := fstest.MapFS{}
	for index, name := range names {
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != objects[index] {
			return nil, fmt.Errorf("unexpected object %q for %s", strings.TrimSpace(header), name)
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, err
		}
		data := make([]byte, size+1)
		if _, err := io.ReadFull(reader, data); err != nil {
			return nil, err
		}
		tree[name] = &fstest.MapFile{Data: data[:size], Mode: 0o644}
	}
	return tree, nil
}

func (c *Commands) provenanceCommits(ctx context.Context, base, revision string) ([]provenanceCommit, error) {
	log, err := c.git(ctx, nil, "log", "-z", "--reverse", "--topo-order", "--format=%H%x1f%P%x1f%(trailers:key="+acknowledgementTrailer+",valueonly,separator=%x20)%x1f%s", base+".."+revision)
	if err != nil {
		return nil, err
	}
	var commits []provenanceCommit
	for record := range strings.SplitSeq(strings.TrimSuffix(string(log.Stdout), "\x00"), "\x00") {
		fields := strings.SplitN(record, "\x1f", 4)
		if len(fields) != 4 || !revisionPattern.MatchString(fields[0]) {
			return nil, fmt.Errorf("unexpected commit record %q", record)
		}
		commit := provenanceCommit{hash: fields[0], parents: strings.Fields(fields[1]), subject: fields[3]}
		for _, acknowledged := range strings.Fields(fields[2]) {
			if revisionPattern.MatchString(acknowledged) {
				commit.acknowledges = append(commit.acknowledges, acknowledged)
			}
		}
		commits = append(commits, commit)
	}
	return commits, nil
}

func (c *Commands) contains(ctx context.Context, descendant, ancestor string) (bool, error) {
	unmerged, err := c.git(ctx, nil, "rev-list", "--max-count=1", ancestor, "^"+descendant)
	return err == nil && len(unmerged.Stdout) == 0, err
}

func (c *Commands) git(ctx context.Context, stdin io.Reader, args ...string) (process.Result, error) {
	execute := c.Runner.Execute
	if execute == nil {
		execute = process.Run
	}
	result, err := execute(ctx, process.Options{Name: "git", Args: args, Dir: c.Runner.Dir, Env: append(append(os.Environ(), c.Runner.Env...), "GIT_NO_REPLACE_OBJECTS=1"), Stdin: stdin})
	if err != nil {
		return result, errors.New(cmp.Or(lastLine(result.Stderr), err.Error()))
	}
	return result, nil
}

func lastLine(output []byte) string {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	return lines[len(lines)-1]
}
