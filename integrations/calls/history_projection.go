package calls

import (
	"context"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// The dirty bit is committed in the operational source write, not in this
// secondary effect. A crash, timeout or failed projection therefore leaves a
// durable reason for every replica to withhold incomplete account totals.
// Acknowledge only the exact source version that was projected. A lost fence or
// unavailable database leaves the bit set; the bounded repair retries it.
func (s *Service) acknowledgeHistorySource(ctx context.Context, owner, projectedKey string, source any) {
	var gate string
	switch row := source.(type) {
	case roomRow:
		if row.KeyID != projectedKey || projectedKey == "" {
			return
		}
		if row.HistoryPending {
			gate = roomGate(row.ID)
		}
	case bookingRow:
		if row.KeyID != projectedKey || projectedKey == "" {
			return
		}
		if row.HistoryPending {
			gate = bookingGate(row.ID)
		}
	case seatRow:
		if row.KeyID != projectedKey || projectedKey == "" {
			return
		}
		if row.HistoryPending {
			gate = roomGate(row.RoomID)
		}
	}
	if gate == "" {
		return
	}
	// The history entry gate has already released. Take one source gate now so
	// this secondary acknowledgment cannot invalidate an in-flight host control's
	// read/version fence. The original projected version is still required.
	release, err := s.gates(ctx, gate)
	if err != nil {
		return
	}
	defer release()
	fields := map[string]any{"historyPending": false}
	switch row := source.(type) {
	case roomRow:
		if row.HistoryPending {
			_ = s.store.updateRoom(ctx, owner, row, fields, memql.VersionTimeAfter(row.Version, s.now()))
		}
	case bookingRow:
		if row.HistoryPending {
			_ = s.store.updateBooking(ctx, owner, row, fields, memql.VersionTimeAfter(row.Version, s.now()))
		}
	case seatRow:
		if row.HistoryPending {
			_ = s.store.updateSeat(ctx, owner, row, fields, memql.VersionTimeAfter(row.Version, s.now()))
		}
	}
}

// Called only after successful Start/Join/Queue returns from its room/account
// gates. Secondary history cannot turn a valid one-time link/pass response into
// a failure. The original source bit is recovery authority if this bounded work
// is interrupted. No goroutine or unbounded background task survives the request.
func (s *Service) observeSessionHistory(ctx context.Context, sess callrules.Session) {
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	h, err := s.authenticatedKey(memql.ContextWithFreshRead(bounded), sess)
	if err == nil {
		err = s.observeCurrentHistory(bounded, h)
	}
	if err != nil && s.logger != nil {
		s.logger.Warn("calls: source history remains pending bounded repair", "component", pluginName)
	}
}
