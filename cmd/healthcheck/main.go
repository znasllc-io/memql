// Package main provides a minimal health check binary for distroless containers.
// It performs an HTTP GET to http://localhost:{port}/healthz and exits with
// code 0 on success (2xx) or code 1 on failure.
//
// Usage: healthcheck [port]
// Default port: 8085
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	port := "8085"
	if len(os.Args) > 1 {
		port = os.Args[1]
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://localhost:%s/healthz", port))
	if err != nil {
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		os.Exit(0)
	}
	os.Exit(1)
}
