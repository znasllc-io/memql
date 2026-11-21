package calls

import (
	"context"
	"encoding/json"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// A departure is one seat mutation: authority, the bound request receipt, the
// host's durable room policy and status=left commit together. Deriving the policy
// from these server-only seat rows avoids a crash between room and seat writes.
// Rejoining never clears a commitment. A firm deadline always wins over Keep.
type departureToken struct {
	Site, Session, Room, Seat string
	JoinedAt, LeftAt          time.Time
	Receipt                   string
}
type departureReceipt struct{ Operation, Request string }
type departureWrite struct {
	session callrules.Session
	request callrules.LeaveRequest
	token   departureToken
}

func leaveToken(pepper []byte, site string, session sessionRow, seat seatRow) string {
	b, _ := json.Marshal(departureToken{site, session.ID, seat.RoomID, seat.ID, seat.JoinedAt, seat.LeftAt, seat.DepartureReceipt})
	return callrules.SealHistoryToken(pepper, "seat-departure", b)
}
func departureConflict() error {
	return callrules.Refuse(callrules.ErrConflict, "leave_changed", "the leave request no longer names the current admission; refresh before making another choice")
}

// LeaveWithOptions also accepts a token-bound guest leave (no mode). Empty legacy
// requests keep Leave's original semantics. The token is not authority: the
// current session, host role, room and admission are rechecked under write gates.
func (s *Service) LeaveWithOptions(ctx context.Context, sess callrules.Session, req callrules.LeaveRequest) error {
	if req == (callrules.LeaveRequest{}) {
		return s.Leave(ctx, sess)
	}
	if (req.Mode != "" && req.Mode != callrules.LeaveKeepRunning && req.Mode != callrules.LeaveWrapUp) || len(req.LeaveToken) > 4096 || !fundingOperationID.MatchString(req.IdempotencyKey) {
		return callrules.ErrInvalid
	}
	site, err := checkSite(sess.Site)
	if err != nil {
		return callrules.ErrNoSession
	}
	fresh := memql.ContextWithFreshRead(ctx)
	session, pepper, err := s.session(fresh, site, sess.Token, s.now())
	if err != nil {
		return err
	}
	var token departureToken
	raw, ok := callrules.OpenHistoryToken(pepper, "seat-departure", req.LeaveToken)
	if !ok || json.Unmarshal(raw, &token) != nil || token.Site != site.SiteID || token.Session != session.ID || token.Room == "" || token.Seat == "" {
		return callrules.ErrInvalid
	}
	room, err := s.store.roomByID(fresh, site.OwnerUserID, token.Room)
	if err != nil {
		return err
	}
	if room == nil || room.SiteID != site.SiteID {
		return departureConflict()
	}
	departure := &departureWrite{sess, req, token}
	seat, err := s.store.seatByID(fresh, site.OwnerUserID, token.Seat)
	if err != nil {
		return err
	}
	if seat == nil || seat.RoomID != room.ID {
		return departureConflict()
	}
	if familyRoom(*room) && !room.open() && seat.KeyID == room.KeyID && seat.Host {
		return nil
	}
	// Refuse stale/unauthorized requests before provider work, and keep receipt
	// replay cheap. This is only a preflight: the gate repeats every check before
	// writing because role, admission and session can change during the checkpoint.
	_, replay, err := s.departureFields(fresh, *room, *seat, *departure, s.now())
	if err != nil {
		return err
	}
	if familyRoom(*room) && seat.KeyID == room.KeyID && seat.Host {
		return s.closeFamily(ctx, *room, "family_guardian_left")
	}
	if !replay {
		s.checkpointUsage(ctx, *room)
	}
	left, ended, err := s.leaveGatedOptions(ctx, site.OwnerUserID, *room, token.Seat, departure)
	if err != nil || left == nil {
		return err
	}
	s.finishLeave(ctx, *room, *left, ended)
	return nil
}

// departureFields returns the atomic additions, or a replay. A receipt is scoped
// to this authenticated session and exact token+mode, not merely the nonce. A
// stale request after a newer admission/leave fails closed instead of acting on
// the session's new call. One receipt per seat bounds storage; superseded retries
// are conflicts, never another leave or a restarted timer.
func (s *Service) departureFields(ctx context.Context, room roomRow, seat seatRow, d departureWrite, now time.Time) (map[string]any, bool, error) {
	session, _, err := s.session(ctx, d.session.Site, d.session.Token, now)
	if err != nil {
		return nil, false, err
	}
	if session.ID != d.token.Session {
		return nil, false, callrules.ErrNoSession
	}
	op := callrules.HistoryDigest(nil, "seat-leave-operation", session.ID, d.request.IdempotencyKey)
	digest := callrules.HistoryDigest(nil, "seat-leave-request", d.request.LeaveToken, d.request.Mode)
	var previous departureReceipt
	if seat.DepartureReceipt != "" && json.Unmarshal([]byte(seat.DepartureReceipt), &previous) != nil {
		return nil, false, departureConflict()
	}
	if previous.Operation == op {
		if previous.Request != digest {
			return nil, false, departureConflict()
		}
		return nil, true, nil
	}
	if !room.open() || session.SeatID != seat.ID || session.RoomID != room.ID || seat.RoomID != room.ID || !seat.occupying() || !seat.JoinedAt.Equal(d.token.JoinedAt) || !seat.LeftAt.Equal(d.token.LeftAt) || seat.DepartureReceipt != d.token.Receipt {
		return nil, false, departureConflict()
	}
	if d.request.Mode == "" && seat.Host && !familyRoom(room) {
		return nil, false, callrules.Refuse(callrules.ErrConflict, "leave_choice_required", "a current host must choose how the call continues")
	}
	if d.request.Mode != "" && !seat.Host {
		return nil, false, callrules.Refuse(callrules.ErrForbidden, callrules.ReasonNotHost, "only a current host can choose the room's departure policy")
	}
	fields := map[string]any{}
	switch d.request.Mode {
	case callrules.LeaveKeepRunning:
		if !room.bounded() || fundingRoomAllowed(room, now) != nil {
			return nil, false, callrules.Refuse(callrules.ErrConflict, "keep_running_unavailable", "this call has no remaining admitted hard deadline")
		}
		fields["departureKeepRunning"] = true
	case callrules.LeaveWrapUp:
		deadline := now.Add(5 * time.Minute)
		if !room.FundingDeadline.IsZero() && room.FundingDeadline.Before(deadline) {
			deadline = room.FundingDeadline
		}
		if !seat.DepartureWrapUpAt.IsZero() && seat.DepartureWrapUpAt.Before(deadline) {
			deadline = seat.DepartureWrapUpAt
		}
		fields["departureWrapUpAt"] = deadline
	}
	b, _ := json.Marshal(departureReceipt{op, digest})
	fields["departureReceipt"] = string(b)
	return fields, false, nil
}

// departurePolicy is stable across hosts, replicas and rejoins. It only consumes
// explicit server-authorized choices; media absence never creates Keep or Wrap-up.
func departurePolicy(room roomRow, seats []seatRow) (keep bool, deadline time.Time) {
	for _, seat := range seats {
		if seat.RoomID != room.ID {
			continue
		}
		keep = keep || seat.DepartureKeepRunning
		if !seat.DepartureWrapUpAt.IsZero() && (deadline.IsZero() || seat.DepartureWrapUpAt.Before(deadline)) {
			deadline = seat.DepartureWrapUpAt
		}
	}
	if !deadline.IsZero() && !room.FundingDeadline.IsZero() && room.FundingDeadline.Before(deadline) {
		deadline = room.FundingDeadline
	}
	// Keep cannot make an old unbounded room immortal, even if its data is malformed.
	return keep && room.bounded() && !room.FundingDeadline.IsZero(), deadline
}
func (s *Service) departureDeadline(ctx context.Context, room roomRow, now time.Time) (time.Time, error) {
	seats, err := s.store.seatsForRoom(ctx, room.OwnerUserID, room.ID)
	if err != nil {
		return time.Time{}, err
	}
	_, deadline := departurePolicy(room, seats)
	if !deadline.IsZero() && !now.Before(deadline) {
		return deadline, callrules.Refuse(callrules.ErrEnded, "wrap_up", "the host's firm wrap-up deadline has arrived")
	}
	return deadline, nil
}
