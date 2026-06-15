//go:build voice

package agent

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// blockingJoiner is a RoomJoiner that blocks in JoinAndServe until its release
// channel is closed (or ctx is cancelled), so a test can hold several sessions
// "live" simultaneously and assert they are served concurrently.
type blockingJoiner struct {
	release <-chan struct{}
}

func (j *blockingJoiner) JoinAndServe(ctx context.Context, _ RoomRequest) (string, error) {
	select {
	case <-j.release:
	case <-ctx.Done():
	}
	return "normal", nil
}

// dispatchTestDial returns a Dialer that hands every session a fresh fakeStream
// that acks the SessionStart, recording each stream so the test can verify the
// per-session gRPC handshake.
func dispatchTestDial() (Dialer, func() []*fakeStream) {
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
	return dial, func() []*fakeStream {
		mu.Lock()
		defer mu.Unlock()
		out := make([]*fakeStream, len(streams))
		copy(out, streams)
		return out
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestDispatchLoop_ConcurrentRoomsServedTogether is the #1395 core behaviour:
// two human-occupied rooms discovered in the same poll are both served at the
// same time, each on its own fresh gRPC client -- the second is NOT made to
// wait for the first to idle out.
func TestDispatchLoop_ConcurrentRoomsServedTogether(t *testing.T) {
	dial, streamsOf := dispatchTestDial()

	release := make(chan struct{})
	newJoiner := func(Config, *Client, *slog.Logger) RoomJoiner {
		return &blockingJoiner{release: release}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rooms := []string{"polyphon-space-a", "polyphon-space-b"}
	discover := func(context.Context) []string { return rooms }

	done := make(chan struct{})
	go func() {
		_ = dispatchLoop(ctx, Config{VoiceMaxRooms: 8}, dial, quietLogger(), discover, newJoiner)
		close(done)
	}()

	// Both rooms should get their own session/stream concurrently while the
	// joiners are still blocked.
	require.Eventually(t, func() bool {
		return len(streamsOf()) == len(rooms)
	}, 2*time.Second, 5*time.Millisecond, "both rooms should be served concurrently")

	// Release the joiners and shut the loop down.
	close(release)
	cancel()
	<-done

	streams := streamsOf()
	require.Len(t, streams, len(rooms), "expected one fresh dial per concurrent room")
	for i, fs := range streams {
		sent := fs.sentEnvelopes()
		require.NotEmpty(t, sent, "session %d sent nothing -- its client never connected", i)
		assert.NotNil(t, sent[0].GetVoiceAgentSessionStart(), "session %d did not open with SessionStart", i)
		assert.NotNil(t, sent[len(sent)-1].GetVoiceAgentSessionEnd(), "session %d did not close with SessionEnd", i)
	}
}

// TestDispatchLoop_NoDoubleServeSameRoom guards against a room discovered on
// successive polls being served twice while its first session is still live --
// the per-replica analog of the cross-replica claim (discovery skips rooms that
// already have a voice-agent participant).
func TestDispatchLoop_NoDoubleServeSameRoom(t *testing.T) {
	dial, streamsOf := dispatchTestDial()

	release := make(chan struct{})
	newJoiner := func(Config, *Client, *slog.Logger) RoomJoiner {
		return &blockingJoiner{release: release}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The same room is returned on every poll while its session is live.
	discover := func(context.Context) []string { return []string{"polyphon-space-a"} }

	done := make(chan struct{})
	go func() {
		_ = dispatchLoop(ctx, Config{VoiceMaxRooms: 8}, dial, quietLogger(), discover, newJoiner)
		close(done)
	}()

	require.Eventually(t, func() bool {
		return len(streamsOf()) == 1
	}, 2*time.Second, 5*time.Millisecond, "room should be served once")

	// Give the loop several poll cycles; it must not spawn a second session for
	// the still-live room.
	time.Sleep(2 * dispatcherPollInterval)
	assert.Len(t, streamsOf(), 1, "live room must not be served twice")

	close(release)
	cancel()
	<-done
}

// TestDispatchLoop_ReservesRoomAfterIdle confirms a room becomes re-eligible
// once its session ends (idle teardown), so a human rejoining is served again.
func TestDispatchLoop_ReservesRoomAfterIdle(t *testing.T) {
	dial, streamsOf := dispatchTestDial()

	// First session releases immediately (simulating idle teardown); later
	// sessions block so the test can observe the re-serve without churn.
	var first atomic.Bool
	first.Store(true)
	newJoiner := func(Config, *Client, *slog.Logger) RoomJoiner {
		rel := make(chan struct{})
		if first.Swap(false) {
			close(rel) // first joiner returns immediately
		}
		return &blockingJoiner{release: rel}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	discover := func(context.Context) []string { return []string{"polyphon-space-a"} }

	done := make(chan struct{})
	go func() {
		_ = dispatchLoop(ctx, Config{VoiceMaxRooms: 8}, dial, quietLogger(), discover, newJoiner)
		close(done)
	}()

	// First session ends and the room is re-served on a later poll -> a second
	// fresh stream appears.
	require.Eventually(t, func() bool {
		return len(streamsOf()) >= 2
	}, 4*time.Second, 10*time.Millisecond, "room should be re-served after its first session idles out")

	cancel()
	<-done
}

// TestDispatchLoop_BoundedByMaxRooms confirms the pool never exceeds the
// configured concurrency cap, deferring excess rooms to a later poll.
func TestDispatchLoop_BoundedByMaxRooms(t *testing.T) {
	dial, streamsOf := dispatchTestDial()

	release := make(chan struct{})
	newJoiner := func(Config, *Client, *slog.Logger) RoomJoiner {
		return &blockingJoiner{release: release}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rooms := []string{"polyphon-a", "polyphon-b", "polyphon-c", "polyphon-d"}
	discover := func(context.Context) []string { return rooms }

	done := make(chan struct{})
	go func() {
		_ = dispatchLoop(ctx, Config{VoiceMaxRooms: 2}, dial, quietLogger(), discover, newJoiner)
		close(done)
	}()

	// Only the cap's worth of sessions should ever be live concurrently.
	require.Eventually(t, func() bool {
		return len(streamsOf()) == 2
	}, 2*time.Second, 5*time.Millisecond, "should reach the room cap")
	time.Sleep(2 * dispatcherPollInterval)
	assert.Len(t, streamsOf(), 2, "must not exceed MEMQL_VOICE_MAX_ROOMS while sessions are live")

	close(release)
	cancel()
	<-done
}

// TestClassifyRoomParticipants_AgentOnlyAndHuman exercises the discovery
// gate's two invariants in one place: a room is eligible only when a human is
// present AND no voice-agent is already serving it.
func TestIsVoiceAgentParticipantIdentity(t *testing.T) {
	cases := map[string]bool{
		"":                                false,
		"abc123def":                       false, // human shortId
		"v1:cognition:space:uuid-ga":      true,  // placeholder GA identity
		"v1:agents:agent:abc":             true,  // real agent-row identity
		"avatar-agent":                    false, // avatar vendor leg, not a GA
	}
	for identity, want := range cases {
		assert.Equalf(t, want, isVoiceAgentParticipantIdentity(identity),
			"isVoiceAgentParticipantIdentity(%q)", identity)
	}
}
