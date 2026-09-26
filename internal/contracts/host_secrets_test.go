package contracts

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"go.yaml.in/yaml/v3"
)

type sopsCreationRule struct {
	PathRegex string `yaml:"path_regex"`
	KeyGroups []struct {
		Age []string `yaml:"age"`
	} `yaml:"key_groups"`
}

func sopsRecipients(t *testing.T, path string) []string {
	t.Helper()
	var file struct {
		SOPS struct {
			Age []struct {
				Recipient string `yaml:"recipient"`
			} `yaml:"age"`
		} `yaml:"sops"`
	}
	if err := yaml.Unmarshal(read(t, path), &file); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var recipients []string
	for _, entry := range file.SOPS.Age {
		recipients = append(recipients, entry.Recipient)
	}
	slices.Sort(recipients)
	return recipients
}

func TestHostSecretsMatchTheirCreationRules(t *testing.T) {
	repository := root(t)
	var config struct {
		CreationRules []sopsCreationRule `yaml:"creation_rules"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, ".sops.yaml")), &config); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(repository, "ansible/roles/*/files/*.sops.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no host secrets: %v", err)
	}
	for _, file := range files {
		relative, err := filepath.Rel(repository, file)
		if err != nil {
			t.Fatal(err)
		}
		index := slices.IndexFunc(config.CreationRules, func(rule sopsCreationRule) bool {
			return regexp.MustCompile(rule.PathRegex).MatchString(relative)
		})
		if index < 0 {
			t.Errorf("%s matches no creation rule", relative)
			continue
		}
		var declared []string
		for _, group := range config.CreationRules[index].KeyGroups {
			declared = append(declared, group.Age...)
		}
		slices.Sort(declared)
		if encrypted := sopsRecipients(t, file); !slices.Equal(encrypted, declared) {
			t.Errorf("%s is encrypted to %v but .sops.yaml declares %v; run sops updatekeys %s", relative, encrypted, declared, relative)
		}
	}
}

func TestMonitorPlaceholdersAreEncryptedSecrets(t *testing.T) {
	repository := root(t)
	config := string(read(t, filepath.Join(repository, "ansible/roles/gatus/templates/config.yaml.j2")))
	matches := regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`).FindAllStringSubmatch(config, -1)
	if len(matches) == 0 || strings.Count(config, "$") != len(matches) {
		t.Fatalf("monitor settings use %d placeholders among %d $ signs; Gatus expands every $", len(matches), strings.Count(config, "$"))
	}
	placeholders := map[string]bool{}
	for _, match := range matches {
		placeholders[match[1]] = true
	}
	var secrets map[string]any
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, "ansible/roles/gatus/files/secrets.sops.yaml")), &secrets); err != nil {
		t.Fatal(err)
	}
	delete(secrets, "sops")
	if got, want := slices.Sorted(maps.Keys(placeholders)), slices.Sorted(maps.Keys(secrets)); !slices.Equal(got, want) {
		t.Fatalf("monitor placeholders %v differ from encrypted secrets %v", got, want)
	}
}

func TestHostSOPSIsThePinnedTool(t *testing.T) {
	var defaults struct {
		URL    string `yaml:"host_secrets_sops_url"`
		Digest string `yaml:"host_secrets_sops_sha256"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(root(t), "ansible/roles/host_secrets/defaults/main.yml")), &defaults); err != nil {
		t.Fatal(err)
	}
	if asset, ok := ci.Tool("sops"); !ok || asset.URL != defaults.URL || asset.Digest != defaults.Digest {
		t.Fatalf("hosts install SOPS %s %s, the toolchain pins %+v", defaults.URL, defaults.Digest, asset)
	}
}

func TestHostSecretsComeFromTheImportingRole(t *testing.T) {
	repository := root(t)
	files, err := filepath.Glob(filepath.Join(repository, "ansible/roles/*/tasks/*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var importers []string
	for _, file := range files {
		var tasks []struct {
			Import struct {
				Name      string `yaml:"name"`
				TasksFrom string `yaml:"tasks_from"`
			} `yaml:"ansible.builtin.import_role"`
			Vars map[string]any `yaml:"vars"`
		}
		if err := yaml.Unmarshal(read(t, file), &tasks); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		role := filepath.Base(filepath.Dir(filepath.Dir(file)))
		for _, task := range tasks {
			if task.Import.Name != "host_secrets" || task.Import.TasksFrom != "install.yml" {
				continue
			}
			importers = append(importers, role)
			source, _ := task.Vars["host_secrets_src"].(string)
			if source == "" || source != filepath.Base(source) || strings.Contains(source, "{{") {
				t.Errorf("%s passes host_secrets_src %q; name a file in its own files directory", role, source)
			} else if _, err := os.Stat(filepath.Join(repository, "ansible/roles", role, "files", source)); err != nil {
				t.Errorf("%s imports missing secrets: %v", role, err)
			}
		}
	}
	if slices.Sort(importers); !slices.Equal(importers, []string{"control_backup", "gatus", "verification_trigger"}) {
		t.Errorf("host secrets imported by %v", importers)
	}
	defaults, err := filepath.Glob(filepath.Join(repository, "ansible/roles/*/defaults/*.yml"))
	if err != nil || len(defaults) == 0 {
		t.Fatalf("no role defaults: %v", err)
	}
	for _, file := range defaults {
		if strings.Contains(string(read(t, file)), "role_path") {
			t.Errorf("%s uses role_path, which resolves to the role of whichever task reads the default", file)
		}
	}
}
