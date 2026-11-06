package customdomain

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// accountdoor_accounts.go -- the two reads that tell the sweep which doors
// SHOULD exist (epic memql#5168, design D).
//
// # WHY THE SWEEP IS DRIVEN FROM THE ACCOUNT SIDE
//
// A door's own work list (`accountFrontDoorsToReconcile`) excludes `live`,
// because a healthy cluster's steady state is doors that are already serving
// and re-checking them every two minutes would make the good case the
// expensive one. But `live` is exactly the state a WITHDRAWN reservation has
// to reach out of (design D9), so something has to notice.
//
// That something is this: one read of the accounts that HOLD a reservation,
// one read of the doors that are open, and the difference in both directions.
// A reservation with no door opens one; a door with no reservation, or one
// whose name has moved, is asked to tear down. The cost is a read whose size
// is the number of accounts an operator typed, once every two minutes.

// Reservation is one account's held MemQL name.
type Reservation struct {
	AccountID    string
	ReservedName string
	// Reason is the reservation reason CURRENTLY on the row, so the sweep can
	// skip a write that would change nothing. lastCheckedAt is not touched by
	// this write, so a no-op write would still version the row -- and a row
	// versioned every two minutes for every account is the strobe the arrival
	// cue's own rule exists to prevent.
	//
	// The read is only as good as the shape the query projects through:
	// accountDomainWalk did not carry this field, so it read "" on every pass,
	// an unheld name's reason was rewritten every two minutes and a held
	// name's was never cleared. TestEveryFieldTheAccountSweepsReadIsProjected
	// fails on any key this function reads that the shape leaves out.
	Reason string
}

// DoorAccountReader reads the account side of the walk.
type DoorAccountReader struct{ engine Engine }

// NewDoorAccountReader wraps an engine.
func NewDoorAccountReader(engine Engine) *DoorAccountReader {
	return &DoorAccountReader{engine: engine}
}

// HeldReservations returns every active account that HOLDS a reserved name.
//
// HELD is the whole predicate, and it is `memqlReservedAt` rather than
// `memqlDomain` (record A, D10). A name is stamped reserved in the SAME write
// that verifies the domain, and only when it also passed the guardrails -- so
// a non-empty `memqlDomain` with no `memqlReservedAt` is a name somebody typed
// that this cluster has not agreed to serve, and opening a door for it would
// request a certificate for a host whose ownership is unproven.
func (s *DoorAccountReader) HeldReservations(ctx context.Context) ([]Reservation, []Reservation, error) {
	rows, err := s.rows(ctx, "query accountsWithAReservedName()")
	if err != nil {
		return nil, nil, err
	}
	held := make([]Reservation, 0, len(rows))
	unheld := make([]Reservation, 0)
	for _, r := range rows {
		name := rowString(r, "memqlDomain")
		if name == "" {
			// Defence in depth: the query filters on it, and a row reaching
			// here without one would open a door with no hosts.
			continue
		}
		res := Reservation{
			AccountID:    memql.BareShortId(rowString(r, "id")),
			ReservedName: name,
			Reason:       rowString(r, "memqlReservationReason"),
		}
		if rowString(r, "memqlReservedAt") == "" {
			unheld = append(unheld, res)
			continue
		}
		held = append(held, res)
	}
	return held, unheld, nil
}

// RecordReservationReason writes WHY an account's name is not held, or clears
// it when the name is held.
//
// IT RIDES recordAccountDomainCheck rather than a writer of its own, on
// memql#5165's own instruction and because the alternative is two writers of
// one row's domain fields. That is only correct if an omitted optional
// argument in a `stamp` block leaves its field alone rather than blanking it,
// which is a property the tree documented nowhere and two callers already
// depended on -- so it is now proved by
// TestAnOmittedStampArgumentLeavesTheFieldAlone in component/memql, with a
// control so it cannot pass against a mutation that writes nothing.
func (s *DoorAccountReader) RecordReservationReason(ctx context.Context, accountID, reason string) error {
	if s == nil || s.engine == nil {
		return fmt.Errorf("customdomain: no engine wired")
	}
	q := fmt.Sprintf("mutation recordAccountDomainCheck(accountId: %s, memqlReservationReason: %s)",
		langparser.QuoteString(accountID), langparser.QuoteString(reason))
	if _, err := s.engine.Execute(doorContext(ctx), q); err != nil {
		return fmt.Errorf("customdomain: %s: %w", firstWord(q), err)
	}
	return nil
}

// The typed reasons a reserved name is not held (epic memql#5168, design C).
//
// The middle two are record A's own guard codes, reused verbatim rather than
// re-spelled: the guard already emits them and the rail already has to key on
// them, and a second spelling of one refusal is a second thing to keep in step.
const (
	ReasonOwnershipUnproven = "ownership_unproven"
)

// LiveAndPendingDoors returns every door that is not `removed` -- the set to
// compare against the reservations above.
//
// INCLUDES `live`, which is what makes the D9 teardown reachable at all, and
// includes `removing` so a door already coming down is not asked to come down
// again on every pass.
func (s *DoorAccountReader) LiveAndPendingDoors(ctx context.Context) ([]Door, error) {
	rows, err := s.rows(ctx, "query accountFrontDoorsOpen()")
	if err != nil {
		return nil, err
	}
	out := make([]Door, 0, len(rows))
	for _, r := range rows {
		out = append(out, doorFromRow(r))
	}
	return out, nil
}

// The reads and the write here run under doorContext, which stamps the
// synthetic cluster-owner identity AND internal origin -- naming
// auth.ContextWithInternalOrigin in this file as well is idempotent and
// deliberate, for the reason accountdoor.go gives: both constructs this file
// calls are @serverOnly, the engine refuses one whose origin is not internal,
// and the conformance gate that catches that reads per FILE. Somebody grepping
// here has to find the stamp rather than chase it two files over.
var _ = auth.ContextWithInternalOrigin

func (s *DoorAccountReader) rows(ctx context.Context, query string) ([]map[string]any, error) {
	if s == nil || s.engine == nil {
		return nil, fmt.Errorf("customdomain: no engine wired")
	}
	res, err := s.engine.Execute(doorContext(ctx), query)
	if err != nil {
		return nil, fmt.Errorf("customdomain: %s: %w", firstWord(query), err)
	}
	return memql.MaterializeRows(res), nil
}
