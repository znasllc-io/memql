package appsession

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// process.go supervises the app process for one session.
//
// The child runs in its OWN PROCESS GROUP, and cancel kills the group
// rather than the direct child. That is the whole point of the cancel
// path: `claude` spawns tools, which spawn compilers and test runners,
// and a cancel that reaps only the process the cockpit forked leaves an
// agent running on somebody's machine with nothing watching it. The
// engine sends cancel when the user asks, when the calling plan's context
// dies, and when the kill switch flips -- all three are statements that
// the work must STOP, not that the cockpit should stop looking.

const (
	// terminateGrace is how long the group gets to exit on SIGTERM
	// before SIGKILL. An agent mid-write deserves the chance to finish
	// the line; it does not get to ignore the signal.
	terminateGrace = 5 * time.Second

	// readChunkBytes is the largest single read from a stream.
	readChunkBytes = 32 << 10

	// flushPartialAfter forces a partial line out even without a
	// newline, so a run that prints a progress bar for ten minutes is
	// not silent for ten minutes.
	flushPartialAfter = 2 * time.Second

	// maxLineBytes caps a single logical line before it is flushed
	// regardless. Without it, an app that emits one enormous JSON object
	// with no newline would buffer forever.
	maxLineBytes = 1 << 20
)

// child is a supervised app process.
//
// The child owns its own reaping. os/exec writes Cmd.ProcessState from
// inside Wait, so anything that inspects the process to decide whether to
// escalate a signal races that write -- and the cancel path does exactly
// that inspection while the run path is blocked in Wait. One Wait behind
// a sync.Once, publishing through a channel, removes the question.
type child struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	waitOnce sync.Once
	waitErr  error
	exited   chan struct{}

	killOnce sync.Once
}

// startChild launches argv in dir with the session's extra environment
// and the child's stdin CLOSED.
//
// The child inherits the worker's environment plus extraEnv, because an
// app run needs the user's PATH, HOME and app credentials to work at all.
// The session's own additions (CODEX_HOME) come last so they win.
func startChild(dir string, argv []string, extraEnv []string) (*child, error) {
	return startChildStdin(dir, argv, extraEnv, false)
}

// startChildStdin is startChild with the stdin decision stated out loud.
//
// WHO IS ALLOWED TO TYPE AT AN APP. The rule this preserves is not "a
// child never gets a stdin"; it is that the ENGINE'S STREAM never
// reaches one. An app a remote caller can type at is a much larger trust
// question than an app handed a prompt and left to run, and nothing in
// this package forwards a chunk, a control or a credential into a
// child's input. A local JSON-RPC client that owns BOTH ends of a pipe
// it opened is not that: it is the protocol. `codex app-server` and
// `codex mcp-server` speak JSON-RPC 2.0 over the child's stdio, so for
// them a closed stdin is not a safety property -- it is a client with no
// way to say anything, and the session dies at the handshake. Claude print
// turns also opt in to send one finite text prompt followed by EOF; the
// harness owns that write, without forwarding interactive engine input.
//
// So the pipe is OPT-IN and the default stays closed, because the
// default's failure mode is the useful one: a prompt-on-stdin app whose
// stdin is closed fails fast, where the same app holding an open pipe
// nobody will ever write to hangs until the wall-clock limit and reports
// nothing at all.
func startChildStdin(dir string, argv []string, extraEnv []string, stdin bool) (*child, error) {
	if len(argv) == 0 {
		return nil, errors.New("app session: empty command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	// nil is the closed default: /dev/null, immediate EOF.
	cmd.Stdin = nil
	applyProcessGroup(cmd)

	c := &child{cmd: cmd, exited: make(chan struct{})}
	if stdin {
		w, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("app session: stdin: %w", err)
		}
		c.stdin = w
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("app session: stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("app session: stderr: %w", err)
	}
	c.stdout, c.stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("app session: start %s: %w", argv[0], err)
	}
	return c, nil
}

// wait reaps the process and returns its exit error. Safe to call from
// several goroutines; only the first actually calls Wait.
func (c *child) wait() error {
	c.waitOnce.Do(func() {
		c.waitErr = c.cmd.Wait()
		close(c.exited)
	})
	return c.waitErr
}

// terminate stops the whole process group: SIGTERM, a grace window, then
// SIGKILL. Safe to call more than once and from more than one goroutine
// -- cancel, the duration limit and stream loss are independent events
// and any of them must be sufficient on its own.
func (c *child) terminate() {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return
	}
	c.killOnce.Do(func() {
		// A JSON-RPC harness calls Terminate before Wait. Start the single reaper
		// here too, or exited cannot close and every clean turn pays the full grace.
		go func() { _ = c.wait() }()
		signalGroup(c.cmd, false)
		select {
		case <-c.exited:
			// The leader exited; reap any descendants that ignored the signal.
			signalGroup(c.cmd, true)
		case <-time.After(terminateGrace):
			signalGroup(c.cmd, true)
		}
	})
}

// exitCode returns the process's real exit code.
//
// The real one, not a normalised one: the engine reads a non-zero exit as
// a FAILED run rather than an ended one, and flattening a 2 to a 1 -- or
// to a 0 -- would misfile the outcome in the ledger.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	// Not an exit status at all: the process could not be waited on.
	// -1 is what ExitCode() itself reports for "no exit status", so it
	// is the honest value here too.
	return -1
}

// streamReader pumps one of the child's streams, cutting it into chunks.
//
// Chunks are LINE-ORIENTED, which is not cosmetic. Every chunk passes
// through the redactor on its way out, and a bearer split across a chunk
// boundary would survive redaction in two halves. Lines bound that: the
// credential is a single token with no newline in it, so it is always
// wholly inside one chunk -- and where a partial line has to be flushed
// early, the tail is held back rather than emitted (see flushPartial).
type streamReader struct {
	name     string
	src      io.Reader
	emit     func(stream string, data []byte) error
	holdBack int
}

// run reads to EOF, emitting chunks. Returns the first emit error, which
// means the stream to the server is gone and the session is over.
func (r *streamReader) run() error {
	reader := bufio.NewReaderSize(r.src, readChunkBytes)
	var pending []byte
	lastFlush := time.Now()

	flush := func(force bool) error {
		if len(pending) == 0 {
			return nil
		}
		out := pending
		if !force && r.holdBack > 0 && len(out) > r.holdBack {
			// Hold back a credential-length tail so a secret straddling
			// this flush boundary is not emitted in two unredactable
			// halves.
			keep := len(out) - r.holdBack
			out = out[:keep]
			pending = pending[keep:]
		} else {
			pending = nil
		}
		if len(out) == 0 {
			return nil
		}
		lastFlush = time.Now()
		return r.emit(r.name, out)
	}

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			pending = append(pending, line...)
			complete := line[len(line)-1] == '\n'
			if complete || len(pending) >= maxLineBytes {
				if ferr := flush(complete); ferr != nil {
					return ferr
				}
			} else if time.Since(lastFlush) >= flushPartialAfter {
				if ferr := flush(false); ferr != nil {
					return ferr
				}
			}
		}
		if err != nil {
			if ferr := flush(true); ferr != nil {
				return ferr
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			// A closed pipe on teardown is the normal end of a killed
			// run, not a failure worth naming.
			if errors.Is(err, os.ErrClosed) || strings.Contains(err.Error(), "file already closed") {
				return nil
			}
			return nil
		}
	}
}
