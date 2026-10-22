package appsession

import (
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStreamReader_SplitsOnLines.
func TestStreamReader_SplitsOnLines(t *testing.T) {
	var mu sync.Mutex
	var got []string
	r := &streamReader{
		name: StreamStdout,
		src:  strings.NewReader("one\ntwo\nthree\n"),
		emit: func(_ string, data []byte) error {
			mu.Lock()
			got = append(got, string(data))
			mu.Unlock()
			return nil
		},
	}
	if err := r.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(got) != 3 || got[0] != "one\n" || got[2] != "three\n" {
		t.Errorf("chunks = %q", got)
	}
}

// TestStreamReader_TrailingPartialLineIsFlushed: output with no final
// newline must still reach the transcript.
func TestStreamReader_TrailingPartialLineIsFlushed(t *testing.T) {
	var got []string
	r := &streamReader{
		name: StreamStdout,
		src:  strings.NewReader("no trailing newline"),
		emit: func(_ string, data []byte) error {
			got = append(got, string(data))
			return nil
		},
	}
	if err := r.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(got) != 1 || got[0] != "no trailing newline" {
		t.Errorf("chunks = %q", got)
	}
}

// TestStreamReader_HoldsBackASecretStraddlingAFlush is the end-to-end
// form of the redactor's holdBack.
//
// An app that prints a very long line with no newline forces an early
// flush. If the flush cut the buffer wherever it happened to be, a
// credential straddling that point would leave in two chunks, each half
// unmatched by the redactor -- and the two halves ARE the credential to
// anyone reading the transcript. Retaining a credential-length tail is
// what makes that impossible.
func TestStreamReader_HoldsBackASecretStraddlingAFlush(t *testing.T) {
	secret := testBearer
	// One enormous line: filler, then the secret, then more filler, with
	// no newline anywhere. maxLineBytes forces the flush.
	body := strings.Repeat("f", maxLineBytes) + secret + strings.Repeat("g", 4096)

	red := newRedactor(secret)
	var mu sync.Mutex
	var chunks []string
	r := &streamReader{
		name: StreamStdout,
		src:  strings.NewReader(body),
		emit: func(_ string, data []byte) error {
			mu.Lock()
			chunks = append(chunks, string(red.apply(data)))
			mu.Unlock()
			return nil
		},
		holdBack: red.holdBack(),
	}
	if err := r.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("want the line flushed in pieces, got %d chunk(s)", len(chunks))
	}

	joined := strings.Join(chunks, "")
	if strings.Contains(joined, secret) {
		t.Fatal("the credential survived redaction across the flush boundary")
	}
	if !strings.Contains(joined, string(redactedMarker)) {
		t.Error("the credential was neither redacted nor emitted; it should be redacted, visibly")
	}
	// Nothing may be lost: every filler byte still has to arrive.
	if got, want := strings.Count(joined, "f"), maxLineBytes; got != want {
		t.Errorf("leading filler = %d bytes, want %d -- the hold-back dropped output", got, want)
	}
	if got, want := strings.Count(joined, "g"), 4096; got != want {
		t.Errorf("trailing filler = %d bytes, want %d", got, want)
	}
}

// TestStreamReader_EmitErrorStopsTheStream: an emit failure means the
// stream to the server is gone, and there is nowhere left to send.
func TestStreamReader_EmitErrorStopsTheStream(t *testing.T) {
	calls := 0
	r := &streamReader{
		name: StreamStdout,
		src:  strings.NewReader("a\nb\nc\n"),
		emit: func(_ string, _ []byte) error {
			calls++
			return io.ErrClosedPipe
		},
	}
	if err := r.run(); err == nil {
		t.Fatal("want the emit error surfaced")
	}
	if calls != 1 {
		t.Errorf("emit called %d times after failing, want 1", calls)
	}
}

// TestExitCode_PassesTheRealCodeThrough. The engine reads a non-zero exit
// as a FAILED run rather than an ended one.
func TestExitCode_PassesTheRealCodeThrough(t *testing.T) {
	if got := exitCode(nil); got != 0 {
		t.Errorf("exitCode(nil) = %d, want 0", got)
	}
	if got := exitCode(io.ErrUnexpectedEOF); got != -1 {
		t.Errorf("a non-exit error = %d, want -1 (no exit status)", got)
	}
}

func TestTerminateReapsWithoutWaitingForTheGraceDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process fixture")
	}
	child, err := startChild(t.TempDir(), []string{"sh", "-c", "sleep 30"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer child.terminate()
	started := time.Now()
	child.terminate() // No caller has invoked Wait yet, as on app-server Close.
	_ = child.wait()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("clean exit waited for shutdown grace: %s", elapsed)
	}
	select {
	case <-child.exited:
	default:
		t.Fatal("process was not reaped")
	}
}
