package memql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

// accounts_include_archived_db_test.go -- memql#4814.
//
// The registry read the MemQL OS Accounts app opens on, driven against a real
// engine and a real database.
//
// ===========================================================================
// WHY A DB TEST AND NOT ONLY THE SEAM TEST BESIDE IT
// ===========================================================================
// filter_arg_lhs_test.go proves the binding at the seam that was missing it.
// What it structurally cannot prove is that `clientAccountsAll` -- the actual
// construct, with its actual filter, its composite row-authz tier and the
// archived disjunct in place -- ANSWERS. That is the property that was broken,
// and it broke with every gate in the repo green: no test in the tree executed
// the query, nothing at load time compiles a filter, and the OS suite drives a
// fake connection whose `clientAccountsAll` is a vi.fn(). The whole feature
// shipped, and the first caller to reach the engine got
// `field "args.includeArchived" is not supported`.
//
// So this reads it three ways -- flag true, flag false, flag omitted -- and
// asserts the ARCHIVED SEMANTICS, not merely the absence of an error. A fix
// that made the term compile by dropping it would pass a smoke test and would
// silently put every filed-away client back in the default list.
func TestClientAccountsAllAnswersWithAndWithoutTheArchiveFlag(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)

	// A cluster owner: v1:accounts:account declares
	// @rowAuthz(owner="ownerUserId", clusterOwner) and the read ANDs the tier
	// in whatever the filter says.
	owner := auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: "v1:identity:user:accounts-probe",
		Role:   auth.RoleOwner,
	})

	active := runMutation(t, auth.ContextWithInternalOrigin(owner), eng, "createClientAccount", map[string]any{
		"accountId": "v1:accounts:account:probe-active",
		"name":      "Probe Active Client",
	})
	archived := runMutation(t, auth.ContextWithInternalOrigin(owner), eng, "createClientAccount", map[string]any{
		"accountId": "v1:accounts:account:probe-archived",
		"name":      "Probe Archived Client",
	})
	runMutation(t, auth.ContextWithInternalOrigin(owner), eng, "archiveClientAccount", map[string]any{
		"accountId": archived,
	})

	read := func(t *testing.T, call string) []string {
		t.Helper()
		result, err := eng.Execute(owner, call)
		if err != nil {
			t.Fatalf("%s failed: %v\n\nThis is the read MemQL OS opens the Accounts app on; a "+
				"failure here is an empty registry behind an error banner, not a degraded list.",
				call, err)
		}
		var ids []string
		for _, node := range result.Bundle.GetNodes() {
			ids = append(ids, node.Id)
		}
		return ids
	}

	t.Run("includeArchived true returns the archived row too", func(t *testing.T) {
		ids := read(t, `query clientAccountsAll(includeArchived: true)`)
		require.Contains(t, ids, active)
		require.Contains(t, ids, archived,
			"passing true must admit EVERYTHING -- a person looking for a client they filed "+
				"away wants it in its place in the list, marked")
	})

	t.Run("includeArchived false returns only the active row", func(t *testing.T) {
		ids := read(t, `query clientAccountsAll(includeArchived: false)`)
		require.Contains(t, ids, active)
		require.NotContains(t, ids, archived,
			"a checkbox bound to a boolean sends false, and false must NARROW. This is the case "+
				"a `when(args.includeArchived)` guard cannot express -- that guard drops on "+
				"ABSENCE and keeps a present false, which would widen the read")
	})

	t.Run("includeArchived omitted returns only the active row", func(t *testing.T) {
		ids := read(t, `query clientAccountsAll()`)
		require.Contains(t, ids, active)
		require.NotContains(t, ids, archived,
			"archived rows are excluded BY DEFAULT; the flag is what returns them")
	})
}

// The error a caller got before memql#4814, pinned by its text, so a
// regression is recognised as this bug rather than investigated as a new one.
func TestClientAccountsAllDoesNotReportAnUnsupportedArgsField(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	owner := auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: "v1:identity:user:accounts-probe",
		Role:   auth.RoleOwner,
	})
	if _, err := eng.Execute(owner, `query clientAccountsAll(includeArchived: true)`); err != nil {
		if strings.Contains(err.Error(), `"args.includeArchived" is not supported`) {
			t.Fatalf("the caller flag reached the filter compiler as a ROW FIELD again: %v", err)
		}
		t.Fatalf("clientAccountsAll failed for another reason: %v", err)
	}
}

// The first-run card's Save, with the id the BROWSER holds.
//
// The card sends `updateClientAccount(accountId: "self")` -- bare, because
// that is what the wire delivered and clients never compose a canonical id
// (docs/public/concepts/identifiers.md). This drives that exact call and
// asserts it stamps `configuredAt` on the SAME row the seed wrote, because
// `configuredAt` is what retires the card: if the bare id did not resolve, the
// write would land somewhere else or fail, and the owner would answer the
// setup question to no effect.
func TestFirstRunSaveResolvesTheBareSelfId(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	owner := auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: "v1:identity:user:accounts-probe",
		Role:   auth.RoleOwner,
	})
	internal := auth.ContextWithInternalOrigin(owner)

	// Seeded the way the boot automation seeds it: CANONICAL id, no
	// configuredAt. That asymmetry -- canonical in, bare back out -- is the
	// whole point of the case.
	runMutation(t, internal, eng, "createClientAccount", map[string]any{
		"accountId": "v1:accounts:account:self",
		"name":      "My company",
	})

	if _, err := eng.Execute(internal, `mutation updateClientAccount(accountId: "self", name: "Acme Consulting")`); err != nil {
		t.Fatalf("the card's Save, sent with the bare id the browser holds, failed: %v", err)
	}

	res, err := eng.Execute(owner, `query clientAccountById(accountId: "self")`)
	require.NoError(t, err, "the detail read takes the same bare id")
	nodes := res.Bundle.GetNodes()
	require.Len(t, nodes, 1, "the bare id must resolve to the seeded singleton, not a second row")
	require.Equal(t, "v1:accounts:account:self", nodes[0].Id,
		"the write must have landed on the row the seed created")

	payload := nodes[0].Payload.AsMap()
	require.Equal(t, "Acme Consulting", payload["name"])
	require.NotEmpty(t, payload["configuredAt"],
		"updateClientAccount stamps configuredAt, and that stamp is what retires the first-run card")
}
