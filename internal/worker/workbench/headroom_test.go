package workbench

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDockerMemoryUsageRejectsIncompleteOrInvalidSamples(t *testing.T) {
	for _, sample := range []string{
		`{"MemUsage":"-- / --"}`,
		`{"MemUsage":"2MiB / 4GiB"}` + "\n" + `{"MemUsage":"-- / --"}`,
		`{}`, `not json`, `{"MemUsage":"-1MiB / 4GiB"}`,
		`{"MemUsage":"NaNMiB / 4GiB"}`, `{"MemUsage":"+InfGiB / 4GiB"}`,
		strings.Repeat("{\"MemUsage\":\"1152921504606846976B / 4GiB\"}\n", 16),
	} {
		if used, err := dockerMemoryUsage([]byte(sample)); err == nil || used != 0 {
			t.Fatalf("incomplete sample admitted: %q: %d, %v", sample, used, err)
		}
	}
	used, err := dockerMemoryUsage([]byte("{\"MemUsage\":\"1GiB / 4GiB\"}\n{\"MemUsage\":\"512MiB / 4GiB\"}\n"))
	if err != nil || used != 1536<<20 {
		t.Fatalf("complete sample: %d, %v", used, err)
	}
	if used, err := dockerMemoryUsage(nil); used != 0 || err != nil {
		t.Fatalf("empty daemon: %d, %v", used, err)
	}
}

// Exercise the actual Docker subprocess boundary, including daemon identity,
// without creating containers or relying on the test host's memory pressure.
func statsFixture(t *testing.T, samples []string, changedDaemon bool) (Docker, Runtime, func() int) {
	t.Helper()
	dir := t.TempDir()
	for i, sample := range samples {
		if err := os.WriteFile(filepath.Join(dir, "sample-"+strconv.Itoa(i+1)), []byte(sample), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "count"), []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -eu
cd "$(dirname "$0")"
test "$1" = --host
test "$2" = unix:///fixture-docker.sock
shift 2
count=$(cat count)
case "$1" in
  info)
    if test -f change-daemon && test "$count" -gt 0; then echo replaced; else echo fixture-daemon; fi
    ;;
  stats)
    count=$((count + 1))
    printf '%s\n' "$count" > count
    if test -f "sample-$count"; then cat "sample-$count"; else cat sample-1; fi
    ;;
  *) exit 91 ;;
esac
`
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if changedDaemon {
		if err := os.WriteFile(filepath.Join(dir, "change-daemon"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return Docker{Binary: path}, Runtime{ID: "fixture-daemon", Endpoint: "unix:///fixture-docker.sock"}, func() int {
		b, err := os.ReadFile(filepath.Join(dir, "count"))
		if err != nil {
			t.Fatal(err)
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
}

func TestDockerMemoryUsageWaitsForCompleteSample(t *testing.T) {
	d, runtime, count := statsFixture(t, []string{`{"MemUsage":"-- / --"}`, `{"MemUsage":"512MiB / 4GiB"}`}, false)
	used, err := d.memoryUsage(context.Background(), runtime)
	if err != nil || used != 512<<20 || count() != 2 {
		t.Fatalf("sample did not recover: %d, %v, calls %d", used, err, count())
	}
}

func TestDockerMemoryUsageKeepsRefusingMissingSample(t *testing.T) {
	d, runtime, count := statsFixture(t, []string{`{"MemUsage":"-- / --"}`}, false)
	used, err := d.memoryUsage(context.Background(), runtime)
	if !errors.Is(err, errDockerStatsUnavailable) || used != 0 || count() != 3 {
		t.Fatalf("missing sample admitted or retry unbounded: %d, %v, calls %d", used, err, count())
	}
}

func TestDockerMemoryUsageDoesNotRetryMalformedMeasurement(t *testing.T) {
	d, runtime, count := statsFixture(t, []string{`{"MemUsage":"-1GiB / 4GiB"}`, `{"MemUsage":"1MiB / 4GiB"}`}, false)
	if _, err := d.memoryUsage(context.Background(), runtime); err == nil || count() != 1 {
		t.Fatalf("malformed reading retried: %v, calls %d", err, count())
	}
}

func TestDockerMemoryUsageRechecksDaemonBeforeRetry(t *testing.T) {
	d, runtime, count := statsFixture(t, []string{`{"MemUsage":"-- / --"}`, `{"MemUsage":"1MiB / 4GiB"}`}, true)
	if _, err := d.memoryUsage(context.Background(), runtime); !errors.Is(err, ErrInterrupted) || count() != 1 {
		t.Fatalf("replacement daemon accepted: %v, calls %d", err, count())
	}
}

func TestDockerMemoryUsageCancellationStopsRetry(t *testing.T) {
	d, runtime, count := statsFixture(t, []string{`{"MemUsage":"-- / --"}`}, false)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := d.memoryUsage(ctx, runtime); err == nil || time.Since(started) > time.Second || count() > 1 {
		t.Fatalf("cancellation ignored: %v, calls %d", err, count())
	}
}
