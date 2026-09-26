package reconciler

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
)

const (
	AWSAccessKeyID        = "aws-access-key-id"
	AWSSecretAccessKey    = "aws-secret-access-key"
	CloudflareAPIToken    = "cloudflare-api-token"
	HcloudToken           = "hcloud-token"
	PlatformMailRecipient = "platform-mail-recipient"
	KubernetesToken       = "kubernetes-token"
	ObserverAppKey        = "observer-app-key"
	GatusToken            = "gatus-token"
)

var VerifyCredentials = []string{AWSAccessKeyID, AWSSecretAccessKey, CloudflareAPIToken, HcloudToken, PlatformMailRecipient, KubernetesToken, ObserverAppKey, GatusToken}

const credentialsLimit = 256 << 10

type Credentials map[string]string

func ConsumeCredentials(path string, names []string) (Credentials, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("credentials file required")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("credentials: %w", err), removeCredentials(path))
	}
	info, err := file.Stat()
	var data []byte
	if err == nil {
		data, err = io.ReadAll(io.LimitReader(file, credentialsLimit+1))
	}
	err = errors.Join(err, file.Close(), os.Remove(path))
	switch {
	case err != nil:
		return nil, fmt.Errorf("credentials: %w", err)
	case !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0:
		return nil, errors.New("credentials must be a regular file readable only by its owner")
	case len(data) > credentialsLimit:
		return nil, errors.New("credentials exceed 256 KiB")
	}
	var declared map[string]json.RawMessage
	if err := json.Unmarshal(data, &declared); err != nil {
		return nil, errors.New("credentials are not a JSON object")
	}
	credentials := Credentials{}
	var problems []error
	for _, name := range names {
		value, err := credential(declared[name], name)
		if err != nil {
			problems = append(problems, fmt.Errorf("credential %s: %w", name, err))
			continue
		}
		credentials[name] = value
	}
	return credentials, errors.Join(problems...)
}

func removeCredentials(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func credential(raw json.RawMessage, name string) (string, error) {
	if raw == nil {
		return "", errors.New("missing")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errors.New("not a string")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("empty")
	}
	if name != ObserverAppKey && strings.ContainsFunc(value, unicode.IsSpace) {
		return "", errors.New("contains whitespace")
	}
	return value, nil
}

type kubeconfig struct {
	APIVersion     string              `json:"apiVersion"`
	Kind           string              `json:"kind"`
	Clusters       []kubeconfigCluster `json:"clusters"`
	Users          []kubeconfigUser    `json:"users"`
	Contexts       []kubeconfigContext `json:"contexts"`
	CurrentContext string              `json:"current-context"`
}

type kubeconfigCluster struct {
	Name    string `json:"name"`
	Cluster struct {
		Server                   string `json:"server"`
		CertificateAuthorityData string `json:"certificate-authority-data"`
	} `json:"cluster"`
}

type kubeconfigUser struct {
	Name string `json:"name"`
	User struct {
		Token string `json:"token"`
	} `json:"user"`
}

type kubeconfigContext struct {
	Name    string `json:"name"`
	Context struct {
		Cluster string `json:"cluster"`
		User    string `json:"user"`
	} `json:"context"`
}

func Kubeconfig(server string, authority []byte, token string) ([]byte, error) {
	if !validURL(server, "https") {
		return nil, fmt.Errorf("kubernetes server %q is not an HTTPS URL", server)
	}
	if err := certificates(authority); err != nil {
		return nil, fmt.Errorf("kubernetes certificate authority: %w", err)
	}
	if token == "" || strings.ContainsFunc(token, unicode.IsSpace) {
		return nil, errors.New("kubernetes token is empty or contains whitespace")
	}
	const name = "production"
	config := kubeconfig{APIVersion: "v1", Kind: "Config", CurrentContext: name}
	cluster := kubeconfigCluster{Name: name}
	cluster.Cluster.Server, cluster.Cluster.CertificateAuthorityData = server, base64.StdEncoding.EncodeToString(authority)
	user := kubeconfigUser{Name: "infrastructure-verify"}
	user.User.Token = token
	context := kubeconfigContext{Name: name}
	context.Context.Cluster, context.Context.User = name, user.Name
	config.Clusters, config.Users, config.Contexts = []kubeconfigCluster{cluster}, []kubeconfigUser{user}, []kubeconfigContext{context}
	return json.Marshal(config)
}

func certificates(data []byte) error {
	found := false
	for rest := bytes.TrimSpace(data); len(rest) > 0; rest = bytes.TrimSpace(rest) {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" {
			return errors.New("not a PEM certificate bundle")
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return err
		}
		found = true
	}
	if !found {
		return errors.New("no certificate")
	}
	return nil
}

func (c Credentials) engineEnvironment(config Config, kubeconfig, runnerToken string) []string {
	environment := []string{
		"AWS_ACCESS_KEY_ID=" + c[AWSAccessKeyID],
		"AWS_SECRET_ACCESS_KEY=" + c[AWSSecretAccessKey],
		"AWS_REGION=" + config.Region,
		"AWS_DEFAULT_REGION=" + config.Region,
		"CLOUDFLARE_API_TOKEN=" + c[CloudflareAPIToken],
		"TF_VAR_hcloud_token=" + c[HcloudToken],
		"TF_VAR_platform_mail_recipient=" + c[PlatformMailRecipient],
		"KUBECONFIG=" + kubeconfig,
		"GH_TOKEN=" + runnerToken,
	}
	if config.Endpoint != "" {
		environment = append(environment, "AWS_ENDPOINT_URL_S3="+config.Endpoint)
	}
	return environment
}
