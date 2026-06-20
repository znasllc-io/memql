package memql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
)

// TestClientToolWaitTimeout proves the per-tool ceiling: interactive UI tools
// get the fast clientToolInteractiveTimeout (#1834) while uiAskUser keeps the
// long human-input ceiling.
func TestClientToolWaitTimeout(t *testing.T) {
	if got := clientToolWaitTimeout("uiClick"); got != clientToolInteractiveTimeout {
		t.Fatalf("uiClick ceiling = %v, want interactive %v", got, clientToolInteractiveTimeout)
	}
	if got := clientToolWaitTimeout("uiRequestControl"); got != clientToolInteractiveTimeout {
		t.Fatalf("uiRequestControl ceiling = %v, want interactive %v", got, clientToolInteractiveTimeout)
	}
	if got := clientToolWaitTimeout("uiAskUser"); got != uiAskUserClientToolTimeout {
		t.Fatalf("uiAskUser ceiling = %v, want %v", got, uiAskUserClientToolTimeout)
	}
	// The fast ceiling must be meaningfully tighter than the old 30s/60s waits.
	if clientToolInteractiveTimeout >= 30*time.Second {
		t.Fatalf("interactive ceiling %v is not fast (want a few seconds, well under 30s)", clientToolInteractiveTimeout)
	}
}

// TestClientToolTimeoutMsMirrorsWait proves the wire TimeoutMs tracks the
// in-process ceiling so the cognition relay sweeper sees the same deadline.
func TestClientToolTimeoutMsMirrorsWait(t *testing.T) {
	if got, want := clientToolTimeoutMs("uiClick"), int32(clientToolInteractiveTimeout.Milliseconds()); got != want {
		t.Fatalf("uiClick TimeoutMs = %d, want %d", got, want)
	}
	if got, want := clientToolTimeoutMs("uiAskUser"), int32(uiAskUserClientToolTimeout.Milliseconds()); got != want {
		t.Fatalf("uiAskUser TimeoutMs = %d, want %d", got, want)
	}
}

// TestClientToolUnreachableErrorIsTypedTimeout proves the timeout error carries
// the CLIENT_TOOL_UNREACHABLE marker, the user-facing message, and classifies
// as a (retryable) timeout via the shared ClassifyToolError so the agent loop
// and voice path both recognise it (#1834).
func TestClientToolUnreachableErrorIsTypedTimeout(t *testing.T) {
	err := clientToolUnreachableError("uiRequestControl", 10*time.Second)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), clientToolUnreachableMarker) {
		t.Fatalf("error %q missing marker %q", err.Error(), clientToolUnreachableMarker)
	}
	if !strings.Contains(err.Error(), clientToolUnreachableUserMessage) {
		t.Fatalf("error %q missing user message", err.Error())
	}
	se := memql.ClassifyToolError(err)
	if se == nil || se.Type != memql.ToolErrorTimeout {
		t.Fatalf("ClassifyToolError = %+v, want type=timeout", se)
	}
	if !se.Retryable {
		t.Fatal("a browser-unreachable timeout should be marked retryable")
	}
}

// TestUserFacingClientToolError proves the marker is swapped for the clean
// user message, while a genuine browser error passes through verbatim.
func TestUserFacingClientToolError(t *testing.T) {
	if got := userFacingClientToolError(nil); got != "" {
		t.Fatalf("nil error -> %q, want empty", got)
	}
	unreachable := clientToolUnreachableError("uiClick", time.Second)
	if got := userFacingClientToolError(unreachable); got != clientToolUnreachableUserMessage {
		t.Fatalf("unreachable -> %q, want user message", got)
	}
	raw := errors.New("the element was not found")
	if got := userFacingClientToolError(raw); got != "the element was not found" {
		t.Fatalf("genuine browser error -> %q, want verbatim", got)
	}
}

// TestAwaitClientToolResult_FastTimeout proves the bounded wait fires on the
// fast interactive ceiling (NOT the old 30s/60s) when no response arrives, and
// reports the timeout outcome -- the core fail-fast contract (#1834). The
// ceiling is shrunk for the test so it never sleeps for real seconds.
func TestAwaitClientToolResult_FastTimeout(t *testing.T) {
	restore := clientToolInteractiveTimeout
	clientToolInteractiveTimeout = 40 * time.Millisecond
	defer func() { clientToolInteractiveTimeout = restore }()

	ch := make(chan *memqlv1.ClientToolResult) // never fires
	start := time.Now()
	result, outcome, elapsed := awaitClientToolResult(t.Context(), ch, "uiClick")
	wall := time.Since(start)

	if outcome != clientToolTimedOut {
		t.Fatalf("outcome = %q, want timeout", outcome)
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil on timeout", result)
	}
	if wall > 5*time.Second {
		t.Fatalf("wait took %v -- did not honour the fast ceiling (would be 30s/60s before #1834)", wall)
	}
	if elapsed < clientToolInteractiveTimeout {
		t.Fatalf("elapsed %v shorter than the ceiling %v", elapsed, clientToolInteractiveTimeout)
	}
}

// TestAwaitClientToolResult_Responds proves a delivered result resolves with
// the responded outcome before the ceiling.
func TestAwaitClientToolResult_Responds(t *testing.T) {
	restore := clientToolInteractiveTimeout
	clientToolInteractiveTimeout = 2 * time.Second
	defer func() { clientToolInteractiveTimeout = restore }()

	ch := make(chan *memqlv1.ClientToolResult, 1)
	ch <- &memqlv1.ClientToolResult{CallId: "c1"}
	result, outcome, _ := awaitClientToolResult(t.Context(), ch, "uiClick")
	if outcome != clientToolResponded {
		t.Fatalf("outcome = %q, want responded", outcome)
	}
	if result == nil || result.GetCallId() != "c1" {
		t.Fatalf("result = %+v, want callId c1", result)
	}
}

// TestAwaitClientToolResult_Cancelled proves a cancelled context resolves with
// the cancelled outcome rather than waiting for the ceiling.
func TestAwaitClientToolResult_Cancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch := make(chan *memqlv1.ClientToolResult)
	_, outcome, _ := awaitClientToolResult(ctx, ch, "uiClick")
	if outcome != clientToolCancelled {
		t.Fatalf("outcome = %q, want cancelled", outcome)
	}
}
