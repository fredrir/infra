package ci

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type deploymentOrder struct {
	Schema   int    `json:"schema"`
	Image    string `json:"image"`
	Revision string `json:"revision"`
	Digest   string `json:"digest"`
	RunID    uint64 `json:"run_id"`
	Attempt  uint64 `json:"attempt"`
}

func verifiedDeploymentOrder(data []byte, visibility, repository string, options DeployOptions) (deploymentOrder, error) {
	var results []struct {
		VerificationResult struct {
			Statement struct {
				Predicate struct {
					RunDetails struct {
						Metadata struct {
							InvocationID string `json:"invocationId"`
						} `json:"metadata"`
					} `json:"runDetails"`
				} `json:"predicate"`
			} `json:"statement"`
		} `json:"verificationResult"`
		Optional map[string]any `json:"optional"`
	}
	if err := json.Unmarshal(data, &results); err != nil {
		return deploymentOrder{}, fmt.Errorf("decode verified deployment provenance: %w", err)
	}
	order := deploymentOrder{Schema: 1, Image: options.Image, Revision: options.Revision, Digest: options.Digest}
	for _, result := range results {
		var run, attempt string
		switch visibility {
		case "public":
			prefix := "https://github.com/" + repository + "/actions/runs/"
			invocation := result.VerificationResult.Statement.Predicate.RunDetails.Metadata.InvocationID
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
		default:
			return deploymentOrder{}, fmt.Errorf("unsupported deployment visibility")
		}
		id, e1 := strconv.ParseUint(run, 10, 64)
		number, e2 := strconv.ParseUint(attempt, 10, 64)
		if e1 != nil || e2 != nil || id == 0 || number == 0 {
			continue
		}
		if id > order.RunID || (id == order.RunID && number > order.Attempt) {
			order.RunID, order.Attempt = id, number
		}
	}
	if order.RunID == 0 {
		return deploymentOrder{}, fmt.Errorf("verified provenance has no deployment run identity")
	}
	return order, nil
}

func decodeDeploymentOrder(data []byte, image string) (deploymentOrder, error) {
	var order deploymentOrder
	if err := json.Unmarshal(data, &order); err != nil {
		return order, err
	}
	if order.Schema != 1 || order.Image != image || !revisionPattern.MatchString(order.Revision) || !imageReferencePattern.MatchString(order.Image+"@"+order.Digest) || order.RunID == 0 || order.Attempt == 0 {
		return order, fmt.Errorf("invalid accepted deployment identity")
	}
	return order, nil
}

func checkDeploymentOrder(candidate, accepted deploymentOrder) error {
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
	return filepath.Join(project, ".deployments", image[strings.LastIndex(image, "/")+1:]+".json")
}

func checkLocalDeploymentOrder(path string, candidate deploymentOrder) error {
	for _, name := range []string{filepath.Dir(path), path} {
		info, err := os.Lstat(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("deployment receipt must not traverse symlinks")
		}
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	accepted, err := decodeDeploymentOrder(data, candidate.Image)
	if err != nil {
		return err
	}
	return checkDeploymentOrder(candidate, accepted)
}

func writeDeploymentOrder(path string, order deploymentOrder) error {
	data, err := json.MarshalIndent(order, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}

func checkFetchedDeploymentOrder(ctx context.Context, runner Runner, path string, candidate deploymentOrder) error {
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
	accepted, err := decodeDeploymentOrder(data, candidate.Image)
	if err != nil {
		return err
	}
	return checkDeploymentOrder(candidate, accepted)
}
