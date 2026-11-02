package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
)

// platform_site_preview_guard_db_test.go -- the candidate guard over real rows.
//
// platform_site_preview_guard_test.go beside this one hands the guard a write
// composed by hand. executeWrite hands it the row the read-merge produced, and
// that difference is what these cases are about: promoteSiteCandidate and
// clearSiteCandidate both store an EMPTY candidateRef, so every later write to
// the row reaches the guard carrying that field. Only the real mutations over a
// stored row show what it does.
//
// The caller is a person throughout. A system actor returns before the promotion
// rule, so a case run as one passes whatever that rule decides. The versions are
// the site's own, because site_bundle_reference.go refuses any other first.
//
// Postgres-gated like its neighbours (sharedReadMergeEngine skips when no DB is
// reachable); MEMQL_REQUIRE_DB=1 turns the skip into a failure.

// candidateTestPerson boots the fixture these cases share: an ordinary member of
// one organization, holding the parts of the Deployables app the four acts ask
// for, so a refusal below is the guard's and not a missing grant.
func candidateTestPerson(t *testing.T, name string) (*MemQLEngine, *bun.DB, context.Context, string) {
	t.Helper()
	// The engine first: its boot judges the DSL's role floors against whatever
	// catalog is installed, and the first test to borrow it may be this one.
	eng, db, _ := sharedReadMergeEngine(t)
	installSiteOrganizationCapabilities(t,
		auth.VerbResource{Verb: auth.VerbExecute, Resource: "app:deployables/deploy"},
		auth.VerbResource{Verb: auth.VerbExecute, Resource: "app:deployables/preview"})
	t.Setenv(memqlDomainEnv, siteTestDomain)
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	eng.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })

	suffix := uniqueSuffix(name)
	account := "account-" + suffix
	return eng, db, siteOrganizationMemberCtx(t, eng, "owner-"+suffix, account), account
}

// candidateTestSite is one live deployable, serving v1, and the person who owns it.
type candidateTestSite struct {
	t   *testing.T
	ctx context.Context
	eng *MemQLEngine
	db  *bun.DB
	id  string
}

func newCandidateTestSite(t *testing.T, eng *MemQLEngine, db *bun.DB, ctx context.Context, account, name, kind string) candidateTestSite {
	t.Helper()
	bare := "site-" + uniqueSuffix(name)
	stored, err := createSiteRaw(t, ctx, eng, map[string]any{
		"siteId":    bare,
		"accountId": account,
		"hostname":  uniqueSuffix(name) + "." + siteTestDomain,
		"kind":      kind,
		"status":    "live",
		"bundleRef": "blob://sites/" + bare + "/v1/",
	})
	if err != nil {
		t.Fatalf("create the deployable: %v", err)
	}
	return candidateTestSite{t: t, ctx: ctx, eng: eng, db: db, id: stored}
}

// version spells one of this site's own versions, as the publisher does.
func (s candidateTestSite) version(name string) string {
	return "blob://sites/" + BareShortId(s.id) + "/" + name + "/"
}

// setCandidate, promote, clear and point are the four acts, each through the
// mutation a person's client calls. point is a publish and a rollback alike:
// they are the same write in opposite directions.
func (s candidateTestSite) setCandidate(name string) error {
	_, err := runSiteMutation(s.t, s.ctx, s.eng, "setSiteCandidate", map[string]any{"siteId": s.id, "candidateRef": s.version(name)})
	return err
}

func (s candidateTestSite) promote(name string) error {
	_, err := runSiteMutation(s.t, s.ctx, s.eng, "promoteSiteCandidate", map[string]any{"siteId": s.id, "candidateRef": s.version(name)})
	return err
}

func (s candidateTestSite) clear() error {
	_, err := runSiteMutation(s.t, s.ctx, s.eng, "clearSiteCandidate", map[string]any{"siteId": s.id})
	return err
}

func (s candidateTestSite) point(name string) error {
	_, err := runSiteMutation(s.t, s.ctx, s.eng, "updateSiteBundle", map[string]any{"siteId": s.id, "bundleRef": s.version(name)})
	return err
}

// must fails the test on a setup step that has to work for the case to mean anything.
func (s candidateTestSite) must(step string, err error) {
	s.t.Helper()
	if err != nil {
		s.t.Fatalf("%s: %v", step, err)
	}
}

// expectRow checks the stored row itself, off the table.
func (s candidateTestSite) expectRow(serving, candidate string) {
	s.t.Helper()
	row := latestPayload(s.t, context.Background(), s.db, conceptPlatformSite, s.id)
	if got := stringFromAny(row["bundleRef"]); got != s.version(serving) {
		s.t.Errorf("bundleRef = %q, want version %s", got, serving)
	}
	want := ""
	if candidate != "" {
		want = s.version(candidate)
	}
	if got := stringFromAny(row["candidateRef"]); got != want {
		s.t.Errorf("candidateRef = %q, want %q", got, want)
	}
}

// AFTER A CANDIDATE ENDS, THE SERVING VERSION STILL MOVES. Promoting and
// withdrawing both end with candidateRef stored as empty, and from then on a
// publish reaches the guard as a moved bundleRef beside an empty candidateRef:
// the two values a promotion carries. It is not one, and a rollback is the same
// write. mutations.memql promises exactly that: "rollback is updateSiteBundle
// pointed back at the previous version, one row write".
func TestTheServingVersionStillMovesAfterACandidateEnds(t *testing.T) {
	eng, db, ctx, account := candidateTestPerson(t, "cand-ends")

	for _, tc := range []struct {
		name string
		slug string
		// end stops v2 being the candidate, and serving is what the site serves after it.
		end     func(candidateTestSite) error
		serving string
		// next is where the write under test points the site.
		next string
	}{
		{
			name: "promote then publish", slug: "cand-promote-publish",
			end: func(s candidateTestSite) error { return s.promote("v2") }, serving: "v2",
			next: "v3",
		},
		{
			name: "promote then rollback", slug: "cand-promote-rollback",
			end: func(s candidateTestSite) error { return s.promote("v2") }, serving: "v2",
			next: "v1",
		},
		{
			name: "clear then publish", slug: "cand-clear-publish",
			end: candidateTestSite.clear, serving: "v1",
			next: "v3",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := newCandidateTestSite(t, eng, db, ctx, account, tc.slug, "spa")
			site.must("set the candidate", site.setCandidate("v2"))
			site.must("end the candidate", tc.end(site))
			site.expectRow(tc.serving, "")

			if err := site.point(tc.next); err != nil {
				t.Fatalf("pointing the site at %s was refused: %v", tc.next, err)
			}
			site.expectRow(tc.next, "")

			// THE WHOLE CYCLE RUNS AGAIN FROM HERE: after these writes a real
			// promotion is still recognised, and still lands.
			site.must("set the next candidate", site.setCandidate("v4"))
			site.expectRow(tc.next, "v4")
			site.must("promote the next candidate", site.promote("v4"))
			site.expectRow("v4", "")
		})
	}

	// A storefront takes no candidate but its serving version, and clearing one
	// is the repair the guard documents for a row that still carries one. That
	// repair must not cost the storefront its next publish.
	t.Run("clear then publish, on a storefront", func(t *testing.T) {
		site := newCandidateTestSite(t, eng, db, ctx, account, "cand-storefront", storefrontSiteKind)
		site.must("set the serving version as the candidate", site.setCandidate("v1"))
		site.must("clear the candidate", site.clear())
		site.expectRow("v1", "")

		if err := site.point("v2"); err != nil {
			t.Fatalf("publishing to a storefront after clearing its candidate was refused: %v", err)
		}
		site.expectRow("v2", "")
	})
}

// A PROMOTION IS STILL REFUSED WHEN IT NAMES ANYTHING BUT THE STORED CANDIDATE.
// These are the other half of the case above, and each one is a stale page's
// click: the row and the serving version it wants to move are identical to a
// publish, so what tells them apart has to be the write itself. Reading only the
// stored candidate would let every one of these through and pass the test above.
func TestAPromotionIsStillRefusedWithoutItsStoredCandidate(t *testing.T) {
	eng, db, ctx, account := candidateTestPerson(t, "cand-stale")

	for _, tc := range []struct {
		name     string
		slug     string
		arrange  func(candidateTestSite)
		promote  string
		wantCode string
		// serving and candidate are what the refused write must leave behind.
		serving, candidate string
	}{
		{
			name: "a deployable that never had a candidate", slug: "cand-never",
			arrange: func(candidateTestSite) {},
			promote: "v2", wantCode: PreviewRefusalNoCandidate,
			serving: "v1",
		},
		{
			name: "a candidate already withdrawn", slug: "cand-withdrawn",
			arrange: func(s candidateTestSite) {
				s.must("set", s.setCandidate("v2"))
				s.must("clear", s.clear())
			},
			promote: "v2", wantCode: PreviewRefusalNoCandidate,
			serving: "v1",
		},
		{
			name: "another version after the candidate was promoted", slug: "cand-promoted",
			arrange: func(s candidateTestSite) {
				s.must("set", s.setCandidate("v2"))
				s.must("promote", s.promote("v2"))
			},
			promote: "v3", wantCode: PreviewRefusalNoCandidate,
			serving: "v2",
		},
		{
			name: "a candidate republished since the page read it", slug: "cand-moved",
			arrange: func(s candidateTestSite) {
				s.must("set", s.setCandidate("v2"))
				s.must("republish", s.setCandidate("v3"))
			},
			promote: "v2", wantCode: PreviewRefusalCandidateMoved,
			serving: "v1", candidate: "v3",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := newCandidateTestSite(t, eng, db, ctx, account, tc.slug, "spa")
			tc.arrange(site)

			err := site.promote(tc.promote)
			if err == nil || !strings.Contains(err.Error(), tc.wantCode) {
				t.Fatalf("promoting %s: got %v, want a refusal carrying %s", tc.promote, err, tc.wantCode)
			}
			site.expectRow(tc.serving, tc.candidate)
		})
	}
}
