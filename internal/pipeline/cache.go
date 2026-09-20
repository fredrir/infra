package pipeline

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
)

func localCache(raw string) (string, string, int, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid local cache URL")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", "", 0, fmt.Errorf("cache forwarding requires a loopback address")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "grpc" {
		return "", "", 0, fmt.Errorf("cache forwarding supports loopback HTTP or gRPC")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", 0, fmt.Errorf("cache forwarding URL cannot contain credentials, query, or fragment")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", "", 0, fmt.Errorf("cache forwarding requires an explicit valid port")
	}
	parsed.Host = net.JoinHostPort("infra-cache", strconv.Itoa(port))
	return parsed.String(), host, port, nil
}
