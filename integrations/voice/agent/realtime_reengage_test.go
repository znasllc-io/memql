package agent

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeReengageSession is a flippable liveness stub. The bridge's real
// *openai.RealtimeSession satisfies reengageSession via IsClosed; here closed is
// an atomic so a rebuild callback can flip it to "open" mid-test.
type fakeReengageSession struct {
	closed atomic.Bool
}

func (f *fakeReengageSession) IsClosed() bool { return f.closed.Load() }

// TestReengageRebuildsClosedSession is the #1449 regression: when the realtime
// session has been torn down (the empty/idle/token teardown the mic-off cycle
// triggers), a human re-presenting MUST cause a FRESH session to be built, NOT a
// session.update against the dead handle. This test fails against the old
// "always update the existing session" behavior (no rebuild seam, rebuild count
// stays 0) and passes with the rebuild path.
func TestReengageRebuildsClosedSession(t *testing.T) {
	// The live session starts CLOSED -- the state after the teardown the mic-off
	// cycle caused. The rebuild callback models a fresh Connect: it swaps in a new
	// OPEN session and records that it ran.
	current := &fakeReengageSession{}
	current.closed.Store(true)

	var rebuilds int
	r := &sessionReengager{
		current: func() reengageSession { return current },
		rebuild: func() error {
			rebuilds++
			// A real rebuild swaps the bridge's session for a fresh open one; model
			// that by flipping the stub the current() closure returns.
			current.closed.Store(false)
			return nil
		},
	}

	// Re-engage on the closed session -> ensureLiveSession must rebuild and report
	// the session usable.
	ok := r.ensureLiveSession()
	assert.True(t, ok, "after a successful rebuild the caller may proceed to update")
	require.Equal(t, 1, rebuilds, "a closed session on re-engage MUST trigger exactly one rebuild (not an update against the dead handle)")
	assert.False(t, current.IsClosed(), "the rebuilt session is live")

	// A SECOND re-engage now finds a live session -> no further rebuild (the
	// common warm path is a no-op).
	ok = r.ensureLiveSession()
	assert.True(t, ok)
	assert.Equal(t, 1, rebuilds, "a live session must not be rebuilt")
}

// TestReengageLiveSessionNoRebuild proves the common path: a live session is
// updated in place, never rebuilt.
func TestReengageLiveSessionNoRebuild(t *testing.T) {
	current := &fakeReengageSession{} // open
	var rebuilds int
	r := &sessionReengager{
		current: func() reengageSession { return current },
		rebuild: func() error { rebuilds++; return nil },
	}

	assert.True(t, r.ensureLiveSession())
	assert.Equal(t, 0, rebuilds, "a live session is updated in place, never rebuilt")
}

// TestReengageRebuildFailureSkipsUpdate proves that when the fresh dial fails the
// caller is told NOT to proceed (so it never pokes the dead handle and never
// spams "session is closed").
func TestReengageRebuildFailureSkipsUpdate(t *testing.T) {
	current := &fakeReengageSession{}
	current.closed.Store(true)
	r := &sessionReengager{
		current: func() reengageSession { return current },
		rebuild: func() error { return errors.New("dial boom") },
	}

	assert.False(t, r.ensureLiveSession(),
		"a failed rebuild must report no usable session so the caller skips the update")
	assert.True(t, current.IsClosed(), "session stays down after a failed rebuild")
}

// TestReengageAcrossPresenceHop models the cross-node shape of the bug: the
// teardown is decided by one observer (e.g. a guardrail trip or a peer-node
// presence-decay signal that closed the session) while the human RE-PRESENCE is
// observed by a different path (the room callback / a presence event relayed
// from another node). The re-engage decision must rebuild based purely on the
// session's liveness, independent of WHICH path closed it -- so a presence hop
// that arrives after a remote teardown still recovers a working session.
func TestReengageAcrossPresenceHop(t *testing.T) {
	current := &fakeReengageSession{} // starts open

	var rebuilds int
	r := &sessionReengager{
		current: func() reengageSession { return current },
		rebuild: func() error { rebuilds++; current.closed.Store(false); return nil },
	}

	// "Other node" / guardrail closes the session out-of-band (not via the path
	// that will observe the re-presence).
	current.closed.Store(true)

	// Re-presence observed here (the human signal hopping in) drives the gate-mode
	// apply -> ensureLiveSession sees the closed session and rebuilds.
	assert.True(t, r.ensureLiveSession())
	assert.Equal(t, 1, rebuilds, "a re-presence after an out-of-band teardown rebuilds the session")
	assert.False(t, current.IsClosed())
}

// TestReengageNilSessionRebuilds covers a nil current session (never connected /
// fully torn down) -- it is treated as closed and triggers a rebuild.
func TestReengageNilSessionRebuilds(t *testing.T) {
	var rebuilds int
	live := &fakeReengageSession{} // open, swapped in by the rebuild
	swapped := false
	r := &sessionReengager{
		current: func() reengageSession {
			if swapped {
				return live
			}
			return nil
		},
		rebuild: func() error { rebuilds++; swapped = true; return nil },
	}

	assert.True(t, r.ensureLiveSession())
	assert.Equal(t, 1, rebuilds, "a nil session is treated as closed and rebuilt")
}
