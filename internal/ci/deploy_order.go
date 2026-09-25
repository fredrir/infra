package ci

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
)

type DeploymentOrder struct {
	Schema   int    `json:"schema"`
	Image    string `json:"image"`
	Revision string `json:"revision"`
	Digest   string `json:"digest"`
	RunID    uint64 `json:"run_id"`
	Attempt  uint64 `json:"attempt"`
}

func attestedDeploymentOrders(data []byte, visibility, repository, image, digest, revision string) ([]DeploymentOrder, error) {
	if visibility != "public" && visibility != "private" {
		return nil, fmt.Errorf("unsupported deployment visibility")
	}
	var results []struct {
		VerificationResult struct {
			Signature struct {
				Certificate struct {
					RunInvocationURI string `json:"runInvocationURI"`
				} `json:"certificate"`
			} `json:"signature"`
		} `json:"verificationResult"`
		Optional map[string]any `json:"optional"`
	}
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, fmt.Errorf("decode verified deployment provenance: %w", err)
	}
	var orders []DeploymentOrder
	for _, result := range results {
		var run, attempt string
		switch visibility {
		case "public":
			prefix := "https://github.com/" + repository + "/actions/runs/"
			invocation := result.VerificationResult.Signature.Certificate.RunInvocationURI
			if !strings.HasPrefix(invocation, prefix) {
				continue
			}
			parts := strings.Split(strings.TrimPrefix(invocation, prefix), "/")
			if len(parts) != 3 || parts[1] != "attempts" {
				continue
			}
			run, attempt = parts[0], parts[2]
		case "private":
			run, _ = result.Optional["source-run-id"].(string)
			attempt, _ = result.Optional["source-run-attempt"].(string)
		}
		id, e1 := strconv.ParseUint(run, 10, 64)
		number, e2 := strconv.ParseUint(attempt, 10, 64)
		if e1 != nil || e2 != nil || id == 0 || number == 0 {
			continue
		}
		orders = append(orders, DeploymentOrder{Schema: 1, Image: image, Revision: revision, Digest: digest, RunID: id, Attempt: number})
	}
	return orders, nil
}

func compareDeploymentRuns(a, b DeploymentOrder) int {
	return cmp.Or(cmp.Compare(a.RunID, b.RunID), cmp.Compare(a.Attempt, b.Attempt))
}

func DecodeDeploymentOrder(data []byte) (DeploymentOrder, error) {
	var order DeploymentOrder
	if err := json.Unmarshal(data, &order); err != nil {
		return order, err
	}
	if order.Schema != 1 || !revisionPattern.MatchString(order.Revision) || !imageReferencePattern.MatchString(order.Image+"@"+order.Digest) || order.RunID == 0 || order.Attempt == 0 {
		return order, fmt.Errorf("invalid accepted deployment identity")
	}
	return order, nil
}

func checkDeploymentOrder(candidate, accepted DeploymentOrder) error {
	if candidate.Image != accepted.Image {
		return fmt.Errorf("deployment receipt image mismatch")
	}
	if candidate.RunID < accepted.RunID || (candidate.RunID == accepted.RunID && candidate.Attempt < accepted.Attempt) {
		return fmt.Errorf("stale deployment run %d/%d; accepted %d/%d", candidate.RunID, candidate.Attempt, accepted.RunID, accepted.Attempt)
	}
	if candidate.RunID == accepted.RunID && candidate.Revision != accepted.Revision {
		return fmt.Errorf("deployment run revision changed")
	}
	if candidate.RunID == accepted.RunID && candidate.Attempt == accepted.Attempt && candidate.Digest != accepted.Digest {
		return fmt.Errorf("deployment attempt digest changed")
	}
	return nil
}

func deploymentReceiptPath(project, image string) string {
	return path.Join(project, ".deployments", image[strings.LastIndex(image, "/")+1:]+".json")
}

func checkLocalDeploymentOrder(root fs.FS, receipt string, candidate DeploymentOrder) error {
	for _, name := range []string{path.Dir(receipt), receipt} {
		info, err := fs.Lstat(root, name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("deployment receipt must not traverse symlinks")
		}
	}
	data, err := fs.ReadFile(root, receipt)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	accepted, err := DecodeDeploymentOrder(data)
	if err != nil {
		return err
	}
	return checkDeploymentOrder(candidate, accepted)
}

func encodeDeploymentOrder(order DeploymentOrder) ([]byte, error) {
	data, err := json.MarshalIndent(order, "", "  ")
	return append(data, '\n'), err
}

func checkFetchedDeploymentOrder(ctx context.Context, runner Runner, path string, candidate DeploymentOrder) error {
	listed, err := runner.Output(ctx, "git", "ls-tree", "--name-only", "FETCH_HEAD", "--", path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(listed)) == "" {
		return nil
	}
	data, err := runner.Output(ctx, "git", "show", "FETCH_HEAD:"+path)
	if err != nil {
		return err
	}
	accepted, err := DecodeDeploymentOrder(data)
	if err != nil {
		return err
	}
	return checkDeploymentOrder(candidate, accepted)
}
