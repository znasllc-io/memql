//go:build computeruse && darwin

package tools

/*
extern int targetMainLoopStart(void);
extern void targetMainLoopTurn(void);
extern void targetMainLoopStop(void);
extern int targetMainLoopRunning(void);
extern unsigned long targetMainLoopTurns(void);
*/
import "C"

import "fmt"

// RunDesktopMainLoop must be called on the original, locked process thread.
// The worker owns its usual cancellation and shutdown in the callback; its
// return ends the loop. AppKit initialization never relaxes the input guard.
func RunDesktopMainLoop(run func()) error {
	if code := C.targetMainLoopStart(); code != 1 {
		return fmt.Errorf("cannot initialize the background desktop worker main loop (stage %d)", int(code))
	}
	defer C.targetMainLoopStop()
	desktopMainJobs.start()
	defer desktopMainJobs.stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		run()
	}()
	for {
		select {
		case <-done:
			return nil
		case job := <-desktopMainJobs.queue:
			job.execute()
		default:
			C.targetMainLoopTurn()
		}
	}
}

// DesktopMainLoopProgress reports scheduling metadata, never UI contents.
func DesktopMainLoopProgress() (running bool, turns uint64) {
	return C.targetMainLoopRunning() != 0, uint64(C.targetMainLoopTurns())
}
