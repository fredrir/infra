package deployment

import (
	"errors"
	"fmt"
	"regexp"
)

var ErrSourceUnavailable = errors.New("source unavailable")

var unavailableOutput = regexp.MustCompile(`(?i)\b(?:5\d\d (?:internal server error|bad gateway|service unavailable|gateway time-?out)|(?:http|status|status code|returned error)[: ]+(?:429|5\d\d)\b|429 too many requests|too many requests|rate limit|i/o timeout|tls handshake timeout|timed out|timeout awaiting|connection reset|connection refused|no such host|temporary failure in name resolution|could not resolve host|failed to connect|unexpected eof|network is unreachable|rpc failed|early eof)`)

func UnavailableOutput(output string) bool {
	return unavailableOutput.MatchString(output)
}

func Unavailable(err error) error {
	if err == nil || errors.Is(err, ErrSourceUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
}
