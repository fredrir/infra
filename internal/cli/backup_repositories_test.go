package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestResticRunsWithOnlyItsRepositorySettings(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "restic"), []byte("#!/bin/sh\nenv\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BACKUP_REPOSITORIES", "primary offsite")
	t.Setenv("PRIMARY_RESTIC_REPOSITORY", "s3:https://seaweedfs-hel1.object-store.svc.cluster.local:8333/restic-y")
	t.Setenv("PRIMARY_RESTIC_PASSWORD", "primary-password")
	t.Setenv("PRIMARY_AWS_SECRET_ACCESS_KEY", "primary-secret")
	t.Setenv("OFFSITE_RESTIC_REPOSITORY", "s3:s3.eu-north-1.amazonaws.com/llunde-pyparser-bucket/restic/platform/y")
	t.Setenv("OFFSITE_RESTIC_PASSWORD", "offsite-password")
	t.Setenv("OFFSITE_AWS_SECRET_ACCESS_KEY", "offsite-secret")
	t.Setenv("RESTIC_CACHE_DIR", "/cache")
	t.Setenv("RESTIC_CACERT", "/unrelated/ca.crt")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "unrelated-secret")
	t.Setenv("RESTIC_PASSWORD_FILE", "/unrelated/password")
	t.Setenv("RESTIC_REPOSITORY_FILE", "/unrelated/repository")
	t.Setenv("AWS_SESSION_TOKEN", "unrelated-token")
	t.Setenv("AWS_PROFILE", "unrelated")
	t.Setenv("AWS_CONFIG_FILE", "/unrelated/config")
	repositories, err := backupRepositories()
	if err != nil {
		t.Fatal(err)
	}
	restic := resticCommand(repositories, nil, nil)
	for _, repository := range repositories {
		output, err := restic(context.Background(), repository, "snapshots")
		if err != nil {
			t.Fatal(err)
		}
		environment := strings.Split(strings.TrimSpace(string(output)), "\n")
		for _, entry := range environment {
			if strings.HasPrefix(entry, "PRIMARY_") || strings.HasPrefix(entry, "OFFSITE_") || strings.Contains(entry, "unrelated") {
				t.Errorf("%s restic sees %s", repository.Name, strings.SplitN(entry, "=", 2)[0])
			}
		}
		password := map[string]string{"primary": "primary-password", "offsite": "offsite-password"}[repository.Name]
		if !slices.Contains(environment, "RESTIC_PASSWORD="+password) || !slices.Contains(environment, "RESTIC_CACHE_DIR=/cache") {
			t.Errorf("%s restic lacks its settings: %v", repository.Name, environment)
		}
	}
}
