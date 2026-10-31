//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func endpointDockerFixture(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	previous := dockerBinary
	dockerBinary = path
	t.Cleanup(func() { dockerBinary = previous })
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
}

func TestPipelineDockerEndpointResolutionAndCommandBinding(t *testing.T) {
	endpointDockerFixture(t, `test "$1" = context
test "$2" = inspect
printf '"unix:///tmp/context-selected.sock"'`)
	t.Setenv("DOCKER_HOST", "unix:///tmp/host-selected.sock")
	endpoint, err := resolvePipelineDockerEndpoint(context.Background())
	if err != nil || endpoint != "unix:///tmp/host-selected.sock" {
		t.Fatalf("host selection: %q %v", endpoint, err)
	}
	t.Setenv("DOCKER_CONTEXT", "selected-context")
	endpoint, err = resolvePipelineDockerEndpoint(context.Background())
	if err != nil || endpoint != "unix:///tmp/context-selected.sock" {
		t.Fatalf("context precedence: %q %v", endpoint, err)
	}
	t.Setenv(workerTokenVariable, "worker-credential")
	ctx := withPipelineDockerEndpoint(context.Background(), endpoint)
	cmd, err := dockerCommandAt(ctx, "start", "attempt")
	if err != nil || !reflect.DeepEqual(cmd.Args[1:], []string{"--host", endpoint, "start", "attempt"}) {
		t.Fatalf("bound command: %v %v", cmd, err)
	}
	for _, entry := range cmd.Env {
		if strings.HasPrefix(entry, "DOCKER_CONTEXT=") || strings.HasPrefix(entry, workerTokenVariable+"=") || entry == "DOCKER_HOST=unix:///tmp/host-selected.sock" {
			t.Fatalf("ambient selector or credential survived: %s", entry)
		}
	}
	if _, err := dockerCommandAt(context.Background(), "rm", "attempt"); err == nil {
		t.Fatal("unbound command would use the mutable default context")
	}
	for _, endpoint := range []string{"", "tcp://127.0.0.1:2375", "ssh://worker", "unix://relative", "unix:///tmp/socket\n", "unix:///tmp/socket?context=other"} {
		if _, err := dockerCommandAt(withPipelineDockerEndpoint(context.Background(), endpoint), "info"); err == nil {
			t.Fatalf("unsafe endpoint accepted: %q", endpoint)
		}
	}
}

// This fake changes its active context immediately after discovery. Every
// measurement, command/service operation and cleanup must still use socket A.
func TestPipelineAttemptPinsAllDockerOperationsAcrossContextChange(t *testing.T) {
	fx := newPipelineFixture(t)
	allowLocalClones(t)
	policy, _ := pipelineTestPolicy(t, "")
	log := filepath.Join(t.TempDir(), "calls")
	selected := filepath.Join(t.TempDir(), "selected")
	if err := os.WriteFile(selected, []byte("unix:///tmp/socket-a.sock"), 0600); err != nil {
		t.Fatal(err)
	}
	endpointDockerFixture(t, `if [ "$1" = context ]; then
  printf 'context\n' >> `+shellLiteral(log)+`
  printf '"%s"' "$(cat `+shellLiteral(selected)+`)"
  printf 'unix:///tmp/socket-b.sock' > `+shellLiteral(selected)+`
  exit 0
fi
test "$1" = --host
test "$2" = unix:///tmp/socket-a.sock
test "${DOCKER_CONTEXT+x}" != x
test "$DOCKER_HOST" = unix:///tmp/socket-a.sock
shift 2
printf '%s\n' "$*" >> `+shellLiteral(log)+`
case "$1" in
info) case "$3" in
  '{{.ID}}') echo daemon-a;;
  *cpus*) printf '{"id":"daemon-a","cpus":32,"memory":68719476736}';;
  *) printf 'daemon-a\nlinux/arm64';;
  esac;;
stats) printf '1GiB / 64GiB\n';;
create) echo created;;
image) echo linux/arm64;;
start) if [ "$2" = --attach ]; then cat >/dev/null; fi;;
inspect) case "$4" in
  *-svc-*) printf '{"Running":true}';;
  *) printf '{"Status":"exited","Running":false,"ExitCode":0}';;
  esac;;
rm) exit 1;;
ps) exit 0;;
network) if [ "$2" = rm ]; then exit 1; fi;;
*) exit 9;;
esac`)
	t.Setenv("DOCKER_CONTEXT", "context-that-will-change")
	extra := map[string]any{"execution": "container", "platform": "linux/arm64", "image": containerTestDigest,
		"services": map[string]any{"database": map[string]any{"image": containerTestDigest}}}
	result := decoded(t)(runPipelineStep(context.Background(), "", stepArgs(t, fx, "exit 0", extra), policy, nil, nil))
	if result.ExitCode != 0 {
		t.Fatalf("command exit: %d", result.ExitCode)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "context\n") != 1 {
		t.Fatalf("endpoint rediscovered during the attempt: %s", calls)
	}
	for _, operation := range []string{"info ", "stats ", "create ", "image inspect ", "start --attach ", "inspect ", "network create ", "rm ", "ps ", "network rm ", "network ls "} {
		if !strings.Contains(string(calls), operation) {
			t.Errorf("operation missing: %s in %s", operation, calls)
		}
	}
}

func TestPipelineRecoveryRefusesEndpointlessLegacyRecord(t *testing.T) {
	reservation := testPipelineReservation(t)
	record := pipelineAttemptRecord{Execution: "container", DockerID: "daemon-a", Container: "memql-step-" + strings.Repeat("a", 32), Workspace: pipelineTestWorkspace(t)}
	if err := reservation.begin(record); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "docker-called")
	endpointDockerFixture(t, "touch "+shellLiteral(marker))
	if err := reconcilePipelineReservation(context.Background(), reservation); !errors.Is(err, errPipelineUnreconciled) || !reservation.dirty {
		t.Fatalf("legacy attempt released: %v dirty=%v", err, reservation.dirty)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("legacy recovery discovered or mutated an unrelated Docker endpoint")
	}
	retained, err := reservation.read()
	if err != nil || !reflect.DeepEqual(retained, record) {
		t.Fatalf("legacy evidence changed: %+v %v", retained, err)
	}
}

func TestPipelineRecoveryRetainsRecordWhenDaemonChangesDuringRemoval(t *testing.T) {
	reservation := testPipelineReservation(t)
	record := pipelineAttemptRecord{Execution: "container", DockerID: "daemon-a", DockerEndpoint: "unix:///tmp/socket-a.sock", Container: "memql-step-" + strings.Repeat("a", 32), Workspace: pipelineTestWorkspace(t)}
	if err := reservation.begin(record); err != nil {
		t.Fatal(err)
	}
	removed := filepath.Join(t.TempDir(), "removed")
	endpointDockerFixture(t, `test "$1" = --host
test "$2" = unix:///tmp/socket-a.sock
shift 2
case "$1" in
info) if [ -f `+shellLiteral(removed)+` ]; then echo daemon-b; else echo daemon-a; fi;;
rm) touch `+shellLiteral(removed)+`;;
*) exit 9;;
esac`)
	t.Setenv("DOCKER_HOST", "unix:///tmp/unrelated.sock")
	t.Setenv("DOCKER_CONTEXT", "unrelated-context")
	if err := reconcilePipelineReservation(context.Background(), reservation); err == nil || !reservation.dirty {
		t.Fatalf("replacement daemon falsely proved cleanup: %v dirty=%v", err, reservation.dirty)
	}
	if _, err := os.Stat(record.Workspace); err != nil {
		t.Fatalf("workspace evidence removed: %v", err)
	}
}
