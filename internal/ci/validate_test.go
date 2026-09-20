package ci

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/fredrir/infra/internal/process"
)

func TestValidateBuildsAggregateAndChildKustomizations(t *testing.T) {
	for _, changed := range []string{"platform/projects/kustomization.yaml", "platform/projects/example/deployment.yaml"} {
		t.Run(changed, func(t *testing.T) {
			root := t.TempDir()
			for _, path := range []string{
				"platform/clusters/production/kustomization.yaml",
				"platform/components/common/kustomization.yaml",
				"platform/components/policy.yaml",
				"platform/projects/kustomization.yaml",
				"platform/projects/example/kustomization.yaml",
				"platform/projects/settings.yaml",
			} {
				filename := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filename, []byte("resources: []\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			var rendered []string
			broken := errors.New("invalid project resource")
			var failure error
			runner := Runner{Dir: root, Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				switch options.Name {
				case "git":
					if !reflect.DeepEqual(options.Args, []string{"ls-files"}) {
						t.Fatalf("unexpected git command: %v", options.Args)
					}
					return process.Result{Stdout: []byte(changed + "\n")}, nil
				case "kubectl":
					if len(options.Args) != 2 || options.Args[0] != "kustomize" {
						t.Fatalf("unexpected kubectl command: %v", options.Args)
					}
					rendered = append(rendered, options.Args[1])
					if options.Args[1] == "platform/projects/example" {
						return process.Result{}, failure
					}
					return process.Result{}, nil
				default:
					t.Fatalf("unexpected validation command: %s", options.Name)
					return process.Result{}, nil
				}
			}}
			if err := Validate(context.Background(), runner, ""); err != nil {
				t.Fatal(err)
			}
			want := []string{"platform/clusters/production", "platform/projects", "platform/components/common", "platform/projects/example"}
			if !reflect.DeepEqual(rendered, want) {
				t.Fatalf("rendered %v, want %v", rendered, want)
			}
			failure = broken
			if err := Validate(context.Background(), runner, ""); !errors.Is(err, broken) {
				t.Fatalf("project rendering error was lost: %v", err)
			}
		})
	}
}

func TestTofuPreparationIsSeparateFromDeclarationChecks(t *testing.T) {
	var calls [][]string
	changed := "tofu/main.tf\n"
	failure := errors.New("invalid tofu declaration")
	var validationFailure error
	runner := Runner{Dir: t.TempDir(), Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		switch options.Name {
		case "git":
			return process.Result{Stdout: []byte(changed)}, nil
		case "tofu":
			calls = append(calls, options.Args)
			if options.Args[1] == "validate" {
				return process.Result{}, validationFailure
			}
			return process.Result{}, nil
		default:
			t.Fatalf("unexpected validator: %s", options.Name)
			return process.Result{}, nil
		}
	}}
	if err := PrepareValidation(context.Background(), runner, ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, [][]string{{"-chdir=tofu", "init", "-backend=false", "-lockfile=readonly", "-input=false"}}) {
		t.Fatalf("preparation commands: %v", calls)
	}
	calls = nil
	if err := Validate(context.Background(), runner, ""); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"-chdir=tofu", "fmt", "-check", "-recursive"}, {"-chdir=tofu", "validate"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("validation commands: %v, want %v", calls, want)
	}
	validationFailure = failure
	if err := Validate(context.Background(), runner, ""); !errors.Is(err, failure) {
		t.Fatalf("validation failure was lost: %v", err)
	}
	changed, calls = "docs/platform.md\n", nil
	if err := PrepareValidation(context.Background(), runner, ""); err != nil || len(calls) != 0 {
		t.Fatalf("unaffected declarations prepared: %v %v", calls, err)
	}
}
