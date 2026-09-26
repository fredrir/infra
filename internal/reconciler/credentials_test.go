package reconciler

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testAuthority(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "k3s-server-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func writeCredentials(t *testing.T, values map[string]any) string {
	t.Helper()
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func anyValues(values map[string]string) map[string]any {
	converted := map[string]any{}
	for name, value := range values {
		converted[name] = value
	}
	return converted
}

func verifyCredentialValues() map[string]string {
	return map[string]string{
		AWSAccessKeyID:        "AKIAVERIFY\n",
		AWSSecretAccessKey:    "aws-secret-value",
		CloudflareAPIToken:    "cloudflare-secret-value",
		HcloudToken:           "hcloud-secret-value",
		PlatformMailRecipient: "operator@example.net",
		KubernetesToken:       "kubernetes-secret-value",
		ObserverAppKey:        "-----BEGIN RSA PRIVATE KEY-----\nobserver\n-----END RSA PRIVATE KEY-----\n",
		GatusToken:            strings.Repeat("gatus-secret-", 3),
	}
}

func TestConsumeCredentialsTrimsRemovesAndNamesOnlyProblems(t *testing.T) {
	values := verifyCredentialValues()
	path := writeCredentials(t, anyValues(values))
	credentials, err := ConsumeCredentials(path, VerifyCredentials)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range values {
		if credentials[name] != strings.TrimSpace(value) {
			t.Errorf("%s read as %q", name, credentials[name])
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("credentials file left behind: %v", err)
	}
	broken := anyValues(values)
	broken[KubernetesToken] = "  \n"
	broken[CloudflareAPIToken] = "split token"
	broken[AWSSecretAccessKey] = 7
	delete(broken, HcloudToken)
	_, err = ConsumeCredentials(writeCredentials(t, broken), VerifyCredentials)
	for _, want := range []string{"credential kubernetes-token: empty", "credential cloudflare-api-token: contains whitespace", "credential hcloud-token: missing", "credential aws-secret-access-key: not a string"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v lacks %q", err, want)
		}
	}
	if err != nil && strings.Contains(err.Error(), "split token") {
		t.Errorf("error printed a credential: %v", err)
	}
	readable := writeCredentials(t, anyValues(values))
	if err := os.Chmod(readable, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := ConsumeCredentials(readable, VerifyCredentials); err == nil || !strings.Contains(err.Error(), "readable only by its owner") {
		t.Errorf("group-readable credentials returned %v", err)
	}
	if _, err := os.Stat(readable); !os.IsNotExist(err) {
		t.Errorf("rejected credentials file left behind: %v", err)
	}
	target := writeCredentials(t, anyValues(values))
	link := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ConsumeCredentials(link, VerifyCredentials); err == nil {
		t.Error("symlinked credentials accepted")
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("rejected credentials link left behind: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("symlink target consumed: %v", err)
	}
	fifo := filepath.Join(t.TempDir(), "credentials.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConsumeCredentials(fifo, VerifyCredentials); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Errorf("named pipe credentials returned %v", err)
	}
	for name, path := range map[string]string{"relative": "credentials.json", "missing": filepath.Join(t.TempDir(), "credentials.json")} {
		if _, err := ConsumeCredentials(path, VerifyCredentials); err == nil {
			t.Errorf("%s credentials file accepted", name)
		}
	}
	array := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(array, []byte(`["secret"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConsumeCredentials(array, VerifyCredentials); err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("non-object credentials returned %v", err)
	}
}

func TestEngineEnvironmentMapsOnlyEngineCredentials(t *testing.T) {
	credentials, err := ConsumeCredentials(writeCredentials(t, anyValues(verifyCredentialValues())), VerifyCredentials)
	if err != nil {
		t.Fatal(err)
	}
	config := validConfig()
	want := []string{
		"AWS_ACCESS_KEY_ID=AKIAVERIFY",
		"AWS_SECRET_ACCESS_KEY=aws-secret-value",
		"AWS_REGION=eu-north-1",
		"AWS_DEFAULT_REGION=eu-north-1",
		"CLOUDFLARE_API_TOKEN=cloudflare-secret-value",
		"TF_VAR_hcloud_token=hcloud-secret-value",
		"TF_VAR_platform_mail_recipient=operator@example.net",
		"KUBECONFIG=/run/kubeconfig",
		"GH_TOKEN=observer-installation-token",
	}
	if got := credentials.engineEnvironment(config, "/run/kubeconfig", "observer-installation-token"); !slices.Equal(got, want) {
		t.Fatalf("engine environment %q, want %q", got, want)
	}
	config.Endpoint = "http://10.0.2.2:9000"
	if got := credentials.engineEnvironment(config, "/run/kubeconfig", "token"); !slices.Contains(got, "AWS_ENDPOINT_URL_S3=http://10.0.2.2:9000") {
		t.Fatalf("engine environment %q ignores the endpoint", got)
	}
}

func TestKubeconfigCarriesTheDeclaredServerAuthorityAndToken(t *testing.T) {
	authority := testAuthority(t)
	data, err := Kubeconfig("https://100.115.121.9:6443", authority, "kubernetes-secret-value")
	if err != nil {
		t.Fatal(err)
	}
	var config kubeconfig
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(config.Clusters[0].Cluster.CertificateAuthorityData)
	if err != nil || string(decoded) != string(authority) {
		t.Fatalf("certificate authority %q: %v", decoded, err)
	}
	if config.CurrentContext != "production" || config.Contexts[0].Context.Cluster != "production" || config.Contexts[0].Context.User != "infrastructure-verify" || config.Clusters[0].Cluster.Server != "https://100.115.121.9:6443" || config.Users[0].User.Token != "kubernetes-secret-value" {
		t.Fatalf("kubeconfig %s", data)
	}
	for name, build := range map[string]func() ([]byte, error){
		"plain server":   func() ([]byte, error) { return Kubeconfig("http://100.115.121.9:6443", authority, "token") },
		"missing server": func() ([]byte, error) { return Kubeconfig("", authority, "token") },
		"no certificate": func() ([]byte, error) {
			return Kubeconfig("https://100.115.121.9:6443", []byte("not a certificate"), "token")
		},
		"private key": func() ([]byte, error) {
			return Kubeconfig("https://100.115.121.9:6443", []byte("-----BEGIN PRIVATE KEY-----\nAA==\n-----END PRIVATE KEY-----\n"), "token")
		},
		"token whitespace": func() ([]byte, error) { return Kubeconfig("https://100.115.121.9:6443", authority, "two words") },
		"missing token":    func() ([]byte, error) { return Kubeconfig("https://100.115.121.9:6443", authority, "") },
	} {
		if _, err := build(); err == nil {
			t.Errorf("%s accepted", name)
		} else if strings.Contains(err.Error(), "two words") {
			t.Errorf("%s printed the token: %v", name, err)
		}
	}
}
