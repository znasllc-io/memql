package harness

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaudeHeadlessLargePromptArrivesOnceBeforeEOF(t *testing.T) {
	binary, log := fakeClaude(t, prints(claudePongTurn))
	h := startClaude(t, claudeSpec(t, binary))
	// Well above Linux's common 128 KiB single-argument ceiling. Leading
	// flags, Unicode, line breaks and the final newline must stay exact.
	prompt := "--resume not-an-option\n" + strings.Repeat("complete evidence λ\n", 32768)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := h.Turn(ctx, prompt, &recorder{})
	if err != nil || res.Text != "pong" {
		t.Fatalf("large prompt did not finish after EOF: %+v, %v", res, err)
	}
	body, err := os.ReadFile(log + ".stdin")
	if err != nil || string(body) != prompt {
		t.Fatalf("prompt changed: got %d bytes, want %d: %v", len(body), len(prompt), err)
	}
	turns := recordedArgv(t, log)
	if len(turns) != 1 || hasArg(turns[0], prompt) || turns[0][len(turns[0])-1] != "--" {
		t.Fatal("prompt entered argv or delivery started another process")
	}
}

func TestClaudeHeadlessDrainsOutputWhileSendingPrompt(t *testing.T) {
	// More than a pipe buffer on BOTH outputs before this fake reads stdin.
	// A synchronous write before draining output would deadlock here.
	binary := fakeBinary(t, "claude", `
head -c 262144 /dev/zero
printf '\n'
head -c 262144 /dev/zero >&2
cat > /dev/null
`+prints(claudePongTurn))
	h := startClaude(t, claudeSpec(t, binary))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := h.Turn(ctx, strings.Repeat("evidence\n", 65536), &recorder{})
	if err != nil || res.Text != "pong" {
		t.Fatalf("duplex prompt delivery stalled: %+v, %v", res, err)
	}
}

// These process doubles exercise transport errors that an ordinary successful
// shell reader cannot deterministically produce. They never run a model.
type claudeInputProcess struct {
	input      io.WriteCloser
	stdout     io.Reader
	stderr     io.Reader
	stop       func()
	terminates atomic.Int32
	waits      atomic.Int32
}

func (p *claudeInputProcess) Stdin() io.WriteCloser { return p.input }
func (p *claudeInputProcess) Stdout() io.Reader     { return p.stdout }
func (p *claudeInputProcess) Stderr() io.Reader     { return p.stderr }
func (p *claudeInputProcess) ExitCode() int         { return 0 }
func (p *claudeInputProcess) Wait() error           { p.waits.Add(1); return nil }
func (p *claudeInputProcess) Terminate() {
	p.terminates.Add(1)
	if p.stop != nil {
		p.stop()
	}
}

type claudeInputWriter struct {
	write    func([]byte) (int, error)
	closeErr error
	writes   atomic.Int32
	closes   atomic.Int32
}

func (w *claudeInputWriter) Write(b []byte) (int, error) {
	w.writes.Add(1)
	return w.write(b)
}
func (w *claudeInputWriter) Close() error { w.closes.Add(1); return w.closeErr }

func startClaudeInputProcess(t *testing.T, p *claudeInputProcess) *claudeHeadless {
	t.Helper()
	spec := claudeSpec(t, "unused-fake-claude")
	spec.Launch = func(_ context.Context, _ string, argv, _ []string, stdin bool) (Process, error) {
		if !stdin {
			t.Error("Claude turn did not request its finite input pipe")
		}
		if argValue(t, argv, "--input-format") != "text" {
			t.Error("non-text input")
		}
		return p, nil
	}
	return startClaude(t, spec)
}

func TestClaudeHeadlessPromptDeliveryFailureCannotReportSuccess(t *testing.T) {
	broken := errors.New("controlled broken prompt pipe")
	for _, name := range []string{"missing pipe", "short write", "write error", "close error"} {
		t.Run(name, func(t *testing.T) {
			w := &claudeInputWriter{write: func(b []byte) (int, error) { return len(b), nil }}
			p := &claudeInputProcess{input: w, stdout: strings.NewReader(claudePongTurn), stderr: strings.NewReader("")}
			var want error
			switch name {
			case "missing pipe":
				p.input = nil
			case "short write":
				w.write = func(b []byte) (int, error) { return len(b) - 1, nil }
				want = io.ErrShortWrite
			case "write error":
				w.write = func([]byte) (int, error) { return 0, broken }
				want = broken
			case "close error":
				w.closeErr = broken
				want = broken
			}
			h := startClaudeInputProcess(t, p)
			const prompt = "private prompt must not appear in the transport error"
			res, err := h.Turn(context.Background(), prompt, &recorder{})
			if err == nil || !strings.Contains(err.Error(), "complete Claude prompt") || (want != nil && !errors.Is(err, want)) {
				t.Fatalf("transport failure became success or lost cause: %+v, %v", res, err)
			}
			if strings.Contains(err.Error(), prompt) {
				t.Fatal("transport error disclosed the prompt")
			}
			if p.terminates.Load() != 1 || p.waits.Load() != 1 {
				t.Fatal("failed delivery did not stop and reap the supervised process")
			}
			if p.input != nil && (w.writes.Load() != 1 || w.closes.Load() != 1) {
				t.Fatal("prompt retried or pipe not closed exactly once")
			}
		})
	}
}

type claudeInputStoppedReader struct{ stopped <-chan struct{} }

func (r claudeInputStoppedReader) Read([]byte) (int, error) { <-r.stopped; return 0, io.EOF }

func TestClaudeHeadlessCancelUnblocksPromptWrite(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	var once sync.Once
	w := &claudeInputWriter{write: func([]byte) (int, error) {
		close(started)
		<-stopped
		return 0, io.ErrClosedPipe
	}}
	p := &claudeInputProcess{input: w, stdout: claudeInputStoppedReader{stopped}, stderr: claudeInputStoppedReader{stopped}, stop: func() { once.Do(func() { close(stopped) }) }}
	h := startClaudeInputProcess(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := h.Turn(ctx, strings.Repeat("prompt", 65536), &recorder{}); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt write did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel left a blocked prompt writer")
	}
	if w.writes.Load() != 1 || w.closes.Load() != 1 || p.waits.Load() != 1 || p.terminates.Load() < 1 {
		t.Fatal("cancel did not finish exactly one input attempt, close and reap")
	}
}

// Failed delivery must retain any independently reported structured evidence
// beside the failure, as other failed turns do; it must never become success.
func TestClaudeHeadlessInputFailureRetainsReportedStructuredResult(t *testing.T) {
	w := &claudeInputWriter{write: func([]byte) (int, error) { return 0, io.ErrClosedPipe }}
	p := &claudeInputProcess{input: w, stdout: strings.NewReader(claudeSchemaTurn), stderr: strings.NewReader("")}
	h := startClaudeInputProcess(t, p)
	h.spec.ResponseSchema = `{"type":"object"}`
	res, err := h.Turn(context.Background(), "structured request", &recorder{})
	if !errors.Is(err, io.ErrClosedPipe) || string(res.ResultJSON) != `{"city":"Paris","millions":2.2}` {
		t.Fatalf("failure or reported structured evidence lost: %+v, %v", res, err)
	}
}
