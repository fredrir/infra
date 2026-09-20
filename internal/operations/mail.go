package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/process"
)

type Runner func(context.Context, process.Options) (process.Result, error)
type MailOptions struct {
	Root, ProviderDirectory, TempRoot string
	Run                               Runner
	Log                               io.Writer
}

func TestMailModule(ctx context.Context, o MailOptions) (err error) {
	providers, e := filepath.EvalSymlinks(o.ProviderDirectory)
	if e != nil {
		return e
	}
	providers, e = filepath.Abs(providers)
	if e != nil {
		return e
	}
	st, e := os.Stat(providers)
	if e != nil {
		return e
	}
	if !st.IsDir() {
		return errors.New("provider directory required")
	}
	if o.Run == nil {
		o.Run = process.Run
	}
	work, e := os.MkdirTemp(o.TempRoot, "infra-mail-test-")
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, os.RemoveAll(work)) }()
	source := filepath.Join(o.Root, "tofu")
	paths, e := filepath.Glob(filepath.Join(source, "*.tf"))
	if e != nil {
		return e
	}
	if len(paths) == 0 {
		return errors.New("OpenTofu configuration missing")
	}
	e = filepath.WalkDir(filepath.Join(source, "modules"), func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.IsDir() && strings.HasSuffix(path, ".tf") {
			paths = append(paths, path)
		}
		return nil
	})
	if e != nil {
		return e
	}
	paths = append(paths, filepath.Join(source, ".terraform.lock.hcl"))
	for _, path := range paths {
		rel, e := filepath.Rel(source, path)
		if e != nil || !filepath.IsLocal(rel) {
			return errors.New("invalid module path")
		}
		st, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("regular configuration required: %s", rel)
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		dest := filepath.Join(work, rel)
		if e = os.MkdirAll(filepath.Dir(dest), 0700); e != nil {
			return e
		}
		if e = os.WriteFile(dest, b, 0600); e != nil {
			return e
		}
	}
	b, e := os.ReadFile(filepath.Join(source, "tests/platform-mail.tftest.hcl"))
	if e != nil {
		return e
	}
	marker := "run \"scoped_smtp_identity\" {\n  command = plan"
	if strings.Count(string(b), marker) != 1 {
		return errors.New("mail test entrypoint changed")
	}
	for _, provider := range []string{"aws", "cloudflare"} {
		if !strings.Contains(string(b), "mock_provider \""+provider+"\"") {
			return errors.New("mail test requires mocked providers")
		}
	}
	if e = os.Mkdir(filepath.Join(work, "tests"), 0700); e != nil {
		return e
	}
	text := strings.Replace(string(b), marker, strings.Replace(marker, "plan", "apply", 1), 1)
	if e = os.WriteFile(filepath.Join(work, "tests/platform-mail.tftest.hcl"), []byte(text), 0600); e != nil {
		return e
	}
	resources := map[string]any{}
	for kind, name := range map[string]string{"aws_sesv2_email_identity": "sender", "cloudflare_dns_record": "dkim", "aws_iam_policy": "sender", "aws_iam_user": "sender"} {
		resources[kind] = map[string]any{name: map[string]any{"lifecycle": map[string]bool{"prevent_destroy": false}}}
	}
	b, e = json.Marshal(map[string]any{"resource": resources})
	if e != nil {
		return e
	}
	if e = os.WriteFile(filepath.Join(work, "modules/platform-mail/test_override.tf.json"), b, 0600); e != nil {
		return e
	}
	env := []string{}
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "SSL_CERT_FILE", "SYSTEMROOT"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	for _, args := range [][]string{{"init", "-backend=false", "-lockfile=readonly", "-input=false", "-plugin-dir=" + providers}, {"test", "-filter=tests/platform-mail.tftest.hcl"}} {
		args = append([]string{"-chdir=" + work}, args...)
		if _, e = o.Run(ctx, process.Options{Name: "tofu", Args: args, Env: env, Stdout: o.Log, Stderr: o.Log, Timeout: 180 * time.Second}); e != nil {
			return e
		}
	}
	return nil
}
