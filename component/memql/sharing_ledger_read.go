package memql

// The `fleetSharingLedger` READ (epic memql#5146, design D6).
//
// What a shared machine has done this week, for the person who lent it.
//
// ===========================================================================
// THE NARROWING HAPPENS HERE, NOT ON THE PAGE
// ===========================================================================
// A decision record carries a great deal a machine's owner must not see. This
// read folds it to counts BEFORE anything leaves the engine, so the promise
// holds even if the page is rewritten by somebody who never read the fold. A
// query returning the rows and letting the surface pick what to show would put
// the promise in a renderer, which is where it would eventually be lost.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// SharingLedgerConcept is the canonical id of the read's answer.
const SharingLedgerConcept = "v1:worker:sharingLedger"

// LedgerQuery and LedgerShape name the DSL constructs this read depends on, and
// ledgerProjection names the fields it takes off each returned row.
//
// THEY ARE NAMED HERE SO A GATE CAN WALK THEM. A query returns the shape it
// declares and nothing else, so a field this file reads that the shape does not
// project arrives as an empty string -- and an empty `executionSurface` fails
// the surface-prefix check below, is skipped, and folds to "No calls have run
// on this machine this week." That is a WRONG ANSWER WITH NO ERROR, told to the
// one person who lent the hardware, and it is exactly what happened when this
// read was first pointed at a shape built for the evidence fold.
const (
	LedgerQuery = "routerCallsOnMachine"
	LedgerShape = "routerCallLedger"

	// The three fields, named rather than positional: the slice below exists
	// for the gate to walk, and reading a field out of it by INDEX would let a
	// reorder swap the caller with the surface in silence.
	ledgerFieldSurface = "executionSurface"
	ledgerFieldUser    = "userId"
	ledgerFieldLevel   = "level"
)

var ledgerProjection = []string{ledgerFieldSurface, ledgerFieldUser, ledgerFieldLevel}

// LedgerProjection returns the fields the ledger read consumes, for the gate.
func LedgerProjection() []string {
	out := make([]string, len(ledgerProjection))
	copy(out, ledgerProjection)
	return out
}

// evaluateFleetSharingLedgerExpression serves the `fleetSharingLedger` builtin.
func (e *MemQLEngine) evaluateFleetSharingLedgerExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	registrationId := strings.TrimSpace(stringArg(args, "registrationId"))
	// THE AUTHORIZED READ FIRST. A ledger is about somebody's own machine, and
	// resolving it through `workersForUser` means a machine that is not the
	// caller's is not in the answer -- this function does not have to be
	// trusted to check, exactly as the pull and the probe do not.
	machine, err := e.modelPullMachineFor(ctx, registrationId)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	week := isoWeekOf(now)
	since := startOfISOWeek(now)

	// THE MACHINE'S OWN CALLS, not the fleet's. routerCallsInWindow is gated on
	// `actor.isClusterOwner` because its caller is a maintenance sweep; running
	// it here returns zero rows for every machine owner who is not also a
	// cluster owner, which is most of them.
	//
	// The surface is derived from a registration id the check above has already
	// proven belongs to the caller, so it cannot be pointed at anybody else's
	// machine by passing a different string.
	call, err := langparser.RenderCall(LedgerQuery, map[string]any{
		"surface": FleetReferencePrefix + trimConceptPrefix(registrationId),
		"since":   since.Format(time.RFC3339),
		"until":   now.Format(time.RFC3339),
	})
	if err != nil {
		return nil, fmt.Errorf("fleetSharingLedger: render the window read: %w", err)
	}
	// THE STAMP IS A LOCAL, AND IT IS NEVER RETURNED (epic memql#5327, D11).
	//
	// routerCallsOnMachine is @serverOnly now, because the ownership check
	// above was the only gate standing between a signed-in caller and every
	// user id that had run a call on any machine in the cluster. This read
	// therefore has to say it is server-initiated.
	//
	// Confined to THIS call, in a distinctly named variable, rather than
	// widening `ctx`: internal origin opens every @serverOnly construct for as
	// long as the context lives, which is the escalation memql#2989 refused.
	// The registration id it reads with has already been proven to belong to
	// the caller, by the authorized read above, so nothing caller-supplied
	// reaches the query un-checked.
	internalCtx := auth.ContextWithInternalOrigin(ctx)
	res, err := e.Execute(internalCtx, call)
	if err != nil {
		// A LEDGER THAT CANNOT BE READ IS NOT AN EMPTY LEDGER. Answering zero
		// would tell somebody who lent their machine that nobody used it, which
		// is a specific and wrong claim; the row says the read failed instead.
		return singleVirtualRow(SharingLedgerConcept, registrationId, map[string]any{
			"machineId": registrationId,
			"week":      week,
			"readable":  false,
			"sentence":  "This week's usage could not be read. It is not that nothing ran -- nobody looked.",
		})
	}

	rows := modelPullRows(res.OutputPayload())
	calls := make([]LedgerCall, 0, len(rows))
	for _, row := range rows {
		// The surface a call ran on is `fleet:<registrationId>`, so the machine
		// is derived rather than read: the decision record names WHERE a call
		// went, and this is that field's one consumer.
		surface := strings.TrimSpace(mapString(row, ledgerFieldSurface))
		machineId := strings.TrimPrefix(surface, FleetReferencePrefix)
		if surface == machineId || machineId == "" {
			continue
		}
		calls = append(calls, LedgerCall{
			MachineId:    machineId,
			ActingUserId: mapString(row, ledgerFieldUser),
			Level:        mapString(row, ledgerFieldLevel),
			Week:         week,
		})
	}

	// THE OWNER IS THE MACHINE'S, read off the row that proved it is the
	// caller's -- the split (design G4) is "for you" versus "for anybody else",
	// and "you" is whoever owns this machine.
	entry := FoldLedger(trimConceptPrefix(registrationId), machine.OwnerUserId, week, calls)
	// The machine id on the row is the CANONICAL one the caller asked with, so
	// the page does not have to know that the fold matched on the bare form.
	payload := entry.Row()
	payload["machineId"] = registrationId
	payload["readable"] = true
	payload["sentence"] = entry.Sentence()
	return singleVirtualRow(SharingLedgerConcept, registrationId, payload)
}

// isoWeekOf renders a time as YYYY-Www.
func isoWeekOf(t time.Time) string {
	year, week := t.ISOWeek()
	return fmt.Sprintf("%04d-W%02d", year, week)
}

// startOfISOWeek is the Monday 00:00 UTC of t's ISO week.
//
// MONDAY, because that is what ISOWeek counts from, and a window that started
// on a different day would label calls with a week number they did not fall in.
func startOfISOWeek(t time.Time) time.Time {
	day := int(t.UTC().Weekday())
	if day == 0 {
		day = 7 // Sunday closes the ISO week rather than opening one.
	}
	monday := t.UTC().AddDate(0, 0, -(day - 1))
	return time.Date(monday.Year(), monday.Month(), monday.Day(), 0, 0, 0, 0, time.UTC)
}
