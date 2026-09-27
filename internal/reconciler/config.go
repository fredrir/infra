package reconciler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/fredrir/infra/internal/reconcile"
)

type Site struct {
	Repository string     `json:"repository"`
	State      string     `json:"state"`
	Cache      string     `json:"cache"`
	Shared     string     `json:"shared"`
	Bucket     string     `json:"bucket"`
	Prefix     string     `json:"prefix"`
	Region     string     `json:"region"`
	Endpoint   string     `json:"endpoint,omitempty"`
	Gatus      string     `json:"gatus"`
	Heartbeat  string     `json:"heartbeat"`
	KnownHosts string     `json:"known_hosts,omitempty"`
	Kubernetes Kubernetes `json:"kubernetes"`
}

type Config struct {
	Site
	Scope    reconcile.Scope `json:"scope"`
	Observer App             `json:"observer"`
}

type ApplyConfig struct {
	Site
	Runner App `json:"runner"`
}

type Kubernetes struct {
	Server               string `json:"server"`
	CertificateAuthority string `json:"certificate_authority"`
}

type App struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	API            string `json:"api"`
}

var (
	bucketPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	prefixPattern    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*(/[A-Za-z0-9_][A-Za-z0-9._-]*)*$`)
	regionPattern    = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	heartbeatPattern = regexp.MustCompile(`^[a-z0-9-]+_[a-z0-9-]+$`)
)

func LoadConfig(path string) (Config, error) {
	var config Config
	return config, load(path, &config, func() error { return config.validate() })
}

func LoadApplyConfig(path string) (ApplyConfig, error) {
	var config ApplyConfig
	return config, load(path, &config, func() error { return config.validate() })
}

func load(path string, config any, validate func() error) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(config); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: trailing data", path)
	}
	if err := validate(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func (c Config) validate() error {
	if err := c.Site.validate(); err != nil {
		return err
	}
	if _, err := reconcile.ParseScope(string(c.Scope)); err != nil {
		return err
	}
	if c.Scope == reconcile.ScopeFull && !cleanAbsolute(c.KnownHosts) {
		return fmt.Errorf("full scope requires a clean absolute known_hosts, not %q", c.KnownHosts)
	}
	return c.Observer.validate("observer")
}

func (c ApplyConfig) validate() error {
	if err := c.Site.validate(); err != nil {
		return err
	}
	if !cleanAbsolute(c.KnownHosts) {
		return fmt.Errorf("known_hosts %q is not a clean absolute path", c.KnownHosts)
	}
	return c.Runner.validate("runner")
}

func (s Site) validate() error {
	switch {
	case !validURL(s.Repository, "https", "http", "git", "file"):
		return fmt.Errorf("repository %q is not a URL", s.Repository)
	case !cleanAbsolute(s.State):
		return fmt.Errorf("state %q is not a clean absolute path", s.State)
	case !cleanAbsolute(s.Cache) || contains(s.State, s.Cache) || contains(s.Cache, s.State):
		return fmt.Errorf("cache %q is not a clean absolute path outside state", s.Cache)
	case !cleanAbsolute(s.Shared) || slices.ContainsFunc([]string{s.State, s.Cache}, func(private string) bool { return contains(private, s.Shared) || contains(s.Shared, private) }):
		return fmt.Errorf("shared %q is not a clean absolute path outside state and cache", s.Shared)
	case !bucketPattern.MatchString(s.Bucket):
		return fmt.Errorf("bucket %q is invalid", s.Bucket)
	case !prefixPattern.MatchString(s.Prefix):
		return fmt.Errorf("prefix %q is invalid", s.Prefix)
	case !regionPattern.MatchString(s.Region):
		return fmt.Errorf("region %q is invalid", s.Region)
	case s.Endpoint != "" && !validURL(s.Endpoint, "https", "http"):
		return fmt.Errorf("endpoint %q is not a URL", s.Endpoint)
	case !validURL(s.Gatus, "https", "http"):
		return fmt.Errorf("gatus %q is not a URL", s.Gatus)
	case !heartbeatPattern.MatchString(s.Heartbeat):
		return fmt.Errorf("heartbeat %q is not GROUP_NAME", s.Heartbeat)
	case s.KnownHosts != "" && !cleanAbsolute(s.KnownHosts):
		return fmt.Errorf("known_hosts %q is not a clean absolute path", s.KnownHosts)
	case !validURL(s.Kubernetes.Server, "https"):
		return fmt.Errorf("kubernetes server %q is not an HTTPS URL", s.Kubernetes.Server)
	case !filepath.IsAbs(s.Kubernetes.CertificateAuthority):
		return fmt.Errorf("kubernetes certificate authority %q is not an absolute path", s.Kubernetes.CertificateAuthority)
	}
	return nil
}

func (a App) validate(name string) error {
	switch {
	case a.AppID <= 0 || a.InstallationID <= 0:
		return fmt.Errorf("%s app_id and installation_id are required", name)
	case !validURL(a.API, "https", "http"):
		return fmt.Errorf("%s api %q is not a URL", name, a.API)
	}
	return nil
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

func contains(directory, path string) bool {
	relative, err := filepath.Rel(directory, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, "../")
}

func validURL(value string, schemes ...string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	for _, scheme := range schemes {
		if parsed.Scheme == scheme && (parsed.Host != "" || scheme == "file") {
			return true
		}
	}
	return false
}
