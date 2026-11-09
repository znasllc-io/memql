package siteaccess

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// store_test.go -- what the store makes of what a read hands back, and who it reads as,
// with no database. Everything about what the graph really answers is
// integration_db_test.go and app/access_surface_db_test.go.

// storeRead is one statement the store sent, and who it ran as.
type storeRead struct {
	query  string
	actor  string
	origin auth.CallOrigin
	fresh  bool
}

// storeFakeEngine answers every read with the rows it was given.
type storeFakeEngine struct {
	rows  []any
	err   error
	reads []storeRead
}

func (f *storeFakeEngine) Execute(ctx context.Context, query string) (any, error) {
	actor := ""
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		actor = ac.UserId
	}
	f.reads = append(f.reads, storeRead{query, actor, auth.OriginFromContext(ctx), memql.FreshReadFromContext(ctx)})
	if f.err != nil {
		return nil, f.err
	}
	return memql.NewResultWithOutput(f.rows), nil
}

// A STAMP IS SET WHEN ANYTHING AT ALL IS THERE. A removed code and an ended session each
// read as still good when the value is not the type the reader expected, so the failure
// this holds off is not a refusal but a door left open. rowTime already reads a stamp in
// both forms a read can carry it, and a shaped datetime arrives as a time.Time.
func TestAStampOfAnyKindEndsACodeAndASession(t *testing.T) {
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		value any
		set   bool
	}{
		"the stored text":      {"2026-10-30T12:00:00Z", true},
		"a time":               {at, true},
		"the zero time":        {time.Time{}, true},
		"a number":             {1793361600, true},
		"a float":              {1793361600.5, true},
		"a boolean":            {true, true},
		"an object":            {map[string]any{"at": "then"}, true},
		"text that is no time": {"sometime", true},
		"an empty string":      {"", false},
		"null":                 {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			row := func(field string) []any {
				return []any{map[string]any{"id": "x1", "siteId": "site1", "generation": 1, field: tc.value}}
			}
			ctx := context.Background()

			code, err := NewGraphStore(&storeFakeEngine{rows: row("revokedAt")}).CodeByID(ctx, "x1")
			if err != nil || code == nil {
				t.Fatalf("CodeByID = %+v, %v", code, err)
			}
			if code.Revoked != tc.set {
				t.Errorf("CodeByID: Revoked = %v for revokedAt = %#v", code.Revoked, tc.value)
			}

			code, err = NewGraphStore(&storeFakeEngine{rows: row("revokedAt")}).CodeByHash(ctx, "site1", "digest")
			if err != nil || code == nil {
				t.Fatalf("CodeByHash = %+v, %v", code, err)
			}
			if code.Revoked != tc.set {
				t.Errorf("CodeByHash: Revoked = %v for revokedAt = %#v", code.Revoked, tc.value)
			}

			session, err := NewGraphStore(&storeFakeEngine{rows: row("endedAt")}).SessionByTokenHash(ctx, "digest")
			if err != nil || session == nil {
				t.Fatalf("SessionByTokenHash = %+v, %v", session, err)
			}
			if session.Ended != tc.set {
				t.Errorf("SessionByTokenHash: Ended = %v for endedAt = %#v", session.Ended, tc.value)
			}

			// What an operator's list says agrees with what the bff does.
			view := codeFromRow(map[string]any{"revokedAt": tc.value})
			if (view.RevokedAt != "") != tc.set {
				t.Errorf("codeFromRow: RevokedAt = %q for revokedAt = %#v", view.RevokedAt, tc.value)
			}
		})
	}

	t.Run("an absent field is not set", func(t *testing.T) {
		rows := []any{map[string]any{"id": "x1", "siteId": "site1", "generation": 1}}
		code, err := NewGraphStore(&storeFakeEngine{rows: rows}).CodeByID(context.Background(), "x1")
		if err != nil || code == nil || code.Revoked {
			t.Fatalf("CodeByID = %+v, %v", code, err)
		}
		session, err := NewGraphStore(&storeFakeEngine{rows: rows}).SessionByTokenHash(context.Background(), "digest")
		if err != nil || session == nil || session.Ended {
			t.Fatalf("SessionByTokenHash = %+v, %v", session, err)
		}
	})

	t.Run("a time reads back in the form the list writes", func(t *testing.T) {
		view := codeFromRow(map[string]any{"revokedAt": at})
		if want := "2026-10-30T12:00:00Z"; view.RevokedAt != want {
			t.Fatalf("RevokedAt = %q, want %q", view.RevokedAt, want)
		}
	})
}

// requireClusterRead holds one statement to who the bff's own reads have to be: the
// cluster acting, with internal origin, and never answered from a cache (D11).
func requireClusterRead(t *testing.T, read storeRead, want string) {
	t.Helper()
	if !strings.Contains(read.query, want) {
		t.Errorf("query = %s, want %s", read.query, want)
	}
	if !auth.IsSystemActorId(read.actor) {
		t.Errorf("%s ran as %q: a row's owner must have no say in this read", read.query, read.actor)
	}
	if read.origin != auth.OriginInternal {
		t.Errorf("%s carries no internal origin", read.query)
	}
	if !read.fresh {
		t.Errorf("%s may be answered from the result cache, and nothing in the access decision is cached", read.query)
	}
}

func TestSiteForAccessIsAFreshPointReadAsTheCluster(t *testing.T) {
	engine := &storeFakeEngine{rows: []any{map[string]any{
		"id":                    "v1:platform:site:site1",
		"ownerUserId":           "v1:identity:user:owner1",
		"accountId":             "v1:accounts:account:acct1",
		"packageId":             "v1:platform:package:pkg1",
		"packageDeployableName": "  client-desk ",
		// A field the projection leaves out, so a handler cannot leak it.
		"bundleRef": "blob://secret",
	}}}

	site, err := NewGraphStore(engine).SiteForAccess(context.Background(), "site1")
	if err != nil {
		t.Fatal(err)
	}
	want := &Site{ID: "site1", OwnerUserID: "owner1", AccountID: "acct1", PackageID: "pkg1", DeployableName: "client-desk"}
	if !reflect.DeepEqual(site, want) {
		t.Fatalf("site = %+v, want %+v: ids bare, because the handler compares them", site, want)
	}
	if len(engine.reads) != 1 {
		t.Fatalf("reads = %+v, want one", engine.reads)
	}
	requireClusterRead(t, engine.reads[0], `query siteById(siteId: "site1")`)
}

func TestSiteForAccessMissesAndFailures(t *testing.T) {
	t.Run("no row is a miss", func(t *testing.T) {
		site, err := NewGraphStore(&storeFakeEngine{}).SiteForAccess(context.Background(), "site1")
		if err != nil || site != nil {
			t.Fatalf("site = %+v, err = %v", site, err)
		}
	})

	t.Run("a blank id reads nothing", func(t *testing.T) {
		engine := &storeFakeEngine{}
		site, err := NewGraphStore(engine).SiteForAccess(context.Background(), "   ")
		if err != nil || site != nil || len(engine.reads) != 0 {
			t.Fatalf("site = %+v, err = %v, reads = %+v", site, err, engine.reads)
		}
	})

	t.Run("a failed read is an error and not a miss", func(t *testing.T) {
		failure := errors.New("the read failed")
		site, err := NewGraphStore(&storeFakeEngine{err: failure}).SiteForAccess(context.Background(), "site1")
		if site != nil || !errors.Is(err, failure) {
			t.Fatalf("site = %+v, err = %v", site, err)
		}
	})
}

func TestPackageDomainsForAccessIsAFreshPointReadAsTheCluster(t *testing.T) {
	engine := &storeFakeEngine{rows: []any{map[string]any{"dslDomains": []any{"desk", "shared"}, "repoUrl": "secret"}}}

	domains, err := NewGraphStore(engine).PackageDomainsForAccess(context.Background(), "pkg1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"desk", "shared"}; !reflect.DeepEqual(domains, want) {
		t.Fatalf("domains = %v, want %v", domains, want)
	}
	if len(engine.reads) != 1 {
		t.Fatalf("reads = %+v, want one", engine.reads)
	}
	requireClusterRead(t, engine.reads[0], `query packageById(packageId: "pkg1")`)

	t.Run("no package id reads nothing", func(t *testing.T) {
		engine := &storeFakeEngine{}
		domains, err := NewGraphStore(engine).PackageDomainsForAccess(context.Background(), " ")
		if err != nil || domains != nil || len(engine.reads) != 0 {
			t.Fatalf("domains = %v, err = %v, reads = %+v", domains, err, engine.reads)
		}
	})

	t.Run("no row and no list are no domains", func(t *testing.T) {
		for _, rows := range [][]any{nil, {map[string]any{"name": "x"}}} {
			domains, err := NewGraphStore(&storeFakeEngine{rows: rows}).PackageDomainsForAccess(context.Background(), "pkg1")
			if err != nil || len(domains) != 0 {
				t.Fatalf("domains = %v, err = %v", domains, err)
			}
		}
	})

	t.Run("a failed read is an error and not an empty list", func(t *testing.T) {
		failure := errors.New("the read failed")
		domains, err := NewGraphStore(&storeFakeEngine{err: failure}).PackageDomainsForAccess(context.Background(), "pkg1")
		if domains != nil || !errors.Is(err, failure) {
			t.Fatalf("domains = %v, err = %v", domains, err)
		}
	})
}

// THE GUEST PASS IS THE ROW NOT REMOVED, among every row its site's guest digest finds. A
// site that turned guest access off and on again holds a row for each time, all with one
// digest, so the first row found is not the answer.
func TestLivePassForSiteIsTheGuestPassNotRemoved(t *testing.T) {
	row := func(id, site, issued string, guest any, revoked any) map[string]any {
		r := map[string]any{
			"id": "v1:platform:siteAccessCode:" + id, "siteId": "v1:platform:site:" + site, "gate": "dashboard",
			"generation": 1, "issuedAt": issued, "expiresAt": "2026-12-01T00:00:00Z", "guest": guest,
		}
		if revoked != nil {
			r["revokedAt"] = revoked
		}
		return r
	}
	removed := row("removed", "site1", "2026-11-01T00:00:00Z", true, "2026-11-02T00:00:00Z")
	live := row("live", "site1", "2026-11-03T00:00:00Z", true, nil)

	t.Run("the one not removed, whatever the order", func(t *testing.T) {
		for name, rows := range map[string][]any{
			"removed first": {removed, live},
			"removed last":  {live, removed},
		} {
			engine := &storeFakeEngine{rows: rows}
			pass, ok, err := NewGraphStore(engine).livePassForSite(context.Background(), "site1")
			if err != nil || !ok || pass.ID != "live" || !pass.Guest || pass.Revoked {
				t.Fatalf("%s: pass = %+v, ok = %v, err = %v; want the live pass", name, pass, ok, err)
			}
		}
	})

	t.Run("it reads the site's guest digest, as the cluster", func(t *testing.T) {
		engine := &storeFakeEngine{rows: []any{live}}
		if _, _, err := NewGraphStore(engine).livePassForSite(context.Background(), "v1:platform:site:site1"); err != nil {
			t.Fatal(err)
		}
		if len(engine.reads) != 1 {
			t.Fatalf("reads = %+v, want one", engine.reads)
		}
		read := engine.reads[0]
		for _, want := range []string{"query siteAccessCodeByHash(", `"site1"`, memql.AccessGuestPassDigest("site1")} {
			if !strings.Contains(read.query, want) {
				t.Errorf("query = %s, want it to carry %s", read.query, want)
			}
		}
		if strings.Contains(read.query, "v1:platform:site:") {
			t.Errorf("query = %s, want the bare site id the digest is of", read.query)
		}
		if !auth.IsSystemActorId(read.actor) || read.origin != auth.OriginInternal {
			t.Errorf("the read ran as %q with origin %v: a row's owner must have no say in it", read.actor, read.origin)
		}
	})

	t.Run("never another site's row, nor a row that is not a pass", func(t *testing.T) {
		engine := &storeFakeEngine{rows: []any{
			row("elsewhere", "site2", "2026-11-04T00:00:00Z", true, nil),
			row("code", "site1", "2026-11-05T00:00:00Z", false, nil),
			row("unmarked", "site1", "2026-11-06T00:00:00Z", nil, nil),
			removed,
		}}
		pass, ok, err := NewGraphStore(engine).livePassForSite(context.Background(), "site1")
		if err != nil || ok {
			t.Fatalf("pass = %+v, ok = %v, err = %v; want no pass", pass, ok, err)
		}
	})

	t.Run("against the invariant, the newest by issuedAt", func(t *testing.T) {
		newer := row("newer", "site1", "2026-11-07T00:00:00Z", true, nil)
		for name, rows := range map[string][]any{"older first": {live, newer}, "newer first": {newer, live}} {
			pass, ok, err := NewGraphStore(&storeFakeEngine{rows: rows}).livePassForSite(context.Background(), "site1")
			if err != nil || !ok || pass.ID != "newer" {
				t.Fatalf("%s: pass = %+v, ok = %v, err = %v; want the newer pass", name, pass, ok, err)
			}
		}
	})

	t.Run("no rows is no pass, and a failed read is an error and not a miss", func(t *testing.T) {
		if pass, ok, err := NewGraphStore(&storeFakeEngine{}).livePassForSite(context.Background(), "site1"); err != nil || ok {
			t.Fatalf("pass = %+v, ok = %v, err = %v", pass, ok, err)
		}
		failure := errors.New("the read failed")
		if _, ok, err := NewGraphStore(&storeFakeEngine{err: failure}).livePassForSite(context.Background(), "site1"); ok || !errors.Is(err, failure) {
			t.Fatalf("ok = %v, err = %v", ok, err)
		}
	})
}

// A ROW IS A GUEST PASS ONLY WHEN IT SAYS SO, and every row written before guest passes
// existed says nothing.
func TestARowSaysWhetherItIsAGuestPass(t *testing.T) {
	for name, tc := range map[string]struct {
		value any
		want  bool
	}{
		"true":          {true, true},
		"the text true": {"true", true},
		"false":         {false, false},
		"null":          {nil, false},
	} {
		if got := codeFromRow(map[string]any{"guest": tc.value}).Guest; got != tc.want {
			t.Errorf("%s: Guest = %v, want %v", name, got, tc.want)
		}
	}
	if codeFromRow(map[string]any{}).Guest {
		t.Error("a row with no guest field reads as a guest pass")
	}
}

// A GUEST PASS'S ID IS FIXED BY ITS SITE AND ITS ORDINAL, so two mints that read the same
// passes ask for the same row. It is a UUID, as every id here is, which the digest
// scrubber leaves alone.
func TestAGuestPassIDIsFixedBySiteAndOrdinal(t *testing.T) {
	first := guestPassID("site1", 1)
	if first != guestPassID("site1", 1) || first != guestPassID("v1:platform:site:site1", 1) {
		t.Errorf("two mints of a site's first pass ask for different ids")
	}
	for name, other := range map[string]string{
		"the second pass":       guestPassID("site1", 2),
		"another site's first":  guestPassID("site2", 1),
		"a site named like one": guestPassID("site", 11),
	} {
		if other == first {
			t.Errorf("%s has the first pass's id", name)
		}
	}
	if _, err := uuid.Parse(first); err != nil {
		t.Errorf("guestPassID = %q, which is not a UUID: %v", first, err)
	}
	if digestRun.MatchString(first) {
		t.Errorf("guestPassID = %q, which the scrubber would take for a digest", first)
	}
}

// BOTH LOOKUPS THAT OPEN A DOOR CARRY THE CODE'S NAME, the operator's label as the view
// holds it, because the handler stamps it on a gated mutation that declares
// accessCodeLabel: the unlock finds a code by its digest, and every call after it finds
// the session's code by id.
func TestTheCodeLookupsCarryTheCodesName(t *testing.T) {
	row := map[string]any{
		"id":         "v1:platform:siteAccessCode:code1",
		"siteId":     "v1:platform:site:site1",
		"gate":       "dashboard",
		"label":      "Client reviewers",
		"generation": 1,
		"expiresAt":  "2026-11-30T00:00:00Z",
	}
	for name, lookup := range map[string]func(*GraphStore) (*Code, error){
		"by its digest": func(s *GraphStore) (*Code, error) { return s.CodeByHash(context.Background(), "site1", "digest") },
		"by its id":     func(s *GraphStore) (*Code, error) { return s.CodeByID(context.Background(), "code1") },
	} {
		t.Run(name, func(t *testing.T) {
			code, err := lookup(NewGraphStore(&storeFakeEngine{rows: []any{row}}))
			if err != nil {
				t.Fatal(err)
			}
			if code == nil || code.Label != "Client reviewers" {
				t.Fatalf("code = %+v, want it to carry its name", code)
			}
		})
	}
}
