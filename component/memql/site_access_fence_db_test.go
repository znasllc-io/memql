package memql

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
)

// site_access_fence_db_test.go -- an operator's act on a code is never silently
// undone by another write to the same row (deployable access codes, review of
// Task 3, finding 1; design record decision D13).
//
// ===========================================================================
// WHAT WENT WRONG, AND WHY IT IS A SECURITY PROPERTY
// ===========================================================================
// Every write to a code is a READ-MERGE of the whole row: the executor reads
// the stored payload, lays the call's fields over it and stores the result as
// the next version. Two writes racing on one row therefore both start from the
// same stored version, and the second to commit stores a row that was merged
// from a STALE read -- every field the first one changed goes back to what it
// was. The review measured it with a heartbeat prototype (a 'last used' write
// on every use of a code) racing an operator act released at the same
// instant, 200 rounds each:
//
//	Remove      lost 123 of 200 -- a removed code was live again
//	Regenerate  lost  65 of 200 -- the OLD digest was back, and siteAccessCodeByHash
//	                               resolved the code the operator had just replaced
//	Set expiry  lost 117 of 200 -- the expiry went back to the old date
//
// A regenerate that is reported successful and does not take effect leaves a
// compromised code working, which is the one act the feature exists for.
//
// THERE IS NO HEARTBEAT (nothing a visitor does writes this row), so the race
// the review measured has no second writer. What remains is two OPERATORS
// acting on one code at once, and the same arithmetic applies to them. So the row also carries the engine's row-version fence
// (executor_mutation.go): the executor records the version it is about to
// merge into, and the write commits only if that version is still the newest.
// A loser is told -- row version changed before the write committed -- and
// nothing of the loser's was written, instead of its stale read undoing the
// winner.
//
// THE PROPERTY, in the words of the review: among the acts REPORTED successful,
// none is lost. An act that was refused is not a loss; it was reported.

// siteAccessRaceAct is one operator act, with the question that decides whether
// the stored row still shows it.
type siteAccessRaceAct struct {
	name string
	// call renders the mutation for this round's code.
	call func(t *testing.T, ids siteAccessIDs, stamp func(time.Duration) string) string
	// shown reports whether the stored row carries the act's effect. `other` is
	// whether the other act of the pair was also reported successful, for the
	// one field two acts both write.
	shown func(row map[string]any, stamp func(time.Duration) string, other bool) bool
}

func siteAccessRaceActs() map[string]siteAccessRaceAct {
	newDigest := siteAccessDigest(0x22)
	return map[string]siteAccessRaceAct{
		"remove": {
			name: "remove",
			call: func(t *testing.T, ids siteAccessIDs, stamp func(time.Duration) string) string {
				return siteAccessCall(t, "mutation", "removeSiteAccessCode", map[string]any{
					"codeId": ids.code, "revokedAt": stamp(time.Minute), "lastChangedBy": "operator-remove",
				})
			},
			shown: func(row map[string]any, stamp func(time.Duration) string, _ bool) bool {
				return fmt.Sprint(row["revokedAt"]) == stamp(time.Minute)
			},
		},
		"regenerate": {
			name: "regenerate",
			call: func(t *testing.T, ids siteAccessIDs, stamp func(time.Duration) string) string {
				return siteAccessCall(t, "mutation", "rotateSiteAccessCode", map[string]any{
					"codeId": ids.code, "codeHash": newDigest, "codeHint": "ZZZZ", "generation": 2,
					"issuedAt": stamp(time.Minute), "expiresAt": stamp(5 * time.Hour), "lastChangedBy": "operator-regenerate",
				})
			},
			shown: func(row map[string]any, _ func(time.Duration) string, _ bool) bool {
				return row["codeHash"] == newDigest && fmt.Sprint(row["generation"]) == "2"
			},
		},
		"setExpiry": {
			name: "setExpiry",
			call: func(t *testing.T, ids siteAccessIDs, stamp func(time.Duration) string) string {
				return siteAccessCall(t, "mutation", "setSiteAccessCodeExpiry", map[string]any{
					"codeId": ids.code, "expiresAt": stamp(3 * time.Hour), "lastChangedBy": "operator-expiry",
				})
			},
			shown: func(row map[string]any, stamp func(time.Duration) string, regenerated bool) bool {
				got := fmt.Sprint(row["expiresAt"])
				// A regenerate also writes the expiry, so when both were
				// reported successful either order is a correct outcome. What
				// is never correct is the date the code was minted with.
				return got == stamp(3*time.Hour) || (regenerated && got == stamp(5*time.Hour))
			},
		},
	}
}

func TestSiteAccessActsRacingOnOneCodeNeverSilentlyUndoEachOther(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	const rounds = 200
	acts := siteAccessRaceActs()
	oldDigest := siteAccessDigest(0x11)

	for _, pair := range [][2]string{{"remove", "regenerate"}, {"remove", "setExpiry"}, {"regenerate", "setExpiry"}} {
		first, second := acts[pair[0]], acts[pair[1]]
		t.Run(first.name+" against "+second.name, func(t *testing.T) {
			// The pairs share nothing but the engine, and running them together
			// keeps 600 rounds inside the time one pair would take serially.
			t.Parallel()
			var lost, fenced, both int
			for round := 0; round < rounds; round++ {
				ids := newSiteAccessIDs(fmt.Sprintf("fence-%s-%s-%d", first.name, second.name, round))
				issued := time.Now().UTC().Truncate(time.Second)
				stamp := func(d time.Duration) string { return issued.Add(d).Format(time.RFC3339) }
				owner := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, siteAccessAs(ids.owner, auth.RoleWriter)))

				res, err := eng.Execute(owner, siteAccessCall(t, "mutation", "createSiteAccessCode", map[string]any{
					"codeId": ids.code, "siteId": ids.site, "label": "race", "gate": "dashboard",
					"codeHash": oldDigest, "codeHint": "FBZA",
					"issuedAt": stamp(0), "expiresAt": stamp(24 * time.Hour), "lastChangedBy": ids.owner,
				}))
				require.NoError(t, err)
				stored := res.Bundle.Nodes[0].Id

				// Both acts are released together: the window the review
				// measured is the few milliseconds between a write's read and
				// its commit.
				start := make(chan struct{})
				var wg sync.WaitGroup
				var errFirst, errSecond error
				wg.Add(2)
				go func() { defer wg.Done(); <-start; _, errFirst = eng.Execute(owner, first.call(t, ids, stamp)) }()
				go func() { defer wg.Done(); <-start; _, errSecond = eng.Execute(owner, second.call(t, ids, stamp)) }()
				close(start)
				wg.Wait()

				// A refused act must be refused FOR THE FENCE. Any other error is
				// a fixture that does not reach the property.
				for _, e := range []error{errFirst, errSecond} {
					if e != nil {
						require.ErrorIs(t, e, ErrRowVersionChanged,
							"an act failed for a reason other than losing the race: %v", e)
						fenced++
					}
				}
				if errFirst == nil && errSecond == nil {
					both++
				}

				row := latestPayload(t, ctx, db, "v1:platform:siteAccessCode", stored)
				for _, c := range []struct {
					act   siteAccessRaceAct
					err   error
					other error
				}{{first, errFirst, errSecond}, {second, errSecond, errFirst}} {
					if c.err != nil {
						continue
					}
					if !c.act.shown(row, stamp, c.other == nil) {
						lost++
						if lost <= 3 {
							t.Errorf("round %d: %s was reported successful and the stored row does not show it: %v", round, c.act.name, row)
						}
					}
				}

				// THE SECURITY CONSEQUENCE of a lost regenerate: the code that was
				// replaced still opens the site. Asked of the read the bff uses.
				if (first.name == "regenerate" && errFirst == nil) || (second.name == "regenerate" && errSecond == nil) {
					got, err := eng.Execute(owner, siteAccessCall(t, "query", "siteAccessCodeByHash", map[string]any{
						"siteId": ids.site, "codeHash": oldDigest,
					}))
					require.NoError(t, err)
					if len(MaterializeRows(got)) != 0 {
						lost++
						if lost <= 3 {
							t.Errorf("round %d: a regenerate was reported successful and the code it replaced still resolves", round)
						}
					}
				}
			}
			t.Logf("%s against %s: %d rounds, %d lost, %d acts refused by the fence, %d rounds where both acts took effect",
				first.name, second.name, rounds, lost, fenced, both)
			require.Zero(t, lost, "%d of %d rounds lost an act that was reported successful", lost, rounds)
		})
	}
}

// THE WITNESS A HANDLER CARRIES. The fence the executor installs on its own covers
// the merge window of ONE mutation. An operator act has a longer window than that: the
// handler reads the code, decides (a regenerate computes generation+1 from what it read,
// and refuses a removed code), and only then writes. A write that was decided from an
// earlier read must fail if the row has moved since, and the handler says so by passing
// the version it read -- ContextWithRowVersionFence, with the row's createdAt, which is
// the version of its latest write (siteAccessCodeView carries it as row.createdAt for
// this purpose). Without that, two regenerates that both read generation 1 would both
// write generation 2: the second code's sessions would be honoured under the first
// code's number, and the first operator would hold a code that no longer works.
//
// This is the protocol the access handlers use, run against the real concept, so that
// the witness a handler can actually obtain from the view is the one the fence accepts.
func TestSiteAccessActDecidedFromAStaleReadIsRefused(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	ids := newSiteAccessIDs("stale-witness")
	issued := time.Now().UTC().Truncate(time.Second)
	stamp := func(d time.Duration) string { return issued.Add(d).Format(time.RFC3339) }
	owner := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, siteAccessAs(ids.owner, auth.RoleWriter)))

	res, err := eng.Execute(owner, siteAccessCall(t, "mutation", "createSiteAccessCode", map[string]any{
		"codeId": ids.code, "siteId": ids.site, "label": "stale", "gate": "dashboard",
		"codeHash": siteAccessDigest(0x41), "codeHint": "FBZA",
		"issuedAt": stamp(0), "expiresAt": stamp(24 * time.Hour), "lastChangedBy": ids.owner,
	}))
	require.NoError(t, err)
	stored := res.Bundle.Nodes[0].Id

	// What a handler reads: the view, and the version of the row in it.
	witness := func() time.Time {
		t.Helper()
		got, err := eng.Execute(owner, siteAccessCall(t, "query", "siteAccessCodeById", map[string]any{"codeId": ids.code}))
		require.NoError(t, err)
		rows := MaterializeRows(got)
		require.Len(t, rows, 1)
		var version time.Time
		switch v := rows[0]["createdAt"].(type) {
		case time.Time:
			version = v
		case string:
			version, err = time.Parse(time.RFC3339Nano, v)
			require.NoError(t, err, "the view's createdAt is not a time: %q", v)
		default:
			t.Fatalf("the view carries no usable createdAt (%T): %v", rows[0]["createdAt"], rows[0])
		}
		return version
	}
	rotate := func(ctx context.Context, seed byte, generation int) error {
		_, err := eng.Execute(ctx, siteAccessCall(t, "mutation", "rotateSiteAccessCode", map[string]any{
			"codeId": ids.code, "codeHash": siteAccessDigest(seed), "codeHint": "ZZZZ", "generation": generation,
			"issuedAt": stamp(time.Minute), "expiresAt": stamp(24 * time.Hour), "lastChangedBy": "operator",
		}))
		return err
	}
	fenced := func(version time.Time) context.Context {
		return ContextWithRowVersionFence(owner, "v1:platform:siteAccessCode", ids.code, version)
	}

	// Two handlers read the same version of the row.
	first, second := witness(), witness()

	// The first writes under what it read, and succeeds: the row is still that version.
	require.NoError(t, rotate(fenced(first), 0x42, 2), "a write decided from the current version was refused, so no handler could ever act")
	row := latestPayload(t, ctx, db, "v1:platform:siteAccessCode", stored)
	require.Equal(t, siteAccessDigest(0x42), row["codeHash"])

	// The second decided from a read that is now out of date. It is refused, loudly,
	// and wrote nothing: the first operator's code is still the code.
	err = rotate(fenced(second), 0x43, 2)
	require.ErrorIs(t, err, ErrRowVersionChanged, "an act decided from a stale read was applied over the act that outdated it")
	row = latestPayload(t, ctx, db, "v1:platform:siteAccessCode", stored)
	require.Equal(t, siteAccessDigest(0x42), row["codeHash"], "the refused act changed the code")

	// And it succeeds on the retry the refusal asks for: read again, decide again, write.
	require.NoError(t, rotate(fenced(witness()), 0x43, 3))
	row = latestPayload(t, ctx, db, "v1:platform:siteAccessCode", stored)
	require.Equal(t, siteAccessDigest(0x43), row["codeHash"])
	require.EqualValues(t, 3, row["generation"])
}
