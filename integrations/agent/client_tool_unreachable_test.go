package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql/taskstamp"
)

// TestStreamingLoop_ClientToolUnreachableFailsFast proves that when an
// interactive client-executed UI tool times out with the CLIENT_TOOL_UNREACHABLE
// marker (memql#1834), the agent streaming loop breaks immediately and surfaces
// the concise, user-facing message -- rather than retrying the call round after
// round until the all-errored / wallclock cap. This is the chat-path half of
// the fast, user-legible failure.
func TestStreamingLoop_ClientToolUnreachableFailsFast(t *testing.T) {
	r := testReplier()
	r.stamper = taskstamp.New(
		&scriptedExecutor{fn: func(_ int, _ string) (string, error) {
			// Mirror the typed error component/grpc returns on a fast timeout:
			// the marker plus the user message, wrapped like the engine would.
			return "", errors.New("CLIENT_TOOL_UNREACHABLE: I couldn't reach the app to take control of the screen — please try again, and reload the page if it keeps happening. (tool \"uiRequestControl\" got no browser response after 10s)")
		}},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	// The model keeps trying the UI tool; without the fast break the loop would
	// run all of these (and beyond, since maxCalls is high by default).
	prov := &fakeStreamProvider{toolName: "uiRequestControl", rawArgs: `{}`}
	sink := &captureSink{}

	res, err := r.runStreamingToolLoop(context.Background(), prov, nil, nil, sink, time.Now(), "req-unreachable", turnContext{})
	if err != nil {
		t.Fatalf("unreachable client tool should break gracefully (nil error), got %v", err)
	}
	// Broke on the FIRST unreachable timeout -- one model round-trip, not a loop.
	if prov.calls != 1 {
		t.Fatalf("stream provider called %d times, want 1 (break on first unreachable timeout)", prov.calls)
	}
	// The user-facing message was streamed and is the turn's final text.
	if !strings.Contains(res.FinalText, clientToolUnreachableUserMessage) {
		t.Fatalf("FinalText = %q, want the unreachable user message", res.FinalText)
	}
	joined := strings.Join(sink.texts, "")
	if !strings.Contains(joined, clientToolUnreachableUserMessage) {
		t.Fatalf("streamed text = %q, want the unreachable user message", joined)
	}
}
