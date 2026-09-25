package policy

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"github.com/fredrir/infra/internal/kustomize"
	"go.yaml.in/yaml/v3"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

type object = map[string]any

const controller = "system:serviceaccount:arc-system:arc-controller"
const reconciler = "system:serviceaccount:flux-system:platform-reconciler"
const projectRunner = "system:serviceaccount:ci-portfolio-amd64:runner"

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, e := os.Getwd()
	if e != nil {
		t.Fatal(e)
	}
	for {
		if _, e = os.Stat(filepath.Join(dir, "platform/components/policy/admission.yaml")); e == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("policy fixtures not found")
		}
		dir = parent
	}
}
func yamlObjects(t *testing.T, data []byte) []object {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var docs []object
	for {
		var doc object
		e := decoder.Decode(&doc)
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return docs
}
func load(t *testing.T, path string) object {
	t.Helper()
	data, e := os.ReadFile(filepath.Join(repoRoot(t), path))
	if e != nil {
		t.Fatal(e)
	}
	docs := yamlObjects(t, data)
	if len(docs) != 1 {
		t.Fatalf("expected one document in %s", path)
	}
	return docs[0]
}
func rendered(t *testing.T, path string) []object {
	t.Helper()
	return renderedWith(t, path, nil)
}
func renderedWith(t *testing.T, path string, overrides map[string][]byte) []object {
	t.Helper()
	memory := filesys.MakeFsInMemory()
	root := repoRoot(t)
	e := filepath.WalkDir(filepath.Join(root, "platform/components/runners"), func(source string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, source)
		if err != nil {
			return err
		}
		target := "/" + filepath.ToSlash(relative)
		if d.IsDir() {
			return memory.MkdirAll(target)
		}
		data, ok := overrides[relative]
		if !ok {
			data, err = os.ReadFile(source)
		}
		if err != nil {
			return err
		}
		return memory.WriteFile(target, data)
	})
	if e != nil {
		t.Fatal(e)
	}
	b, e := kustomize.BuildFileSystem(memory, "/"+filepath.ToSlash(path))
	if e != nil {
		t.Fatal(e)
	}
	return yamlObjects(t, b)
}
func at(v any, path ...any) any {
	for _, key := range path {
		switch key := key.(type) {
		case string:
			v = v.(map[string]any)[key]
		case int:
			v = v.([]any)[key]
		default:
			panic("invalid path")
		}
	}
	return v
}
func set(v any, value any, path ...any) {
	last := path[len(path)-1]
	target := at(v, path[:len(path)-1]...)
	switch key := last.(type) {
	case string:
		target.(map[string]any)[key] = value
	case int:
		target.([]any)[key] = value
	}
}
func clone(v any) any {
	switch v := v.(type) {
	case map[string]any:
		r := object{}
		for k, x := range v {
			r[k] = clone(x)
		}
		return r
	case []any:
		r := make([]any, len(v))
		for i, x := range v {
			r[i] = clone(x)
		}
		return r
	case int:
		return int64(v)
	default:
		return v
	}
}
func appendAt(v any, value any, path ...any) {
	items, _ := at(v, path...).([]any)
	set(v, append(items, value), path...)
}
func runner(t *testing.T, path string) object {
	t.Helper()
	p := clone(at(load(t, "platform/components/runners/"+path), "spec", "values", "template")).(object)
	if p["metadata"] == nil {
		p["metadata"] = object{}
	}
	set(p, "runner-1", "metadata", "name")
	return p
}
func rustRunner(t *testing.T, variant string) object {
	t.Helper()
	docs := rendered(t, "platform/components/runners/rust/"+variant)
	if len(docs) != 1 {
		t.Fatal("unexpected rust pool resources")
	}
	values := at(docs[0], "spec", "values").(object)
	p := clone(values["template"]).(object)
	p["metadata"] = object{"name": "rust-1", "labels": object{"actions.github.com/scale-set-name": values["runnerScaleSetName"]}}
	return p
}

type evaluator struct{ programs map[string][]cel.Program }

func newEvaluator(t *testing.T) evaluator {
	t.Helper()
	env, e := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType), cel.Variable("request", cel.DynType))
	if e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(filepath.Join(repoRoot(t), "platform/components/policy/admission.yaml"))
	if e != nil {
		t.Fatal(e)
	}
	v := evaluator{programs: map[string][]cel.Program{}}
	for _, doc := range yamlObjects(t, data) {
		if doc["kind"] != "ValidatingAdmissionPolicy" {
			continue
		}
		name := at(doc, "metadata", "name").(string)
		for _, item := range at(doc, "spec", "validations").([]any) {
			expr := at(item, "expression").(string)
			if _, issues := env.Compile(expr); issues.Err() != nil {
				t.Fatalf("compile %s: %v", name, issues.Err())
			}
			ast, issues := env.Parse(expr)
			if issues.Err() != nil {
				t.Fatalf("compile %s: %v", name, issues.Err())
			}
			p, e := env.Program(ast)
			if e != nil {
				t.Fatal(e)
			}
			v.programs[name] = append(v.programs[name], p)
		}
	}
	return v
}
func (e evaluator) admitted(names []string, obj object, namespace, user, operation string, old any) bool {
	for _, name := range names {
		programs := e.programs[name]
		if len(programs) == 0 {
			panic(fmt.Sprintf("missing policy %s", name))
		}
		for _, p := range programs {
			value, _, err := p.Eval(map[string]any{"object": clone(obj), "oldObject": clone(old), "request": object{"namespace": namespace, "operation": operation, "userInfo": object{"username": user}}})
			if err != nil || value != types.True {
				return false
			}
		}
	}
	return true
}
func (e evaluator) runner(p object, namespace, user string) bool {
	return e.admitted([]string{"ci-sandbox", "ci-job-credentials", "workload-isolation"}, p, namespace, user, "CREATE", nil)
}
func (e evaluator) project(policy string, p object) bool {
	return e.admitted([]string{policy}, p, "portfolio", projectRunner, "CREATE", nil)
}
