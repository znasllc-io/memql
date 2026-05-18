package client

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ParseClusterEndpoint accepts a raw endpoint string in any of the
// shapes the user is likely to paste into clusters.yaml or the Add /
// Edit form, and returns the gRPC dial address (host:port) plus a
// flag indicating whether the transport must be TLS.
//
// Accepted shapes:
//
//	bare host:port      -> ("host:port", false)              -- plaintext
//	host                -> ("host:50051", false)             -- plaintext, default gRPC port
//	grpc://host:port    -> ("host:port", false)              -- plaintext
//	http://host:port    -> ("host:port", false)              -- plaintext
//	grpcs://host[:port] -> ("host:port_or_443", true)        -- TLS
//	https://host[:port] -> ("host:port_or_443", true)        -- TLS
//
// The canonical memql client endpoint behind NGINX is
// https://bff.${DOMAIN}, which parses to ("bff.${DOMAIN}:443", true).
// The cluster-mode plaintext path (docker-compose.cluster.yml exposes
// 50050:50051) parses to ("localhost:50050", false) when the user
// pastes "localhost:50050". Both round-trip cleanly.
//
// Empty inputs and parse failures return a typed error -- the caller
// can surface them through the cluster-add form's validation lane.
func ParseClusterEndpoint(raw string) (dial string, useTLS bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false, errors.New("endpoint is empty")
	}

	// Bare host:port (or just host) -- no scheme. Plaintext gRPC.
	if !strings.Contains(raw, "://") {
		host := raw
		if !strings.Contains(host, ":") {
			host = host + ":50051"
		}
		return host, false, nil
	}

	u, parseErr := url.Parse(raw)
	if parseErr != nil {
		return "", false, fmt.Errorf("parse %q: %w", raw, parseErr)
	}
	host := u.Host
	if host == "" {
		return "", false, fmt.Errorf("endpoint %q missing host", raw)
	}

	switch strings.ToLower(u.Scheme) {
	case "http", "grpc":
		if !strings.Contains(host, ":") {
			host = host + ":50051"
		}
		return host, false, nil
	case "https", "grpcs":
		if !strings.Contains(host, ":") {
			host = host + ":443"
		}
		return host, true, nil
	default:
		return "", false, fmt.Errorf("endpoint %q has unsupported scheme %q (use http, https, grpc, grpcs, or bare host:port)", raw, u.Scheme)
	}
}
