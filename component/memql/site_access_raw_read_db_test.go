package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// site_access_raw_read_db_test.go -- no client reads an access code or a session
// by any path but the builtins (deployable access codes, review of Task 3,
// finding 3; design record decision D14).
//
// ===========================================================================
// WHAT WENT WRONG
// ===========================================================================
// The builtins pass two gates before they read a row: the app:deployables/access
// part, and the organization gate that judges the part against the organization
// the call selected. A raw read passes neither, because it never reaches a
// builtin; the row tier alone decides it, and the code's tier admits the site's
// owner, a cluster owner and the people of the account it is tied to. In review,
// `concept==v1:platform:siteAccessCode && siteId==...` returned the labels, hints
// and digests of a site in another organization to a person the builtin refused
// for the same site -- and so did parentOf, sort(paginate(...)) and asOf, which are
// the same read dressed differently. No working code was exposed, only digests.
//
// The ruling reverses an earlier acceptance of that gap: both concepts are
// readable only by internal origin or by the cluster acting on its own, a
// synthetic actor (clientUnreadableConcepts, site_access_guards.go), which is
// what the server's own readers are and what no request can be. These tests assert it for every
// read shape the review named, for the four people who matter, with the
// server's own paths held up as controls so a pass cannot be an empty fixture.

// accessReadFixture is one code and one session, plus the four callers.
type accessReadFixture struct {
	ids                            siteAccessIDs
	owner, operator, member, other string
	site, codeStored, sessionStore string
	digest, tokenDigest, label     string
}

func newAccessReadFixture(t *testing.T, eng *MemQLEngine, name string) accessReadFixture {
	t.Helper()
	ids := newSiteAccessIDs(name)
	f := accessReadFixture{
		ids: ids, owner: ids.owner, operator: "operator-" + ids.owner, member: "member-" + ids.owner, other: "stranger-" + ids.owner,
		site: "v1:platform:site:" + ids.site, digest: siteAccessDigest(0x51), tokenDigest: siteAccessDigest(0x52), label: "Acme reviewers",
	}
	seedPrincipal(t, eng, f.owner, auth.RoleWriter)
	seedPrincipal(t, eng, f.operator, auth.RoleOwner)
	seedPrincipal(t, eng, f.member, auth.RoleWriter)
	seedPrincipal(t, eng, f.other, auth.RoleWriter)
	require.NoError(t, organizationInsert(t, eng, groupSeedCtx(), conceptAccountsAccount, ids.account,
		map[string]any{"name": ids.account, "status": "active", "domainStatus": "unverified"}))
	seedGroup(t, eng, "g-"+ids.account, "Acme", "account", ids.account)
	seedMembership(t, eng, "g-"+ids.account, f.member, "active")
	require.NoError(t, organizationInsert(t, eng, groupSeedCtx(), conceptPlatformSite, f.site, map[string]any{
		"hostname": ids.site + "." + siteHostnamePolicyDomain(), "kind": "spa", "status": "draft",
		"bundleRef": "blob://site-access/v1", "ownerUserId": "v1:identity:user:" + f.owner, "accountId": ids.account,
	}))

	issued := time.Now().UTC().Truncate(time.Second)
	internal := auth.ContextWithInternalOrigin(partGateActor(f.owner, auth.RoleWriter))
	res, err := eng.Execute(internal, siteAccessCall(t, "mutation", "createSiteAccessCode", map[string]any{
		"codeId": ids.code, "siteId": f.site, "accountId": ids.account, "label": f.label, "gate": "dashboard",
		"codeHash": f.digest, "codeHint": "FBZA",
		"issuedAt": issued.Format(time.RFC3339), "expiresAt": issued.Add(time.Hour).Format(time.RFC3339),
		"lastChangedBy": f.owner,
	}))
	require.NoError(t, err)
	f.codeStored = res.Bundle.Nodes[0].Id
	res, err = eng.Execute(internal, siteAccessCall(t, "mutation", "createSiteAccessSession", map[string]any{
		"sessionId": ids.session, "siteId": f.site, "codeId": ids.code, "generation": 1,
		"tokenHash": f.tokenDigest, "issuedAt": issued.Format(time.RFC3339),
	}))
	require.NoError(t, err)
	f.sessionStore = res.Bundle.Nodes[0].Id
	return f
}

// callers are the four people the review's matrix names. tierAdmit is what the
// concept's row tier says about them; it is what makes the refusal below the
// CLAUSE's doing, since the tier alone would let three of them read the row.
func (f accessReadFixture) callers() []struct {
	name      string
	userID    string
	role      auth.Role
	tierAdmit bool
} {
	return []struct {
		name      string
		userID    string
		role      auth.Role
		tierAdmit bool
	}{
		{"the site's owner", f.owner, auth.RoleWriter, true},
		{"a cluster owner", f.operator, auth.RoleOwner, true},
		{"a member of the code's organization", f.member, auth.RoleWriter, true},
		{"a stranger", f.other, auth.RoleWriter, false},
	}
}

func TestNoClientReadsAnAccessCodeOrSessionByAnyPath(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	f := newAccessReadFixture(t, eng, "rawread")
	now := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)

	// Every shape of read the review named, and the neighbours that reach the
	// same rows by a different door. `leaks` are the values that appear in a reply
	// only if a row of one of the two concepts came back. `only` marks a form whose
	// reply is made of nothing but such rows, so that ANY row is a leak.
	//
	// The graph traversals that matter are parentOf (a session's parent is its code)
	// and the raw forms. childOf the site and childOf the code return nothing at all
	// under internal origin -- neither concept declares the child edge -- and
	// references follows a row's own pointers outward, so neither reaches these rows.
	leaks := []string{f.ids.code, f.ids.session, f.digest, f.tokenDigest, f.label}
	forms := []struct {
		name, query string
		only        bool
	}{
		{"a raw read by site", fmt.Sprintf(`concept==v1:platform:siteAccessCode && siteId==%q`, f.site), true},
		{"a raw read by id", fmt.Sprintf(`concept==v1:platform:siteAccessCode && id==%q`, f.codeStored), true},
		{"a raw read by digest", fmt.Sprintf(`concept==v1:platform:siteAccessCode && codeHash==%q`, f.digest), true},
		{"a raw read of the whole concept", `concept==v1:platform:siteAccessCode`, true},
		{"sort(paginate(...))", `sort(paginate(concept==v1:platform:siteAccessCode, 10), "createdAt", "desc")`, true},
		{"asOf", fmt.Sprintf(`asOf(concept==v1:platform:siteAccessCode && siteId==%q, %q)`, f.site, now), true},
		{"a raw session read by site", fmt.Sprintf(`concept==v1:platform:siteAccessSession && siteId==%q`, f.site), true},
		{"a raw session read by token digest", fmt.Sprintf(`concept==v1:platform:siteAccessSession && tokenHash==%q`, f.tokenDigest), true},
		{"a raw read of the whole session concept", `concept==v1:platform:siteAccessSession`, true},
		{"parentOf the session", fmt.Sprintf(`parentOf(concept==v1:platform:siteAccessSession && id==%q)`, f.sessionStore), true},
	}

	reply := func(t *testing.T, ctx context.Context, query string) (string, int, error) {
		t.Helper()
		res, err := eng.Execute(ctx, query)
		if err != nil {
			return "", 0, err
		}
		return resultBlob(t, res), len(MaterializeRows(res)), nil
	}

	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			// THE CONTROL: with internal origin, which is what every server reader
			// stamps, the owner reads the rows through this very query. A form that
			// returned nothing here would pass every refusal below for nothing.
			internal := auth.ContextWithInternalOrigin(rankActorCtx(f.owner, auth.RoleWriter))
			blob, rows, err := reply(t, internal, form.query)
			require.NoError(t, err, "the control read failed, so the form below proves nothing")
			require.NotZero(t, rows, "the control read returned no rows, so the form below proves nothing: %s", blob)
			found := false
			for _, leak := range leaks {
				found = found || strings.Contains(blob, leak)
			}
			require.True(t, found, "the control read returned rows that are not an access code or session: %s", blob)

			for _, caller := range f.callers() {
				client := rankActorCtx(caller.userID, caller.role)
				blob, rows, err := reply(t, client, form.query)
				if err != nil {
					continue // a refusal is a refusal
				}
				if form.only {
					require.Zero(t, rows, "%s read %d row(s) through %s: %s", caller.name, rows, form.name, blob)
				}
				for _, leak := range leaks {
					require.NotContains(t, blob, leak, "%s was handed %q through %s", caller.name, leak, form.name)
				}
			}
		})
	}

	t.Run("the tier alone would have admitted three of the four", func(t *testing.T) {
		// What the clause changes, stated as a control on the tier: with internal
		// origin the row tier decides, and it admits the owner, a cluster owner and
		// a member of the code's organization and refuses the stranger. So the
		// refusals above are the clause's, and the stranger's is not a coincidence.
		form := fmt.Sprintf(`concept==v1:platform:siteAccessCode && siteId==%q`, f.site)
		for _, caller := range f.callers() {
			internal := auth.ContextWithInternalOrigin(rankActorCtx(caller.userID, caller.role))
			blob, _, err := reply(t, internal, form)
			require.NoError(t, err)
			if caller.tierAdmit {
				require.Contains(t, blob, f.ids.code, "%s is not admitted by the tier either, so the matrix above does not measure the clause", caller.name)
			} else {
				require.NotContains(t, blob, f.ids.code, "%s is admitted by the tier", caller.name)
			}
		}
	})
}

// A SUBSCRIPTION IS A READ. Graph events are fanned out through the same per-row
// admission (AdmitSubscriptionRow), so the clause covers a live feed of access
// codes the way it covers a query; the component/grpc test proves the fan-out
// actually calls it. Control: a row of an owner-tier concept with the same
// owner is delivered to its owner, so the harness can deliver.
func TestNoSubscriptionDeliversAnAccessCodeOrSessionRow(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	f := newAccessReadFixture(t, eng, "subscription")
	owner := "v1:identity:user:" + f.owner

	rows := []struct {
		concept string
		id      string
		payload map[string]any
	}{
		{siteAccessCodeConcept, f.codeStored, map[string]any{"siteId": f.site, "ownerUserId": owner, "accountId": f.ids.account, "label": f.label}},
		{siteAccessSessionConcept, f.sessionStore, map[string]any{"siteId": f.site, "ownerUserId": owner, "codeId": f.ids.code}},
	}
	admit := func(access *auth.AccessContext, concept, id string, payload map[string]any) SubscriptionAdmission {
		t.Helper()
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		return AdmitSubscriptionRow(eng.SubscriptionRankContext(context.Background()), access, concept, id, raw)
	}

	t.Run("control: an owner-tier row reaches its owner", func(t *testing.T) {
		got := admit(&auth.AccessContext{UserId: f.owner, Role: auth.RoleWriter}, "v1:platform:sitePreviewGrant",
			"v1:platform:sitePreviewGrant:g1", map[string]any{"siteId": f.site, "ownerUserId": owner})
		require.Equal(t, SubscriptionAdmit, got, "the control row was not delivered to its owner, so no denial below is informative")
	})

	for _, row := range rows {
		for _, caller := range f.callers() {
			t.Run(fmt.Sprintf("%s: %s gets nothing", BareShortId(row.concept), caller.name), func(t *testing.T) {
				got := admit(&auth.AccessContext{UserId: caller.userID, Role: caller.role}, row.concept, row.id, row.payload)
				require.Equal(t, SubscriptionDeny, got, "%s would be sent a live %s event", caller.name, row.concept)
			})
		}
		t.Run(BareShortId(row.concept)+": a stream with no identity gets nothing", func(t *testing.T) {
			require.Equal(t, SubscriptionDeny, admit(nil, row.concept, row.id, row.payload))
		})
	}
}

// THE SERVER STILL READS. The clause has two exits, internal origin and the
// cluster acting on its own, and each is the way a real server reader gets in:
// every handler under the builtins and the bff's store stamp internal origin on
// their reads, and the retention sweep runs as the maintenance actor, which is
// synthetic. A clause that refused either would leave the feature inert -- no
// list, no unlock, and a session table that never empties -- with nothing in a
// log to say why.
func TestTheServerStillReadsAccessCodesAndSessions(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	f := newAccessReadFixture(t, eng, "serverreads")

	t.Run("the named reads under internal origin, as the handlers and the bff run them", func(t *testing.T) {
		for _, caller := range f.callers() {
			if !caller.tierAdmit {
				continue
			}
			internal := auth.ContextWithInternalOrigin(rankActorCtx(caller.userID, caller.role))
			for _, call := range []string{
				siteAccessCall(t, "query", "siteAccessCodeRowsForSite", map[string]any{"siteId": f.site}),
				siteAccessCall(t, "query", "siteAccessCodeById", map[string]any{"codeId": f.ids.code}),
				siteAccessCall(t, "query", "siteAccessCodeByHash", map[string]any{"siteId": f.site, "codeHash": f.digest}),
			} {
				res, err := eng.Execute(internal, call)
				require.NoError(t, err)
				require.Contains(t, resultBlob(t, res), f.ids.code, "%s cannot read the code through %q with internal origin", caller.name, call)
			}
		}
		// The session lookup is the owner's: the bff reads it under the owner the
		// edge stamped, and the tier has no account arm to widen it.
		res, err := eng.Execute(auth.ContextWithInternalOrigin(rankActorCtx(f.owner, auth.RoleWriter)),
			siteAccessCall(t, "query", "siteAccessSessionByToken", map[string]any{"tokenHash": f.tokenDigest}))
		require.NoError(t, err)
		require.Contains(t, resultBlob(t, res), f.ids.session)
	})

	t.Run("the retention sweep's principal admits both rows, and the owner as a client does not", func(t *testing.T) {
		payloads := map[string]memorynodes.MemoryNode{}
		for concept, row := range map[string]struct {
			id      string
			payload map[string]any
		}{
			siteAccessCodeConcept:    {f.codeStored, map[string]any{"siteId": f.site, "ownerUserId": f.owner, "accountId": f.ids.account}},
			siteAccessSessionConcept: {f.sessionStore, map[string]any{"siteId": f.site, "ownerUserId": f.owner, "codeId": f.ids.code}},
		} {
			raw, err := json.Marshal(row.payload)
			require.NoError(t, err)
			payloads[concept] = memorynodes.MemoryNode{ID: row.id, Concept: concept, Payload: raw}
		}
		sweep := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("retireDeclaredConceptVersions"))
		for concept, node := range payloads {
			require.True(t, AdmitSourceRow(sweep, node), "the sweep's principal is refused a %s row", concept)
			require.False(t, AdmitSourceRow(rankActorCtx(f.owner, auth.RoleWriter), node),
				"the owner reading a %s row as a client is admitted by AdmitSourceRow", concept)
		}
	})
}

// BORROWED AUTHORITY IS NOT AN EXIT (design D14, narrowed by the ruling on the review's
// follow-up). auth.ContextWithUserActor lets server code act for a named person, and it is
// also exactly what a visitor's gated call runs under: the site owner's authority, lent to a
// request a stranger made, at CLIENT origin. It is unranked, so a clause that let every
// unranked actor through would admit it and leave the row tier to decide -- and the tier's
// owner arm admits the owner, so a gated construct that read these concepts would hand a
// visitor the labels and digests of the site's codes and the digests of live cookies.
//
// So the exit is the cluster acting (a synthetic actor), and a read under borrowed authority
// is a client read until internal origin is stamped on it, which is what the store behind the
// handlers and the bff does. Asserted three ways: the borrowed owner is refused every read
// shape at client origin; the same actor with internal origin reads (so the refusal is the
// origin's absence and not a broken fixture); and the cluster acting -- the retention sweep's
// maintenance actor and an automation's system actor -- still reads with no origin at all.
func TestBorrowedAuthorityReadsAccessCodesAndSessionsOnlyWithInternalOrigin(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	f := newAccessReadFixture(t, eng, "borrowed")
	borrowed := auth.ContextWithUserActor(context.Background(), f.owner)
	internalBorrowed := auth.ContextWithInternalOrigin(borrowed)

	// THE PREMISE: the actor is the one the ruling is about -- unranked, and not the cluster
	// -- and it IS the site's owner, so the row tier alone would admit it.
	access, ok := auth.AccessFromContext(borrowed)
	require.True(t, ok)
	require.True(t, access.Unranked && !access.Synthetic,
		"ContextWithUserActor is no longer unranked-and-not-synthetic, so this test no longer measures borrowed authority")
	require.Equal(t, f.owner, BareShortId(access.UserId))

	leaks := []string{f.ids.code, f.ids.session, f.digest, f.tokenDigest, f.label}
	forms := []struct{ name, query string }{
		{"a raw code read by site", fmt.Sprintf(`concept==v1:platform:siteAccessCode && siteId==%q`, f.site)},
		{"a raw session read by site", fmt.Sprintf(`concept==v1:platform:siteAccessSession && siteId==%q`, f.site)},
		{"a raw session read by token digest", fmt.Sprintf(`concept==v1:platform:siteAccessSession && tokenHash==%q`, f.tokenDigest)},
		{"parentOf the session", fmt.Sprintf(`parentOf(concept==v1:platform:siteAccessSession && id==%q)`, f.sessionStore)},
	}
	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			// The control: with internal origin the same actor reads the row.
			res, err := eng.Execute(internalBorrowed, form.query)
			require.NoError(t, err, "the control read failed, so the refusal below proves nothing")
			require.NotZero(t, len(MaterializeRows(res)), "the control read returned no rows: %s", resultBlob(t, res))

			// Without it, the borrowed owner is a client.
			res, err = eng.Execute(borrowed, form.query)
			if err != nil {
				return // a refusal is a refusal
			}
			blob := resultBlob(t, res)
			require.Zero(t, len(MaterializeRows(res)), "borrowed authority at client origin read rows through %s: %s", form.name, blob)
			for _, leak := range leaks {
				require.NotContains(t, blob, leak, "borrowed authority at client origin was handed %q through %s", leak, form.name)
			}
		})
	}

	nodes := map[string]memorynodes.MemoryNode{}
	for concept, row := range map[string]struct {
		id      string
		payload map[string]any
	}{
		siteAccessCodeConcept:    {f.codeStored, map[string]any{"siteId": f.site, "ownerUserId": f.owner, "accountId": f.ids.account}},
		siteAccessSessionConcept: {f.sessionStore, map[string]any{"siteId": f.site, "ownerUserId": f.owner, "codeId": f.ids.code}},
	} {
		raw, err := json.Marshal(row.payload)
		require.NoError(t, err)
		nodes[concept] = memorynodes.MemoryNode{ID: row.id, Concept: concept, Payload: raw}
	}

	t.Run("the per-row admission agrees, for a read and for a subscription", func(t *testing.T) {
		for concept, node := range nodes {
			require.False(t, AdmitSourceRow(borrowed, node), "borrowed authority at client origin is admitted a %s row", concept)
			require.True(t, AdmitSourceRow(internalBorrowed, node), "borrowed authority with internal origin is refused a %s row, so the store behind the handlers could not read it", concept)
			require.Equal(t, SubscriptionDeny,
				AdmitSubscriptionRow(eng.SubscriptionRankContext(context.Background()), access, concept, node.ID, node.Payload),
				"a stream running under borrowed authority is delivered a %s event", concept)
		}
	})

	t.Run("the cluster acting still reads, with no origin at all", func(t *testing.T) {
		// The retention sweep's principal.
		sweep := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("retireDeclaredConceptVersions"))
		for concept, node := range nodes {
			require.True(t, AdmitSourceRow(sweep, node), "the sweep's principal is refused a %s row", concept)
		}
		// An automation's system actor, through a real read.
		system := auth.ContextWithSystemActor(context.Background(), "an-automation")
		for _, query := range []string{
			fmt.Sprintf(`concept==v1:platform:siteAccessCode && siteId==%q`, f.site),
			fmt.Sprintf(`concept==v1:platform:siteAccessSession && siteId==%q`, f.site),
		} {
			res, err := eng.Execute(system, query)
			require.NoError(t, err)
			require.NotZero(t, len(MaterializeRows(res)), "a system actor read nothing through %q: %s", query, resultBlob(t, res))
		}
	})
}
