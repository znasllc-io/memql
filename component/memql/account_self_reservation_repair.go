package memql

// THE OPERATOR ROW'S RESERVATION REPAIR (memql#5792).
//
// A cluster seeded before the installation hostname was kept out of the self
// account stores the cluster's own address as that row's `memqlDomain`. The
// client-domain guard refuses a name in cluster space, and it judges the
// MERGED payload -- so every write to that row, the owner's profile edit and
// the domain walk's own bookkeeping alike, was refused for a value nobody had
// submitted. normalizeSelfAccountReservation now clears such a name on the
// next write of any kind. This sweep is that write for the row nobody edits.
//
// # Why at boot and not as a SQL migration
//
// Whether a stored name is in cluster space depends on MEMQL_DOMAIN, which a
// migration cannot read, and the row is append-only: the repair is a new
// VERSION written through the engine, so the account's ordinary write path --
// its read-merge, its hooks, its node.updated automations -- is the one that
// stores it. The sweep only decides whether that write is needed.
//
// # Idempotent, and best-effort
//
// It writes only when the stored name is in cluster space, and the write
// clears it, so every boot after the first is one read and no write -- on every
// replica, in any order. A failure is reported to the caller, which logs it and
// lets boot continue, the account group backfill's discipline.
//
// The read goes through the ENGINE rather than a hand-rolled select, so the
// staged-data question is answered by the engine's own injection and there is
// no direct read site here to adjudicate.

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// repairSelfAccountReservation clears a cluster-space reserved name from the
// stored self account. Reports whether it wrote.
func (m *SeedMaterializer) repairSelfAccountReservation(ctx context.Context) (bool, error) {
	if m == nil || m.engine == nil {
		return false, fmt.Errorf("seed materializer: no engine")
	}
	// existingSelfAccount and recordAccountDomainCheck are both @serverOnly,
	// and the self row is cluster-owned, so the cluster-owner escape of the
	// system actor is the only branch that can see it -- the seed's own
	// reasoning (dsl/accounts/queries.memql).
	sweepCtx := auth.ContextWithInternalOrigin(systemActorContext(ctx))
	result, err := m.engine.Execute(sweepCtx, `query existingSelfAccount()`)
	if err != nil {
		return false, fmt.Errorf("existingSelfAccount: %w", err)
	}
	rows := MaterializeRows(result)
	if len(rows) == 0 {
		// Not materialized yet. The seed creates it without a domain, so
		// there is nothing to repair.
		return false, nil
	}
	name := normalizeDomainValue(stringFromAny(rows[0]["memqlDomain"]))
	if name == "" || !accountNameInClusterSpace(name, customDomainPolicyDomain()) {
		return false, nil
	}
	// The write names the two reservation stamps and NOT the name: an
	// omitted `memqlDomain` is inherited by the read-merge, which is exactly
	// the shape normalizeSelfAccountReservation clears. A mutation that
	// accepted the name would be a second writer of a field the domain walk
	// deliberately cannot set.
	call := fmt.Sprintf(`mutation recordAccountDomainCheck(accountId: %s, memqlReservedAt: "", memqlReservationReason: "")`,
		langparser.QuoteString(selfAccountId))
	if _, err := m.engine.Execute(sweepCtx, call); err != nil {
		return false, fmt.Errorf("recordAccountDomainCheck on %s: %w", selfAccountId, err)
	}
	return true, nil
}
