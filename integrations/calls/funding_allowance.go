package calls

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// Simulated funding is deliberately bounded, not a production financial
// ledger. Legacy pass allowance uses operational room time; usage previews
// never charge a wallet. Private history cannot grant either funding mode.
const fundingRoomScanLimit = 1000
const endReasonAllowance = "allowance_exhausted"

// Usage preview is an operator-authorized local resource grant, not a price or
// debit. These hard bounds remain independent of the illustrative UI rates.
const usagePreviewMode = "usage_preview"
const usagePreviewDuration = 180 * time.Minute
const usagePreviewMaxPeople = 8

type roomFunding struct {
	UsageConfig, UsageOperation, UsageDigest, UsageMeter string
	UsageReservedCents                                   int64
	UsageCostCents                                       *int64
	FundingPassID, FundingMode                           string
	FundingStartedAt, FundingDeadline                    time.Time
	FundingMaxPeople                                     int
}

func fundingFromRow(r map[string]any) roomFunding {
	return roomFunding{UsageConfig: rowString(r, "usageConfig"), UsageOperation: rowString(r, "usageOperation"), UsageDigest: rowString(r, "usageDigest"), UsageMeter: rowString(r, "usageMeter"), UsageReservedCents: int64(rowInt(r, "usageReservedCents")), UsageCostCents: optionalRoomCents(r, "usageCostCents"), FundingMode: rowString(r, "fundingMode"), FundingPassID: memql.BareShortId(rowString(r, "fundingPassId")), FundingStartedAt: rowTime(r, "fundingStartedAt"), FundingDeadline: rowTime(r, "fundingDeadline"), FundingMaxPeople: rowInt(r, "fundingMaxPeople")}
}

// A mode marker is necessary: old rooms without it retain their exact pass
// semantics, while preview rooms must never fall through to unlimited legacy.
func (f roomFunding) bounded() bool { return f.FundingPassID != "" || f.FundingMode != "" }

// Opening from a link may have no payer session. Check the durable claimed key
// and live credentials under the payer gate rather than trusting client canHost.
func (s *Service) usagePreviewEligible(ctx context.Context, site callrules.SiteRef, keyID string, wallet *simulationWalletEntry, policy callrules.Policy) (bool, error) {
	if !policy.SimulatedFunding || (!policy.UsagePreview && !policy.CallConfiguration) || wallet == nil || wallet.BalanceCents <= 0 {
		return false, nil
	}
	key, err := s.store.keyByID(ctx, site.OwnerUserID, keyID)
	if err != nil {
		return false, err
	}
	if key == nil || key.SiteID != site.SiteID || !keyActive(key.Status) || key.UserHandle == "" {
		return false, nil
	}
	credentials, err := s.store.credentialsForKey(ctx, site.OwnerUserID, keyID)
	if err != nil {
		return false, err
	}
	for _, credential := range credentials {
		if credential.RevokedAt.IsZero() {
			return true, nil
		}
	}
	return false, nil
}

func allowanceUnavailable() error {
	return callrules.Refuse(callrules.ErrConflict, "allowance_unavailable", "the complete pass usage could not be verified")
}

// passAllowance reads one bounded, owner/site/key/pass-scoped page, including
// continuation metadata. A full scan is required: truncation never grants time.
// There is no raw SQL, client cursor or query interpolation at this seam.
func (s *Service) passAllowance(ctx context.Context, owner, siteID, keyID string, pass callrules.AccountPass, atTime time.Time) (callrules.KeyAllowance, error) {
	call, err := langparser.RenderCall("callsRoomsForFundingPass", map[string]any{"siteId": siteID, "keyId": keyID, "passId": pass.ID})
	if err != nil {
		return callrules.KeyAllowance{}, allowanceUnavailable()
	}
	owned, ok := rowsAs(auth.ContextWithInternalOrigin(ctx), owner)
	if !ok {
		return callrules.KeyAllowance{}, allowanceUnavailable()
	}
	result, err := s.store.engine.Execute(memql.ContextWithRootsOnlyRead(memql.ContextWithFreshRead(owned)), "query "+call)
	if err != nil {
		return callrules.KeyAllowance{}, allowanceUnavailable()
	}
	executed, ok := result.(*memql.ExecuteResult)
	if !ok || executed == nil || (executed.GetMeta() != nil && (executed.GetMeta().Cursor != "" || executed.GetMeta().HasMore)) {
		return callrules.KeyAllowance{}, allowanceUnavailable()
	}
	rows := memql.MaterializeRows(executed.OutputPayload())
	if len(rows) > fundingRoomScanLimit || (len(rows) == fundingRoomScanLimit && executed.GetMeta() == nil) {
		return callrules.KeyAllowance{}, allowanceUnavailable()
	}
	intervals := make([]callrules.FundedRoomTime, 0, len(rows))
	for _, raw := range rows {
		room := roomFromRow(raw)
		if room.SiteID != siteID || room.KeyID != keyID || room.OwnerUserID != owner || room.FundingPassID != pass.ID || (!room.open() && room.EndedAt.IsZero()) {
			return callrules.KeyAllowance{}, allowanceUnavailable()
		}
		intervals = append(intervals, callrules.FundedRoomTime{Start: room.FundingStartedAt, End: room.EndedAt, Deadline: room.FundingDeadline})
	}
	allowance, err := callrules.PassAllowance(pass, intervals, atTime)
	if err != nil {
		return callrules.KeyAllowance{}, allowanceUnavailable()
	}
	return allowance, nil
}

// fundingForOpening runs under the payer key gate. A wallet's existence is
// permanent provenance: disabling the operator switch or exhausting a pass
// must never turn that key into a legacy unlimited operator key.
func (s *Service) fundingForOpening(ctx context.Context, site callrules.SiteRef, keyID string, now time.Time) (roomFunding, callrules.KeyAllowance, error) {
	wallet, err := s.simulatedWallet(ctx, site.OwnerUserID, site.SiteID, keyID)
	if err != nil || wallet == nil {
		return roomFunding{}, callrules.KeyAllowance{}, err
	}
	policy, err := s.store.PolicyFor(ctx, site.OwnerUserID, site.SiteID)
	if err != nil {
		return roomFunding{}, callrules.KeyAllowance{}, err
	}
	if !policy.SimulatedFunding {
		return roomFunding{}, callrules.KeyAllowance{}, callrules.Refuse(callrules.ErrForbidden, "simulation_disabled", "simulated funding is disabled for this site")
	}
	rooms, err := s.store.openRoomsForKey(ctx, site.OwnerUserID, keyID)
	if err != nil {
		return roomFunding{}, callrules.KeyAllowance{}, err
	}
	if len(rooms) > 0 {
		return roomFunding{}, callrules.KeyAllowance{}, callrules.Refuse(callrules.ErrConflict, callrules.ReasonKeyInCall, "one funded key covers one open room")
	}
	preview, err := s.usagePreviewEligible(ctx, site, keyID, wallet, policy)
	if err != nil {
		return roomFunding{}, callrules.KeyAllowance{}, err
	}
	if preview && policy.UsagePreview {
		deadline := now.Add(usagePreviewDuration)
		return roomFunding{FundingMode: usagePreviewMode, FundingDeadline: deadline, FundingMaxPeople: usagePreviewMaxPeople}, callrules.KeyAllowance{AvailableUntil: deadline}, nil
	}
	if wallet.Pass == nil || now.Before(wallet.Pass.ActivatedAt) || !now.Before(wallet.Pass.ExpiresAt) {
		return roomFunding{}, callrules.KeyAllowance{}, callrules.Refuse(callrules.ErrConflict, "pass_required", "an active simulated pass or funded usage preview is required")
	}
	allowance, err := s.passAllowance(ctx, site.OwnerUserID, site.SiteID, keyID, *wallet.Pass, now)
	if err != nil {
		return roomFunding{}, callrules.KeyAllowance{}, err
	}
	if !allowance.AvailableUntil.After(now) {
		return roomFunding{}, callrules.KeyAllowance{}, callrules.Refuse(callrules.ErrConflict, endReasonAllowance, "the pass has no room time available now")
	}
	return roomFunding{FundingPassID: wallet.Pass.ID, FundingDeadline: wallet.Pass.ExpiresAt, FundingMaxPeople: wallet.Pass.MaxPeople}, allowance, nil
}

// A booking is an intention, not a reserved financial balance. Preview bookings
// obey the local duration ceiling; legacy bookings must fit their pass allowance.
// Opening rechecks eligibility because it may change after booking.
// Cancellation deliberately does not call this: expiry cannot prevent cleanup.
func (s *Service) checkFundingBooking(ctx context.Context, h keyHolder, start, end time.Time) error {
	wallet, err := s.simulatedWallet(ctx, h.site.OwnerUserID, h.site.SiteID, h.row.KeyID)
	if err != nil || wallet == nil {
		return err
	}
	policy, err := s.store.PolicyFor(ctx, h.site.OwnerUserID, h.site.SiteID)
	if err != nil {
		return err
	}
	if !policy.SimulatedFunding {
		return callrules.Refuse(callrules.ErrForbidden, "simulation_disabled", "simulated funding is disabled for this site")
	}
	preview, err := s.usagePreviewEligible(ctx, h.site, h.row.KeyID, wallet, policy)
	if err != nil {
		return err
	}
	if preview && policy.UsagePreview {
		if end.Sub(start) > usagePreviewDuration {
			return callrules.Refuse(callrules.ErrInvalid, "preview_duration", "local preview calls last at most 180 minutes")
		}
		return nil
	}
	if wallet.Pass == nil || !s.now().Before(wallet.Pass.ExpiresAt) || start.Before(wallet.Pass.ActivatedAt) || end.After(wallet.Pass.ExpiresAt) {
		return callrules.Refuse(callrules.ErrConflict, "pass_required", "the scheduled call must fit within the current simulated pass")
	}
	allowance, err := s.passAllowance(ctx, h.site.OwnerUserID, h.site.SiteID, h.row.KeyID, *wallet.Pass, start)
	if err != nil {
		return err
	}
	if end.After(allowance.AvailableUntil) {
		return callrules.Refuse(callrules.ErrConflict, endReasonAllowance, "the scheduled call exceeds the pass allowance")
	}
	return nil
}

// Explain a valid account's unavailable entitlement without changing the
// stronger frozen/banned-key refusal or revealing another site's key.
func (s *Service) fundingKeyRefusal(ctx context.Context, site callrules.SiteRef, keyID string) error {
	key, err := s.store.keyByID(ctx, site.OwnerUserID, keyID)
	if err != nil || key == nil || key.SiteID != site.SiteID || !keyActive(key.Status) {
		return err
	}
	policy, err := s.store.PolicyFor(ctx, site.OwnerUserID, site.SiteID)
	if err != nil {
		return err
	}
	if !policy.SimulatedFunding {
		return callrules.Refuse(callrules.ErrForbidden, "simulation_disabled", "simulated funding is disabled for this site")
	}
	if policy.CallConfiguration {
		return fundingConflict("insufficient_balance")
	}
	wallet, err := s.simulatedWallet(ctx, site.OwnerUserID, site.SiteID, keyID)
	if err != nil || wallet == nil {
		return err
	}
	return callrules.Refuse(callrules.ErrConflict, "pass_required", "an active simulated pass is required")
}

// Spending entitlement and cancellation authority are separate. A pass that
// expired, or a simulation the operator disabled, still owns its bookings.
func (s *Service) heldKeyForCancellation(ctx context.Context, h keyHolder, now time.Time) error {
	current, err := s.store.sessionByID(ctx, h.site.OwnerUserID, h.row.ID)
	if err != nil {
		return err
	}
	if !current.usable(now) || current.KeyID != h.row.KeyID {
		return callrules.ErrNoSession
	}
	key, err := s.store.keyByID(ctx, h.site.OwnerUserID, current.KeyID)
	if err != nil {
		return err
	}
	if key == nil || key.SiteID != h.site.SiteID || !keyActive(key.Status) {
		return callrules.Refuse(callrules.ErrForbidden, callrules.ReasonKeyNotActive, "the key may not cancel this booking")
	}
	return nil
}

func fundedPolicy(policy callrules.Policy, funding roomFunding) callrules.Policy {
	if funding.bounded() {
		policy.MaxPeople = min(policy.MaxPeople, max(0, funding.FundingMaxPeople))
		if funding.FundingMode == usageMeteredMode {
			policy.MaxPeople = max(0, funding.FundingMaxPeople)
		}
		// Current invitations all enter as person seats, including external
		// agents. A separate machine seat must not add capacity above this pass.
		policy.MaxAgents = 0
	}
	return policy
}

func fundingRoomAllowed(room roomRow, now time.Time) error {
	if !room.bounded() {
		return nil
	}
	if room.FundingMode != "" && ((room.FundingMode != usagePreviewMode && room.FundingMode != usageMeteredMode) || room.FundingPassID != "") {
		return allowanceUnavailable()
	}
	if room.FundingDeadline.IsZero() || room.FundingMaxPeople <= 0 {
		return allowanceUnavailable()
	}
	if room.FundingMode == usagePreviewMode && (room.OpenedAt.IsZero() || !room.FundingDeadline.After(room.OpenedAt) || room.FundingDeadline.After(room.OpenedAt.Add(usagePreviewDuration)) || room.FundingMaxPeople > usagePreviewMaxPeople) {
		return allowanceUnavailable()
	}
	if room.FundingMode == usageMeteredMode {
		intervals, e := readUsageMeter(room)
		if e != nil {
			return e
		}
		if len(intervals) >= usageIntervalLimit {
			return callrules.Refuse(callrules.ErrEnded, "meter_capacity", "the local simulation connection limit has been reached")
		}
		c, err := decodeUsageConfig(room.UsageConfig)
		if err != nil || room.FundingMaxPeople != c.ParticipantLimit || room.UsageReservedCents != callrules.PreviewReservation(c) || !room.FundingDeadline.Equal(room.OpenedAt.Add(time.Duration(c.DurationMinutes)*time.Minute)) {
			return allowanceUnavailable()
		}
	}
	if !now.Before(room.FundingDeadline) {
		return callrules.Refuse(callrules.ErrEnded, endReasonAllowance, "the room's admitted allowance has ended")
	}
	return nil
}

// beginRoomFunding follows the successful seat write and precedes issuing a
// media token. A crash between those writes is repaired from the earliest
// persisted admission, never from the retry's clock. All callers hold the
// payer key and room gates in the same acquisition; no nested pool reservation.
func (s *Service) beginRoomFunding(ctx context.Context, room roomRow, now time.Time) (roomRow, error) {
	if !room.bounded() || !room.FundingStartedAt.IsZero() {
		return room, fundingRoomAllowed(room, now)
	}
	seats, err := s.store.seatsForRoom(ctx, room.OwnerUserID, room.ID)
	if err != nil {
		return room, err
	}
	var began time.Time
	for _, seat := range seats {
		if !seat.JoinedAt.IsZero() && (began.IsZero() || seat.JoinedAt.Before(began)) {
			began = seat.JoinedAt
		}
	}
	if began.IsZero() {
		return room, fundingRoomAllowed(room, now)
	}
	// Preview keeps the creation-time ceiling even after queuing or a failed
	// first-admission write. Admission never charges money or a legacy pass;
	// configured usage is settled separately from provider-proven intervals.
	if room.FundingMode == usagePreviewMode || room.FundingMode == usageMeteredMode {
		if err := fundingRoomAllowed(room, now); err != nil {
			return room, err
		}
		version := memql.VersionTimeAfter(room.Version, now)
		if err := s.store.updateRoom(ctx, room.OwnerUserID, room, map[string]any{"fundingStartedAt": began}, version); err != nil {
			return room, err
		}
		room.FundingStartedAt, room.Version = began, version
		return room, nil
	}
	wallet, err := s.simulatedWallet(ctx, room.OwnerUserID, room.SiteID, room.KeyID)
	if err != nil || wallet == nil || wallet.Pass == nil || wallet.Pass.ID != room.FundingPassID {
		return room, allowanceUnavailable()
	}
	allowance, err := s.passAllowance(ctx, room.OwnerUserID, room.SiteID, room.KeyID, *wallet.Pass, began)
	if err != nil {
		return room, err
	}
	if !allowance.AvailableUntil.After(began) {
		return room, callrules.Refuse(callrules.ErrEnded, endReasonAllowance, "the pass has no room time available")
	}
	version := memql.VersionTimeAfter(room.Version, now)
	if err := s.store.updateRoom(ctx, room.OwnerUserID, room, map[string]any{"fundingStartedAt": began, "fundingDeadline": allowance.AvailableUntil}, version); err != nil {
		return room, err
	}
	room.FundingStartedAt, room.FundingDeadline, room.Version = began, allowance.AvailableUntil, version
	return room, fundingRoomAllowed(room, now)
}

// Admission gates take the payer lock before room/server locks. Authentication
// may prepend its own account gate; the payer is immutable for a room.
func fundingAdmissionGates(room roomRow) []string {
	keys := []string{}
	if room.bounded() {
		keys = append(keys, keyGate(room.KeyID))
	}
	return append(keys, roomGate(room.ID), serverGate(room.MediaServerID))
}

// reconcileFunding reaches a known time ceiling before asking the media
// participant list. A failed list read must not postpone a deadline whose
// authority comes entirely from persisted rows. Network deletion stays outside
// all write gates; a failure keeps the room open and blocks a new sponsored room
// until the next reconciliation retries. This is a bounded periodic cleanup,
// not a claim of hard real-time media disconnection during provider outages.
func (s *Service) reconcileFunding(ctx context.Context, hint roomRow, backend MediaBackend, tally *reconcileTally) (bool, error) {
	if !hint.bounded() {
		return false, nil
	}
	if !hint.FundingStartedAt.IsZero() && s.now().Before(hint.FundingDeadline) {
		return false, nil
	}
	due, room, err := s.fundingEndState(ctx, hint)
	if err != nil || !due {
		return false, err
	}
	if familyRoom(room) {
		return true, s.closeFamily(ctx, room, "family_deadline")
	}
	s.checkpointUsage(ctx, room)
	if err := bounded(ctx, func(ctx context.Context) error { return backend.DeleteRoom(ctx, room.MediaRoom) }); err != nil {
		return true, err
	}
	release, err := s.gates(ctx, endGates(room)...)
	if err != nil {
		return true, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	current, err := s.store.roomByID(fresh, room.OwnerUserID, room.ID)
	if err != nil || current == nil || !current.open() {
		return true, err
	}
	if !current.bounded() || s.now().Before(current.FundingDeadline) {
		return true, nil
	}
	v, err := s.viewRoom(fresh, *current)
	if err != nil {
		return true, err
	}
	endedAt := current.FundingDeadline
	if endedAt.IsZero() {
		endedAt = s.now()
	}
	if err := s.endRows(fresh, v, endReasonAllowance, endedAt); err != nil {
		return true, err
	}
	tally.ended++
	return true, nil
}

func (s *Service) fundingEndState(ctx context.Context, hint roomRow) (bool, roomRow, error) {
	release, err := s.gates(ctx, endGates(hint)...)
	if err != nil {
		return false, hint, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	room, err := s.store.roomByID(fresh, hint.OwnerUserID, hint.ID)
	if err != nil || room == nil || !room.open() || !room.bounded() {
		return false, hint, err
	}
	now := s.now()
	if now.Before(room.FundingDeadline) && room.FundingStartedAt.IsZero() {
		// Repair a seat admission whose process stopped before recording the
		// funding start. The original joinedAt determines the same deadline.
		repaired, err := s.beginRoomFunding(fresh, *room, now)
		if err != nil && callrules.ReasonOf(err) != endReasonAllowance {
			return false, *room, err
		}
		*room = repaired
	}
	return !now.Before(room.FundingDeadline), *room, nil
}

// Clamp newly minted tokens to the room's immutable ceiling. JWT expiry alone
// does not disconnect existing LiveKit participants; reconciliation separately
// deletes the room, but an old token cannot reopen it beyond this deadline.
func (s *Service) admittedWithFunding(ctx context.Context, owner string, server MediaServer, credential MediaCredential, roomID string, seat seatRow, policy callrules.Policy) (callrules.JoinResult, error) {
	release, e := s.gates(ctx, roomGate(roomID))
	if e != nil {
		return callrules.JoinResult{}, e
	}
	defer release()
	room, err := s.store.roomByID(memql.ContextWithFreshRead(ctx), owner, roomID)
	if err != nil {
		return callrules.JoinResult{}, err
	}
	if room == nil || !room.open() {
		return refuseJoin(callrules.ReasonCallEnded), fmt.Errorf("%w: the call has ended", callrules.ErrEnded)
	}
	// Media-room creation can be delayed while another replica processes Leave,
	// Remove or a new admission. Never return a pass for the earlier generation.
	current, err := s.store.seatByID(memql.ContextWithFreshRead(ctx), owner, seat.ID)
	if err != nil {
		return callrules.JoinResult{}, err
	}
	if current == nil || !current.occupying() || current.RoomID != room.ID || current.Identity != seat.Identity {
		return refuseJoin("admission_changed"), callrules.Refuse(callrules.ErrConflict, "admission_changed", "the admission changed while connecting; rejoin the call")
	}
	if err := s.agentSeatAllowed(ctx, *room, seat); err != nil {
		return refuseJoin(callrules.ReasonOf(err)), err
	}
	if err := s.familySeatAllowedLocked(ctx, *room, seat); err != nil {
		return refuseJoin(callrules.ReasonOf(err)), err
	}
	if err := fundingRoomAllowed(*room, s.now()); err != nil {
		return refuseJoin(callrules.ReasonOf(err)), err
	}
	deadline, err := s.departureDeadline(memql.ContextWithFreshRead(ctx), *room, s.now())
	if err != nil {
		return refuseJoin(callrules.ReasonOf(err)), err
	}
	if !deadline.IsZero() {
		policy.PassTTL = min(policy.PassTTL, deadline.Sub(s.now()))
		if policy.PassTTL < time.Second {
			return refuseJoin("wrap_up"), callrules.Refuse(callrules.ErrEnded, "wrap_up", "the host's wrap-up deadline has arrived")
		}
	}
	if room.bounded() {
		policy.PassTTL = min(policy.PassTTL, room.FundingDeadline.Sub(s.now()))
		if policy.PassTTL < time.Second {
			return refuseJoin(endReasonAllowance), callrules.Refuse(callrules.ErrEnded, endReasonAllowance, "the room's allowance has ended")
		}
	}
	if err := s.ensureMessageReceipt(ctx, *room, *current); err != nil {
		return callrules.JoinResult{}, err
	}
	return admitted(server, credential, room.MediaRoom, seat, policy, room.MessageProtocol == callrules.MessageProtocol)
}

func fundedSeatKind(room roomRow, kind callrules.SeatKind) callrules.SeatKind {
	if room.bounded() && kind == callrules.KindAgent {
		return callrules.KindPerson
	}
	return kind
}
