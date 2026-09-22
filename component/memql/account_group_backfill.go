package memql

// THE ACCOUNT GROUP BACKFILL (epic memql#5165, D5).
//
// Every account gets its group. The `ensureAccountGroup` automation covers
// accounts created from now on; this covers the ones already there -- the self
// account included, and every client an operator entered before this shipped.
//
// # Why it is here and not an automation
//
// An automation fires on an EVENT. There is no event for "an account that has
// existed since before groups did", so a cluster upgrading into this epic
// would have a registry full of accounts whose people can reach nothing, with
// every screen looking correct. The boot sweep is the only thing that closes
// that, and it is the same shape reconcileSkillCatalog uses for the same
// reason.
//
// # Idempotency is the derived id, not this loop
//
// `groupEnsureForAccount` writes at acct-<accountShortId> and returns without
// writing when an active group is already there, so running this on every boot
// of every replica costs one read per account and writes nothing. That
// property belongs to the builtin rather than to any of its three callers --
// which is what makes it true for the automation and the sweep alike.
//
// # Best-effort, per row
//
// A failure on one account is logged and the sweep continues, the
// reconcileAssistantSkills discipline: one malformed row must not stop every
// other account from getting its group, and a boot that refuses to finish
// because a client's name is odd is a worse failure than a missing group.

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// AccountGroupBackfillReport is what one sweep did.
type AccountGroupBackfillReport struct {
	// Scanned is the number of active accounts the sweep saw.
	Scanned int
	// Created is the number that gained a group. Zero on every boot after
	// the first, which is the expected steady state rather than a problem.
	Created int
	// Errors is one entry per account that failed, and never aborts the
	// pass.
	Errors []string
}

// reconcileAccountGroups gives every active account its group.
//
// Every read below goes through the ENGINE rather than a hand-rolled select, so
// the staged-data question is answered by the engine's own injection and there
// is no direct read site here to adjudicate. (A verdict comment with no read to
// rule on pre-authorizes whatever lands in the file next, which is why one is
// deliberately absent rather than copied from a sibling.)
func (m *SeedMaterializer) reconcileAccountGroups(ctx context.Context) (AccountGroupBackfillReport, error) {
	var report AccountGroupBackfillReport
	if m == nil || m.engine == nil {
		return report, fmt.Errorf("seed materializer: no engine")
	}
	// @serverOnly, so the origin stamp is what makes it run at all -- and
	// the system actor is what gives it an identity to run under. The two
	// are separate questions and both have to be answered here.
	sweepCtx := auth.ContextWithInternalOrigin(systemActorContext(ctx))
	result, err := m.engine.Execute(sweepCtx, `query accountsForGroupSweep()`)
	if err != nil {
		return report, fmt.Errorf("accountsForGroupSweep: %w", err)
	}
	ids := extractRowIds(result)
	report.Scanned = len(ids)

	for _, accountId := range ids {
		created, err := m.ensureGroupForAccount(sweepCtx, accountId)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", accountId, err))
			continue
		}
		if created {
			report.Created++
		}
	}
	users, err := m.listUserIds(sweepCtx)
	if err != nil {
		return report, err
	}
	for _, userID := range users {
		_, err := m.engine.Execute(sweepCtx, fmt.Sprintf(`builtin groupEnsureOperatorMembership(userId: %s)`, langparser.QuoteString(userID)))
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("operator %s: %v", userID, err))
		}
	}
	return report, nil
}

// ensureGroupForAccount calls the builtin and reads back whether it wrote.
//
// THROUGH THE BUILTIN, not by writing the row here. The automation, this sweep
// and any later caller must agree about what an account's group is named, what
// it is called and when it is skipped -- and three copies of that would
// disagree exactly when somebody changed one of them.
func (m *SeedMaterializer) ensureGroupForAccount(ctx context.Context, accountId string) (bool, error) {
	call := fmt.Sprintf(`builtin groupEnsureForAccount(accountId: %s)`, langparser.QuoteString(accountId))
	result, err := m.engine.Execute(ctx, call)
	if err != nil {
		return false, err
	}
	for _, row := range MaterializeRows(result) {
		if created, ok := row["created"].(bool); ok {
			return created, nil
		}
	}
	// A reply that does not say is reported as "not created" rather than
	// guessed at: the count is a report, and a report that inflates itself
	// on an unrecognised reply is worse than one that undercounts.
	return false, nil
}
