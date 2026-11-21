package main

import (
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/sdk/go/client"
)

func configuration(env func(string) string) (client.ConnectConfig, error) {
	var cfg client.ConnectConfig
	invalid := errors.New("invalid_configuration")
	raw := strings.TrimSpace(env("MEMQL_WORKER_ENDPOINT"))
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return cfg, invalid
	}
	secure := u.Scheme == "https" || u.Scheme == "grpcs"
	if !secure {
		ip := net.ParseIP(u.Hostname())
		if (u.Scheme != "http" && u.Scheme != "grpc") || env("MEMQL_WORKER_ALLOW_LOCAL") != "1" || u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return cfg, invalid
		}
		// An explicit local override never resolves an arbitrary hostname.
		if u.Hostname() == "localhost" {
			port := u.Port()
			if port == "" {
				port = "50051"
			}
			u.Host = net.JoinHostPort("127.0.0.1", port)
		}
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return cfg, invalid
		}
	}
	token := env("MEMQL_WORKER_OPERATOR_TOKEN")
	if token == "" || len(token) > 16384 || strings.ContainsAny(token, " \t\r\n") {
		return cfg, invalid
	}
	cfg.Endpoint, cfg.Token = u.String(), token
	if filename := env("MEMQL_WORKER_CA_FILE"); filename != "" {
		if !secure {
			return client.ConnectConfig{}, invalid
		}
		file, err := os.Open(filename)
		if err != nil {
			return client.ConnectConfig{}, invalid
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
			return client.ConnectConfig{}, invalid
		}
		pem, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
		pool := x509.NewCertPool()
		if err != nil || len(pem) > 1024*1024 || !pool.AppendCertsFromPEM(pem) {
			return client.ConnectConfig{}, invalid
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}
