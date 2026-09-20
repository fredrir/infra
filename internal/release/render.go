package release

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Metadata struct {
	Packages []CargoPackage `json:"packages"`
	Members  []string       `json:"workspace_members"`
	Metadata struct {
		Release struct {
			Package string `json:"package"`
		} `json:"release"`
	} `json:"metadata"`
}

type CargoPackage struct {
	ID, Name, Description, License, Repository, Homepage string
	Authors                                              []string
	Manifest                                             string `json:"manifest_path"`
	Targets                                              []struct {
		Name string
		Kind []string
	}
	Metadata struct {
		Release struct {
			Binary, Maintainer, Section string
			Features                    []string
			ExtraFiles                  []string `json:"extra-files"`
			Depends                     map[string][]string
			Optional                    []string
		}
	}
}

type Settings struct {
	Name        string              `json:"name"`
	Binary      string              `json:"binary"`
	Description string              `json:"description"`
	License     string              `json:"license"`
	NixLicense  string              `json:"nix_license"`
	Homepage    string              `json:"homepage"`
	Repository  string              `json:"repository"`
	Maintainer  string              `json:"maintainer"`
	Section     string              `json:"section"`
	Features    []string            `json:"features"`
	ExtraFiles  []string            `json:"extra_files"`
	Depends     map[string][]string `json:"depends"`
	Optional    []string            `json:"optional"`
	Directory   string              `json:"-"`
}

var releaseRepository = regexp.MustCompile(`^fredrir/[A-Za-z0-9_.-]+$`)
var packageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
var licenseSeparator = regexp.MustCompile(`\s+(OR|AND)\s+`)
var nixLicenses = map[string]string{"0BSD": "bsd0", "Apache-2.0": "asl20", "BSD-2-Clause": "bsd2", "BSD-3-Clause": "bsd3", "GPL-3.0-only": "gpl3Only", "GPL-3.0-or-later": "gpl3Plus", "ISC": "isc", "MIT": "mit", "MPL-2.0": "mpl20", "Unlicense": "unlicense", "Zlib": "zlib"}

func ReleaseSettings(metadata Metadata, repository string) (Settings, error) {
	if !releaseRepository.MatchString(repository) {
		return Settings{}, fmt.Errorf("only fredrir repositories can be released")
	}
	var candidates []CargoPackage
	for _, pkg := range metadata.Packages {
		if !slices.Contains(metadata.Members, pkg.ID) {
			continue
		}
		selected := pkg.Name == metadata.Metadata.Release.Package
		if metadata.Metadata.Release.Package == "" {
			for _, target := range pkg.Targets {
				selected = selected || slices.Contains(target.Kind, "bin")
			}
		}
		if selected {
			candidates = append(candidates, pkg)
		}
	}
	if len(candidates) != 1 {
		return Settings{}, fmt.Errorf("set [workspace.metadata.release] package to the crate that ships the binary")
	}
	pkg := candidates[0]
	var binaries []string
	for _, target := range pkg.Targets {
		if slices.Contains(target.Kind, "bin") {
			binaries = append(binaries, target.Name)
		}
	}
	binary := pkg.Metadata.Release.Binary
	if binary == "" && len(binaries) != 0 {
		binary = binaries[0]
	}
	if !slices.Contains(binaries, binary) || !packageName.MatchString(binary) || !packageName.MatchString(pkg.Name) {
		return Settings{}, fmt.Errorf("invalid release package or binary name")
	}
	firstLicense := licenseSeparator.Split(strings.Trim(pkg.License, "()"), -1)[0]
	nixLicense, ok := nixLicenses[firstLicense]
	if !ok {
		return Settings{}, fmt.Errorf("no Nix license mapping for %s", firstLicense)
	}
	maintainer := pkg.Metadata.Release.Maintainer
	if maintainer == "" && len(pkg.Authors) > 0 {
		maintainer = pkg.Authors[0]
	}
	if maintainer == "" || pkg.Description == "" {
		return Settings{}, fmt.Errorf("Cargo manifest needs a description and release maintainer")
	}
	for _, name := range pkg.Metadata.Release.ExtraFiles {
		if !filepath.IsLocal(name) || strings.ContainsAny(name, "\x00\r\n\"`$") {
			return Settings{}, fmt.Errorf("extra file %q must be a safe path inside the crate", name)
		}
	}
	homepage := pkg.Homepage
	if homepage == "" {
		homepage = pkg.Repository
	}
	if homepage == "" {
		homepage = "https://github.com/" + repository
	}
	section := pkg.Metadata.Release.Section
	if section == "" {
		section = "utils"
	}
	settings := Settings{Name: pkg.Name, Binary: binary, Description: pkg.Description, License: pkg.License, NixLicense: nixLicense, Homepage: homepage, Repository: repository, Maintainer: maintainer, Section: section, Directory: filepath.Dir(pkg.Manifest), Features: pkg.Metadata.Release.Features, ExtraFiles: pkg.Metadata.Release.ExtraFiles, Depends: pkg.Metadata.Release.Depends, Optional: pkg.Metadata.Release.Optional}
	if settings.Features == nil {
		settings.Features = []string{}
	}
	if settings.ExtraFiles == nil {
		settings.ExtraFiles = []string{}
	}
	if settings.Optional == nil {
		settings.Optional = []string{}
	}
	if settings.Depends == nil {
		settings.Depends = map[string][]string{}
	}
	return settings, nil
}

func GoReleaser(settings Settings, root, dist, sdk string) (map[string]any, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(root, settings.Directory)
	if err != nil || !filepath.IsLocal(relative) {
		return nil, fmt.Errorf("release package must remain inside source root")
	}
	repository := strings.SplitN(settings.Repository, "/", 2)
	if len(repository) != 2 {
		return nil, fmt.Errorf("invalid release repository")
	}
	flags := []string{"--release", "--locked"}
	if len(settings.Features) > 0 {
		flags = append(flags, "--features="+strings.Join(settings.Features, ","))
	}
	files := append([]string{"LICENSE*", "README*"}, settings.ExtraFiles...)
	var builds, archives []map[string]any
	for _, target := range []struct {
		name, triple string
		targets      []string
	}{
		{"gnu", "unknown-linux-gnu", []string{"x86_64-unknown-linux-gnu.2.28", "aarch64-unknown-linux-gnu.2.28"}},
		{"musl", "unknown-linux-musl", []string{"x86_64-unknown-linux-musl", "aarch64-unknown-linux-musl"}},
		{"darwin", "apple-darwin", []string{"x86_64-apple-darwin", "aarch64-apple-darwin"}},
	} {
		environment := []string{"LIBZ_SYS_STATIC=1"}
		if target.name == "darwin" {
			environment = append(environment, "SDKROOT="+sdk, "MACOSX_DEPLOYMENT_TARGET=13.0", "CARGO_PROFILE_RELEASE_STRIP=false")
		}
		builds = append(builds, map[string]any{"id": target.name, "builder": "rust", "binary": settings.Binary, "dir": filepath.ToSlash(relative), "targets": target.targets, "flags": flags, "env": environment})
		archives = append(archives, map[string]any{"id": target.name, "ids": []string{target.name}, "formats": []string{"tar.gz"}, "files": files, "name_template": `{{ .ProjectName }}-{{ if eq .Arch "amd64" }}x86_64{{ else }}aarch64{{ end }}-` + target.triple + `-v{{ .Version }}`})
	}
	install := []string{fmt.Sprintf(`install -Dm755 "./%s" "${pkgdir}/usr/bin/%s"`, settings.Binary, settings.Binary), `install -Dm644 ./LICENSE* -t "${pkgdir}/usr/share/licenses/${pkgname}/"`}
	for _, name := range settings.ExtraFiles {
		install = append(install, fmt.Sprintf(`install -Dm644 "./%s" -t "${pkgdir}/usr/share/licenses/${pkgname}/"`, name))
	}
	installText := strings.Join(install, "\n")
	maintainer := strings.NewReplacer("@", " at ", ".", " dot ").Replace(settings.Maintainer)
	common := func() map[string]any {
		return map[string]any{"homepage": settings.Homepage, "description": settings.Description, "license": settings.License, "maintainers": []string{maintainer}, "skip_upload": true, "optdepends": settings.Optional}
	}
	aur := common()
	for key, value := range map[string]any{"name": settings.Name + "-bin", "ids": []string{"gnu"}, "provides": []string{settings.Name}, "conflicts": []string{settings.Name}, "depends": settings.Depends["arch"], "git_url": "ssh://aur@aur.archlinux.org/" + settings.Name + "-bin.git", "package": installText} {
		aur[key] = value
	}
	source := common()
	for key, value := range map[string]any{
		"name": settings.Name, "arches": []string{"x86_64", "aarch64"}, "makedepends": []string{"cargo"}, "depends": append([]string{"gcc-libs"}, settings.Depends["arch"]...), "conflicts": []string{settings.Name + "-bin"}, "git_url": "ssh://aur@aur.archlinux.org/" + settings.Name + ".git",
		"prepare": "cd \"${srcdir}/${pkgname}-${pkgver}\"\nexport RUSTUP_TOOLCHAIN=stable\ncargo fetch --locked --target \"$(rustc -vV | sed -n 's/host: //p')\"",
		"build":   "cd \"${srcdir}/${pkgname}-${pkgver}\"\nexport RUSTUP_TOOLCHAIN=stable CARGO_TARGET_DIR=target\nexport CFLAGS=\"${CFLAGS//-flto=auto/}\" CXXFLAGS=\"${CXXFLAGS//-flto=auto/}\" LDFLAGS=\"${LDFLAGS//-flto=auto/}\"\ncargo build --frozen --release --package " + settings.Name,
		"package": "cd \"${srcdir}/${pkgname}-${pkgver}\"\n" + strings.ReplaceAll(installText, `"./`+settings.Binary+`"`, `"target/release/`+settings.Binary+`"`),
	} {
		source[key] = value
	}
	return map[string]any{
		"version": 2, "project_name": settings.Name, "dist": dist, "builds": builds, "archives": archives,
		"source":   map[string]any{"enabled": true, "name_template": "{{ .ProjectName }}-{{ .Version }}-source", "prefix_template": "{{ .ProjectName }}-{{ .Version }}/"},
		"checksum": map[string]any{"name_template": "checksums.txt", "algorithm": "sha256"}, "changelog": map[string]any{"disable": true}, "release": map[string]any{"github": map[string]any{"owner": repository[0], "name": repository[1]}},
		"homebrew_casks": []map[string]any{{"name": settings.Name, "ids": []string{"musl", "darwin"}, "binaries": []string{settings.Binary}, "directory": "Casks", "homepage": settings.Homepage, "description": settings.Description, "license": settings.License, "skip_upload": true, "repository": map[string]any{"owner": repository[0], "name": "homebrew-tap"}, "custom_block": fmt.Sprintf("postflight_steps do\n  on_macos do\n    run \"/usr/bin/xattr\", args: [\"-dr\", \"com.apple.quarantine\", \"{{ \"{{staged_path}}\" }}/%s\"]\n  end\nend", settings.Binary)}},
		"aurs":           []map[string]any{aur}, "aur_sources": []map[string]any{source},
		"nix":   []map[string]any{{"name": settings.Name, "ids": []string{"musl", "darwin"}, "path": "pkgs/" + settings.Name + "/default.nix", "homepage": settings.Homepage, "description": settings.Description, "license": settings.NixLicense, "main_program": settings.Binary, "skip_upload": true, "repository": map[string]any{"owner": repository[0], "name": "nur-packages"}}},
		"nfpms": []any{}, "report_sizes": true,
	}, nil
}

type RenderOptions struct{ Metadata, Root, Repository, Dist, SDK, Config, Summary string }

func Render(options RenderOptions) error {
	data, err := os.ReadFile(options.Metadata)
	if err != nil {
		return err
	}
	var metadata Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return err
	}
	settings, err := ReleaseSettings(metadata, options.Repository)
	if err != nil {
		return err
	}
	config, err := GoReleaser(settings, options.Root, options.Dist, options.SDK)
	if err != nil {
		return err
	}
	data, err = yaml.Marshal(config)
	if err != nil {
		return err
	}
	if err := os.WriteFile(options.Config, data, 0644); err != nil {
		return err
	}
	data, err = json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(options.Summary, append(data, '\n'), 0644)
}
