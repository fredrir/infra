package deployment

import (
	"errors"
	"fmt"
	"testing"
)

func TestUnavailableOutputSeparatesOutagesFromRejections(t *testing.T) {
	for output, want := range map[string]bool{
		"Error: failed to fetch attestations: HTTP 502: Bad Gateway":                                                 true,
		"Error: GET https://ghcr.io/v2/fredrir/y/manifests/sha256-0: unexpected status code 503 Service Unavailable": true,
		"Error: HTTP 429: Too Many Requests":                                                                         true,
		"Error: API rate limit exceeded for installation":                                                            true,
		"dial tcp: lookup ghcr.io on 127.0.0.53:53: no such host":                                                    true,
		"net/http: TLS handshake timeout":                                                                            true,
		"read tcp 10.0.0.1:443: read: connection reset by peer":                                                      true,
		"fatal: unable to access 'https://github.com/fredrir/infra.git/': Could not resolve host: github.com":        true,
		"error: RPC failed; HTTP 500 curl 22 The requested URL returned error: 500":                                  true,
		"Error: no matching attestations found":                                                                      false,
		"Error: none of the expected identities matched what was in the certificate":                                 false,
		"fatal: remote error: upload-pack: not our ref 0123456789abcdef":                                             false,
		"Error: GET https://ghcr.io/v2/fredrir/y/manifests/sha256-0: MANIFEST_UNKNOWN: manifest unknown":             false,
		"Error: HTTP 401: Bad credentials":                                                                           false,
		"Error: HTTP 404: Not Found":                                                                                 false,
	} {
		if got := UnavailableOutput(output); got != want {
			t.Errorf("UnavailableOutput(%q) = %v, want %v", output, got, want)
		}
	}
}

func TestUnavailableWrapsOnce(t *testing.T) {
	cause := errors.New("HTTP 502")
	wrapped := Unavailable(Unavailable(cause))
	if !errors.Is(wrapped, ErrSourceUnavailable) || !errors.Is(wrapped, cause) || wrapped.Error() != "source unavailable: HTTP 502" || Unavailable(nil) != nil {
		t.Fatalf("wrapped %v", wrapped)
	}
	if errors.Is(fmt.Errorf("rejected: %w", cause), ErrSourceUnavailable) {
		t.Fatal("an unwrapped error is unavailable")
	}
}
