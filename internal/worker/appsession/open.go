package appsession

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
)

// open.go implements the `open` session kind (memql-cockpit#350): launch
// the app FOR THE HUMAN, with the workspace open and the prompt loaded,
// rather than running it headless.
//
// THE FAILURE PATH IS THE WHOLE POINT. An `open` that cannot launch the
// app ends the session immediately with a non-empty error. It must not
// return success and wait for a window that will never appear, must not
// fall back to a headless run -- the user asked to drive it themselves --
// and must not exit silently. An `open` that did nothing is
// indistinguishable from one the user ignored, and the plan waiting on it
// has no way to tell those apart.
//
// So this preflights before it launches: the app binary is resolved on
// PATH, the platform's terminal is resolved, and a display is confirmed
// where one is needed. Each of those failures names itself.

// openWatchInterval is how often the exit file is checked while the
// launcher runs.
const openWatchInterval = 250 * time.Millisecond

// launchOpen puts the app in front of the human and waits for it to
// finish. Returns the app's real exit code.
//
// openCommand resolves the platform's way of showing a window
// (platformOpenCommand, unless a test says otherwise).
func launchOpen(ctx context.Context, spec apps.Spec, workspace, prompt string, env []string, configPath, exitFile string,
	openCommand func(script string) ([]string, string, error)) (*child, string, error) {
	// Preflight 1: the app itself. `open -a Terminal` succeeds whether
	// or not the command inside the window exists, so without this the
	// failure would surface as a window that flashes and closes -- which
	// is exactly the silent nothing this kind must never produce.
	if _, err := exec.LookPath(spec.Binary); err != nil {
		return nil, "", fmt.Errorf("open %s: %q is not on PATH on this machine", spec.ID, spec.Binary)
	}

	if err := os.MkdirAll(filepath.Dir(exitFile), configDirMode); err != nil {
		return nil, "", fmt.Errorf("open %s: session directory: %w", spec.ID, err)
	}
	_ = os.Remove(exitFile)

	script, err := writeOpenLauncher(spec, workspace, prompt, env, configPath, exitFile)
	if err != nil {
		return nil, "", err
	}

	// Preflight 2: the platform's way of showing a window. On a headless
	// Linux box this is where the session fails, by name, instead of
	// pretending a window appeared.
	argv, note, err := openCommand(script)
	if err != nil {
		return nil, "", fmt.Errorf("open %s: %w", spec.ID, err)
	}

	c, err := startChild(workspace, argv, env)
	if err != nil {
		return nil, "", fmt.Errorf("open %s: %w", spec.ID, err)
	}
	return c, note, nil
}

// waitForOpen blocks until the human's session ends, and reports the
// app's own exit code.
//
// Two signals, whichever lands first:
//
//   - the exit file, written by the launcher when the app itself
//     returns. This is the precise one, and it is preferred: on macOS
//     `open -W` waits for the TERMINAL APPLICATION to quit, which for an
//     already-running Terminal is much later than the run's window
//     closing.
//   - the launcher process exiting. This covers a window closed abruptly,
//     where the script is killed before it can record anything.
func waitForOpen(ctx context.Context, c *child, exitFile string, readersDone <-chan struct{}) int {
	settle := func() int {
		// Drain before reaping: os/exec closes the pipes it handed out
		// from inside Wait.
		<-readersDone
		code := exitCode(c.wait())
		// The file is written just before the launcher script exits, so
		// give it precedence when it is there.
		if fileCode, ok := readOpenExitFile(exitFile); ok {
			return fileCode
		}
		return code
	}

	ticker := time.NewTicker(openWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			go c.terminate()
			return settle()
		case <-readersDone:
			return settle()
		case <-ticker.C:
			if _, ok := readOpenExitFile(exitFile); ok {
				// The app is done; the terminal application may linger.
				// Stop waiting on it and report what the app said.
				go c.terminate()
				return settle()
			}
		}
	}
}

// The bounds a real process exit status can occupy.
//
// A shell reports 0-255 -- `exit 256` wraps to 0 before anyone sees it,
// and a signalled process reports 128+n, which tops out at 255. Go's own
// ExitCode() reports -1 for "no exit status", which exitCode() in
// process.go passes through, so the file is allowed to say that too.
const (
	minOpenExitCode = -1
	maxOpenExitCode = 255
)

// readOpenExitFile reads the exit code the launcher script recorded.
//
// IT REFUSES ANYTHING THE WIRE CANNOT CARRY, and that is a correctness
// bound rather than a tidy-up. AppSessionEnd.ExitCode is an int32 and
// this function returns an int, so a file containing 4294967296
// truncates to exactly 0 at the conversion -- and the engine reads 0 as
// a run that SUCCEEDED. A failed session would be filed as a good one,
// in a record people read back later when deciding whether the thing
// worked, and nothing downstream could detect it. The same file also
// feeds a log line, so a number nobody can produce would be printed as
// though a process had returned it.
//
// Refusing costs nothing, because the caller has a better answer: this
// is one of two signals, and settle() falls back to the launcher
// PROCESS's own exit status whenever the file says nothing usable. The
// file is preferred when it is trustworthy, not when it is merely
// present.
//
// The value is NOT clamped into range, for the reason sendEnd and
// exitCode both give about normalising: flattening a number to 1 -- or
// worse to 0 -- invents an outcome. A file outside the range is not a
// large exit code, it is not an exit code, and saying so is the honest
// answer.
func readOpenExitFile(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	// ParseInt with an explicit bit size rather than Atoi: Atoi returns
	// an int, which is 64 bits on every platform this cockpit ships to,
	// so the range check would be the only thing standing between the
	// file and the conversion. Asking for 32 bits makes the parser
	// itself refuse what cannot travel, and the range check below is
	// then about what an exit code IS rather than about arithmetic.
	code, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 32)
	if err != nil {
		return 0, false
	}
	if code < minOpenExitCode || code > maxOpenExitCode {
		return 0, false
	}
	return int(code), true
}

// writeOpenLauncher renders the shell script the terminal runs.
//
// The environment is exported INSIDE the script rather than inherited: a
// terminal launched through `open -a` on macOS gets the login
// environment, not the worker's, so anything the session needs has to be
// stated here. The session bearer is deliberately not among those things
// -- it lives in the MCP configuration file and nowhere else, and a
// script is exactly the kind of file that gets left behind and read.
func writeOpenLauncher(spec apps.Spec, workspace, prompt string, env []string, configPath, exitFile string) (string, error) {
	args := spec.InteractiveArgs(prompt)
	if spec.ID == apps.IDClaudeCode {
		// The interactive door previously discovered the shared project config.
		// Use the same explicit private path as the automated Claude harness.
		args = append([]string{"--mcp-config", configPath, "--strict-mcp-config", "--"}, args...)
	}

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Written by memql for an app session; removed when the session ends.\n")
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "export %s=%s\n", key, shellQuote(value))
	}
	fmt.Fprintf(&b, "cd %s || exit 127\n", shellQuote(workspace))
	b.WriteString(shellQuote(spec.Binary))
	for _, a := range args {
		b.WriteString(" ")
		b.WriteString(shellQuote(a))
	}
	b.WriteString("\n")
	b.WriteString("code=$?\n")
	fmt.Fprintf(&b, "printf '%%s' \"$code\" > %s\n", shellQuote(exitFile))
	b.WriteString("exit $code\n")

	script := filepath.Join(filepath.Dir(exitFile), "open-launcher.sh")
	if err := os.MkdirAll(filepath.Dir(script), configDirMode); err != nil {
		return "", fmt.Errorf("open %s: %w", spec.ID, err)
	}
	// 0700: it is executed, and it names the workspace and the prompt.
	if err := os.WriteFile(script, []byte(b.String()), 0o700); err != nil {
		return "", fmt.Errorf("open %s: write launcher: %w", spec.ID, err)
	}
	return script, nil
}

// shellQuote wraps a value in single quotes, escaping any it contains.
// The prompt is arbitrary server-supplied text and it lands in a shell
// script; nothing about it may be assumed.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
