//go:build voice

package agent

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// TestDispatchLoop_SequentialSessionsBothConnect is the #1409 regression:
// Session.Run closes its client on teardown, so a dispatcher that reused one
// shared client had every session after the first fail Connect with
// ErrClientClosed (the "voice-agent gRPC client is closed" 3s hot-loop). Two
// sequential sessions must each complete the gRPC handshake on their own
// fresh stream.
func TestDispatchLoop_SequentialSessionsBothConnect(t *testing.T) {
	var mu sync.Mutex
	var streams []*fakeStream
	dial := func(_ context.Context, _, _ string) (streamConn, func(), error) {
		fs := newFakeStream()
		ackEcho(fs, &memqlv1.VoiceAgentSessionAck{Success: true})
		mu.Lock()
		streams = append(streams, fs)
		mu.Unlock()
		return fs, func() {}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rooms := []string{"polyphon-space-a", "polyphon-space-b"}
	served := 0
	discover := func(context.Context) string {
		if served >= len(rooms) {
			cancel()
			return ""
		}
		room := rooms[served]
		served++
		return room
	}

	newJoiner := func(Config, *Client, *slog.Logger) RoomJoiner {
		j := &recordingJoiner{reason: "normal", release: make(chan struct{})}
		close(j.release) // return immediately
		return j
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, dispatchLoop(ctx, Config{}, dial, logger, discover, newJoiner))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, streams, len(rooms), "expected one fresh dial per session")
	for i, fs := range streams {
		sent := fs.sentEnvelopes()
		require.NotEmpty(t, sent, "session %d sent nothing -- its client never connected", i)
		assert.NotNil(t, sent[0].GetVoiceAgentSessionStart(), "session %d did not open with SessionStart", i)
		assert.NotNil(t, sent[len(sent)-1].GetVoiceAgentSessionEnd(), "session %d did not close with SessionEnd", i)
	}
}
