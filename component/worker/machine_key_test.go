package worker

import (
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func TestMachineKeyPrefersStableMachineId(t *testing.T) {
	got := MachineKeyFromLabelsAndPlatform(map[string]string{
		LabelMachineId: "uuid-1",
		"hostname":     "ignored",
	}, map[string]any{"hostname": "MacBook", "os": "darwin", "arch": "arm64"})
	if got != "id:uuid-1" {
		t.Fatalf("got %q, want id:uuid-1", got)
	}
}

func TestMachineKeyFallsBackToHostOSArch(t *testing.T) {
	got := MachineKeyFromRegister(&memqlv1.Register{
		Platform: &memqlv1.PlatformInfo{Hostname: "MacBook.local", Os: "darwin", Arch: "arm64"},
	})
	if got != "host:macbook.local|darwin|arm64" {
		t.Fatalf("got %q", got)
	}
}

func TestMachineKeyEmptyWhenIncomplete(t *testing.T) {
	if got := MachineKeyFromRegister(&memqlv1.Register{Platform: &memqlv1.PlatformInfo{Hostname: "only-host"}}); got != "" {
		t.Fatalf("incomplete platform must not invent a key, got %q", got)
	}
}
