package images

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Image struct {
	Image        string   `yaml:"image" json:"image"`
	Dockerfile   string   `yaml:"dockerfile" json:"dockerfile"`
	Inputs       []string `yaml:"inputs" json:"inputs,omitempty"`
	Excludes     []string `yaml:"excludes,omitempty" json:"excludes,omitempty"`
	Check        string   `yaml:"check" json:"check"`
	ScanSkipDirs string   `yaml:"scan-skip-dirs,omitempty" json:"scan-skip-dirs,omitempty"`
	Shell        string   `yaml:"shell,omitempty" json:"shell,omitempty"`
	Tag          string   `yaml:"-" json:"tag,omitempty"`
}

type Planner struct {
	Root     string
	Registry string
	Refresh  bool
	Client   *http.Client
	Log      io.Writer
}

var imageName = regexp.MustCompile(`^ghcr\.io/fredrir/[a-z0-9][a-z0-9._/-]*$`)

func (p Planner) Plan(ctx context.Context, catalog string) ([]Image, error) {
	registry, err := url.Parse(p.Registry)
	if err != nil || registry.Host == "" || (registry.Scheme != "https" && registry.Scheme != "http") || registry.User != nil || registry.RawQuery != "" || registry.Fragment != "" {
		return nil, errors.New("registry must be an HTTP(S) URL without credentials, query, or fragment")
	}
	file, err := os.Open(filepath.Join(p.Root, catalog))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var entries []Image
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&entries); err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("catalog must contain one YAML document")
	}
	seen := make(map[string]bool)
	sharedInputs := []string{".github/workflows/images.yml", ".github/workflows/build-image.yml", ".github/workflows/infra-cli.yml", ".dockerignore"}
	paths := append([]string(nil), sharedInputs...)
	for _, entry := range entries {
		if !imageName.MatchString(entry.Image) || seen[entry.Image] {
			return nil, fmt.Errorf("invalid or duplicate image: %q", entry.Image)
		}
		seen[entry.Image] = true
		if entry.Dockerfile == "" || entry.Check == "" || len(entry.Inputs) == 0 {
			return nil, fmt.Errorf("%s requires dockerfile, check, and inputs", entry.Image)
		}
		for _, path := range entry.Inputs {
			if !filepath.IsLocal(path) || strings.ContainsAny(path, "\r\n") {
				return nil, fmt.Errorf("%s has invalid input path %q", entry.Image, path)
			}
		}
		for _, exclude := range entry.Excludes {
			if !filepath.IsLocal(exclude) || filepath.Clean(exclude) != exclude || strings.ContainsAny(exclude, "\r\n") || !slices.ContainsFunc(entry.Inputs, func(input string) bool { return strings.HasPrefix(exclude, input+"/") }) {
				return nil, fmt.Errorf("%s has exclude %q outside its inputs", entry.Image, exclude)
			}
		}
		paths = append(paths, entry.Inputs...)
	}
	objects, err := p.objects(ctx, paths)
	if err != nil {
		return nil, err
	}
	var shared strings.Builder
	for _, path := range sharedInputs {
		fmt.Fprintln(&shared, objects[path])
	}
	plan := make([]Image, 0, len(entries))
	trees := make(map[string]string)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		fmt.Fprintf(hash, "%s\n%s\n", data, shared.String())
		for _, path := range entry.Inputs {
			excluded := excludedUnder(entry.Excludes, path)
			if len(excluded) == 0 {
				fmt.Fprintln(hash, objects[path])
				continue
			}
			listing, err := p.tree(ctx, path, excluded, trees)
			if err != nil {
				return nil, err
			}
			fmt.Fprintln(hash, listing)
		}
		entry.Tag = fmt.Sprintf("inputs-%x", hash.Sum(nil))
		if !p.Refresh && p.published(ctx, entry) {
			fmt.Fprintln(p.Log, "Unchanged:", entry.Image)
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fmt.Fprintln(p.Log, "Building:", entry.Image)
		entry.Inputs, entry.Excludes = nil, nil
		plan = append(plan, entry)
	}
	return plan, nil
}

func (p Planner) objects(ctx context.Context, paths []string) (map[string]string, error) {
	var input strings.Builder
	for _, path := range paths {
		fmt.Fprintln(&input, "HEAD:"+path)
	}
	command := exec.CommandContext(ctx, "git", "cat-file", "--batch-check=%(objectname)")
	command.Dir = p.Root
	command.Stdin = strings.NewReader(input.String())
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("read Git build inputs: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	if len(lines) != len(paths) {
		return nil, errors.New("Git returned an incomplete set of build inputs")
	}
	objects := make(map[string]string, len(paths))
	for index, object := range lines {
		if strings.Contains(object, " ") || object == "" {
			return nil, fmt.Errorf("build input is missing from HEAD: %s", paths[index])
		}
		objects[paths[index]] = object
	}
	return objects, nil
}

func (p Planner) published(ctx context.Context, entry Image) bool {
	name := strings.TrimPrefix(entry.Image, "ghcr.io/")
	registry := strings.TrimRight(p.Registry, "/")
	query := url.Values{"service": {"ghcr.io"}, "scope": {"repository:" + name + ":pull"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, registry+"/token?"+query.Encode(), nil)
	if err != nil {
		return false
	}
	response, err := p.Client.Do(request)
	if err != nil {
		return false
	}
	var token struct {
		Token string `json:"token"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&token)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || err != nil || token.Token == "" {
		return false
	}
	request, err = http.NewRequestWithContext(ctx, http.MethodHead, registry+"/v2/"+name+"/manifests/"+entry.Tag, nil)
	if err != nil {
		return false
	}
	request.Header.Set("Authorization", "Bearer "+token.Token)
	request.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json")
	response, err = p.Client.Do(request)
	if err != nil {
		return false
	}
	response.Body.Close()
	return response.StatusCode == http.StatusOK
}

func excludedUnder(excludes []string, input string) []string {
	var matched []string
	for _, exclude := range excludes {
		if strings.HasPrefix(exclude, input+"/") {
			matched = append(matched, exclude)
		}
	}
	return matched
}

func (p Planner) tree(ctx context.Context, path string, excludes []string, cache map[string]string) (string, error) {
	key := path + "\x00" + strings.Join(excludes, "\x00")
	if listing, ok := cache[key]; ok {
		return listing, nil
	}
	command := exec.CommandContext(ctx, "git", "ls-tree", "-r", "-z", "--full-tree", "HEAD", "--", path)
	command.Dir = p.Root
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("read Git build input tree %s: %w", path, err)
	}
	var listing strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00") {
		_, entryPath, ok := strings.Cut(line, "\t")
		if !ok {
			return "", fmt.Errorf("unexpected Git tree entry for %s", path)
		}
		if slices.ContainsFunc(excludes, func(exclude string) bool { return entryPath == exclude || strings.HasPrefix(entryPath, exclude+"/") }) {
			continue
		}
		listing.WriteString(line)
		listing.WriteByte('\n')
	}
	cache[key] = listing.String()
	return cache[key], nil
}
