package calls

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// A provider connection SID, not an admission or browser heartbeat, identifies
// one continuous media connection. Start/Through are provider-proven bounds;
// End is a signed disconnect. Missing observations never extrapolate through
// a gap. The bounded journal lives in the room's fenced row, so a crash cannot
// advance a checkpoint without retaining its intervals. Old rooms have no meter.
type usageInterval struct {
	SID      string    `json:"s"`
	Identity string    `json:"i"`
	Start    time.Time `json:"a"`
	Through  time.Time `json:"b"`
	End      time.Time `json:"e,omitempty"`
}

const usageIntervalLimit = 4096

func optionalRoomCents(r map[string]any, name string) *int64 {
	n, ok := rowIntPresent(r, name)
	if !ok {
		return nil
	}
	v := int64(n)
	return &v
}
func readUsageMeter(room roomRow) ([]usageInterval, error) {
	var rows []usageInterval
	if room.UsageMeter == "" || len(room.UsageMeter) > 2*1024*1024 || json.Unmarshal([]byte(room.UsageMeter), &rows) != nil || len(rows) > usageIntervalLimit {
		return nil, allowanceUnavailable()
	}
	for _, r := range rows {
		if r.SID == "" || r.Identity == "" || r.Start.IsZero() || r.Through.Before(r.Start) {
			return nil, allowanceUnavailable()
		}
	}
	return rows, nil
}
func clampUsageTime(at time.Time, room roomRow, now time.Time) time.Time {
	if at.After(now) {
		at = now
	}
	if at.After(room.FundingDeadline) {
		at = room.FundingDeadline
	}
	if at.Before(room.OpenedAt) {
		at = room.OpenedAt
	}
	return at.UTC().Truncate(time.Microsecond)
}
func (s *Service) writeUsageMeter(ctx context.Context, room roomRow, rows []usageInterval) (roomRow, error) {
	data, _ := json.Marshal(rows)
	if string(data) == room.UsageMeter {
		return room, nil
	}
	version := memql.VersionTimeAfter(room.Version, s.now())
	err := s.store.updateRoom(ctx, room.OwnerUserID, room, map[string]any{"usageMeter": string(data)}, version)
	if err == nil {
		room.UsageMeter, room.Version = string(data), version
	}
	return room, err
}
func usageConnection(rows []usageInterval, sid string) int {
	for i := range rows {
		if rows[i].SID == sid {
			return i
		}
	}
	return -1
}
func (s *Service) meterEvent(ctx context.Context, room roomRow, seat *seatRow, ev mediaEvent) (roomRow, error) {
	if room.FundingMode != usageMeteredMode || !room.open() || ev.ParticipantSID == "" || seat == nil {
		return room, nil
	}
	rows, err := readUsageMeter(room)
	if err != nil {
		return room, err
	}
	i := usageConnection(rows, ev.ParticipantSID)
	at := clampUsageTime(ev.At, room, s.now())
	if ev.Kind == eventParticipantLeft && !seat.LeftAt.IsZero() && at.After(seat.LeftAt) {
		at = seat.LeftAt
	}
	if ev.Kind == eventParticipantJoined {
		if !seat.occupying() || at.Before(seat.JoinedAt) {
			return room, nil
		}
		if i < 0 {
			if len(rows) >= usageIntervalLimit {
				return room, callrules.Refuse(callrules.ErrConflict, "meter_capacity", "the local simulation connection journal is full")
			}
			rows = append(rows, usageInterval{SID: ev.ParticipantSID, Identity: ev.Identity, Start: at, Through: at})
		}
	} else if i >= 0 && rows[i].Identity == ev.Identity && !at.Before(rows[i].Start) {
		// A late disconnect may shorten an earlier poll's observation. Settlement
		// consumes the smaller bound, so out-of-order webhooks cannot overcharge.
		if rows[i].End.IsZero() || at.Before(rows[i].End) {
			rows[i].End = at
		}
		rows[i].Through = at
	}
	return s.writeUsageMeter(ctx, room, rows)
}
func (s *Service) meterPresence(ctx context.Context, room roomRow, seats []seatRow, present []MediaParticipant, observedAt time.Time) (roomRow, error) {
	if room.FundingMode != usageMeteredMode {
		return room, nil
	}
	rows, err := readUsageMeter(room)
	if err != nil {
		return room, err
	}
	allowed := map[string]seatRow{}
	for _, seat := range seats {
		if seat.occupying() {
			allowed[seat.Identity] = seat
		}
	}
	for _, p := range present {
		seat, ok := allowed[p.Identity]
		if !ok || p.SID == "" || p.JoinedAt.IsZero() || p.JoinedAt.After(observedAt) {
			continue
		}
		start := clampUsageTime(p.JoinedAt, room, observedAt)
		if start.Before(seat.JoinedAt) {
			start = seat.JoinedAt
		}
		through := clampUsageTime(observedAt, room, observedAt)
		if through.Before(start) {
			continue
		}
		i := usageConnection(rows, p.SID)
		if i < 0 {
			if len(rows) >= usageIntervalLimit {
				continue
			}
			rows = append(rows, usageInterval{SID: p.SID, Identity: p.Identity, Start: start, Through: through})
			continue
		}
		if rows[i].Identity != p.Identity || !rows[i].End.IsZero() {
			continue
		}
		// The authenticated provider's same SID and JoinedAt prove this connection
		// persisted across an unavailable reconciliation pass. A new SID cannot.
		if through.After(rows[i].Through) {
			rows[i].Through = through
		}
	}
	return s.writeUsageMeter(ctx, room, rows)
}

// usageCost is a sweep over retained connection intervals. Identity overlap is
// unioned, so reordered reconnects never count one seat twice. Concurrent-count
// pricing is recalculated at each boundary, never over historical elapsed time.
func usageCost(room roomRow, end time.Time) (int64, error) {
	configuration, configErr := decodeUsageConfig(room.UsageConfig)
	if configErr != nil {
		return 0, configErr
	}
	if room.UsageReservedCents < 0 || room.UsageReservedCents > simulatedMaximumBalanceCents || room.FundingMaxPeople < 1 || room.FundingMaxPeople > 100 {
		return 0, allowanceUnavailable()
	}
	rows, err := readUsageMeter(room)
	if err != nil {
		return 0, err
	}
	type boundary struct {
		at       int64
		identity string
		delta    int
	}
	var events []boundary
	for _, r := range rows {
		a := max(r.Start.UnixMilli(), room.OpenedAt.UnixMilli())
		b := min(r.Through.UnixMilli(), end.UnixMilli(), room.FundingDeadline.UnixMilli())
		if !r.End.IsZero() {
			b = min(b, r.End.UnixMilli())
		}
		if b > a {
			events = append(events, boundary{a, r.Identity, 1}, boundary{b, r.Identity, -1})
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].at < events[j].at })
	counts := map[string]int{}
	people := 0
	var units, last int64
	for _, e := range events {
		if last != 0 {
			units += (e.at - last) * callrules.ConfigurationRate(configuration, min(people, room.FundingMaxPeople))
		}
		last = e.at
		before := counts[e.identity]
		counts[e.identity] += e.delta
		if before == 0 && counts[e.identity] > 0 {
			people++
		}
		if before > 0 && counts[e.identity] == 0 {
			people--
		}
	}
	// USD microdollars × milliseconds / 60,000 / 10,000 = cents. Round only
	// once, at final settlement; the reservation always rounds its maximum up.
	return min(room.UsageReservedCents, (units+300000000)/600000000), nil
}
func usageSettlementID(room roomRow) string {
	return callrules.HistoryDigest(nil, "simulated-room-settlement", room.SiteID, room.KeyID, room.ID)[:32]
}
func (s *Service) settleUsage(ctx context.Context, room roomRow, end time.Time) (*int64, error) {
	if room.FundingMode != usageMeteredMode {
		return nil, nil
	}
	c, configErr := decodeUsageConfig(room.UsageConfig)
	if configErr != nil || room.UsageReservedCents != callrules.PreviewReservation(c) {
		return nil, allowanceUnavailable()
	}
	operation := usageSettlementID(room)
	prior, err := s.store.simulatedWalletEntryByID(ctx, room.OwnerUserID, operation)
	if err != nil {
		return nil, err
	}
	if prior != nil {
		if prior.SiteID != room.SiteID || prior.KeyID != room.KeyID || prior.Kind != "settlement" || prior.DebitedCents > room.UsageReservedCents || prior.RequestDigest != callrules.HistoryDigest(nil, "simulated-room-cost", room.ID, room.UsageConfig) {
			return nil, allowanceUnavailable()
		}
		v := prior.DebitedCents
		return &v, nil
	}
	cost, err := usageCost(room, end)
	if err != nil {
		return nil, err
	}
	wallet, err := s.simulatedWallet(ctx, room.OwnerUserID, room.SiteID, room.KeyID)
	if err != nil {
		return nil, err
	}
	if wallet == nil || wallet.BalanceCents < cost {
		return nil, allowanceUnavailable()
	}
	entry := simulationWalletEntry{ID: operation, SiteID: room.SiteID, KeyID: room.KeyID, Kind: "settlement", RequestDigest: callrules.HistoryDigest(nil, "simulated-room-cost", room.ID, room.UsageConfig), Version: memql.VersionTimeAfter(wallet.Version, s.now()), BalanceCents: wallet.BalanceCents - cost, DebitedCents: cost, Pass: wallet.Pass}
	if err = s.store.createSimulatedWalletEntry(ctx, room.OwnerUserID, entry); err != nil {
		return nil, err
	}
	return &cost, nil
}

// Reservations are durable room rows, not destructive wallet debits. A committed
// settlement receipt releases its hold even if the final room-end write failed.
func (s *Service) usageWalletAmounts(ctx context.Context, owner, site, key string, wallet *simulationWalletEntry) (available, reserved int64, err error) {
	if wallet == nil {
		return 0, 0, nil
	}
	rooms, err := s.store.openRoomsForKey(ctx, owner, key)
	if err != nil {
		return 0, 0, err
	}
	for _, room := range rooms {
		if room.SiteID != site || room.KeyID != key {
			return 0, 0, allowanceUnavailable()
		}
		if room.FundingMode != usageMeteredMode {
			continue
		}
		c, configErr := decodeUsageConfig(room.UsageConfig)
		if configErr != nil || room.UsageReservedCents != callrules.PreviewReservation(c) {
			return 0, 0, allowanceUnavailable()
		}
		receipt, err := s.store.simulatedWalletEntryByID(ctx, owner, usageSettlementID(room))
		if err != nil {
			return 0, 0, err
		}
		if receipt == nil {
			reserved += room.UsageReservedCents
		}
	}
	if reserved < 0 || reserved > wallet.BalanceCents {
		return 0, 0, allowanceUnavailable()
	}
	return wallet.BalanceCents - reserved, reserved, nil
}

func equalOptionalCents(a, b *int64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// Take one final authenticated provider observation before an explicit leave,
// removal or end. A provider outage must never trap someone in a call: failure
// keeps the last proven interval and therefore undercharges, never extrapolates.
func (s *Service) checkpointUsage(ctx context.Context, hint roomRow) {
	if hint.FundingMode != usageMeteredMode || !hint.open() {
		return
	}
	observed := s.now()
	var present []MediaParticipant
	err := bounded(ctx, func(ctx context.Context) error {
		backend, e := s.backendFor(ctx, hint)
		if e != nil {
			return e
		}
		if backend == nil {
			return nil
		}
		present, e = backend.Participants(ctx, hint.MediaRoom)
		return e
	})
	if err != nil {
		return
	}
	release, err := s.gates(ctx, roomGate(hint.ID))
	if err != nil {
		return
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	room, err := s.store.roomByID(fresh, hint.OwnerUserID, hint.ID)
	if err != nil || room == nil || !room.open() {
		return
	}
	seats, err := s.store.seatsForRoom(fresh, hint.OwnerUserID, hint.ID)
	if err != nil {
		return
	}
	_, _ = s.meterPresence(fresh, *room, seats, present, observed)
}

// Coherent lock-free read for callers that may already own the payer gate
// (fundingAnswer). A settlement/top-up advances the immutable head; retry when
// it changed around the room/receipt read instead of mixing old total with a
// released hold. Reservation-only writes have a linearization point at the room
// scan and do not require a wallet mutation.
func (s *Service) usageWalletSnapshot(ctx context.Context, owner, site, key string) (*simulationWalletEntry, int64, int64, error) {
	fresh := memql.ContextWithFreshRead(ctx)
	for range 3 {
		before, err := s.simulatedWallet(fresh, owner, site, key)
		if err != nil {
			return nil, 0, 0, err
		}
		available, reserved, amountErr := s.usageWalletAmounts(fresh, owner, site, key, before)
		after, err := s.simulatedWallet(fresh, owner, site, key)
		if err != nil {
			return nil, 0, 0, err
		}
		stable := before == nil && after == nil || before != nil && after != nil && before.ID == after.ID && before.Version.Equal(after.Version)
		if stable {
			return before, available, reserved, amountErr
		}
	}
	return nil, 0, 0, fundingConflict("wallet_snapshot_changed")
}

func (s *Service) reconcileMeterCapacity(ctx context.Context, hint roomRow, backend MediaBackend, tally *reconcileTally) (bool, error) {
	if hint.FundingMode != usageMeteredMode {
		return false, nil
	}
	rows, err := readUsageMeter(hint)
	if err != nil {
		return true, err
	}
	if len(rows) < usageIntervalLimit {
		return false, nil
	}
	if familyRoom(hint) {
		return true, s.closeFamily(ctx, hint, "family_meter_capacity")
	}
	s.checkpointUsage(ctx, hint)
	if err = bounded(ctx, func(ctx context.Context) error { return backend.DeleteRoom(ctx, hint.MediaRoom) }); err != nil {
		return true, err
	}
	release, err := s.gates(ctx, endGates(hint)...)
	if err != nil {
		return true, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	room, err := s.store.roomByID(fresh, hint.OwnerUserID, hint.ID)
	if err != nil || room == nil || !room.open() {
		return true, err
	}
	v, err := s.viewRoom(fresh, *room)
	if err != nil {
		return true, err
	}
	if err = s.endRows(fresh, v, "meter_capacity", s.now()); err != nil {
		return true, err
	}
	tally.ended++
	return true, nil
}
