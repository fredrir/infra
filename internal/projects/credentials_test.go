package projects

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/process"
)

func TestRunnerCredentialsComeFromTheSelectedCheckout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	var logs bytes.Buffer
	provider := NativeProvider{Runner: process.Runner{Stdout: &logs, Stderr: &logs, Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		want := []string{"decrypt", "--output-type", "json", filepath.Join(root, "secrets/operator.sops.yaml")}
		if options.Name != "sops" || !reflect.DeepEqual(options.Args, want) || options.Stdout != nil {
			t.Fatalf("credential command has the wrong source or exposes stdout: %s %v", options.Name, options.Args)
		}
		return process.Result{Stdout: []byte(`{"ARC_GITHUB_APP_ID":"1","ARC_GITHUB_APP_INSTALLATION_ID":"2","ARC_GITHUB_APP_PRIVATE_KEY":"private-key","UNRELATED":"other-secret"}`)}, nil
	}}}
	got, err := provider.Credentials(context.Background(), root)
	want := map[string]string{"github_app_id": "1", "github_app_installation_id": "2", "github_app_private_key": "private-key"}
	if err != nil || !reflect.DeepEqual(got, want) || logs.Len() != 0 {
		t.Fatalf("runner credentials did not load privately: %v", err)
	}
}

func TestRunnerCredentialsRejectMissingOrInvalidSecrets(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, response string
		err            error
	}{
		{name: "decrypt failure", err: errors.New("decryption failed")},
		{name: "invalid document", response: "private-key"},
		{name: "non-string value", response: `{"ARC_GITHUB_APP_PRIVATE_KEY":{"private-key":true}}`},
		{name: "missing key", response: `{"ARC_GITHUB_APP_ID":"1","ARC_GITHUB_APP_INSTALLATION_ID":"2"}`},
		{name: "empty key", response: `{"ARC_GITHUB_APP_ID":"1","ARC_GITHUB_APP_INSTALLATION_ID":"2","ARC_GITHUB_APP_PRIVATE_KEY":""}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			provider := NativeProvider{Runner: process.Runner{Execute: func(context.Context, process.Options) (process.Result, error) {
				return process.Result{Stdout: []byte(test.response)}, test.err
			}}}
			credentials, err := provider.Credentials(context.Background(), t.TempDir())
			if err == nil || credentials != nil || strings.Contains(err.Error(), "private-key") {
				t.Fatalf("invalid credentials accepted or leaked: %v", err)
			}
		})
	}
}
