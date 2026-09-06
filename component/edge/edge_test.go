// component/edge/edge_test.go
package edge

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// fakeEngine is the concrete-executor counterpart to stubExec: it stands in
// for the live engine's Execute(ctx, query string) (any, error) seam and
// records what it was called with, so a test can inspect the ACTOR the
// query ran under as well as the rendered query string.
type fakeEngine struct {
	gotCtx   context.Context
	gotQuery string
	rows     []map[string]any
	err      error
}

func (f *fakeEngine) Execute(ctx context.Context, query string) (any, error) {
	f.gotCtx = ctx
	f.gotQuery = query
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// The edge is a service, not a user: v1:platform:site declares
// @rowAuthz(owner="ownerUserId", clusterOwner) -- the COMPOSITE tier
// (memql#4344) -- and siteByHostname's own filter carries that tier's
// predicate written out, `ownerUserId==actor.userId || actor.isClusterOwner`.
// The edge owns no site row, so the only branch that can admit it is the
// cluster-owner one. Without a synthetic cluster-owner actor on the ctx the
// engine hands to Execute, every real lookup is refused twice over -- once by
// the declared tier, once by the textual conjunct -- and every hosted site
// 404s in a real cluster while this exact test, run against stubExec, would
// keep passing. That is the false-signal class this test exists to close off.
func TestEngineExecutorRunsUnderASyntheticClusterOwnerActor(t *testing.T) {
	fe := &fakeEngine{rows: []map[string]any{
		{"id": "v1:platform:site:abc123", "hostname": "shop.example.com", "status": "live"},
	}}
	ex := NewEngineExecutor(fe)

	if _, err := ex.SiteByHostname(context.Background(), "shop.example.com"); err != nil {
		t.Fatalf("SiteByHostname: %v", err)
	}

	ac, ok := auth.AccessFromContext(fe.gotCtx)
	if !ok || ac == nil {
		t.Fatalf("engine.Execute ran with no AccessContext on ctx; the engine will refuse the clusterOwner-tier read")
	}
	if !ac.IsClusterOwner() {
		t.Errorf("engine.Execute ran as role %q, want a cluster owner -- siteByHostname's actor.isClusterOwner==true conjunct will refuse this actor", ac.Role)
	}
}

// SiteByHostname projects the engine's row onto Site, bare-ifying the id the
// way every other named-query reader in the tree does.
func TestEngineExecutorSiteByHostnameProjectsTheRow(t *testing.T) {
	fe := &fakeEngine{rows: []map[string]any{{
		"id":          "v1:platform:site:abc123",
		"hostname":    "shop.example.com",
		"kind":        "spa",
		"bundleRef":   "file:///app/os",
		"status":      "live",
		"title":       "Shop",
		"apiProxy":    true,
		"systemOwned": true,
	}}}
	ex := NewEngineExecutor(fe)

	got, err := ex.SiteByHostname(context.Background(), "shop.example.com")
	if err != nil {
		t.Fatalf("SiteByHostname: %v", err)
	}
	want := &Site{
		ID: "abc123", Hostname: "shop.example.com", Kind: "spa",
		BundleRef: "file:///app/os", Status: "live", Title: "Shop",
		APIProxy: true, SystemOwned: true,
		// EMPTY, NOT NIL, and the difference is the assertion: a row with no
		// `settings` projects an empty map (epic memql#4906) so the runtime-
		// config document always carries the key. Both print as `map[]` under
		// %+v, so a failure here reads as "want X, got X" -- which is what
		// this line exists to stop somebody hunting for.
		Settings: map[string]string{},
	}
	// reflect.DeepEqual rather than *got != *want: Site carries the row's
	// `binding` object as a map now (memql#4345), and a struct holding a map
	// is not comparable with ==.
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SiteByHostname returned %+v, want %+v", got, want)
	}
}

// A miss is (nil, nil), matching the QueryExecutor contract stubExec's tests
// already pin -- the concrete implementation must not turn "no such site"
// into an error just because it is the one actually talking to the engine.
func TestEngineExecutorSiteByHostnameMissReturnsNilWithoutError(t *testing.T) {
	ex := NewEngineExecutor(&fakeEngine{rows: nil})

	got, err := ex.SiteByHostname(context.Background(), "nope.example.com")
	if err != nil {
		t.Fatalf("SiteByHostname returned an error for a miss: %v", err)
	}
	if got != nil {
		t.Fatalf("SiteByHostname returned %+v for a miss, want nil", got)
	}
}

// An engine failure (the query itself erroring, as opposed to a clean zero
// rows) must surface as an error rather than be swallowed into a miss --
// those are different conditions for a caller deciding between a 404 and a
// 500.
func TestEngineExecutorSiteByHostnameSurfacesEngineError(t *testing.T) {
	ex := NewEngineExecutor(&fakeEngine{err: errors.New("boom")})

	_, err := ex.SiteByHostname(context.Background(), "shop.example.com")
	if err == nil {
		t.Fatal("SiteByHostname swallowed an engine error")
	}
}

// The hostname reaches the engine as a query argument, not interpolated
// unescaped into the statement -- a hostname containing a quote must not be
// able to break out of the argument position.
func TestEngineExecutorSiteByHostnameQuotesTheArgument(t *testing.T) {
	fe := &fakeEngine{rows: nil}
	ex := NewEngineExecutor(fe)

	if _, err := ex.SiteByHostname(context.Background(), `shop".example.com`); err != nil {
		t.Fatalf("SiteByHostname: %v", err)
	}
	if !strings.Contains(fe.gotQuery, "siteByHostname") || !strings.Contains(fe.gotQuery, `\"`) {
		t.Errorf("query %q does not look like an escaped siteByHostname call", fe.gotQuery)
	}
}
