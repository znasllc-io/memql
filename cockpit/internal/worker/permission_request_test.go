package worker

import (
	"os"
	"testing"
	"time"
)

func TestPermissionRequestRejectsUnsupportedStaleAndInvalidWithoutPrompt(t *testing.T) {
	for _, tc := range []struct {
		name, permission string
		pid              int
		supported        bool
	}{
		{"unknown permission", "all", os.Getpid(), true},
		{"stale worker", "accessibility", os.Getpid() + 1, true},
		{"missing worker", "screen_recording", 0, true},
		{"headless worker", "screen_recording", os.Getpid(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gate permissionRequestGate
			called := make(chan string, 1)
			if err := gate.request(tc.permission, tc.pid, tc.supported, func(p string) { called <- p }); err == nil {
				t.Fatal("unsafe request accepted")
			}
			if gate.pending.Load() || len(called) != 0 {
				t.Fatal("refused request queued an OS prompt")
			}
		})
	}
}

func TestPermissionRequestAcknowledgesWithoutWaitingAndSerializesDialogs(t *testing.T) {
	var gate permissionRequestGate
	entered, release := make(chan string, 1), make(chan struct{})
	defer close(release)
	returned := make(chan error, 1)
	go func() {
		returned <- gate.request("accessibility", os.Getpid(), true, func(p string) {
			entered <- p
			<-release
		})
	}()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("request waited for OS dialog instead of acknowledging")
	}
	select {
	case p := <-entered:
		if p != "accessibility" {
			t.Fatal(p)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted prompt did not execute")
	}
	if err := gate.request("screen_recording", os.Getpid(), true, func(string) { t.Error("second dialog executed") }); err == nil {
		t.Fatal("concurrent dialog accepted")
	}
}

func TestPermissionRequestReleasesGateAfterDialog(t *testing.T) {
	var gate permissionRequestGate
	done := make(chan struct{})
	if err := gate.request("screen_recording", os.Getpid(), true, func(string) { close(done) }); err != nil {
		t.Fatal(err)
	}
	<-done
	eventuallyControl(t, func() bool { return !gate.pending.Load() })
	if err := gate.request("accessibility", os.Getpid(), true, func(string) {}); err != nil {
		t.Fatal(err)
	}
	eventuallyControl(t, func() bool { return !gate.pending.Load() })
}
