package memql

import (
	"context"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// What an OMITTED optional argument does to a stamp{} field (epic memql#5165).
//
// `accept`/`stamp` are parser sugar over a plain `update{}`, so a body line
// reading `domainToken: args.domainToken` has to answer one question when the
// caller never passed `domainToken`: does the field land in the delta as an
// empty value -- and therefore WIN the read-merge, blanking what is stored --
// or is it absent from the delta and inherited?
//
// The answer is load-bearing for the domain walk, which advances one state per
// pass and passes only the fields that state knows about
// (integrations/customdomain.writeAccountDomain renders the argument list and
// omits absent keys entirely). If an omitted arg blanked its field, every pass
// would erase the token it minted on the pass before, and the walk would never
// converge -- while looking, at each individual step, exactly correct.
//
// dropNoUnsetFields' comment (executor_mutation.go) states the neighbouring
// rule: read-merge inherits only for fields ABSENT from the delta, and a body
// written `x: args.x ?? ""` MATERIALISES an omitted arg into an explicit empty
// string that wins the merge. That says what `??` does. It does not say what a
// BARE `args.x` does, and the walk depends on the bare form. This measures it.
func TestAnOmittedOptionalArgLeavesItsStampFieldUntouched(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("partialstamp")
	accountId := "acct-" + suffix

	owner := "owner-" + suffix
	seedPrincipal(t, eng, owner, auth.RoleOwner)
	ctx := auth.ContextWithInternalOrigin(rankActorCtx(owner, auth.RoleOwner))

	// Create the account, then fill every domain field in one call.
	mustExec(t, eng, ctx, fmt.Sprintf(
		`mutation createClientAccount(accountId: %s, name: "Partial Stamp Co", domain: "partialstamp.example")`,
		langparser.QuoteString(accountId)))

	mustExec(t, eng, ctx, fmt.Sprintf(
		`mutation recordAccountDomainCheck(accountId: %s, domainToken: "tok-original", `+
			`domainStatus: "verifying", domainFailureReason: "dns_token_missing", `+
			`domainFailureDetail: "zone answered with nothing", `+
			`domainLastCheckedAt: "2026-09-08T10:00:00Z", `+
			`domainVerifiedAt: "2026-09-07T09:00:00Z", `+
			`memqlReservedAt: "2026-09-07T09:00:01Z")`,
		langparser.QuoteString(accountId)))

	before := readAccountRow(t, eng, ctx, accountId)
	for _, f := range []string{"domainToken", "domainStatus", "domainFailureReason",
		"domainFailureDetail", "domainLastCheckedAt", "domainVerifiedAt", "memqlReservedAt"} {
		if s, _ := before[f].(string); s == "" {
			t.Fatalf("setup did not populate %q: %#v", f, before[f])
		}
	}

	// The measurement: one field, seven omitted.
	mustExec(t, eng, ctx, fmt.Sprintf(
		`mutation recordAccountDomainCheck(accountId: %s, domainStatus: "verified")`,
		langparser.QuoteString(accountId)))

	after := readAccountRow(t, eng, ctx, accountId)

	if got, _ := after["domainStatus"].(string); got != "verified" {
		t.Errorf("domainStatus: the one supplied field did not land: got %q, want %q", got, "verified")
	}
	for _, f := range []string{"domainToken", "domainFailureReason", "domainFailureDetail",
		"domainLastCheckedAt", "domainVerifiedAt", "memqlReservedAt"} {
		wantV, _ := before[f].(string)
		gotV, _ := after[f].(string)
		if gotV != wantV {
			t.Errorf("%s: an omitted argument changed a stored field: got %q, want %q (unchanged)",
				f, gotV, wantV)
		}
	}
}

func mustExec(t *testing.T, eng *MemQLEngine, ctx context.Context, q string) {
	t.Helper()
	if _, err := eng.Execute(ctx, q); err != nil {
		t.Fatalf("execute %s: %v", q, err)
	}
}

// readAccountRow reads the account back through the real query + shape, so the
// assertion measures what a caller would actually see rather than raw storage.
func readAccountRow(t *testing.T, eng *MemQLEngine, ctx context.Context, accountId string) map[string]any {
	t.Helper()
	res, err := eng.Execute(ctx, fmt.Sprintf("query clientAccountById(accountId: %s)",
		langparser.QuoteString(accountId)))
	if err != nil {
		t.Fatalf("read account %s: %v", accountId, err)
	}
	rows := MaterializeRows(res)
	if len(rows) != 1 {
		t.Fatalf("read account %s: got %d rows, want 1", accountId, len(rows))
	}
	return rows[0]
}
