//go:build darwin && computeruse

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// TestMain runs on the process's original thread (locked by the platform
// entrypoint). This child exercises scheduling/shutdown only: no AX reads,
// application activation, screenshots or input events.
func TestMain(m *testing.M) {
	if os.Getenv("MEMQL_TEST_DESKTOP_LOOP_CHILD") == "1" {
		progress := false
		err := tools.RunDesktopMainLoop(func() {
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				running, turns := tools.DesktopMainLoopProgress()
				if running && turns >= 2 {
					progress = true
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
		running, _ := tools.DesktopMainLoopProgress()
		if err != nil || !progress || running {
			fmt.Fprintf(os.Stderr, "loop lifecycle: err=%v progress=%t running=%t\n", err, progress, running)
			os.Exit(1)
		}
		fmt.Println("desktop main loop progressed and stopped")
		return
	}
	os.Exit(m.Run())
}

func TestDesktopMainLoopProgressAndShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "MEMQL_TEST_DESKTOP_LOOP_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "desktop main loop progressed and stopped") {
		t.Fatalf("main-thread loop lifecycle failed: %v\n%s", err, out)
	}
}
