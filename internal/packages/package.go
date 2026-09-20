package packages

import (
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/release"
)

func NFPMConfig(settings release.Settings, version, architecture, payload string) (map[string]any, error) {
	if !toolName.MatchString(settings.Name) || !binaryName.MatchString(settings.Binary) || !stableTag.MatchString("v"+version) || (architecture != "amd64" && architecture != "arm64") {
		return nil, fmt.Errorf("invalid package identity")
	}
	contents := []map[string]any{{"src": filepath.Join(payload, settings.Binary), "dst": "/usr/bin/" + settings.Binary, "file_info": map[string]any{"mode": 0755}}}
	licenses, err := filepath.Glob(filepath.Join(payload, "LICENSE*"))
	if err != nil {
		return nil, err
	}
	slices.Sort(licenses)
	for _, extra := range settings.ExtraFiles {
		if !filepath.IsLocal(extra) {
			return nil, fmt.Errorf("extra file escapes package")
		}
		licenses = append(licenses, filepath.Join(payload, extra))
	}
	for _, license := range licenses {
		contents = append(contents, map[string]any{"src": license, "dst": "/usr/share/licenses/" + settings.Name + "/" + filepath.Base(license), "file_info": map[string]any{"mode": 0644}})
	}
	recommends := []string{}
	for _, entry := range settings.Optional {
		name, _, _ := strings.Cut(entry, ":")
		recommends = append(recommends, strings.TrimSpace(name))
	}
	depends := func(format string) []string {
		value := settings.Depends[format]
		if value == nil {
			return []string{}
		}
		return value
	}
	section := settings.Section
	if section == "" {
		section = "utils"
	}
	return map[string]any{"name": settings.Name, "arch": architecture, "platform": "linux", "version": version, "release": "1", "section": section, "priority": "optional", "maintainer": settings.Maintainer, "description": settings.Description, "vendor": "fredrir", "homepage": settings.Homepage, "license": settings.License, "contents": contents,
		"overrides": map[string]any{"deb": map[string]any{"recommends": recommends, "depends": depends("deb")}, "rpm": map[string]any{"recommends": recommends, "depends": depends("rpm")}, "apk": map[string]any{"depends": depends("apk")}},
		"rpm":       map[string]any{"signature": map[string]any{"key_file": "${RPM_SIGNING_KEY}"}}, "apk": map[string]any{"signature": map[string]any{"key_file": "${APK_SIGNING_KEY}", "key_name": "fredrir"}},
	}, nil
}

type NFPM struct{ Runner ci.Runner }

func (builder NFPM) Build(ctx context.Context, settings release.Settings, version, releaseDirectory, work, site string) error {
	for _, architecture := range []struct{ triple, name string }{{"x86_64", "amd64"}, {"aarch64", "arm64"}} {
		for _, flavour := range []string{"gnu", "musl"} {
			archive := filepath.Join(releaseDirectory, settings.Name+"-"+architecture.triple+"-unknown-linux-"+flavour+"-v"+version+".tar.gz")
			payload := filepath.Join(work, settings.Name+"-"+version+"-"+architecture.name+"-"+flavour)
			if err := extractPackage(archive, payload); err != nil {
				return err
			}
			config, err := NFPMConfig(settings, version, architecture.name, payload)
			if err != nil {
				return err
			}
			configPath := payload + ".json"
			if err := writeJSON(configPath, config); err != nil {
				return err
			}
			formats := []string{"deb", "rpm"}
			if flavour == "musl" {
				formats = []string{"apk"}
			}
			for _, format := range formats {
				var destination string
				switch format {
				case "deb":
					destination = filepath.Join(site, "deb/pool/main", settings.Name)
				case "rpm":
					destination = filepath.Join(site, "rpm", architecture.triple)
				case "apk":
					destination = filepath.Join(site, "apk", architecture.triple, settings.Name+"-"+version+"-r1.apk")
				}
				directory := destination
				if format == "apk" {
					directory = filepath.Dir(destination)
				}
				if err := os.MkdirAll(directory, 0755); err != nil {
					return err
				}
				if err := builder.Runner.Run(ctx, "nfpm", "package", "--config", configPath, "--packager", format, "--target", destination); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func extractPackage(archive, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return err
	}
	if err := os.Mkdir(destination, 0755); err != nil {
		return err
	}
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer compressed.Close()
	return ci.ExtractTar(compressed, destination, "", 2<<30)
}
