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
	var manifest struct {
		Config struct{ Digest string }
		Layers []struct{ MediaType string }
	}
	if err := json.Unmarshal(entries["blobs/"+strings.ReplaceAll(index.Manifests[0].Digest, ":", "/")], &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Layers) == 0 {
		t.Fatal("exported image has no layers")
	}
	for _, layer := range manifest.Layers {
		if layer.MediaType != "application/vnd.oci.image.layer.v1.tar+zstd" {
			t.Fatalf("layer compressed as %s, want zstd", layer.MediaType)
		}
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

func TestDaggerImageNormalizesSourceModesWithoutChangingRuntimeOrHostFiles(t *testing.T) {
	root := os.Getenv("INFRA_DAGGER_IMAGE_TEST_ROOT")
	if root == "" {
		t.Skip("INFRA_DAGGER_IMAGE_TEST_ROOT enables the isolated engine integration")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	toolchain, err := ReadToolchain(root)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	t.Chdir(work)
	write := func(path, content string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	write("Dockerfile", "FROM "+toolchain.Image+" AS runtime\nRUN mkdir /runtime && printf runtime > /runtime/private && chmod 0600 /runtime/private\nFROM scratch\nCOPY . /source/\nCOPY --from=runtime /runtime/private /runtime/private\n", 0600)
	outside := filepath.Join(work, "outside-secret")
	write(outside, "outside", 0600)
	write("infra", "injected", 0600)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, modes := range []struct {
		name             string
		file, executable os.FileMode
	}{
		{"restricted", 0600, 0700}, {"readable", 0644, 0755},
	} {
		t.Run(modes.name, func(t *testing.T) {
			source := filepath.Join(work, modes.name)
			write(filepath.Join(source, "nested/readable"), "source", modes.file)
			write(filepath.Join(source, "executable"), "#!/bin/sh\n", modes.executable)
			write(filepath.Join(source, ".env"), "root-secret", 0600)
			write(filepath.Join(source, "nested/.env.private"), "nested-secret", 0600)
			write(filepath.Join(source, ".infra-artifacts/secret"), "artifact-secret", 0600)
			if err := os.Chmod(filepath.Join(source, "nested"), modes.executable); err != nil {
				t.Fatal(err)
			}
			for name, target := range map[string]string{"alias": "executable", "outside": outside} {
				if err := os.Symlink(target, filepath.Join(source, name)); err != nil {
					t.Fatal(err)
				}
			}
			export := filepath.Join(work, "export-"+modes.name)
			if _, err := Image(ctx, ImageOptions{Root: root, Context: source, Dockerfile: "Dockerfile", InfraBinary: "infra", Platform: "linux/amd64", ExportDirectory: export, Log: io.Discard}); err != nil {
				t.Fatal(err)
			}
			for name, mode := range map[string]os.FileMode{"source/nested": 0755, "source/nested/readable": 0644, "source/executable": 0755, "source/.infra.Containerfile": 0644, "source/.infra-artifacts/infra": 0755, "runtime/private": 0600} {
				info, err := os.Stat(filepath.Join(export, name))
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != mode {
					t.Errorf("%s mode %04o, want %04o", name, info.Mode().Perm(), mode)
				}
			}
			for name, target := range map[string]string{"alias": "executable", "outside": outside} {
				actual, err := os.Readlink(filepath.Join(export, "source", name))
				if err != nil || actual != target {
					t.Errorf("symlink %s changed to %q: %v", name, actual, err)
				}
			}
			for _, excluded := range []string{".env", "nested/.env.private", ".infra-artifacts/secret"} {
				if _, err := os.Stat(filepath.Join(export, "source", excluded)); !os.IsNotExist(err) {
					t.Errorf("private build input %s exported: %v", excluded, err)
				}
			}
			for path, mode := range map[string]os.FileMode{filepath.Join(source, "nested/readable"): modes.file, filepath.Join(source, "executable"): modes.executable, "infra": 0600, outside: 0600} {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != mode {
					t.Errorf("host file %s changed: %+v %v", path, info, err)
				}
			}
		})
	}
}
