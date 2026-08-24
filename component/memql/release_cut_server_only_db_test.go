package memql

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// release_cut_server_only_db_test.go -- epic memql#4434.
//
// Proves the release-cut surface WORKS, against a real engine and a real
// database, rather than proving that the code that would drive it compiles.
//
// ===========================================================================
// WHY THIS LIVES HERE AND NOT IN integrations/release
// ===========================================================================
// That package's suite drives a fake engine: it asserts what was written and
// under which call origin, which is the right question there. What it
// structurally cannot answer is whether the ENGINE accepts those calls, and
// two gates stand between the integration and a row:
//
//   the @serverOnly gate, which refuses any origin that is not internal;
//   the @rowAuthz(clusterOwner) tier on v1:cluster:releaseCut, which decides
//   the rows a caller may read and write.
//
// Both are enforced in this package. A test in integrations/ that stubbed
// either would be testing its own stub.
//
// ===========================================================================
// THE FAILURE THIS EXISTS TO CATCH IS SILENT IN BOTH DIRECTIONS
// ===========================================================================
// auth.CallOrigin's zero value is OriginClient, so a missing stamp refuses
// every write with ONE WARN and an empty table behind it -- the release
// history simply never fills. And on the read side it is worse: an
// unauthorized row read returns ZERO ROWS, which is indistinguishable from
// "this version was never cut", so releaseCutStatus would report
// version_not_cut for every release the cluster actually made.
//
// Neither shows up as an error anywhere. Hence: drive both directions.

// releaseCutProbeVersion is the row id this test owns. It is a version no
// repository will ever carry, so the row cannot collide with a real cut on a
// shared test database.
const releaseCutProbeVersion = "v0.0.0-releasecut-probe"

// releaseCutProbeCall builds the create call the integration builds, with the
// same argument set.
//
// Quoted through langparser.QuoteString rather than %q, for the reason
// TestDSLCallStringsDoNotUseGoQuoting gives: %q emits escapes the MemQL lexer
// rejects, so a test modelling the call form with %q teaches the next copy of
// it the same mistake.
func releaseCutProbeCall() string {
	q := langparser.QuoteString
	return fmt.Sprintf(`mutation createReleaseCut(version:%s,bump:"patch",baseSha:%s,`+
		`requestedBy:%s,requestedByEmail:%s,status:"dispatched",tagName:%s,`+
		`releaseUrl:%s,dispatchedAt:"2026-08-24T00:00:00Z")`,
		q(releaseCutProbeVersion), q("0000000000000000000000000000000000000000"),
		q("v1:identity:user:releasecut-probe"), q("probe@example.test"),
		q(releaseCutProbeVersion), q("https://example.test/releases/probe"))
}

// TestReleaseCutWritesAreServerOnlyAndInternalOriginPasses drives both
// mutations from both origins.
//
// The REFUSAL half is the security property: a client-reachable
// createReleaseCut lets any caller write another owner's name into an
// append-only release history, and a client-reachable updateReleaseCutStatus
// lets anyone mark a version images_available without an image existing --
// exactly the false green the design is arranged to prevent.
//
// The PASSING half is what makes the annotation survivable rather than merely
// strict. Without it the feature is inert and every other test in the epic
// still passes.
func TestReleaseCutWritesAreServerOnlyAndInternalOriginPasses(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)

	// The engine must be acting for a CLUSTER OWNER, because the concept
	// declares @rowAuthz(clusterOwner) and the write guard consults the
	// tier. sharedReadMergeEngine's context carries a token and no
	// AccessContext, which is not an owner.
	ownerCtx := auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: "v1:identity:user:releasecut-probe",
		Role:   auth.RoleOwner,
	})

	cases := []struct {
		construct string
		call      string
		// exposure says what a wire-reachable version would hand a caller.
		// Per-case, because the answers differ and "it is server-only" is
		// not a reason.
		exposure string
	}{
		{
			construct: "createReleaseCut",
			call:      releaseCutProbeCall(),
			exposure:  "writing another owner's name into an append-only release history, uncorrectably",
		},
		{
			construct: "updateReleaseCutStatus",
			call: fmt.Sprintf(`mutation updateReleaseCutStatus(version:%s,status:"images_available",`+
				`checkedAt:"2026-08-24T00:05:00Z")`, langparser.QuoteString(releaseCutProbeVersion)),
			exposure: "marking a version images_available with no image existing -- the false green D5 exists to prevent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.construct, func(t *testing.T) {
			t.Run("a client-origin call is refused", func(t *testing.T) {
				_, err := eng.Execute(ownerCtx, tc.call)
				if err == nil {
					t.Fatalf("%s answered a client-origin call, which puts %s on the wire. "+
						"Either @serverOnly was dropped or the gate stopped being enforced.",
						tc.construct, tc.exposure)
				}
				if !strings.Contains(err.Error(), "server-only") {
					t.Errorf("%s was refused, but NOT by the server-only gate -- so this case would "+
						"keep passing if the gate were removed and something else happened to "+
						"fail: %v", tc.construct, err)
				}
			})

			t.Run("an internal-origin call passes the gate", func(t *testing.T) {
				if _, err := eng.Execute(auth.ContextWithInternalOrigin(ownerCtx), tc.call); err != nil {
					if strings.Contains(err.Error(), "server-only") {
						t.Fatalf("%s refused an INTERNAL-origin call. integrations/release stamps "+
							"exactly this, so nothing can reach the construct and the release "+
							"history stays permanently empty with one WARN per cut: %v",
							tc.construct, err)
					}
					t.Fatalf("%s failed for a non-gate reason -- fix the fixture, the gate is fine: %v",
						tc.construct, err)
				}
			})
		})
	}
}

// TestReleaseCutsReadIsReachableForAnOwnerAndReturnsTheRow closes the half a
// refusal test cannot: that the row written above can actually be READ back.
//
// This is the check the memory of memql#4312 asks for. Declaring a tier
// narrows every existing read, there is no internal-origin bypass on the READ
// path, and an unauthorized read returns zero rows rather than an error -- so
// "the query is gated correctly" and "the query returns nothing to anybody"
// look identical from outside. releaseCutStatus reads through this query to
// decide version_not_cut, so a silently-empty read turns every status check
// into "this cluster never cut that".
func TestReleaseCutsReadIsReachableForAnOwnerAndReturnsTheRow(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	ownerCtx := auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: "v1:identity:user:releasecut-probe",
		Role:   auth.RoleOwner,
	})

	// Seed through the same door the integration uses, so this test cannot
	// pass against a row written by a path production does not have.
	if _, err := eng.Execute(auth.ContextWithInternalOrigin(ownerCtx), releaseCutProbeCall()); err != nil {
		t.Fatalf("seeding the probe row failed: %v", err)
	}

	// The read the integration issues, verbatim.
	res, err := eng.Execute(auth.ContextWithInternalOrigin(ownerCtx), "query releaseCuts()")
	if err != nil {
		t.Fatalf("releaseCuts refused an owner: %v", err)
	}
	if res == nil || res.Bundle == nil {
		t.Fatal("releaseCuts returned no bundle at all")
	}
	found := false
	for _, n := range res.Bundle.GetNodes() {
		if n == nil || n.GetPayload() == nil {
			continue
		}
		if v, _ := n.GetPayload().AsMap()["version"].(string); v == releaseCutProbeVersion {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("releaseCuts did not return the row that was just written under the SAME actor. "+
			"An unauthorized row read returns zero rows rather than an error, so this reads as "+
			"'never cut' -- which is what releaseCutStatus would then report for every real "+
			"release. Rows returned: %d", len(res.Bundle.GetNodes()))
	}
}

// TestReleaseCutsReadIsRefusedForANonOwner is the other direction, and without
// it the test above is compatible with a query that returns the history to
// everybody.
//
// requiresOwner is `role == "owner"`, so an ADMIN is the interesting case: it
// clears every other gate on the Deployments page.
func TestReleaseCutsReadIsRefusedForANonOwner(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	ownerCtx := auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: "v1:identity:user:releasecut-probe",
		Role:   auth.RoleOwner,
	})
	if _, err := eng.Execute(auth.ContextWithInternalOrigin(ownerCtx), releaseCutProbeCall()); err != nil {
		t.Fatalf("seeding the probe row failed: %v", err)
	}

	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleDeveloper, auth.RoleWriter, auth.RoleReader} {
		t.Run(string(role), func(t *testing.T) {
			nonOwner := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
				UserId: "v1:identity:user:someone-else",
				Role:   role,
			})
			res, err := eng.Execute(auth.ContextWithInternalOrigin(nonOwner), "query releaseCuts()")
			if err != nil {
				// A refusal is a correct answer too.
				return
			}
			if res == nil || res.Bundle == nil {
				return
			}
			for _, n := range res.Bundle.GetNodes() {
				if n == nil || n.GetPayload() == nil {
					continue
				}
				if v, _ := n.GetPayload().AsMap()["version"].(string); v == releaseCutProbeVersion {
					t.Fatalf("role %q read the release-cut history. requiresOwner is `role == \"owner\"`; "+
						"if this passes, the portal card's owner-only rendering is the ONLY thing "+
						"standing between a non-owner and the history -- and that is a courtesy, "+
						"not a control.", role)
				}
			}
		})
	}
}

// TestReleaseCutByVersionFindsAVersionPastTheListsPageBoundary is the
// regression this query exists for.
//
// releaseCuts paginates 50, which is right for a portal list and wrong for a
// lookup. Store.CutByVersion used to scan that list, so an installation past
// its fiftieth release answered version_not_cut for every older version -- and
// version_not_cut MEANS "cut by hand, or on another installation". A confident
// wrong answer produced by a page boundary invisible from the message.
//
// The test seeds past the boundary deliberately: 55 newer rows, then asks for
// the oldest. Against the old scan this fails; against the by-id read it
// passes. The DB is what makes that difference visible -- no fake-engine test
// can have a page boundary.
func TestReleaseCutByVersionFindsAVersionPastTheListsPageBoundary(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	own := auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: "v1:identity:user:releasecut-probe",
		Role:   auth.RoleOwner,
	})
	internal := auth.ContextWithInternalOrigin(own)

	// The one this test goes looking for, written FIRST so every other row
	// sorts newer than it.
	const buried = "v0.0.0-releasecut-buried"
	seed := func(version string) {
		t.Helper()
		call := fmt.Sprintf(`mutation createReleaseCut(version:%s,bump:"patch",baseSha:%s,`+
			`requestedBy:%s,status:"dispatched",tagName:%s,dispatchedAt:"2026-08-24T00:00:00Z")`,
			langparser.QuoteString(version),
			langparser.QuoteString("0000000000000000000000000000000000000000"),
			langparser.QuoteString("v1:identity:user:releasecut-probe"),
			langparser.QuoteString(version))
		if _, err := eng.Execute(internal, call); err != nil {
			t.Fatalf("seeding %s: %v", version, err)
		}
	}
	seed(buried)
	// 55 > the list's paginate 50, so the buried row cannot be on the page.
	for n := 0; n < 55; n++ {
		seed(fmt.Sprintf("v0.0.%d-releasecut-filler", n))
	}

	// The CONTROL: prove the buried row really is off the list, or the
	// assertion below is about nothing.
	list, err := eng.Execute(internal, "query releaseCuts()")
	if err != nil {
		t.Fatalf("releaseCuts: %v", err)
	}
	onTheList := false
	for _, n := range list.Bundle.GetNodes() {
		if n == nil || n.GetPayload() == nil {
			continue
		}
		if v, _ := n.GetPayload().AsMap()["version"].(string); v == buried {
			onTheList = true
		}
	}
	if onTheList {
		t.Skipf("the buried row is still on the paginated list (%d rows returned), so this test "+
			"cannot demonstrate the boundary -- raise the filler count above the page size",
			len(list.Bundle.GetNodes()))
	}

	// The by-id read must find it anyway.
	res, err := eng.Execute(internal, fmt.Sprintf("query releaseCutByVersion(version:%s)",
		langparser.QuoteString(buried)))
	if err != nil {
		t.Fatalf("releaseCutByVersion refused an owner: %v", err)
	}
	found := false
	for _, n := range res.Bundle.GetNodes() {
		if n == nil || n.GetPayload() == nil {
			continue
		}
		if v, _ := n.GetPayload().AsMap()["version"].(string); v == buried {
			found = true
		}
	}
	if !found {
		t.Fatalf("releaseCutByVersion did not find %s, which IS in the database but is off the "+
			"history list's first page. releaseCutStatus would answer version_not_cut -- "+
			"'cut by hand, or on another installation' -- for a release this cluster made.", buried)
	}
}

// TestUpdateReleaseCutStatusClearsAStaleError pins the behaviour
// integrations/release's renderCall depends on, and which its comment used to
// describe backwards.
//
// renderCall omits a blank string rather than sending it, and the plausible
// story for why -- "update{} is a read-merge, so an omitted field is left
// alone" -- is FALSE for these mutations. The body spells `error: args.error ??
// ""`, and `??` is blank-coalescing over an ABSENT argument too, so the field
// is written as "" either way.
//
// That is the behaviour the feature wants: a version that reached
// images_available must not still carry the failure message from when it was
// half-done. It is pinned here rather than left as a comment because the two
// readings differ only in an outcome nothing else asserts -- a stale error
// beside a green status is exactly the kind of wrong-but-plausible row an
// operator would act on.
func TestUpdateReleaseCutStatusClearsAStaleError(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	own := auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: "v1:identity:user:releasecut-probe",
		Role:   auth.RoleOwner,
	})
	internal := auth.ContextWithInternalOrigin(own)
	q := langparser.QuoteString
	const version = "v0.0.0-releasecut-staleerror"

	// A half-done cut, carrying the failure that state exists to record.
	create := fmt.Sprintf(`mutation createReleaseCut(version:%s,bump:"patch",baseSha:%s,`+
		`requestedBy:%s,status:"tag_created_release_failed",tagName:%s,error:%s)`,
		q(version), q("0000000000000000000000000000000000000000"),
		q("v1:identity:user:releasecut-probe"), q(version),
		q("tag_created_release_failed: GitHub refused the Release"))
	if _, err := eng.Execute(internal, create); err != nil {
		t.Fatalf("seeding the half-done row: %v", err)
	}

	// Somebody publishes the Release by hand, the images build, and a check
	// finds them. Store.UpdateStatus sends no `error` argument, because
	// renderCall drops the blank.
	update := fmt.Sprintf(`mutation updateReleaseCutStatus(version:%s,status:"images_available",`+
		`checkedAt:"2026-08-24T00:05:00Z")`, q(version))
	if _, err := eng.Execute(internal, update); err != nil {
		t.Fatalf("update: %v", err)
	}

	res, err := eng.Execute(internal, fmt.Sprintf("query releaseCutByVersion(version:%s)", q(version)))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	checked := false
	for _, n := range res.Bundle.GetNodes() {
		if n == nil || n.GetPayload() == nil {
			continue
		}
		m := n.GetPayload().AsMap()
		if v, _ := m["version"].(string); v != version {
			continue
		}
		checked = true
		if status, _ := m["status"].(string); status != "images_available" {
			t.Errorf("status = %q, want images_available", status)
		}
		if errText, _ := m["error"].(string); errText != "" {
			t.Errorf("error = %q, want it cleared. A stale failure message beside a green "+
				"status is a row an operator would act on -- and if this is preserved rather "+
				"than cleared, renderCall dropping the blank is a real behaviour change and "+
				"not the shortening its comment says it is.", errText)
		}
	}
	if !checked {
		t.Fatal("the row was not read back at all, so this asserts nothing")
	}
}
