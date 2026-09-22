package worker

// What a LIVE stream re-checks about itself (epic memql#5327).
//
// Everything here exists because one fact was established at stream open and
// then believed for the life of a connection that can outlive it by days: the
// token that admitted it, and the clock that timestamps it. Neither is checked
// again anywhere else on the recv path, and neither announces its own change.
//
// The registration broadcast (registration_watcher.go) is the OTHER half, and
// the two are deliberately independent rather than belt-and-braces: the
// watcher is fast and reaches every replica, but it needs the mesh; this needs
// nothing but the database the stream is already writing to.

import (
	"context"
	"errors"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// recordClockSkew stores the difference between the machine's stamp on this
// beat and the server's own clock (design D6).
//
// POSITIVE means the machine is AHEAD. A beat with no stamp -- a cockpit that
// predates the field -- leaves the last measurement alone rather than
// recording a zero, because zero is a real answer here ("the clocks agree")
// and silence is not that answer.
func (s *streamSession) recordClockSkew(hb *memqlv1.Heartbeat, serverNow time.Time) {
	if s == nil || hb == nil || hb.GetTs() == nil {
		return
	}
	skew := hb.GetTs().AsTime().Sub(serverNow)
	s.clockSkewMs = int(skew.Milliseconds())
	s.clockSkewSeen = true

	// One line per stream, not per beat. A machine's clock does not drift on
	// heartbeat timescales, and a warning every fifteen seconds for a laptop
	// whose NTP is off would bury the connect and disconnect lines it sits
	// between. The threshold is the online window, because that is the figure
	// at which the skew starts changing an answer somebody reads.
	if !s.clockSkewLogged && (skew > OnlineWindow || skew < -OnlineWindow) {
		s.clockSkewLogged = true
		if s.server != nil && s.server.logger != nil {
			s.server.logger.Warn("worker: the machine's clock disagrees with the cluster's by more than the online window",
				"registration_id", s.worker.RegistrationId,
				"skew_ms", s.clockSkewMs,
				"online_window", OnlineWindow.String(),
			)
		}
	}
}

// recheckIdentity re-resolves the worker identity that admitted this stream
// and ends the stream when nothing admits it any more (design D3).
//
// Returns true when the stream has been terminated, so the heartbeat handler
// stops rather than persisting a lastSeenAt that would keep a revoked machine
// reading as online for one more window.
//
// ONLY A STATED REFUSAL ENDS A STREAM. Three answers are possible and only one
// of them is a decision:
//
//   - active==false, or a past expiresAt -> END IT. Somebody decided, or the
//     clock did. This is the whole point.
//   - a lookup ERROR -> keep it. component/node's equivalent gate fails closed
//     and is right to: it runs at stream OPEN, where a wrong refusal costs one
//     retry a second later. This runs on live connections, where failing
//     closed on a database hiccup disconnects every paired machine in the
//     cluster, each of which then reconnects into the same hiccup.
//   - NOT FOUND -> keep it, and this is the one worth arguing. Revocation here
//     is a soft flag (revokeWorkerTokenIdentity sets active=false); nothing in
//     the tree deletes an identity row. So absence is not a decision anybody
//     made -- it is a read that did not see what is there, which is an outage
//     or a bug, and treating it as a revoke would disconnect every machine in
//     the cluster on a query that returned an empty page. Design D3.
//
// What the fail-open directions cost is at most one interval of extra life,
// and the reconnect afterwards goes through the open-time check, which DOES
// fail closed on all three.
func (s *streamSession) recheckIdentity(now time.Time) bool {
	if s == nil || s.server == nil || s.server.store == nil {
		return false
	}
	if !s.lastIdentityCheck.IsZero() && now.Sub(s.lastIdentityCheck) < IdentityRecheckInterval {
		return false
	}
	// Stamped BEFORE the read, not after. A lookup that takes longer than the
	// interval would otherwise re-enter on the next beat and queue a second
	// read behind the first, which is how a slow database turns into a read
	// storm on the one path that was supposed to be cheap.
	s.lastIdentityCheck = now

	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	ident, err := s.server.store.IdentityById(ctx, s.worker.IdentityId, s.worker.OwnerUserId)
	if err != nil {
		if s.server.logger != nil {
			s.server.logger.Warn("worker: could not re-resolve the stream's credential; keeping the stream",
				"registration_id", s.worker.RegistrationId,
				"identity_id", s.worker.IdentityId,
				"error", err,
			)
		}
		return false
	}

	if ident == nil {
		// See the not-found paragraph above. Logged at debug rather than warn:
		// a build with no credential table wired -- the dev-mode path -- would
		// otherwise warn once a minute per machine about a table it does not
		// have.
		if s.server.logger != nil {
			s.server.logger.Debug("worker: the stream's credential was not found; keeping the stream",
				"registration_id", s.worker.RegistrationId,
				"identity_id", s.worker.IdentityId,
			)
		}
		return false
	}
	reason := ""
	switch {
	case !ident.Active:
		reason = DisconnectReasonRevoked
	case !ident.ExpiresAt.IsZero() && !now.Before(ident.ExpiresAt):
		reason = DisconnectReasonTokenExpired
	}
	if reason == "" {
		return false
	}
	if s.server.logger != nil {
		s.server.logger.Info("worker: ending the stream; its credential no longer admits it",
			"registration_id", s.worker.RegistrationId,
			"identity_id", s.worker.IdentityId,
			"reason", reason,
		)
	}
	s.terminate(reason)
	return true
}

// terminate ends this stream at once under a named reason.
//
// It is the hook behind Worker.SetTerminateFunc, and it differs from
// requestDrain in exactly one way that matters: no grace. A drain is extended
// to work the cluster still wants finished; the callers here -- a revoked
// credential, an expired one, a superseded hold -- are decisions that this
// connection should not exist, and three more seconds of dispatches on it is
// three more seconds of what the decision was against.
//
// The reason lands on the "worker disconnected" line through the same
// drainReason field requestDrain uses, because a reader asking why a stream
// ended does not care which of the two paths set it.
func (s *streamSession) terminate(reason string) {
	if s == nil {
		return
	}
	if reason == "" {
		reason = DisconnectReasonRevoked
	}
	s.mu.Lock()
	if s.drainReason == "" {
		s.drainReason = reason
	}
	s.mu.Unlock()
	s.cancel()
}

// rotateCredential renews this stream's worker token and returns the new
// plaintext with its expiry (design D5).
//
// It is a thin wrapper over the store's one implementation, and the wrapping
// earns its place by doing the two things the handler must not do inline: it
// bounds the write with its own timeout rather than the stream's (a rotation
// that hung would hold the recv goroutine and stop every other message on the
// connection), and it keeps the plaintext a return value rather than a local
// in a function that also logs.
func (s *streamSession) rotateCredential(ctx context.Context) (string, time.Time, error) {
	if s == nil || s.server == nil || s.server.store == nil {
		return "", time.Time{}, errRotationUnavailable
	}
	// Derived from the handler's context rather than the session's: a rotation
	// is a message being answered, and its lifetime is that exchange.
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.server.store.RotateIdentity(writeCtx, s.worker.IdentityId, s.worker.OwnerUserId)
}

// errRotationUnavailable is the answer when this build has no store wired --
// the dev-mode path that runs the worker subsystem without persistence. The
// handler turns it into an empty RotationResponse, which is what every cockpit
// saw before rotation existed.
var errRotationUnavailable = errors.New("worker: no store is wired, so this stream's credential cannot be rotated")
