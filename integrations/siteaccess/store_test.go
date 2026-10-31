package siteaccess

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

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
