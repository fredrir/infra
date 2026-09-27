package policy

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

func TestAWSDatasetUserReachesOnlyTheMirroredPrefixes(t *testing.T) {
	root := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(root, objectStore, "mirror.sh"))
	if err != nil {
		t.Fatal(err)
	}
	var mirrored []string
	for _, match := range regexp.MustCompile(`'--include=/([a-z]+)/\*\*'`).FindAllStringSubmatch(string(script), -1) {
		mirrored = append(mirrored, match[1])
	}
	terraform, err := os.ReadFile(filepath.Join(root, "tofu/parser-aws.tf"))
	if err != nil {
		t.Fatal(err)
	}
	statement := regexp.MustCompile(`(?s)sid\s+= "ReadWriteDatasetObjects"\s+actions\s+= \["s3:GetObject", "s3:PutObject", "s3:DeleteObject"\]\s+resources\s+= \[for prefix in \[([^\]]*)\] : "\$\{data\.aws_s3_bucket\.dataset\.arn\}/\$\{prefix\}/\*"\]\s+\}`).FindSubmatch(terraform)
	if statement == nil {
		t.Fatal("platform-dataset-parser object access is not a per-prefix statement")
	}
	var granted []string
	for _, match := range regexp.MustCompile(`"([a-z]+)"`).FindAllSubmatch(statement[1], -1) {
		granted = append(granted, string(match[1]))
	}
	if len(mirrored) == 0 || !slices.Equal(slices.Sorted(slices.Values(granted)), slices.Sorted(slices.Values(mirrored))) {
		t.Fatalf("platform-dataset-parser reaches %v, the mirror syncs %v", granted, mirrored)
	}
}
