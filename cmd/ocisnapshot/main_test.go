package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSourcePinRequiresExplicitPublicDigest(t *testing.T) {
	source := "index.docker.io/timescale/timescaledb@sha256:" + strings.Repeat("a", 64)
	valid := `{"version":"16.0.1","platforms":["linux/amd64","linux/arm64"],"source":"` + source + `"}`
	if _, _, err := parsePin([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		strings.Replace(valid, source, "timescale/timescaledb:latest-pg16", 1),
		strings.Replace(valid, "index.docker.io", "localhost:5000", 1),
		strings.Replace(valid, "index.docker.io", "user:password@ghcr.io", 1),
		strings.Replace(valid, "16.0.1", "016.0.1", 1),
		strings.Replace(valid, "16.0.1", "16.0.1-dev", 1),
		strings.Replace(valid, `"version":`, `"source":"other","version":`, 1),
		strings.Replace(valid, `"source":`, `"credential":"secret","source":`, 1),
		strings.Replace(valid, `,"platforms":["linux/amd64","linux/arm64"]`, "", 1),
		strings.Replace(valid, "linux/arm64", "linux/amd64", 1),
		valid + `{}`, "null", strings.Repeat(" ", 4097),
	} {
		if _, _, err := parsePin([]byte(raw)); err == nil {
			t.Fatal("ambiguous/unpinned/private source accepted")
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSnapshotTransportRefusesWritesAndBoundsChunkedBodies(t *testing.T) {
	var calls atomic.Int64
	tpt := &readTransport{inner: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("12345")), Header: http.Header{}, Request: r}, nil
	})}
	tpt.remaining.Store(4)
	for _, method := range []string{"PUT", "POST", "PATCH", "DELETE"} {
		r, _ := http.NewRequest(method, "https://ghcr.io/v2/", nil)
		if _, err := tpt.RoundTrip(r); err == nil {
			t.Fatal("write admitted")
		}
	}
	r, _ := http.NewRequest("GET", "http://ghcr.io/v2/", nil)
	if _, err := tpt.RoundTrip(r); err == nil {
		t.Fatal("plaintext admitted")
	}
	if calls.Load() != 0 {
		t.Fatal("refused request reached network")
	}
	r.URL.Scheme = "https"
	resp, err := tpt.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("unknown-length body exceeded global budget")
	}
}

func TestSnapshotPreservesExistingOutputAndCleansCancellation(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "retained")
	if err := os.WriteFile(output, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	pin, ref, err := parsePin([]byte(`{"version":"16.0.1","platforms":["linux/amd64","linux/arm64"],"source":"ghcr.io/example/fixture@sha256:` + strings.Repeat("a", 64) + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot(context.Background(), pin, ref, output); err == nil {
		t.Fatal("existing output replaced")
	}
	b, err := os.ReadFile(output)
	if err != nil || string(b) != "keep" {
		t.Fatal("existing output changed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := snapshot(ctx, pin, ref, filepath.Join(dir, "cancelled")); err == nil {
		t.Fatal("cancelled download succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "retained" {
		t.Fatal("partial snapshot left behind", entries, err)
	}
}
