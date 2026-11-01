//go:build computeruse

package tools

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestDesktopRuntimeFailureClosesOnlyDesktop(t *testing.T) {
	if os.Getenv("MEMQL_TEST_DESKTOP_UNAVAILABLE") == "1" {
		backend := &fakeInputBackend{}
		desktopInputGuard.backend = backend
		DisableDesktopRuntime()
		desc := ComputeCapabilities()
		if desc.ComputerUseAvailable || desc.DisplayServer != "none" || desc.Displays != 0 ||
			len(desc.Actions) != 1 || desc.Actions[0] != "wait" || desc.InputTargetPolicy != nil {
			t.Fatalf("unavailable desktop was advertised: %+v", desc)
		}
		if desc.ActionContracts["workerHost.pipeline_step"] == 0 || desc.ActionContracts["workerHost.fs_read"] == 0 {
			t.Fatal("desktop failure removed host contracts")
		}
		for action := range computerActionHandlers {
			_, fail := (&Dispatcher{}).dispatchComputer(context.Background(), action, nil)
			if fail == nil || fail.ErrorCode != "desktop_unavailable" {
				t.Fatalf("%s reached an unavailable desktop: %v", action, fail)
			}
		}
		if len(backend.events) != 0 || !desktopInputGuard.halted {
			t.Fatal("startup failure did not stop input")
		}
		return
	}
	// The production disable switch is deliberately irreversible. Isolate it
	// in a child instead of introducing a reset seam into the runtime.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDesktopRuntimeFailureClosesOnlyDesktop$")
	cmd.Env = append(os.Environ(), "MEMQL_TEST_DESKTOP_UNAVAILABLE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unavailable desktop boundary: %v\n%s", err, out)
	}
}
