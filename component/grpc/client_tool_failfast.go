package memql

import (
	"context"
	"fmt"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// client_tool_failfast.go
// =======================
//
// Bounded, typed, fast-failing waits for client-executed tools (memql#1834).
//
// An interactive client-executed UI tool (uiRequestControl, uiClick, uiType,
// ...) runs in the browser the instant the browser receives the request, so a
// healthy round-trip is sub-second. When the browser is gone -- or unreachable
// across nodes because the cross-node relay didn't deliver (#1835) -- the call
// previously stranded the agent tool loop / voice tool worker for the full
// 30s per-call wait (and the 60s cognition relay TTL backstop), turning a
// single chat turn into a ~68s "Replying..." stall and surfacing a bare
// "it timed out" on voice.
//
// This file makes that failure FAST and USER-LEGIBLE:
//   - clientToolInteractiveTimeout caps the browser handshake at a few seconds.
//   - on timeout, interactive tools resolve with a TYPED error carrying the
//     CLIENT_TOOL_UNREACHABLE marker so the agent streaming loop + the voice
//     CallTool path swap in clientToolUnreachableUserMessage for the user.
//
// The 60s cognition relay TTL (pendingClientToolCallTTL) stays as the outer
// correlation-cleanup backstop; this ceiling fires first.

const (
	// clientToolUnreachableMarker is the sentinel substring stamped on the
	// typed error returned when an interactive client-executed UI tool times
	// out with no browser response (#1834). The agent streaming loop
	// (integrations/agent/streaming.go) and the voice CallTool path detect it
	// and surface clientToolUnreachableUserMessage; component/memql's
	// ClassifyToolError buckets the error as a (retryable) timeout. Mirrors
	// the WHEEL_CONTESTED pattern -- a duplicated string constant kept in sync
	// with its consumer rather than a shared import (component/grpc must not be
	// imported by integrations/agent's streaming loop just for this).
	clientToolUnreachableMarker = "CLIENT_TOOL_UNREACHABLE"

	// clientToolUnreachableUserMessage is the concise, actionable explanation
	// the user sees -- in chat (streamed as the turn's reply) and in voice
	// (spoken by the model) -- when an interactive UI tool can't reach the
	// browser. Kept in sync with the copy in integrations/agent/streaming.go.
	clientToolUnreachableUserMessage = "I couldn't reach the app to take control of the screen — please try again, and reload the page if it keeps happening."

	// uiAskUserClientToolTimeout bounds a uiAskUser round-trip. uiAskUser blocks
	// on a HUMAN reading a one-sentence prompt and typing an answer, so it gets
	// a much longer ceiling than the programmatic UI tools and is exempt from
	// the fast-fail typed error (an unanswered ask is modelled as "user
	// dismissed", not "browser unreachable").
	uiAskUserClientToolTimeout = 120 * time.Second
)

// clientToolInteractiveTimeout caps how long any non-uiAskUser
// client-executed tool waits for the browser to respond (#1834). Interactive
// UI tools execute programmatically in the browser the instant it receives
// them; a sub-second round-trip is normal, so this multi-second ceiling means
// an unreachable browser fails fast and legibly instead of stalling the tool
// loop. A package var (not a const) so tests can shrink it without sleeping.
var clientToolInteractiveTimeout = 10 * time.Second

// clientToolWaitTimeout resolves the bounded wait for a client-executed tool
// by name: the human-input ceiling for uiAskUser, the fast interactive ceiling
// for everything else.
func clientToolWaitTimeout(toolName string) time.Duration {
	if toolName == "uiAskUser" {
		return uiAskUserClientToolTimeout
	}
	return clientToolInteractiveTimeout
}

// clientToolWaitOutcome classifies the result of a bounded client-tool wait
// so each call site can log it consistently (#1834).
type clientToolWaitOutcome string

const (
	clientToolResponded clientToolWaitOutcome = "responded"
	clientToolTimedOut  clientToolWaitOutcome = "timeout"
	clientToolCancelled clientToolWaitOutcome = "cancelled"
)

// awaitClientToolResult performs the bounded wait shared by every client-tool
// relay path (agent InvokeClientTool, voice relayClientToolToBrowser, and the
// direct-browser executeClientTool). It blocks until the browser answers (ch),
// the context is cancelled, or the per-tool ceiling fires, and returns the raw
// result (nil unless responded), the outcome, and the elapsed wait for logging.
func awaitClientToolResult(
	ctx context.Context,
	ch <-chan *memqlv1.ClientToolResult,
	toolName string,
) (*memqlv1.ClientToolResult, clientToolWaitOutcome, time.Duration) {
	started := time.Now()
	select {
	case <-ctx.Done():
		return nil, clientToolCancelled, time.Since(started)
	case <-time.After(clientToolWaitTimeout(toolName)):
		return nil, clientToolTimedOut, time.Since(started)
	case result := <-ch:
		return result, clientToolResponded, time.Since(started)
	}
}

// clientToolUnreachableError builds the typed, user-legible error returned
// when an interactive client-executed UI tool times out with no browser
// response (#1834). The CLIENT_TOOL_UNREACHABLE marker lets the agent
// streaming loop + voice CallTool path swap in the user-facing message.
func clientToolUnreachableError(toolName string, elapsed time.Duration) error {
	// "timed out" keeps component/memql's ClassifyToolError bucketing this as a
	// (retryable) timeout rather than a generic internal error.
	return fmt.Errorf("%s: %s (tool %q timed out after %s with no browser response)",
		clientToolUnreachableMarker, clientToolUnreachableUserMessage,
		toolName, elapsed.Round(time.Millisecond))
}

// userFacingClientToolError renders a client-tool error for the model/user:
// the clean user-facing message when it is our unreachable timeout, otherwise
// the raw error text (e.g. a genuine browser-side execution failure the model
// should see verbatim).
func userFacingClientToolError(err error) string {
	if err == nil {
		return ""
	}
	if strings.Contains(err.Error(), clientToolUnreachableMarker) {
		return clientToolUnreachableUserMessage
	}
	return err.Error()
}
