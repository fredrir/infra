package pipeline

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImageRejectsMissingEscapingAndMalformedInputsBeforeConnecting(t *testing.T) {
	for _, options := range []ImageOptions{
		{Dockerfile: "../Dockerfile", CheckOnly: true},
		{Dockerfile: "/etc/passwd", CheckOnly: true},
		{Dockerfile: "missing", CheckOnly: true},
		{Dockerfile: "Dockerfile", Target: "unit tests", CheckOnly: true},
		{Dockerfile: "Dockerfile", Target: "--opt", CheckOnly: true},
	} {
		if _, err := Image(context.Background(), options); err == nil || !strings.Contains(err.Error(), "Dockerfile") {
			t.Fatalf("unsafe image inputs were not rejected before engine setup: %v", err)
		}
	}
}

func TestMeasuredImageChecksRequireReceiptsAndVerifiedBinary(t *testing.T) {
	for _, options := range []ImageOptions{
		{Dockerfile: "Dockerfile", CheckOnly: true, CheckTarget: "check-reports"},
		{Dockerfile: "Dockerfile", CheckOnly: true, CheckTarget: "check-reports", InfraBinary: "infra"},
		{Dockerfile: "Dockerfile", CheckOnly: true, TestCommand: "test true", CheckReportDir: t.TempDir()},
	} {
		if _, err := Image(context.Background(), options); err == nil || !strings.Contains(err.Error(), "measured image checks require") {
			t.Fatalf("unmeasured image check accepted: %v", err)
		}
	}
}

func TestDaggerImageBuildSelectsStageAndPreservesLiteralBuildArguments(t *testing.T) {
	root := os.Getenv("INFRA_DAGGER_IMAGE_TEST_ROOT")
	if root == "" {
		t.Skip("INFRA_DAGGER_IMAGE_TEST_ROOT enables the isolated engine integration")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	t.Chdir(work)
	if err := os.WriteFile("Dockerfile", []byte("FROM scratch AS unit-tests\nCOPY marker /marker\nARG CI_REVISION\nLABEL org.example.revision=$CI_REVISION\nFROM nonexistent.invalid/not-used AS final\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("marker", []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	options := ImageOptions{Root: root, Context: work, Dockerfile: "Dockerfile", Target: "unit-tests", Platform: "linux/amd64", BuildArgs: map[string]string{"CI_REVISION": "a b; $(literal)"}, Export: filepath.Join(work, "image.tar"), Log: io.Discard}
	if _, err := Image(ctx, options); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(options.Export)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	archive := tar.NewReader(file)
	entries := map[string][]byte{}
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if header.Size > 1<<20 {
			t.Fatal("scratch export unexpectedly large")
		}
		entries[header.Name], err = io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
	}
	var index struct{ Manifests []struct{ Digest string } }
	if err := json.Unmarshal(entries["index.json"], &index); err != nil || len(index.Manifests) != 1 {
		t.Fatalf("invalid OCI index: %v", err)
	}
	var manifest struct{ Config struct{ Digest string } }
	if err := json.Unmarshal(entries["blobs/"+strings.ReplaceAll(index.Manifests[0].Digest, ":", "/")], &manifest); err != nil {
		t.Fatal(err)
	}
	var config struct {
		Config struct{ Labels map[string]string }
	}
	if err := json.Unmarshal(entries["blobs/"+strings.ReplaceAll(manifest.Config.Digest, ":", "/")], &config); err != nil {
		t.Fatal(err)
	}
	if config.Config.Labels["org.example.revision"] != "a b; $(literal)" {
		t.Fatalf("build argument changed: %v", config.Config.Labels)
	}
	options.Export, options.ExportDirectory = "", filepath.Join(work, "reports")
	if _, err := Image(ctx, options); err != nil {
		t.Fatal(err)
	}
	if marker, err := os.ReadFile(filepath.Join(options.ExportDirectory, "marker")); err != nil || string(marker) != "fixture" {
		t.Fatalf("exported stage contents differ: %q, %v", marker, err)
	}
	options.ExportDirectory, options.CheckOnly = "", true
	if _, err := Image(ctx, options); err != nil {
		t.Fatal(err)
	}
}
