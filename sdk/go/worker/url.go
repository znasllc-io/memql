package worker

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ParseClusterURL converts an operator-facing cluster URL into the
// `host:port` dial address + a TLS flag suitable for DialConfig.
//
// Accepts:
//   - `http://host[:port]` / `grpc://host[:port]`   -> useTLS=false
//   - `https://host[:port]` / `grpcs://host[:port]` -> useTLS=true (defaults :443)
//   - bare `host:port`                              -> useTLS=false
//   - bare `host`                                   -> useTLS=false
//
// Empty input is rejected. Schemes other than the four above pass
// through with useTLS=false; the caller can override DialConfig.UseTLS
// explicitly if a non-standard scheme is in play.
//
// IPv6 hostnames in `[::1]:443` form aren't expected from the
// cockpit's worker config (which writes a hostname into
// ~/.memql/worker.yaml), so this parser does not special-case them.
func ParseClusterURL(raw string) (endpoint string, useTLS bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false, errors.New("sdk/worker: cluster_url is empty")
	}
	if !strings.Contains(raw, "://") {
		return raw, false, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false, fmt.Errorf("sdk/worker: parse cluster_url: %w", err)
	}
	host := u.Host
	if host == "" {
		return "", false, fmt.Errorf("sdk/worker: cluster_url missing host: %s", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "grpc":
		return host, false, nil
	case "https", "grpcs":
		if !strings.Contains(host, ":") {
			host = host + ":443"
		}
		return host, true, nil
	default:
		return host, false, nil
	}
}
