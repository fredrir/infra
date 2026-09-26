package contracts

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/images"
	"go.yaml.in/yaml/v3"
)

func TestRustRunnerTrustsTheObjectStoreOnlyInItsClients(t *testing.T) {
	repository := root(t)
	containerfile := string(read(t, filepath.Join(repository, "images/runner-rust/Containerfile")))
	shim := strings.TrimSpace(string(read(t, filepath.Join(repository, "images/runner-rust/sccache"))))
	if shim != "#!/bin/sh\nSSL_CERT_FILE=/usr/local/share/object-store/trust.pem exec /usr/local/libexec/sccache \"$@\"" {
		t.Errorf("sccache shim does not pin the object store bundle:\n%s", shim)
	}
	for _, required := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)^COPY \S+/ca\.crt /usr/local/share/object-store/ca\.crt$`),
		regexp.MustCompile(`(?m)^RUN cat /etc/ssl/certs/ca-certificates\.crt /usr/local/share/object-store/ca\.crt > /usr/local/share/object-store/trust\.pem$`),
		regexp.MustCompile(`(?m)^COPY --chmod=755 images/runner-rust/sccache /usr/local/bin/sccache$`),
		regexp.MustCompile(`-C /usr/local/libexec "sccache-v\$\{SCCACHE_VERSION\}-x86_64-unknown-linux-musl/sccache"`),
		regexp.MustCompile(`(?m)^ENV SCCACHE_IDLE_TIMEOUT=0$`),
		regexp.MustCompile(`(?m)^ENV OBJECT_STORE_CA_FILE=/usr/local/share/object-store/ca\.crt$`),
	} {
		if !required.MatchString(containerfile) {
			t.Errorf("runner-rust Containerfile lacks %s", required)
		}
	}
	for _, forbidden := range []string{"update-ca-certificates", "/usr/local/share/ca-certificates", "SCCACHE_S3_USE_SSL", "-C /usr/local/bin \"sccache-"} {
		if strings.Contains(containerfile, forbidden) {
			t.Errorf("runner-rust Containerfile contains %s", forbidden)
		}
	}
	var catalog []images.Image
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, "images/catalog.yaml")), &catalog); err != nil {
		t.Fatal(err)
	}
	index := slices.IndexFunc(catalog, func(image images.Image) bool { return image.Image == "ghcr.io/fredrir/infra-runner-rust" })
	if index < 0 {
		t.Fatal("runner-rust missing from the image catalog")
	}
	for _, check := range []string{
		"test ! -e /etc/ssl/certs/object-store.pem;",
		`if grep -qxF "$(sed -n 2p "$OBJECT_STORE_CA_FILE")" /etc/ssl/certs/ca-certificates.crt; then exit 1; fi;`,
		`grep -qxF "$(sed -n 2p "$OBJECT_STORE_CA_FILE")" /usr/local/share/object-store/trust.pem;`,
		"grep -qF 'SSL_CERT_FILE=/usr/local/share/object-store/trust.pem exec /usr/local/libexec/sccache' /usr/local/bin/sccache;",
		`test "$SCCACHE_IDLE_TIMEOUT" = 0;`,
		`test -z "${SCCACHE_S3_USE_SSL+set}";`,
	} {
		if !strings.Contains(catalog[index].Check, check) {
			t.Errorf("runner-rust image check lacks %s", check)
		}
	}
}
