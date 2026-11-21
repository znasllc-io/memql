package calls

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// Guest retirement is an access lifecycle, never a financial/evidence deletion.
// The approved policy excludes every claimed or previously funded account.
const guestInactivityPeriod = 30 * 24 * time.Hour
const guestRetirementBatch = 50

type guestRetirementScan struct {
	mu     sync.Mutex
	cursor string
}

// ReconcileGuestKeys visits one bounded keyset page. Each process keeps its own
// cursor for fairness; the key/account gates and fresh rows decide eligibility,
// so a restart or another replica can repeat the scan without repeating an effect.
func (s *Service) ReconcileGuestKeys(ctx context.Context) error {
	if s.guestRetirement == nil {
		return fmt.Errorf("calls: guest retirement scanner is unavailable")
	}
	s.guestRetirement.mu.Lock()
	defer s.guestRetirement.mu.Unlock()
	rows, next, err := s.store.guestRetirementCandidates(ctx, s.guestRetirement.cursor)
	if err != nil {
		return err
	}
	var first error
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		keyID := memql.BareShortId(rowString(row, "id"))
		owner := memql.BareShortId(rowString(row, "ownerUserId"))
		if keyID == "" || owner == "" {
			return fmt.Errorf("calls: invalid guest retirement candidate")
		}
		if err := s.retireGuestKey(ctx, owner, keyID); err != nil && first == nil {
			first = err
		}
	}
	s.guestRetirement.cursor = next
	return first
}

func (s *Service) retireGuestKey(ctx context.Context, owner, keyID string) error {
	release, err := s.gates(ctx, accountGate(keyID), keyGate(keyID))
	if err != nil {
		return err
	}
	defer release()
	fresh := auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx))
	key, err := s.store.keyByID(fresh, owner, keyID)
	if err != nil {
		return err
	}
	now := s.now()
	earliest := now.Add(-guestInactivityPeriod)
	if key == nil || key.Kind != "guest" || !keyActive(key.Status) || key.UserHandle != "" || key.IssuedAt.IsZero() || key.IssuedAt.After(earliest) || key.Version.After(earliest) {
		return nil
	}
	policy, err := s.store.PolicyFor(fresh, owner, key.SiteID)
	if err != nil {
		return err
	}
	if policy.GuestInactiveDays < 30 || policy.GuestInactiveDays > 365 {
		return fmt.Errorf("calls: invalid guest inactivity policy")
	}
	cutoff := now.Add(-time.Duration(policy.GuestInactiveDays) * 24 * time.Hour)
	if key.IssuedAt.After(cutoff) || key.Version.After(cutoff) {
		return nil
	}
	credentials, err := s.store.credentialsForKey(fresh, owner, keyID)
	if err != nil {
		return err
	}
	if len(credentials) != 0 {
		return nil
	}
	// Keep every account with any financial record, even a now-empty wallet or
	// failed/pending provider operation. Retirement never adjudicates its value.
	wallet, err := s.simulatedWallet(fresh, owner, key.SiteID, keyID)
	if err != nil {
		return err
	}
	if wallet != nil {
		return nil
	}
	commerce, err := s.store.commerceState(fresh, owner, key.SiteID, keyID)
	if err != nil {
		return err
	}
	if commerce != nil {
		return nil
	}
	for _, name := range []string{"callsGuestRecentSessions", "callsGuestRecentSeats", "callsGuestHeldBookings", "callsGuestHeldRooms"} {
		args := map[string]any{"keyId": keyID}
		if name == "callsGuestRecentSessions" || name == "callsGuestRecentSeats" {
			args["cutoff"] = versionStamp(cutoff)
		}
		if name == "callsGuestRecentSessions" || name == "callsGuestHeldBookings" {
			args["asOf"] = versionStamp(now)
		}
		call, err := langparser.RenderCall(name, args)
		rows, err := s.store.readAs(fresh, owner, "guest retirement activity", call, err)
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			return nil
		}
	}
	call, err := langparser.RenderCall("callsGuestActiveSeats", map[string]any{"keyId": keyID})
	seats, err := s.store.readAs(fresh, owner, "guest retirement active seats", call, err)
	if err != nil {
		return err
	}
	if len(seats) >= 101 {
		return nil
	} // Incomplete inventory is never permission.
	for _, row := range seats {
		room, err := s.store.roomByID(fresh, owner, memql.BareShortId(rowString(row, "roomId")))
		if err != nil {
			return err
		}
		if room == nil || room.open() {
			return nil
		}
	}
	// This one authoritative status revokes all sessions and delegated actions;
	// their history remains intact. Join/claim/funding recheck while holding these
	// same account/key gates and cannot resurrect an already-retired guest.
	call, err = langparser.RenderCall("updateCallsKey", map[string]any{"keyId": keyID, "status": "retired", "versionTime": versionStamp(memql.VersionTimeAfter(key.Version, now))})
	return s.store.writeAs(fresh, owner, at(keyConcept, keyID, key.Version), "guest retirement", call, err)
}

// The engine's opaque keyset cursor handles ordering and equal-key boundaries;
// comparing graph identifiers lexically in DSL would not be a supported filter.
func (s *Store) guestRetirementCandidates(ctx context.Context, cursor string) ([]map[string]any, string, error) {
	cluster, err := asCluster(ctx)
	if err != nil {
		return nil, "", err
	}
	call, err := langparser.RenderCall("callsGuestRetirementCandidates", nil)
	if err != nil {
		return nil, "", fmt.Errorf("calls: render guest retirement: %w", scrubbed(err))
	}
	result, err := s.engine.Execute(memql.ContextWithCursor(memql.ContextWithRootsOnlyRead(memql.ContextWithFreshRead(cluster)), cursor), "query "+call)
	if err != nil {
		return nil, "", fmt.Errorf("calls: guest retirement inventory: %w", scrubbed(err))
	}
	executed, ok := result.(*memql.ExecuteResult)
	if !ok {
		return nil, "", fmt.Errorf("calls: guest retirement inventory has no continuation metadata")
	}
	next := ""
	if executed.GetMeta() != nil {
		next = executed.GetMeta().Cursor
	}
	return memql.MaterializeRows(executed.OutputPayload()), next, nil
}
