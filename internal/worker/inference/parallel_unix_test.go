//go:build linux || darwin

package inference

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// readUnitWithin runs managedParallelAt and fails the test, rather than the
// whole run, when it does not come back: reading a FIFO blocks until
// something writes to it, and that is the bug under test.
func readUnitWithin(t *testing.T, unit string, limit time.Duration) int {
	t.Helper()
	got := make(chan int, 1)
	go func() { got <- managedParallelAt(unit) }()
	select {
	case n := <-got:
		return n
	case <-time.After(limit):
		t.Fatalf("managedParallelAt(%s) did not return within %v: it is blocked reading something that is not a regular file", unit, limit)
		return 0
	}
}

// releaseFIFO opens the pipe for writing when the test ends, so a reader the
// test abandoned (the failing case) is let go instead of leaking for the life
// of the test binary. It never blocks: O_NONBLOCK fails with ENXIO when
// nobody is reading, which is the passing case.
func releaseFIFO(t *testing.T, path string) {
	t.Helper()
	t.Cleanup(func() {
		if f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	})
}

// TestManagedParallelAtReadsRegularFilesOnly. The drop-in directory is the
// owner's, and an entry in it that is not a regular file must not stop
// discovery. managedParallelAt runs under the model inventory's lock, on the
// path every model call and every registration takes, so one stray pipe named
// *.conf that blocked the open would hold the whole worker behind it.
// systemd reads a drop-in only if it is a regular file, or a link to one.
func TestManagedParallelAtReadsRegularFilesOnly(t *testing.T) {
	const limit = 2 * time.Second
	newUnit := func(t *testing.T) string {
		t.Helper()
		unit := filepath.Join(t.TempDir(), "memql-ollama.service")
		mustWriteFile(t, unit, "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=2\n")
		return unit
	}
	mkfifo := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		releaseFIFO(t, path)
	}

	t.Run("a pipe named like a drop-in is skipped, and the files beside it still count", func(t *testing.T) {
		unit := newUnit(t)
		mkfifo(t, unit+".d/20-pipe.conf")
		mustWriteFile(t, unit+".d/10-raise.conf", "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=3\n")
		mustWriteFile(t, unit+".d/30-raise.conf", "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=4\n")
		if got := readUnitWithin(t, unit, limit); got != 4 {
			t.Errorf("a pipe in the drop-in directory changed the answer to %d, want 4 from the drop-ins beside it", got)
		}
	})

	t.Run("a link to a regular file is followed", func(t *testing.T) {
		unit := newUnit(t)
		real := filepath.Join(t.TempDir(), "elsewhere.conf")
		mustWriteFile(t, real, "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=5\n")
		if err := os.MkdirAll(unit+".d", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, unit+".d/50-linked.conf"); err != nil {
			t.Skipf("symlink: %v", err)
		}
		if got := readUnitWithin(t, unit, limit); got != 5 {
			t.Errorf("a drop-in that is a link to a file was not read: got %d, want 5", got)
		}
	})

	t.Run("a link to a pipe is skipped", func(t *testing.T) {
		unit := newUnit(t)
		pipe := filepath.Join(t.TempDir(), "pipe")
		mkfifo(t, pipe)
		if err := os.MkdirAll(unit+".d", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(pipe, unit+".d/50-linked.conf"); err != nil {
			t.Skipf("symlink: %v", err)
		}
		if got := readUnitWithin(t, unit, limit); got != 2 {
			t.Errorf("a link to a pipe changed the answer to %d, want the unit's 2", got)
		}
	})

	t.Run("a link to nothing is skipped", func(t *testing.T) {
		unit := newUnit(t)
		if err := os.MkdirAll(unit+".d", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), unit+".d/50-dangling.conf"); err != nil {
			t.Skipf("symlink: %v", err)
		}
		if got := readUnitWithin(t, unit, limit); got != 2 {
			t.Errorf("a dangling link changed the answer to %d, want the unit's 2", got)
		}
	})

	t.Run("a directory named like a drop-in is skipped", func(t *testing.T) {
		unit := newUnit(t)
		if err := os.MkdirAll(unit+".d/50-dir.conf", 0o755); err != nil {
			t.Fatal(err)
		}
		if got := readUnitWithin(t, unit, limit); got != 2 {
			t.Errorf("a directory changed the answer to %d, want the unit's 2", got)
		}
	})

	t.Run("a unit that is a pipe says nothing", func(t *testing.T) {
		unit := filepath.Join(t.TempDir(), "memql-ollama.service")
		mkfifo(t, unit)
		if got := readUnitWithin(t, unit, limit); got != 0 {
			t.Errorf("a unit that is a pipe gave %d, want 0 (no managed runtime)", got)
		}
	})
}
