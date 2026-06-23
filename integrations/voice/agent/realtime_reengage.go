package agent

import "log/slog"

// realtime_reengage.go is the CGO-free re-engagement decision for the realtime
// voice path (#1449). It is the seam the voice-tagged room bridge
// (room_realtime_voice.go) drives when a human (re)presents and the gate mode is
// about to be applied: if the current realtime session has been torn down /
// closed (an idle / token / duration guardrail trip, a transient
// disconnect+reconnect, or a transport drop), the bridge MUST establish a FRESH
// session rather than issue a session.update against the dead handle -- the bug
// behind "mic off -> on in the same space leaves voice dead" (the re-engage path
// spammed "openai realtime: session is closed" and never rebuilt).
//
// The decision + sequencing live here, behind two small interfaces, so they are
// unit-tested in the default (CGO-free) lane against fakes -- a test can prove
// that a closed session triggers a rebuild (not an update) without a live OpenAI
// dial or the libopus media plane. The voice build wires the real
// RealtimeSession + a real reconnect; the cluster-e2e harness exercises the live
// hop.

// reengageSession is the minimal liveness surface the re-engage decision reads
// off a realtime session. *openai.RealtimeSession satisfies it via IsClosed;
// tests supply a fake whose closed state is flippable.
type reengageSession interface {
	// IsClosed reports whether the session has been torn down and can no longer
	// accept writes (every UpdateSession would fail "session is closed").
	IsClosed() bool
}

// sessionReengager owns the rebuild-vs-update decision for one realtime room
// bridge. It does not itself dial OpenAI or touch media -- it reads the live
// session's closed state and, when closed, invokes the injected rebuild
// callback (which the voice bridge implements as a fresh Connect + executor
// re-wire). It is single-purpose and stateless beyond its collaborators so it
// stays trivially unit-testable.
type sessionReengager struct {
	// current returns the session whose liveness is being evaluated. Indirected
	// (not a stored pointer) so the reengager always reads the bridge's CURRENT
	// session -- a rebuild swaps the bridge's session field, and a subsequent
	// re-engage must evaluate the new one.
	current func() reengageSession
	// rebuild establishes a fresh realtime session + executor and swaps them onto
	// the bridge. Returns an error if the fresh dial fails (the caller then leaves
	// the dead session in place and skips the update; the room idle watchdog and
	// cascade fallback still apply). The voice bridge implements this as
	// rebuildSessionLocked; tests record the call + flip the fake to "open".
	rebuild func() error
	logger  *slog.Logger
	spaceID string
}

// ensureLiveSession guarantees a usable session before a gate-mode update is
// issued. It returns true when the caller may proceed to UpdateSession against
// the (now) live session, and false when no usable session is available (a
// rebuild was needed but failed) so the caller MUST skip the update rather than
// poke a dead handle. When the current session is closed it rebuilds exactly
// once; an already-live session is a no-op (the common path).
//
// This is the #1449 fix in one place: the re-engage path checks liveness BEFORE
// updating, so a mic re-enable after an empty/idle teardown rebuilds a working
// realtime session instead of spamming "session is closed".
func (r *sessionReengager) ensureLiveSession() bool {
	if r == nil {
		return true
	}
	sess := r.current()
	if sess != nil && !sess.IsClosed() {
		return true // live -- proceed to update (common path).
	}
	if r.rebuild == nil {
		return false
	}
	if r.logger != nil {
		r.logger.Info("voice-agent realtime room: re-engage on a closed session -- rebuilding (#1449)",
			"partition_id", r.spaceID)
	}
	if err := r.rebuild(); err != nil {
		if r.logger != nil {
			r.logger.Warn("voice-agent realtime room: re-engage rebuild failed -- leaving session down",
				"partition_id", r.spaceID, "err", err)
		}
		return false
	}
	return true
}
