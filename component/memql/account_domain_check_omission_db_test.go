package memql

import (
	"context"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// Does an OMITTED optional argument in a `stamp` block skip the field, or write
// it empty?
//
// # WHY THIS TEST EXISTS RATHER THAN AN ASSUMPTION
//
// `recordAccountDomainCheck` (epic memql#5165) has eight optional arguments
// and a `stamp` block naming all eight. Its one caller, the domain walk in
// `integrations/customdomain/account_domain.go`, advances a row by at most one
// state per pass and OMITS the arguments that state says nothing about --
// which is only correct if omission is a skip. If it is a blank, that caller
// erases `domainToken` on every pass that does not mint one, and the ownership
// token a client published is lost while the walk reports progress.
//
// Epic memql#5168 then adds a ninth argument (`memqlReservationReason`) and
// writes it from a DIFFERENT sweep, which passes only that one. So the
// question stopped being academic: if omission blanks, that write would clear
// the whole domain walk on every front-door pass.
//
// The intent is documented in both places. The BEHAVIOUR was documented
// nowhere, and a shared assumption held by two callers is exactly the kind
// that is discovered by a client losing their verification.
//
// # THE ANSWER, AND THE HALF THIS TEST DOES NOT COVER
//
// Omission is a SKIP: the key is absent from the delta and the read-merge
// inherits the stored value. memql#5165's author measured the same thing
// independently and found the sharper half -- `args.X ?? ""` materialises an
// explicit empty string, which is PRESENT in the delta and WINS the merge. So
// the danger is not omission, it is somebody adding a `??` default to one of
// these lines for tidiness. That trap is guarded where a person would write it
// (a comment in the mutation body) rather than here, and by
// TestAnOmittedOptionalArgLeavesItsStampFieldUntouched, which is landing in a
// follow-up PR and drives the generic property with a `??` negative control.
//
// This test stays because it covers the specific caller this epic depends on:
// recordAccountDomainCheck, written with one field by the front-door sweep.
//
// Postgres-gated like its neighbours. CI's db-tests lane runs this package
// with MEMQL_REQUIRE_DB=1, so a skip there is a failure rather than a green.
func TestAnOmittedStampArgumentLeavesTheFieldAlone(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	seedPrincipal(t, eng, "stamp-omission-probe", auth.RoleOwner)
	owner := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: "v1:identity:user:stamp-omission-probe",
		Role:   auth.RoleOwner,
	}))

	id := runMutation(t, owner, eng, "createClientAccount", map[string]any{
		"accountId": "v1:accounts:account:" + uniqueSuffix("stamp-omission"),
		"name":      "Stamp Omission Probe",
	})

	// Fill the whole walk, the way a minting pass does.
	runMutation(t, owner, eng, "recordAccountDomainCheck", map[string]any{
		"accountId":           id,
		"domainToken":         "tok-do-not-lose-me",
		"domainStatus":        "verifying",
		"domainFailureReason": "dns_token_missing",
		"domainLastCheckedAt": "2026-09-08T12:00:00Z",
	})

	// Now write ONE field, the way the front-door sweep does.
	runMutation(t, owner, eng, "recordAccountDomainCheck", map[string]any{
		"accountId":           id,
		"domainLastCheckedAt": "2026-09-08T12:02:00Z",
	})

	row := readOneAccount(t, owner, eng, id)
	if got := fmt.Sprint(row["domainToken"]); got != "tok-do-not-lose-me" {
		t.Errorf("domainToken = %q after a write that omitted it.\n\n"+
			"An omitted optional argument BLANKS the field rather than skipping it. Two "+
			"callers depend on the opposite -- the domain walk omits the arguments each "+
			"state says nothing about, and the front-door sweep writes one field -- so "+
			"both are erasing the walk they are supposed to be advancing. Neither can use "+
			"this mutation; each needs a write naming only its own fields.", got)
	}
	if got := fmt.Sprint(row["domainStatus"]); got != "verifying" {
		t.Errorf("domainStatus = %q after a write that omitted it, want verifying", got)
	}
	if got := fmt.Sprint(row["domainFailureReason"]); got != "dns_token_missing" {
		t.Errorf("domainFailureReason = %q after a write that omitted it", got)
	}

	// THE CONTROL. Without it, every assertion above is satisfied by a
	// mutation that writes nothing at all.
	if got := fmt.Sprint(row["domainLastCheckedAt"]); got != "2026-09-08T12:02:00Z" {
		t.Errorf("domainLastCheckedAt = %q -- the field the second write DID name did not "+
			"change, so this test proves nothing about omission", got)
	}
}

// readOneAccount reads a client account back through its own named query.
func readOneAccount(t *testing.T, ctx context.Context, eng *MemQLEngine, id string) map[string]any {
	t.Helper()
	res, err := eng.Execute(ctx, fmt.Sprintf(
		"query clientAccountById(accountId: %s)", langparser.QuoteString(id)))
	if err != nil {
		t.Fatalf("reading the account back: %v", err)
	}
	rows := MaterializeRows(res)
	if len(rows) != 1 {
		t.Fatalf("read %d rows for account %q, want 1", len(rows), id)
	}
	return rows[0]
}
