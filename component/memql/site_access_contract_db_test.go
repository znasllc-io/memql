package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// site_access_contract_db_test.go -- deployable access codes (design record
// 2026-10-29), the platform DSL contract.
//
// Proves the contract WORKS, against a real engine and a real database, rather
// than proving that the declarations parse.
//
// ===========================================================================
// WHY THE CLIENT-ORIGIN REFUSAL IS THE SECURITY PROPERTY
// ===========================================================================
// A code is a bearer credential for part of somebody's site, and the three
// facts that make it one are all things a caller must not be able to choose:
// the DIGEST (a caller who picks it picks the code), the EXPIRY (the only bound
// a bearer credential has) and the GENERATION a session is held to (a caller
// who writes it keeps a session alive across a regeneration, which is the one
// act that is supposed to end it). actor.userId scoping reaches none of them:
// every write below is stamped or scoped to the site's owner and is still
// wrong to leave on the wire. So every write is @serverOnly, and the reachable
// surface is the six builtins (siteAccess*), which mint, check, judge the
// organization and audit before they write.
//
// The reads are @serverOnly for two reasons, and the test names both. The
// by-hash and by-token lookups are the visitor's credential check, and a
// client-callable one is an oracle for whether a guessed digest is a live code.
// The by-id and the per-site reads are the operator handlers' rows, and they are
// server-only because the shared organization gate judges builtins and mutations
// and NEVER a query: a client-callable one would be a read that skips the check
// (TestSiteAccessListIsJudgedByTheOrganizationGate is the proof of the gate).
//
// ===========================================================================
// THE REFUSAL AND THE PASS ARE TESTED TOGETHER
// ===========================================================================
// auth.CallOrigin's zero value is OriginClient, so a missing stamp refuses
// every write with ONE WARN and an empty table behind it -- the feature is
// inert, not degraded, and no other test in the epic would notice (the
// recovery-key incident test/dslconformance/server_only_callers_stamp_test.go
// records). Hence both directions, in one place, over the same rows.

// siteAccessIDs is one test's worth of ids, unique per run so a rerun against
// the same database cannot collide with the rows a previous one left.
type siteAccessIDs struct {
	owner   string
	site    string
	account string
	code    string
	session string
}

func newSiteAccessIDs(name string) siteAccessIDs {
	suffix := fmt.Sprintf("%s-%d-%d", name, os.Getpid(), time.Now().UnixNano())
	return siteAccessIDs{
		owner:   "site-access-owner-" + suffix,
		site:    "site-access-site-" + suffix,
		account: "site-access-acct-" + suffix,
		code:    "site-access-code-" + suffix,
		session: "site-access-session-" + suffix,
	}
}

// siteAccessDigest is a well-formed lowercase hex SHA-256 the contract accepts.
// The DSL only checks the shape; whose digest it is is the integration's.
func siteAccessDigest(seed byte) string {
	return strings.Repeat(fmt.Sprintf("%02x", seed), 32)
}

// siteAccessCall renders a named call with JSON-encoded values, the form
// runMutation uses: valid MemQL for every literal this contract takes.
func siteAccessCall(t *testing.T, kind, name string, args map[string]any) string {
	t.Helper()
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		raw, err := json.Marshal(args[k])
		require.NoError(t, err)
		parts = append(parts, k+": "+string(raw))
	}
	return kind + " " + name + "(" + strings.Join(parts, ", ") + ")"
}

func siteAccessAs(userID string, role auth.Role) *auth.AccessContext {
	return &auth.AccessContext{UserId: userID, Role: role}
}

func TestSiteAccessConstructsAreServerOnlyAndInternalOriginPasses(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	ids := newSiteAccessIDs("serveronly")
	issued := time.Now().UTC().Truncate(time.Second)
	expires := issued.Add(time.Hour).Format(time.RFC3339)
	issuedAt := issued.Format(time.RFC3339)

	// A CLUSTER OWNER is the strongest client there is, and it is the one the
	// refusal is asserted for: the property is the CHANNEL, not the rank.
	client := auth.ContextWithClientOrigin(auth.ContextWithAccess(ctx, siteAccessAs("operator-"+ids.owner, auth.RoleOwner)))
	// The write is made as the SITE'S OWNER with internal origin stamped, which
	// is what integrations/siteaccess does: the row's owner is then the site's,
	// whoever the operator was.
	internal := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, siteAccessAs(ids.owner, auth.RoleWriter)))

	createArgs := map[string]any{
		"codeId": ids.code, "siteId": ids.site, "accountId": ids.account,
		"label": "Client reviewers", "gate": "dashboard", "subject": "acme",
		"codeHash": siteAccessDigest(0xa1), "codeHint": "FBZA",
		"issuedAt": issuedAt, "expiresAt": expires, "lastChangedBy": "operator-" + ids.owner,
	}
	sessionArgs := map[string]any{
		"sessionId": ids.session, "siteId": ids.site, "codeId": ids.code, "generation": 1,
		"tokenHash": siteAccessDigest(0xb2), "issuedAt": issuedAt,
	}

	// The constructs in the order a code lives: minted, rotated, re-timed,
	// removed; and a session opened and ended. Each is called with VALID
	// arguments, so a refusal can only be the gate, never the arguments.
	calls := []struct {
		construct string
		call      string
		// exposure says what a wire-reachable version would hand a caller.
		exposure string
	}{
		{"createSiteAccessCode", siteAccessCall(t, "mutation", "createSiteAccessCode", createArgs),
			"minting a code whose digest, expiry and owner the caller chose"},
		{"rotateSiteAccessCode", siteAccessCall(t, "mutation", "rotateSiteAccessCode", map[string]any{
			"codeId": ids.code, "codeHash": siteAccessDigest(0xa2), "codeHint": "ABCD", "generation": 2,
			"issuedAt": issuedAt, "expiresAt": expires, "lastChangedBy": "operator-" + ids.owner,
		}), "swapping in a digest the caller knows the code for"},
		{"setSiteAccessCodeExpiry", siteAccessCall(t, "mutation", "setSiteAccessCodeExpiry", map[string]any{
			"codeId": ids.code, "expiresAt": expires, "lastChangedBy": "operator-" + ids.owner,
		}), "choosing a code's lifetime, the only bound a bearer credential has"},
		{"removeSiteAccessCode", siteAccessCall(t, "mutation", "removeSiteAccessCode", map[string]any{
			"codeId": ids.code, "revokedAt": issuedAt, "lastChangedBy": "operator-" + ids.owner,
		}), "ending a code without the audit event the builtin writes beside it"},
		{"createSiteAccessSession", siteAccessCall(t, "mutation", "createSiteAccessSession", sessionArgs),
			"opening a session without a code, which is the whole of what a code is for"},
		{"endSiteAccessSession", siteAccessCall(t, "mutation", "endSiteAccessSession", map[string]any{
			"sessionId": ids.session, "endedAt": issuedAt,
		}), "ending somebody else's session"},
		{"siteAccessCodeById", siteAccessCall(t, "query", "siteAccessCodeById", map[string]any{"codeId": ids.code}),
			"reading one code's metadata past the gate that judges the builtins"},
		{"siteAccessCodeRowsForSite", siteAccessCall(t, "query", "siteAccessCodeRowsForSite", map[string]any{"siteId": ids.site}),
			"listing a site's codes without the organization check -- the finding that made the list a builtin"},
		{"siteAccessCodeByHash", siteAccessCall(t, "query", "siteAccessCodeByHash", map[string]any{
			"siteId": ids.site, "codeHash": siteAccessDigest(0xa1),
		}), "an oracle for whether a guessed digest is a live code"},
		{"siteAccessSessionByToken", siteAccessCall(t, "query", "siteAccessSessionByToken", map[string]any{
			"tokenHash": siteAccessDigest(0xb2),
		}), "an oracle for whether a guessed digest is a live session"},
		{"siteAccessSessionLatestForCode", siteAccessCall(t, "query", "siteAccessSessionLatestForCode", map[string]any{
			"codeId": ids.code, "generation": 1,
		}), "when a code was last opened, past the gate that judges the list it is shown in"},
	}

	// Every client-origin call first, before anything exists: the refusal must
	// not depend on there being a row to protect.
	for _, tc := range calls {
		t.Run("a client-origin "+tc.construct+" is refused", func(t *testing.T) {
			_, err := eng.Execute(client, tc.call)
			require.Error(t, err, "%s answered a client-origin call, which puts %s on the wire. "+
				"Either @serverOnly was dropped or the gate stopped being enforced.", tc.construct, tc.exposure)
			require.Contains(t, err.Error(), "server-only",
				"%s was refused, but NOT by the server-only gate -- so this case would keep passing "+
					"if the gate were removed and something else happened to fail", tc.construct)
		})
	}

	// The same calls pass the gate under internal origin, in lifecycle order.
	// The first one has to create the row the rest act on.
	for _, tc := range calls {
		if strings.HasPrefix(tc.call, "query ") {
			continue
		}
		t.Run("an internal-origin "+tc.construct+" passes the gate", func(t *testing.T) {
			_, err := eng.Execute(internal, tc.call)
			if err != nil {
				require.NotContains(t, err.Error(), "server-only",
					"%s refused an INTERNAL-origin call. integrations/siteaccess stamps exactly this, "+
						"so nothing can reach the construct and the feature is inert with one WARN per act", tc.construct)
				t.Fatalf("%s failed for a non-gate reason -- fix the fixture, the gate is fine: %v", tc.construct, err)
			}
		})
	}
}

// The row's owner is the SITE'S, stamped from the actor the write runs as, and
// never an argument. This is TestDeclaredOwnerFieldsAreServerStamped's rule met
// with a real write: a tier declared over a field a caller supplies records a
// guarantee nothing provides.
func TestSiteAccessCodeIsStampedWithTheOwnerAndTheFirstGeneration(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	ids := newSiteAccessIDs("stamp")
	issued := time.Now().UTC().Truncate(time.Second)
	internal := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, siteAccessAs(ids.owner, auth.RoleWriter)))

	// A DIFFERENT PERSON is named as the author of the change, which is the
	// operator who asked; the row's owner must still be the actor.
	const operator = "an-operator-who-is-not-the-site-owner"
	res, err := eng.Execute(internal, siteAccessCall(t, "mutation", "createSiteAccessCode", map[string]any{
		"codeId": ids.code, "siteId": ids.site, "accountId": ids.account,
		"label": "Client reviewers", "gate": "dashboard",
		"codeHash": siteAccessDigest(0xc1), "codeHint": "FBZA",
		"issuedAt": issued.Format(time.RFC3339), "expiresAt": issued.Add(time.Hour).Format(time.RFC3339),
		"lastChangedBy": operator,
	}))
	require.NoError(t, err)
	require.NotEmpty(t, res.Bundle.Nodes)

	row := latestPayload(t, ctx, db, "v1:platform:siteAccessCode", res.Bundle.Nodes[0].Id)
	require.Equal(t, ids.owner, BareShortId(fmt.Sprint(row["ownerUserId"])),
		"the code is owned by the site's owner, because the write ran as them")
	require.EqualValues(t, 1, row["generation"], "a minted code starts at generation 1, and the integration cannot choose otherwise")
	require.Equal(t, siteAccessDigest(0xc1), row["codeHash"], "the digest is what the row stores")
	require.Equal(t, ids.account, BareShortId(fmt.Sprint(row["accountId"])), "the account is copied from the site at mint time")
	require.Equal(t, operator, BareShortId(fmt.Sprint(row["lastChangedBy"])), "who made the change is the caller's, not the owner's")
	_, hasSubject := row["subject"]
	require.False(t, hasSubject, "a code minted without a subject carries none, so the stamp is absent rather than empty")
}

// The OS-facing shape never carries the digest, and the queries behind it
// answer for the person the tier admits and nobody else.
func TestSiteAccessCodeViewNeverCarriesTheDigest(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	ids := newSiteAccessIDs("view")
	issued := time.Now().UTC().Truncate(time.Second)
	owner := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, siteAccessAs(ids.owner, auth.RoleWriter)))
	stranger := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, siteAccessAs("stranger-"+ids.owner, auth.RoleWriter)))

	digest := siteAccessDigest(0xd1)
	_, err := eng.Execute(owner, siteAccessCall(t, "mutation", "createSiteAccessCode", map[string]any{
		"codeId": ids.code, "siteId": ids.site, "accountId": ids.account,
		"label": "Client reviewers", "gate": "dashboard", "subject": "acme",
		"codeHash": digest, "codeHint": "FBZA",
		"issuedAt": issued.Format(time.RFC3339), "expiresAt": issued.Add(time.Hour).Format(time.RFC3339),
		"lastChangedBy": ids.owner,
	}))
	require.NoError(t, err)

	byID := siteAccessCall(t, "query", "siteAccessCodeById", map[string]any{"codeId": ids.code})
	byHash := siteAccessCall(t, "query", "siteAccessCodeByHash", map[string]any{"siteId": ids.site, "codeHash": digest})
	forSite := siteAccessCall(t, "query", "siteAccessCodeRowsForSite", map[string]any{"siteId": ids.site})

	for name, query := range map[string]string{"by id": byID, "by hash": byHash, "for site": forSite} {
		t.Run(name, func(t *testing.T) {
			res, err := eng.Execute(owner, query)
			require.NoError(t, err)
			blob := resultBlob(t, res)
			require.Contains(t, blob, ids.code, "the owner cannot read their own code %s: %s", name, blob)
			require.NotContains(t, blob, "codeHash", "the view carries the digest %s: %s", name, blob)
			require.NotContains(t, blob, digest, "the digest itself is in the reply %s: %s", name, blob)
			for _, field := range []string{"label", "gate", "subject", "codeHint", "generation", "issuedAt", "expiresAt"} {
				require.Contains(t, blob, field, "the view lacks %q %s: %s", field, name, blob)
			}

			// THE TIER DECIDES THE ROW. Another person asking for the same code
			// resolves zero rows. (A forged owner stamp is a different case: the
			// account arm admits a member, so the bff compares the stamp with the
			// site's own owner before it relies on the tier.)
			res, err = eng.Execute(stranger, query)
			require.NoError(t, err)
			require.NotContains(t, resultBlob(t, res), ids.code, "a stranger read the owner's code %s", name)
		})
	}

	t.Run("a digest that is not the code's resolves nothing", func(t *testing.T) {
		res, err := eng.Execute(owner, siteAccessCall(t, "query", "siteAccessCodeByHash",
			map[string]any{"siteId": ids.site, "codeHash": siteAccessDigest(0xd2)}))
		require.NoError(t, err)
		require.Empty(t, MaterializeRows(res))
	})
	t.Run("another site's digest lookup resolves nothing", func(t *testing.T) {
		res, err := eng.Execute(owner, siteAccessCall(t, "query", "siteAccessCodeByHash",
			map[string]any{"siteId": ids.site + "-other", "codeHash": digest}))
		require.NoError(t, err)
		require.Empty(t, MaterializeRows(res), "a code opened a site it was not minted for")
	})
}

// A PAGE CAP WOULD HIDE A LIVE CODE FROM THE PERSON WHO HAS TO REMOVE IT. The rows under
// the list are @unbounded for that reason, and the reason has a number in it: an unmarked
// list read is capped at 50 rows (MEMQL_MEMORY_ENGINE_DEFAULT_LIST_CAP), so a deployable
// with sixty codes would list fifty and say nothing. Sixty is asserted, not assumed.
func TestSiteAccessCodeRowsForSiteIsNotPaged(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	ids := newSiteAccessIDs("unbounded")
	issued := time.Now().UTC().Truncate(time.Second)
	owner := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, siteAccessAs(ids.owner, auth.RoleWriter)))

	const codes = 60
	for i := 0; i < codes; i++ {
		_, err := eng.Execute(owner, siteAccessCall(t, "mutation", "createSiteAccessCode", map[string]any{
			"codeId": fmt.Sprintf("%s-%02d", ids.code, i), "siteId": ids.site, "label": fmt.Sprintf("Code %02d", i), "gate": "dashboard",
			"codeHash": siteAccessDigest(byte(i + 1)), "codeHint": "FBZA",
			"issuedAt": issued.Format(time.RFC3339), "expiresAt": issued.Add(time.Hour).Format(time.RFC3339),
			"lastChangedBy": ids.owner,
		}))
		require.NoError(t, err)
	}
	res, err := eng.Execute(owner, siteAccessCall(t, "query", "siteAccessCodeRowsForSite", map[string]any{"siteId": ids.site}))
	require.NoError(t, err)
	require.Len(t, MaterializeRows(res), codes, "the list stopped at its first page: a live code past it could not be removed")
}

// A code's life is a chain of small updates to one logical row. Each must
// change only what it names, and a rotation must make the previous digest stop
// resolving -- reads see the newest version per id, which is the property the
// whole of "regenerate gets rid of the previous one" rests on.
func TestSiteAccessCodeLifecycleChangesOnlyWhatItNames(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	ids := newSiteAccessIDs("life")
	issued := time.Now().UTC().Truncate(time.Second)
	owner := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, siteAccessAs(ids.owner, auth.RoleWriter)))
	stamp := func(d time.Duration) string { return issued.Add(d).Format(time.RFC3339) }

	oldDigest, newDigest := siteAccessDigest(0xe1), siteAccessDigest(0xe2)
	res, err := eng.Execute(owner, siteAccessCall(t, "mutation", "createSiteAccessCode", map[string]any{
		"codeId": ids.code, "siteId": ids.site, "accountId": ids.account,
		"label": "Client reviewers", "gate": "dashboard", "subject": "acme",
		"codeHash": oldDigest, "codeHint": "FBZA",
		"issuedAt": stamp(0), "expiresAt": stamp(time.Hour), "lastChangedBy": "operator-a",
	}))
	require.NoError(t, err)
	stored := res.Bundle.Nodes[0].Id
	unchanged := func(row map[string]any) {
		t.Helper()
		require.Equal(t, "Client reviewers", row["label"])
		require.Equal(t, "dashboard", row["gate"])
		require.Equal(t, "acme", row["subject"])
		require.Equal(t, ids.owner, BareShortId(fmt.Sprint(row["ownerUserId"])))
		require.Equal(t, ids.account, BareShortId(fmt.Sprint(row["accountId"])))
		require.Equal(t, ids.site, BareShortId(fmt.Sprint(row["siteId"])))
	}
	run := func(name string, args map[string]any) map[string]any {
		t.Helper()
		_, err := eng.Execute(owner, siteAccessCall(t, "mutation", name, args))
		require.NoError(t, err, name)
		row := latestPayload(t, ctx, db, "v1:platform:siteAccessCode", stored)
		unchanged(row)
		return row
	}
	resolves := func(digest string) bool {
		t.Helper()
		got, err := eng.Execute(owner, siteAccessCall(t, "query", "siteAccessCodeByHash",
			map[string]any{"siteId": ids.site, "codeHash": digest}))
		require.NoError(t, err)
		return len(MaterializeRows(got)) == 1
	}
	require.True(t, resolves(oldDigest))

	// Expiry moves; the code and its generation do not.
	row := run("setSiteAccessCodeExpiry", map[string]any{"codeId": ids.code, "expiresAt": stamp(48 * time.Hour), "lastChangedBy": "operator-b"})
	require.Equal(t, siteAccessDigest(0xe1), row["codeHash"], "a change of expiry replaced the secret")
	require.EqualValues(t, 1, row["generation"], "a change of expiry advanced the generation, which would end every open session")
	require.Equal(t, "operator-b", BareShortId(fmt.Sprint(row["lastChangedBy"])))
	require.Equal(t, stamp(48*time.Hour), fmt.Sprint(row["expiresAt"]))

	// A visitor's use is NOT recorded on the code (D13): nothing a visitor does
	// writes this row, so the only versions it has are operators' acts. "Last
	// opened" is read from the code's sessions (siteAccessSessionLatestForCode).
	for _, heartbeat := range []string{"lastUsedAt", "lastSeenAt", "lastOpenedAt"} {
		_, present := row[heartbeat]
		require.False(t, present, "the code carries %q, a field only a visitor's use could write", heartbeat)
	}

	// Regenerate: new digest, new hint, next generation, new issue time. The
	// previous digest resolves nothing from that instant.
	row = run("rotateSiteAccessCode", map[string]any{
		"codeId": ids.code, "codeHash": newDigest, "codeHint": "ZZZZ", "generation": 2,
		"issuedAt": stamp(2 * time.Minute), "expiresAt": stamp(72 * time.Hour), "lastChangedBy": "operator-c",
	})
	require.Equal(t, newDigest, row["codeHash"])
	require.Equal(t, "ZZZZ", row["codeHint"])
	require.EqualValues(t, 2, row["generation"])
	require.Equal(t, stamp(2*time.Minute), fmt.Sprint(row["issuedAt"]))
	require.True(t, resolves(newDigest), "the regenerated code does not open the site")
	require.False(t, resolves(oldDigest), "the code that was regenerated away still opens the site")

	// Remove is soft: the row stays, marked, and the digest still resolves so
	// the bff can say the same 'invalid' for a revoked code as for an unknown
	// one (the handler decides; the read does not hide the row).
	row = run("removeSiteAccessCode", map[string]any{"codeId": ids.code, "revokedAt": stamp(3 * time.Minute), "lastChangedBy": "operator-d"})
	require.Equal(t, stamp(3*time.Minute), fmt.Sprint(row["revokedAt"]))
	require.Equal(t, newDigest, row["codeHash"], "removing a code changed its secret")
	require.True(t, resolves(newDigest), "a removed code vanished from the read, so the record of who could get in is gone")

	// A second remove is harmless: the same instant is written again.
	row = run("removeSiteAccessCode", map[string]any{"codeId": ids.code, "revokedAt": stamp(3 * time.Minute), "lastChangedBy": "operator-d"})
	require.Equal(t, stamp(3*time.Minute), fmt.Sprint(row["revokedAt"]))
}

// A stored digest is ALWAYS a well-formed SHA-256, because the DSL says so
// (sitePreviewGrant's @maxLength and @pattern, kept). The integration computes
// every digest, so this is the second line rather than the first: a malformed
// value reaching a write is a bug, and a row holding one is a credential nothing
// can ever match -- an access code that looks minted and opens nothing, or a
// session cookie that can never be looked up.
func TestSiteAccessWritesRefuseAMalformedDigest(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	ids := newSiteAccessIDs("digest")
	issued := time.Now().UTC().Truncate(time.Second)
	internal := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, siteAccessAs(ids.owner, auth.RoleWriter)))

	for name, bad := range map[string]string{
		"empty":         "",
		"too short":     strings.Repeat("a", 63),
		"too long":      strings.Repeat("a", 65),
		"upper case":    strings.Repeat("A", 64),
		"not hex":       strings.Repeat("z", 64),
		"a plain code":  "K7Q4M-2XPDN-8RT3V-WJ6HC-FBZA",
		"a bare cookie": "mql_acs_" + strings.Repeat("a", 43),
	} {
		t.Run("a code with a digest that is "+name, func(t *testing.T) {
			_, err := eng.Execute(internal, siteAccessCall(t, "mutation", "createSiteAccessCode", map[string]any{
				"codeId": ids.code + name, "siteId": ids.site, "label": "x", "gate": "dashboard",
				"codeHash": bad, "codeHint": "FBZA",
				"issuedAt": issued.Format(time.RFC3339), "expiresAt": issued.Add(time.Hour).Format(time.RFC3339),
				"lastChangedBy": ids.owner,
			}))
			require.Error(t, err, "a code was stored with the digest %q", bad)
			// REFUSED FOR THE DIGEST, not for something else the fixture got wrong.
			require.Contains(t, err.Error(), `argument "codeHash"`, "refused, but not by the digest's own validator")
		})
		t.Run("a session with a token digest that is "+name, func(t *testing.T) {
			_, err := eng.Execute(internal, siteAccessCall(t, "mutation", "createSiteAccessSession", map[string]any{
				"sessionId": ids.session + name, "siteId": ids.site, "codeId": ids.code, "generation": 1,
				"tokenHash": bad, "issuedAt": issued.Format(time.RFC3339),
			}))
			require.Error(t, err, "a session was stored with the digest %q", bad)
			require.Contains(t, err.Error(), `argument "tokenHash"`, "refused, but not by the digest's own validator")
		})
	}
}

// A session is bound to ONE code generation and to the site's owner, is found
// only by its digest, and ends by a timestamp rather than by deletion.
func TestSiteAccessSessionIsBoundToItsOwnerAndGeneration(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	ids := newSiteAccessIDs("session")
	issued := time.Now().UTC().Truncate(time.Second)
	owner := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, siteAccessAs(ids.owner, auth.RoleWriter)))
	stranger := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, siteAccessAs("stranger-"+ids.owner, auth.RoleWriter)))

	token := siteAccessDigest(0xf1)
	res, err := eng.Execute(owner, siteAccessCall(t, "mutation", "createSiteAccessSession", map[string]any{
		"sessionId": ids.session, "siteId": ids.site, "codeId": ids.code, "generation": 3,
		"tokenHash": token, "issuedAt": issued.Format(time.RFC3339),
	}))
	require.NoError(t, err)
	stored := res.Bundle.Nodes[0].Id

	row := latestPayload(t, ctx, db, "v1:platform:siteAccessSession", stored)
	require.Equal(t, ids.owner, BareShortId(fmt.Sprint(row["ownerUserId"])), "a session is owned by the site's owner, stamped from the actor")
	require.EqualValues(t, 3, row["generation"], "the session records the generation it was opened against")
	_, ended := row["endedAt"]
	require.False(t, ended, "a session is open until something ends it")

	byToken := siteAccessCall(t, "query", "siteAccessSessionByToken", map[string]any{"tokenHash": token})
	got, err := eng.Execute(owner, byToken)
	require.NoError(t, err)
	blob := resultBlob(t, got)
	require.Contains(t, blob, ids.session)
	require.Contains(t, blob, ids.code, "the session's shape must name the code it was opened with")
	require.NotContains(t, blob, "tokenHash", "the session shape carries the digest of a live cookie")
	require.NotContains(t, blob, token)

	got, err = eng.Execute(stranger, byToken)
	require.NoError(t, err)
	require.Empty(t, MaterializeRows(got), "a stranger resolved the owner's session by digest")

	// Ending writes a timestamp and keeps the row: the history of who was in.
	_, err = eng.Execute(owner, siteAccessCall(t, "mutation", "endSiteAccessSession", map[string]any{
		"sessionId": ids.session, "endedAt": issued.Add(time.Minute).Format(time.RFC3339),
	}))
	require.NoError(t, err)
	row = latestPayload(t, ctx, db, "v1:platform:siteAccessSession", stored)
	require.Equal(t, issued.Add(time.Minute).Format(time.RFC3339), fmt.Sprint(row["endedAt"]))
	require.Equal(t, token, row["tokenHash"], "ending a session changed its digest")
	require.EqualValues(t, 3, row["generation"], "ending a session changed the generation it was bound to")
	got, err = eng.Execute(owner, byToken)
	require.NoError(t, err)
	require.Contains(t, resultBlob(t, got), "endedAt", "an ended session must still resolve, so the handler can say 'ended'")
}

// "LAST OPENED" IS THE NEWEST SESSION OF THE CODE'S CURRENT GENERATION (D13), and
// the code carries no heartbeat to say so. The list handler fills `lastOpenedAt`
// from this one read, so the properties below are the panel's:
//
//   - the NEWEST by issuedAt, whatever order the rows were written in;
//   - of ONE generation, so a regenerated code reads as not opened though older
//     sessions of the replaced secret exist -- the new secret has not been used;
//   - an ENDED session still counts, because it was opened;
//   - one row at most, and never the digest of a live cookie.
func TestSiteAccessSessionLatestForCodeIsTheNewestOfThatGeneration(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	ids := newSiteAccessIDs("latest")
	issued := time.Now().UTC().Truncate(time.Second)
	at := func(d time.Duration) string { return issued.Add(d).Format(time.RFC3339) }
	owner := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, siteAccessAs(ids.owner, auth.RoleWriter)))
	stranger := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, siteAccessAs("stranger-"+ids.owner, auth.RoleWriter)))

	open := func(label, code string, generation int, opened string, seed byte) string {
		t.Helper()
		id := ids.session + "-" + label
		_, err := eng.Execute(owner, siteAccessCall(t, "mutation", "createSiteAccessSession", map[string]any{
			"sessionId": id, "siteId": ids.site, "codeId": code, "generation": generation,
			"tokenHash": siteAccessDigest(seed), "issuedAt": opened,
		}))
		require.NoError(t, err)
		return id
	}
	// Generation 1 is written out of order: the newest is NOT the last row written.
	open("g1-early", ids.code, 1, at(time.Minute), 0x01)
	newest := open("g1-newest", ids.code, 1, at(5*time.Minute), 0x02)
	open("g1-middle", ids.code, 1, at(2*time.Minute), 0x03)
	// Generation 2 is older than generation 1's newest, and still the answer for 2.
	second := open("g2", ids.code, 2, at(3*time.Minute), 0x04)
	// Another code's session must never answer for this one.
	other := open("other", ids.code+"-other", 1, at(9*time.Minute), 0x05)

	latest := func(ctx context.Context, code string, generation int) (string, []map[string]any) {
		t.Helper()
		res, err := eng.Execute(ctx, siteAccessCall(t, "query", "siteAccessSessionLatestForCode", map[string]any{"codeId": code, "generation": generation}))
		require.NoError(t, err)
		return resultBlob(t, res), MaterializeRows(res)
	}

	blob, rows := latest(owner, ids.code, 1)
	require.Len(t, rows, 1, "the read must return one row: %s", blob)
	require.Contains(t, blob, newest, "the newest session of the generation is not the answer")
	require.Contains(t, blob, at(5*time.Minute), "the reply must carry issuedAt: it is what the panel shows as 'last opened'")
	require.NotContains(t, blob, "tokenHash", "the reply carries the digest of a live cookie")
	require.NotContains(t, blob, siteAccessDigest(0x02))

	blob, rows = latest(owner, ids.code, 2)
	require.Len(t, rows, 1)
	require.Contains(t, blob, second, "generation 2 was answered by a session of another generation")

	_, rows = latest(owner, ids.code, 3)
	require.Empty(t, rows, "a generation nobody opened reads as opened: a regenerated code would show the replaced secret's last use")

	blob, rows = latest(owner, ids.code+"-other", 1)
	require.Len(t, rows, 1)
	require.Contains(t, blob, other)

	_, rows = latest(owner, ids.code+"-nowhere", 1)
	require.Empty(t, rows, "a code nobody opened answered with a row")

	// An ended session was still opened.
	_, err := eng.Execute(owner, siteAccessCall(t, "mutation", "endSiteAccessSession", map[string]any{
		"sessionId": newest, "endedAt": at(6 * time.Minute),
	}))
	require.NoError(t, err)
	blob, rows = latest(owner, ids.code, 1)
	require.Len(t, rows, 1)
	require.Contains(t, blob, newest, "ending the newest session changed which one is the newest")
	require.Contains(t, blob, at(5*time.Minute), "the answer is when it was opened, not when it ended")

	// The tier decides the row: sessions are the site owner's, so a stranger
	// reads none of them.
	_, rows = latest(stranger, ids.code, 1)
	require.Empty(t, rows, "a stranger read the owner's session through the latest-session read")
}

// THE TIER IS THE SITE'S, ACCOUNT ARM INCLUDED, and that is what lets the three
// reads carry no caller term (tierDecidesTheRead in rowauthz_enforce_gate_test.go
// adjudicates each on this test's strength). A code is owned by the SITE'S
// owner, so a developer who manages a client's deployable without having
// created it can only see its codes through the account arm; a written-out
// `own || clusterOwner` would have shown them an empty panel.
//
// All three halves are asserted, because the first alone proves nothing about
// the third: the people the account admits read the code, and a person in no
// group does not, for the list and for each lookup.
func TestSiteAccessCodesAreReadableByTheAccountsPeopleAndNotByAStranger(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	ids := newSiteAccessIDs("tier")
	member := "site-access-member-" + ids.owner
	developer := "site-access-developer-" + ids.owner
	stranger := "site-access-stranger-" + ids.owner
	seedPrincipal(t, eng, ids.owner, auth.RoleWriter)
	seedPrincipal(t, eng, member, auth.RoleWriter)
	seedPrincipal(t, eng, developer, auth.RoleDeveloper)
	seedPrincipal(t, eng, stranger, auth.RoleWriter)
	seedGroup(t, eng, "g-"+ids.account, "Acme", "account", ids.account)
	seedMembership(t, eng, "g-"+ids.account, member, "active")

	issued := time.Now().UTC().Truncate(time.Second)
	owner := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, siteAccessAs(ids.owner, auth.RoleWriter)))
	digest := siteAccessDigest(0x77)
	_, err := eng.Execute(owner, siteAccessCall(t, "mutation", "createSiteAccessCode", map[string]any{
		"codeId": ids.code, "siteId": ids.site, "accountId": ids.account,
		"label": "Acme reviewers", "gate": "dashboard", "codeHash": digest, "codeHint": "FBZA",
		"issuedAt": issued.Format(time.RFC3339), "expiresAt": issued.Add(time.Hour).Format(time.RFC3339),
		"lastChangedBy": ids.owner,
	}))
	require.NoError(t, err)

	// A CODE TIED TO NO ACCOUNT is its owner's alone, which is the control that
	// makes the reads below mean something: the arm admits an account's people,
	// not everyone.
	untied := ids.code + "-untied"
	_, err = eng.Execute(owner, siteAccessCall(t, "mutation", "createSiteAccessCode", map[string]any{
		"codeId": untied, "siteId": ids.site + "-untied",
		"label": "Nobody's", "gate": "dashboard", "codeHash": siteAccessDigest(0x78), "codeHint": "ABCD",
		"issuedAt": issued.Format(time.RFC3339), "expiresAt": issued.Add(time.Hour).Format(time.RFC3339),
		"lastChangedBy": ids.owner,
	}))
	require.NoError(t, err)

	reads := func(userID string, role auth.Role) (list, byID, byHash string) {
		t.Helper()
		// Internal origin is the integration's: it passes the capability gate
		// and the server-only gate and leaves the actor, and therefore the
		// tier, exactly as it found it.
		caller := auth.ContextWithInternalOrigin(rankActorCtx(userID, role))
		run := func(call string) string {
			res, err := eng.Execute(caller, call)
			require.NoError(t, err)
			return resultBlob(t, res)
		}
		return run(siteAccessCall(t, "query", "siteAccessCodeRowsForSite", map[string]any{"siteId": ids.site})),
			run(siteAccessCall(t, "query", "siteAccessCodeById", map[string]any{"codeId": ids.code})),
			run(siteAccessCall(t, "query", "siteAccessCodeByHash", map[string]any{"siteId": ids.site, "codeHash": digest}))
	}

	for name, who := range map[string]struct {
		id   string
		role auth.Role
	}{
		"the site's owner":                {ids.owner, auth.RoleWriter},
		"a member of the account's group": {member, auth.RoleWriter},
		"a developer who is in no group":  {developer, auth.RoleDeveloper},
	} {
		t.Run(name+" reads the code", func(t *testing.T) {
			list, byID, byHash := reads(who.id, who.role)
			require.Contains(t, list, ids.code, "the list is empty for %s", name)
			require.Contains(t, byID, ids.code, "the by-id read is empty for %s", name)
			require.Contains(t, byHash, ids.code, "the by-hash read is empty for %s", name)
		})
	}

	t.Run("a stranger reads nothing", func(t *testing.T) {
		list, byID, byHash := reads(stranger, auth.RoleWriter)
		require.NotContains(t, list, ids.code, "a person in no group can list the account's codes")
		require.NotContains(t, byID, ids.code, "a person in no group can read a code by id")
		require.NotContains(t, byHash, ids.code, "a person in no group can resolve a code by digest")
	})

	t.Run("the account's people do not read a code tied to no account", func(t *testing.T) {
		caller := auth.ContextWithInternalOrigin(rankActorCtx(member, auth.RoleWriter))
		res, err := eng.Execute(caller, siteAccessCall(t, "query", "siteAccessCodeById", map[string]any{"codeId": untied}))
		require.NoError(t, err)
		require.NotContains(t, resultBlob(t, res), untied, "the account arm admitted a code that belongs to no account")
	})
}

// installAccessExecutorRecorders stands a recording executor behind each named
// siteAccess capability for one test: reaching it is the proof the gates admitted
// the call, and NOT reaching it is the proof a refusal wrote and read nothing.
// Set on the dispatch map directly, as installDeployRecorder does, because the
// integration lives in a module component/memql cannot import and the shared
// engine outlives the test.
func installAccessExecutorRecorders(t *testing.T, eng *MemQLEngine, executors ...string) map[string]*int {
	t.Helper()
	if eng.builtinExecutorHandlers == nil {
		if err := eng.initBuiltinExecutorHandlers(); err != nil {
			t.Fatalf("init builtin handlers: %v", err)
		}
	}
	reached := map[string]*int{}
	for _, executor := range executors {
		executor := executor
		n := 0
		reached[executor] = &n
		previous, had := eng.builtinExecutorHandlers[executor]
		eng.builtinExecutorHandlers[executor] = func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
			n++
			return nil, nil
		}
		t.Cleanup(func() {
			if had {
				eng.builtinExecutorHandlers[executor] = previous
			} else {
				delete(eng.builtinExecutorHandlers, executor)
			}
		})
	}
	return reached
}

// THE ORGANIZATION GATE JUDGES EVERY ACCESS-CODE BUILTIN, THE LIST AND THE THREE ACTS ON A
// CODE AS IT JUDGES MINTING.
//
// The shared gate judges builtins and mutations and never a query, so a client-callable
// list would let a person who held the part in one organization read the labels, hints
// and expiries of a site in another whose ROW TIER admitted them. This is that person,
// built literally: they OWN a site tied to Beta (so the owner arm of the tier admits every
// code on it) while their only standing, and the only place their group grant gives them
// the part, is Acme.
//
// The premise is asserted first, because the claim is meaningless without it: the
// rows under the list ARE visible to this person through the tier alone. Then the
// gate. The builtins fall in two groups, judged against two different rows: minting,
// the list and the gate listing ask about the SITE, and regenerate, set-expiry and
// remove ask about the CODE, whose accountId was copied off its site when it was
// minted. All six are refused for Beta's with capability_not_held and reach no
// executor, and all six pass for Acme's, where the same grant holds. A member of Beta
// who was never granted the part is refused on Beta's own site and code, so the pass on
// Acme's is the GRANT's doing and not an accident of the fixture. A code or a site that
// does not exist is refused, never answered empty or passed through to the handler.
func TestSiteAccessListIsJudgedByTheOrganizationGate(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	eng.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })

	ids := newSiteAccessIDs("orggate")
	acme, beta := "acme-"+ids.account, "beta-"+ids.account
	person, betaMember := ids.owner, "beta-member-"+ids.owner
	siteAcme, siteBeta := "v1:platform:site:"+ids.site+"-acme", "v1:platform:site:"+ids.site+"-beta"

	seedPrincipal(t, eng, person, auth.RoleWriter)
	seedPrincipal(t, eng, betaMember, auth.RoleWriter)
	for _, account := range []string{acme, beta} {
		require.NoError(t, organizationInsert(t, eng, groupSeedCtx(), conceptAccountsAccount, account,
			map[string]any{"name": account, "status": "active", "domainStatus": "unverified"}))
		seedGroup(t, eng, "g-"+account, account, "account", account)
	}
	seedMembership(t, eng, "g-"+acme, person, "active")
	seedMembership(t, eng, "g-"+beta, betaMember, "active")
	// The part is held in ACME ONLY, by Acme's group grant. Nobody holds it in Beta.
	writeGrant(t, eng, auth.SubjectKindGroup, "g-"+acme, auth.VerbExecute, "app:deployables/access", auth.GrantAllow)
	for site, account := range map[string]string{siteAcme: acme, siteBeta: beta} {
		require.NoError(t, organizationInsert(t, eng, groupSeedCtx(), conceptPlatformSite, site, map[string]any{
			"hostname": BareShortId(site) + "." + siteHostnamePolicyDomain(), "kind": "spa", "status": "draft",
			"bundleRef": "blob://site-access/v1", "ownerUserId": "v1:identity:user:" + person, "accountId": account,
		}))
	}

	// THE PREMISE: a code on Beta's site, and the tier alone shows it to this person.
	// A second code on Acme's site is where the same grant DOES hold.
	issued := time.Now().UTC().Truncate(time.Second)
	internal := auth.ContextWithInternalOrigin(partGateActor(person, auth.RoleWriter))
	codeBeta, codeAcme := ids.code, ids.code+"-acme"
	for code, mint := range map[string]struct{ site, account string }{codeBeta: {siteBeta, beta}, codeAcme: {siteAcme, acme}} {
		_, err := eng.Execute(internal, siteAccessCall(t, "mutation", "createSiteAccessCode", map[string]any{
			"codeId": code, "siteId": mint.site, "accountId": mint.account, "label": "Reviewers", "gate": "dashboard",
			"codeHash": siteAccessDigest(byte(len(code))), "codeHint": "FBZA",
			"issuedAt": issued.Format(time.RFC3339), "expiresAt": issued.Add(time.Hour).Format(time.RFC3339),
			"lastChangedBy": person,
		}))
		require.NoError(t, err)
	}
	rows, err := eng.Execute(internal, siteAccessCall(t, "query", "siteAccessCodeRowsForSite", map[string]any{"siteId": siteBeta}))
	require.NoError(t, err)
	require.Contains(t, resultBlob(t, rows), codeBeta,
		"the premise failed: the row tier does not admit this person to Beta's code, so the gate below is not the only thing in the way")

	// scope is what a call is about: a site for the three builtins that ask about
	// the site, a code for the three that ask about the code.
	type scope struct{ site, code string }
	inBeta, inAcme := scope{siteBeta, codeBeta}, scope{siteAcme, codeAcme}
	expiry := issued.Add(2 * time.Hour).Format(time.RFC3339)
	acts := []struct {
		builtin  string
		executor string
		call     func(scope) string
	}{
		{"siteAccessCodesForSite", "integration.siteAccess.list", func(s scope) string {
			return siteAccessCall(t, "builtin", "siteAccessCodesForSite", map[string]any{"siteId": s.site})
		}},
		{"siteAccessCodeCreate", "integration.siteAccess.create", func(s scope) string {
			return siteAccessCall(t, "builtin", "siteAccessCodeCreate", map[string]any{
				"siteId": s.site, "label": "Probe", "gate": "dashboard", "expiresAt": issued.Add(time.Hour).Format(time.RFC3339),
			})
		}},
		{"siteAccessGates", "integration.siteAccess.gates", func(s scope) string {
			return siteAccessCall(t, "builtin", "siteAccessGates", map[string]any{"siteId": s.site})
		}},
		{"siteAccessCodeRegenerate", "integration.siteAccess.regenerate", func(s scope) string {
			return siteAccessCall(t, "builtin", "siteAccessCodeRegenerate", map[string]any{"codeId": s.code})
		}},
		{"siteAccessCodeSetExpiry", "integration.siteAccess.setExpiry", func(s scope) string {
			return siteAccessCall(t, "builtin", "siteAccessCodeSetExpiry", map[string]any{"codeId": s.code, "expiresAt": expiry})
		}},
		{"siteAccessCodeRemove", "integration.siteAccess.remove", func(s scope) string {
			return siteAccessCall(t, "builtin", "siteAccessCodeRemove", map[string]any{"codeId": s.code})
		}},
	}
	executors := make([]string, 0, len(acts))
	for _, act := range acts {
		executors = append(executors, act.executor)
	}
	reached := installAccessExecutorRecorders(t, eng, executors...)

	refused := func(t *testing.T, err error, builtin string) {
		t.Helper()
		require.Error(t, err, "%s was admitted", builtin)
		require.Contains(t, err.Error(), "capability_not_held", "%s was refused, but not by the capability gate", builtin)
		require.Contains(t, err.Error(), builtin, "the refusal does not name the act")
	}

	// A CLIENT-ORIGIN CALL: internal origin skips the gate by design, and the gate is the point.
	personCtx := partGateActor(person, auth.RoleWriter)
	for _, act := range acts {
		t.Run(act.builtin+" on a site or code in another organization is refused", func(t *testing.T) {
			before := *reached[act.executor]
			_, err := eng.Execute(personCtx, act.call(inBeta))
			refused(t, err, act.builtin)
			require.Equal(t, before, *reached[act.executor], "the executor ran for a call the gate refused")
		})
		t.Run(act.builtin+" on a site or code in the organization that granted the part passes the gate", func(t *testing.T) {
			before := *reached[act.executor]
			_, err := eng.Execute(personCtx, act.call(inAcme))
			require.NoError(t, err, "%s was refused on the organization the grant belongs to", act.builtin)
			require.Equal(t, before+1, *reached[act.executor], "the gate admitted the call but the executor was not reached")
		})
	}

	t.Run("a member of the organization who was never granted the part is refused on its own site and code", func(t *testing.T) {
		for _, act := range acts {
			before := *reached[act.executor]
			_, err := eng.Execute(partGateActor(betaMember, auth.RoleWriter), act.call(inBeta))
			refused(t, err, act.builtin)
			require.Equal(t, before, *reached[act.executor], "the executor ran for a call the gate refused")
		}
	})

	t.Run("a site or code that does not exist is refused, not answered empty or handed to the handler", func(t *testing.T) {
		nowhere := scope{ids.site + "-nowhere", ids.code + "-nowhere"}
		for _, act := range acts {
			before := *reached[act.executor]
			_, err := eng.Execute(personCtx, act.call(nowhere))
			refused(t, err, act.builtin)
			require.Equal(t, before, *reached[act.executor], "the executor ran for a target that does not exist")
		}
	})

	t.Run("a code named by its canonical id is judged by the same row", func(t *testing.T) {
		// The gate must not be steerable by spelling: the canonical form of Beta's
		// code is refused as the bare form is, and Acme's passes as it does.
		for _, act := range acts[3:] {
			before := *reached[act.executor]
			_, err := eng.Execute(personCtx, act.call(scope{siteBeta, "v1:platform:siteAccessCode:" + codeBeta}))
			refused(t, err, act.builtin)
			require.Equal(t, before, *reached[act.executor], "the executor ran for a canonical id the gate refused")
		}
	})

	t.Run("a blank code is refused", func(t *testing.T) {
		for _, act := range acts[3:] {
			before := *reached[act.executor]
			_, err := eng.Execute(personCtx, act.call(scope{siteAcme, " "}))
			require.Error(t, err, "%s was admitted for a blank code", act.builtin)
			require.Equal(t, before, *reached[act.executor], "the executor ran for a blank code")
		}
	})
}

// A RAW WRITE IS A WRITE, AND @serverOnly BARS ONLY THE NAMED CALL.
//
// "Every client-callable write on access codes is a builtin" is only true if the
// builtins are the only way in, and the mutations being @serverOnly does not make
// them so: an insert() that names the concept never consults @serverOnly, and a
// second insert at an existing id is an update. Both go through executeWrite, which
// is where @serverWritten lives (it is how the pipelines and workbench concepts keep
// service-authored rows out of reach, and what outbound_protected_row_write_guard.go
// does for one more). Without it, the site's own OWNER -- the strongest caller the
// row tier admits, and no more than a writer -- could mint a code with a digest they
// chose and an expiry past the 366-day bound, forge a session for it, unremove a
// removed code, or move an expiry, all with no part, no organization check and no
// audit event.
//
// So this is asserted as the owner, with client origin, against both concepts, for a
// create and for an update of a row that exists; and each refusal is checked to be
// the @serverWritten one, and the stored row to be exactly what the builtin wrote.
func TestSiteAccessRowsCannotBeWrittenAroundTheBuiltins(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	ids := newSiteAccessIDs("raw")
	issued := time.Now().UTC().Truncate(time.Second)
	stamp := func(d time.Duration) string { return issued.Add(d).Format(time.RFC3339) }
	owner := "v1:identity:user:" + ids.owner
	asOwner := partGateActor(ids.owner, auth.RoleWriter) // client origin
	internal := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, siteAccessAs(ids.owner, auth.RoleWriter)))

	// A real code and a real session, written the only way they may be.
	res, err := eng.Execute(internal, siteAccessCall(t, "mutation", "createSiteAccessCode", map[string]any{
		"codeId": ids.code, "siteId": ids.site, "label": "Real", "gate": "dashboard",
		"codeHash": siteAccessDigest(0x91), "codeHint": "FBZA",
		"issuedAt": stamp(0), "expiresAt": stamp(time.Hour), "lastChangedBy": ids.owner,
	}))
	require.NoError(t, err)
	storedCode := res.Bundle.Nodes[0].Id
	res, err = eng.Execute(internal, siteAccessCall(t, "mutation", "createSiteAccessSession", map[string]any{
		"sessionId": ids.session, "siteId": ids.site, "codeId": ids.code, "generation": 1,
		"tokenHash": siteAccessDigest(0x92), "issuedAt": stamp(0),
	}))
	require.NoError(t, err)
	storedSession := res.Bundle.Nodes[0].Id
	beforeCode := latestPayload(t, ctx, db, "v1:platform:siteAccessCode", storedCode)
	beforeSession := latestPayload(t, ctx, db, "v1:platform:siteAccessSession", storedSession)

	refused := func(t *testing.T, err error, what string) {
		t.Helper()
		require.Error(t, err, "%s was written around the builtins", what)
		require.Contains(t, err.Error(), "@serverWritten", "%s was refused, but not by the concept's own write gate: %v", what, err)
	}

	t.Run("a code cannot be created by a raw insert", func(t *testing.T) {
		refused(t, organizationInsert(t, eng, asOwner, "v1:platform:siteAccessCode", ids.code+"-forged", map[string]any{
			"siteId": ids.site, "ownerUserId": owner, "label": "Forged", "gate": "dashboard",
			"codeHash": siteAccessDigest(0x93), "codeHint": "AAAA", "generation": 1,
			"issuedAt": stamp(0), "expiresAt": stamp(24 * 365 * 5 * time.Hour), "lastChangedBy": ids.owner,
		}), "a code with a chosen digest and a five year expiry")
	})
	t.Run("a code cannot be moved or unremoved by a raw write at its id", func(t *testing.T) {
		forged := map[string]any{}
		for k, v := range beforeCode {
			forged[k] = v
		}
		forged["expiresAt"] = stamp(24 * 365 * 5 * time.Hour)
		forged["generation"] = 99
		refused(t, organizationInsert(t, eng, asOwner, "v1:platform:siteAccessCode", ids.code, forged), "a raw update of a code")
		require.Equal(t, beforeCode, latestPayload(t, ctx, db, "v1:platform:siteAccessCode", storedCode),
			"the refused raw write changed the stored code")
	})
	t.Run("a session cannot be created by a raw insert", func(t *testing.T) {
		refused(t, organizationInsert(t, eng, asOwner, "v1:platform:siteAccessSession", ids.session+"-forged", map[string]any{
			"siteId": ids.site, "codeId": ids.code, "generation": 1, "ownerUserId": owner,
			"tokenHash": siteAccessDigest(0x94), "issuedAt": stamp(0),
		}), "a session with a chosen token digest")
	})
	t.Run("a session cannot be revived or rebound by a raw write at its id", func(t *testing.T) {
		forged := map[string]any{}
		for k, v := range beforeSession {
			forged[k] = v
		}
		forged["generation"] = 99
		refused(t, organizationInsert(t, eng, asOwner, "v1:platform:siteAccessSession", ids.session, forged), "a raw update of a session")
		require.Equal(t, beforeSession, latestPayload(t, ctx, db, "v1:platform:siteAccessSession", storedSession),
			"the refused raw write changed the stored session")
	})
	t.Run("the builtins' own writes still pass", func(t *testing.T) {
		_, err := eng.Execute(internal, siteAccessCall(t, "mutation", "setSiteAccessCodeExpiry", map[string]any{
			"codeId": ids.code, "expiresAt": stamp(2 * time.Hour), "lastChangedBy": ids.owner,
		}))
		require.NoError(t, err, "internal origin is refused by @serverWritten, which would leave every integration write dead")
	})
}
