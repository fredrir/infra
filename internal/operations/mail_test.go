package operations

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/process"
)

func writeFixture(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func mailFixture(t *testing.T) MailOptions {
	t.Helper()
	root := t.TempDir()
	for name, data := range map[string]string{
		"tofu/main.tf":                        "module configuration",
		"tofu/modules/platform-mail/main.tf":  "mail module",
		"tofu/.terraform.lock.hcl":            "locked providers",
		"tofu/terraform.tfstate":              "private state",
		"tofu/secrets.tfvars":                 "secret values",
		"tofu/tests/platform-mail.tftest.hcl": "mock_provider \"aws\" {}\nmock_provider \"cloudflare\" {}\nrun \"scoped_smtp_identity\" {\n  command = plan\n}\nrun \"other\" {\n  command = plan\n}\n",
	} {
		writeFixture(t, root, name, data)
	}
	return MailOptions{Root: root, ProviderDirectory: t.TempDir(), TempRoot: t.TempDir()}
}

func TestMailUsesIsolatedMockConfiguration(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "must-not-inherit")
	t.Setenv("TF_VAR_sensitive", "must-not-inherit")
	o := mailFixture(t)
	calls := 0
	providers, err := filepath.EvalSymlinks(o.ProviderDirectory)
	if err != nil {
		t.Fatal(err)
	}
	var work string
	o.Run = func(_ context.Context, p process.Options) (process.Result, error) {
		calls++
		work = strings.TrimPrefix(p.Args[0], "-chdir=")
		if work == filepath.Join(o.Root, "tofu") || p.Name != "tofu" || p.Timeout != 180*time.Second {
			t.Fatalf("unsafe invocation: %+v", p)
		}
		for _, entry := range p.Env {
			if strings.Contains(entry, "must-not-inherit") {
				t.Fatal("credential inherited")
			}
		}
		for _, name := range []string{"terraform.tfstate", "secrets.tfvars"} {
			if _, err := os.Stat(filepath.Join(work, name)); !os.IsNotExist(err) {
				t.Fatalf("copied %s", name)
			}
		}
		body, err := os.ReadFile(filepath.Join(work, "tests/platform-mail.tftest.hcl"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(body), "command = apply") != 1 || strings.Count(string(body), "command = plan") != 1 {
			t.Fatalf("unexpected tests: %s", body)
		}
		override, err := os.ReadFile(filepath.Join(work, "modules/platform-mail/test_override.tf.json"))
		if err != nil || strings.Count(string(override), `"prevent_destroy":false`) != 4 {
			t.Fatalf("override missing: %s %v", override, err)
		}
		if calls == 1 && (!strings.Contains(strings.Join(p.Args, " "), "-backend=false -lockfile=readonly -input=false") || p.Args[len(p.Args)-1] != "-plugin-dir="+providers) {
			t.Fatal(p.Args)
		}
		if calls == 2 && strings.Join(p.Args[1:], " ") != "test -filter=tests/platform-mail.tftest.hcl" {
			t.Fatal(p.Args)
		}
		return process.Result{}, nil
	}
	if err := TestMailModule(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatal("temporary module remains")
	}
	original, _ := os.ReadFile(filepath.Join(o.Root, "tofu/tests/platform-mail.tftest.hcl"))
	if strings.Contains(string(original), "command = apply") {
		t.Fatal("modified source")
	}
}

func TestMailFailureRemovesTemporaryModule(t *testing.T) {
	for _, scenario := range []string{"command", "entrypoint", "missing-mock", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			o := mailFixture(t)
			switch scenario {
			case "entrypoint":
				writeFixture(t, o.Root, "tofu/tests/platform-mail.tftest.hcl", "unexpected")
			case "missing-mock":
				writeFixture(t, o.Root, "tofu/tests/platform-mail.tftest.hcl", "run \"scoped_smtp_identity\" {\n  command = plan\n}")
			case "symlink":
				if err := os.Symlink("main.tf", filepath.Join(o.Root, "tofu/linked.tf")); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			o.Run = func(context.Context, process.Options) (process.Result, error) {
				calls++
				return process.Result{}, errors.New("command failed")
			}
			if err := TestMailModule(context.Background(), o); err == nil {
				t.Fatal("accepted failure")
			}
			if scenario != "command" && calls != 0 {
				t.Fatal("ran unsafe module")
			}
			entries, err := os.ReadDir(o.TempRoot)
			if err != nil || len(entries) != 0 {
				t.Fatalf("temporary data remains: %v %v", entries, err)
			}
		})
	}
}
