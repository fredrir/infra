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
)

type Config struct {
	Repository string     `json:"repository"`
	State      string     `json:"state"`
	Bucket     string     `json:"bucket"`
	Prefix     string     `json:"prefix"`
	Region     string     `json:"region"`
	Endpoint   string     `json:"endpoint,omitempty"`
	Gatus      string     `json:"gatus"`
	Heartbeat  string     `json:"heartbeat"`
	Kubernetes Kubernetes `json:"kubernetes"`
	Observer   Observer   `json:"observer"`
}

type Kubernetes struct {
	Server               string `json:"server"`
	CertificateAuthority string `json:"certificate_authority"`
}

type Observer struct {
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
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("%s: trailing data", path)
	}
	if err := config.validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return config, nil
}

func (c Config) validate() error {
	switch {
	case !validURL(c.Repository, "https", "http", "git", "file"):
		return fmt.Errorf("repository %q is not a URL", c.Repository)
	case !filepath.IsAbs(c.State) || filepath.Clean(c.State) != c.State:
		return fmt.Errorf("state %q is not a clean absolute path", c.State)
	case !bucketPattern.MatchString(c.Bucket):
		return fmt.Errorf("bucket %q is invalid", c.Bucket)
	case !prefixPattern.MatchString(c.Prefix):
		return fmt.Errorf("prefix %q is invalid", c.Prefix)
	case !regionPattern.MatchString(c.Region):
		return fmt.Errorf("region %q is invalid", c.Region)
	case c.Endpoint != "" && !validURL(c.Endpoint, "https", "http"):
		return fmt.Errorf("endpoint %q is not a URL", c.Endpoint)
	case !validURL(c.Gatus, "https", "http"):
		return fmt.Errorf("gatus %q is not a URL", c.Gatus)
	case !heartbeatPattern.MatchString(c.Heartbeat):
		return fmt.Errorf("heartbeat %q is not GROUP_NAME", c.Heartbeat)
	case !validURL(c.Kubernetes.Server, "https"):
		return fmt.Errorf("kubernetes server %q is not an HTTPS URL", c.Kubernetes.Server)
	case !filepath.IsAbs(c.Kubernetes.CertificateAuthority):
		return fmt.Errorf("kubernetes certificate authority %q is not an absolute path", c.Kubernetes.CertificateAuthority)
	case c.Observer.AppID <= 0 || c.Observer.InstallationID <= 0:
		return errors.New("observer app_id and installation_id are required")
	case !validURL(c.Observer.API, "https", "http"):
		return fmt.Errorf("observer api %q is not a URL", c.Observer.API)
	}
	return nil
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
