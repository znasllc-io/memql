package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
)

// transport_access_surface_test.go -- what the mount and the adapter promise, with
// no database. What the graph really answers them is access_surface_db_test.go.

// accessRecordedRead is one statement the adapter sent, and who it ran as.
type accessRecordedRead struct {
	query  string
	actor  string
	fresh  bool
	origin auth.CallOrigin
}

// accessFakeGraph answers the two reads the adapter makes itself.
type accessFakeGraph struct {
	site, pkg       map[string]any
	siteErr, pkgErr error
	reads           []accessRecordedRead
}

func (f *accessFakeGraph) Execute(ctx context.Context, query string) (any, error) {
	actor := ""
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		actor = ac.UserId
	}
	f.reads = append(f.reads, accessRecordedRead{query, actor, memql.FreshReadFromContext(ctx), auth.OriginFromContext(ctx)})
	row, err := f.pkg, f.pkgErr
	if strings.Contains(query, "siteById") {
		row, err = f.site, f.siteErr
	}
	if err != nil {
		return nil, err
	}
	if row == nil {
		return memql.NewResultWithOutput([]any{}), nil
	}
	return memql.NewResultWithOutput([]any{row}), nil
}

func accessFakeSiteRow() map[string]any {
	return map[string]any{
		"id":                    "v1:platform:site:site1",
		"ownerUserId":           "v1:identity:user:owner1",
		"accountId":             "v1:accounts:account:acct1",
		"packageId":             "v1:platform:package:pkg1",
		"packageDeployableName": "client-desk",
		// Fields the projection leaves out, so a handler cannot leak them.
		"bundleRef": "blob://secret",
	}
}

func TestSiteAsOwnerIsReadFreshUnderTheNamedOwner(t *testing.T) {
	graph := &accessFakeGraph{site: accessFakeSiteRow(), pkg: map[string]any{"dslDomains": []any{"desk", "shared"}}}
	store := newAccessStore(graph)

	site, err := store.SiteAsOwner(context.Background(), "site1", "v1:identity:user:owner1")
	if err != nil {
		t.Fatal(err)
	}
	want := &server.AccessSite{
		ID: "site1", OwnerUserID: "owner1", AccountID: "acct1", PackageID: "pkg1", DeployableName: "client-desk",
		PackageDomains: []string{"desk", "shared"},
	}
	if !reflect.DeepEqual(site, want) {
		t.Fatalf("site = %+v, want %+v: ids bare, because the handler compares them", site, want)
	}
	if len(graph.reads) != 2 {
		t.Fatalf("reads = %+v, want the site and its package", graph.reads)
	}
	for _, read := range graph.reads {
		if read.actor != "owner1" {
			t.Errorf("%s ran as %q: read as anybody else it is not a check on the stamp", read.query, read.actor)
		}
		if !read.fresh {
			t.Errorf("%s may be answered from the result cache, and nothing in the access decision is cached", read.query)
		}
		if read.origin == auth.OriginInternal {
			t.Errorf("%s carries internal origin: this read IS the authorization check, and a stamp would be the engine's escape for trusted Go", read.query)
		}
	}
	if !strings.Contains(graph.reads[0].query, `query siteById(siteId: "site1")`) {
		t.Fatalf("first read = %s", graph.reads[0].query)
	}
	if !strings.Contains(graph.reads[1].query, `packageId: "pkg1"`) {
		t.Fatalf("second read = %s", graph.reads[1].query)
	}
}

func TestSiteAsOwnerMisses(t *testing.T) {
	t.Run("a site the owner cannot read", func(t *testing.T) {
		graph := &accessFakeGraph{}
		site, err := newAccessStore(graph).SiteAsOwner(context.Background(), "site1", "owner1")
		if err != nil || site != nil {
			t.Fatalf("site = %+v, err = %v", site, err)
		}
	})

	// A SITE WITH NO PACKAGE DEPLOYABLE IS READ, AND IT IS NOT A MISS. It has no codes to
	// open, and the handler answers it with the one 404, but a miss is what the handler
	// logs as a site its owner cannot read, and this is not that. Its package has nothing
	// to say, so that read is not made.
	for name, value := range map[string]any{"no package deployable": nil, "a blank one": "   "} {
		t.Run(name, func(t *testing.T) {
			row := accessFakeSiteRow()
			if value == nil {
				delete(row, "packageDeployableName")
			} else {
				row["packageDeployableName"] = value
			}
			graph := &accessFakeGraph{site: row, pkg: map[string]any{"dslDomains": []any{"desk"}}}
			site, err := newAccessStore(graph).SiteAsOwner(context.Background(), "site1", "owner1")
			if err != nil || site == nil {
				t.Fatalf("site = %+v, err = %v", site, err)
			}
			if site.DeployableName != "" || len(site.PackageDomains) != 0 || site.OwnerUserID != "owner1" {
				t.Fatalf("site = %+v", site)
			}
			if len(graph.reads) != 1 {
				t.Fatalf("reads = %+v", graph.reads)
			}
		})
	}

	// ContextWithUserActor leaves a context unchanged for a blank id, so this read
	// would otherwise run as whoever the request happened to be.
	for name, tc := range map[string][2]string{"a blank owner": {"site1", "  "}, "a blank site": {"  ", "owner1"}} {
		t.Run(name, func(t *testing.T) {
			graph := &accessFakeGraph{site: accessFakeSiteRow()}
			site, err := newAccessStore(graph).SiteAsOwner(context.Background(), tc[0], tc[1])
			if err != nil || site != nil || len(graph.reads) != 0 {
				t.Fatalf("site = %+v, err = %v, reads = %+v", site, err, graph.reads)
			}
		})
	}

	t.Run("a failed read of the site is an error and not a miss", func(t *testing.T) {
		failure := errors.New("the site read failed")
		graph := &accessFakeGraph{siteErr: failure}
		site, err := newAccessStore(graph).SiteAsOwner(context.Background(), "site1", "owner1")
		if site != nil || !errors.Is(err, failure) {
			t.Fatalf("site = %+v, err = %v", site, err)
		}
	})
}

// A FAILED READ IS AN ERROR AND NEVER LOOKS LIKE A MISS. This read is made on the way to a
// construct and nothing else in the request is left to answer, so the handler refuses the
// call with a retryable 502 rather than saying a package has no domains, which would be a
// 404 that says nothing about why.
func TestAFailedPackageReadUnderTheOwnerIsAnError(t *testing.T) {
	failure := errors.New("the package read failed")
	graph := &accessFakeGraph{site: accessFakeSiteRow(), pkgErr: failure}

	site, err := newAccessStore(graph).SiteAsOwner(context.Background(), "site1", "owner1")

	if site != nil || !errors.Is(err, failure) {
		t.Fatalf("site = %+v, err = %v", site, err)
	}
}

// THE READS THAT DECIDE MOST REQUESTS ARE THE CLUSTER'S, one point read each, and what
// they cost a status check is the difference between a poll and a load problem. They go
// through integrations/siteaccess, which is allowed to stamp internal origin and this
// package is not, and they say nothing about who may do what: the handler compares.
func TestSiteAndPackageAreReadFreshAsTheCluster(t *testing.T) {
	t.Run("the site", func(t *testing.T) {
		graph := &accessFakeGraph{site: accessFakeSiteRow()}
		site, err := newAccessStore(graph).SiteAsCluster(context.Background(), "site1")
		if err != nil {
			t.Fatal(err)
		}
		want := &server.AccessSite{
			ID: "site1", OwnerUserID: "owner1", AccountID: "acct1", PackageID: "pkg1", DeployableName: "client-desk",
		}
		if !reflect.DeepEqual(site, want) {
			t.Fatalf("site = %+v, want %+v", site, want)
		}
		if len(graph.reads) != 1 {
			t.Fatalf("reads = %+v, want the site and nothing else", graph.reads)
		}
		read := graph.reads[0]
		if !strings.Contains(read.query, `query siteById(siteId: "site1")`) {
			t.Fatalf("read = %s", read.query)
		}
		if !auth.IsSystemActorId(read.actor) || read.origin != auth.OriginInternal || !read.fresh {
			t.Fatalf("read = %+v, want a fresh read as the cluster acting", read)
		}
	})

	t.Run("the package", func(t *testing.T) {
		graph := &accessFakeGraph{pkg: map[string]any{"dslDomains": []any{"desk", "shared"}}}
		domains, err := newAccessStore(graph).PackageDomainsAsCluster(context.Background(), "pkg1")
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"desk", "shared"}; !reflect.DeepEqual(domains, want) {
			t.Fatalf("domains = %v, want %v", domains, want)
		}
		if len(graph.reads) != 1 {
			t.Fatalf("reads = %+v", graph.reads)
		}
		read := graph.reads[0]
		if !strings.Contains(read.query, `packageId: "pkg1"`) {
			t.Fatalf("read = %s", read.query)
		}
		if !auth.IsSystemActorId(read.actor) || read.origin != auth.OriginInternal || !read.fresh {
			t.Fatalf("read = %+v, want a fresh read as the cluster acting", read)
		}
	})

	t.Run("a site with no row is a miss, and a failed read is not", func(t *testing.T) {
		site, err := newAccessStore(&accessFakeGraph{}).SiteAsCluster(context.Background(), "site1")
		if err != nil || site != nil {
			t.Fatalf("site = %+v, err = %v", site, err)
		}
		failure := errors.New("the site read failed")
		site, err = newAccessStore(&accessFakeGraph{siteErr: failure}).SiteAsCluster(context.Background(), "site1")
		if site != nil || !errors.Is(err, failure) {
			t.Fatalf("site = %+v, err = %v", site, err)
		}
	})
}

func TestSiteAsOwnerWithNoPackageOrNoListHasNoDomains(t *testing.T) {
	t.Run("a site with no package id", func(t *testing.T) {
		row := accessFakeSiteRow()
		delete(row, "packageId")
		graph := &accessFakeGraph{site: row}
		site, err := newAccessStore(graph).SiteAsOwner(context.Background(), "site1", "owner1")
		if err != nil || site == nil || len(site.PackageDomains) != 0 || len(graph.reads) != 1 {
			t.Fatalf("site = %+v, err = %v, reads = %+v", site, err, graph.reads)
		}
	})
	t.Run("a package the owner cannot read", func(t *testing.T) {
		graph := &accessFakeGraph{site: accessFakeSiteRow()}
		site, err := newAccessStore(graph).SiteAsOwner(context.Background(), "site1", "owner1")
		if err != nil || site == nil || len(site.PackageDomains) != 0 {
			t.Fatalf("site = %+v, err = %v", site, err)
		}
	})
	t.Run("a package with no list", func(t *testing.T) {
		graph := &accessFakeGraph{site: accessFakeSiteRow(), pkg: map[string]any{"name": "x"}}
		site, err := newAccessStore(graph).SiteAsOwner(context.Background(), "site1", "owner1")
		if err != nil || site == nil || len(site.PackageDomains) != 0 {
			t.Fatalf("site = %+v, err = %v", site, err)
		}
	})
}

// THE ROUTE IS MOUNTED BY ONE CALL ON ONE NODE, and nothing else proves it: every
// test here drives mountAccessSurface itself, so a transportBFF that lost the line
// would serve no visitor with the whole suite green. It also has to run ahead of
// createHTTPServer, or requireUnsealedSurface refuses the boot.
func TestTheBFFMountsTheAccessSurface(t *testing.T) {
	source, err := os.ReadFile("transport_bff.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	mount, seal := strings.Index(text, "a.mountAccessSurface()"), strings.Index(text, "a.createHTTPServer()")
	if mount < 0 {
		t.Fatal("transportBFF does not mount the access surface")
	}
	if seal < 0 || mount > seal {
		t.Fatal("the mount must run before createHTTPServer seals the surface")
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file == "transport_bff.go" || strings.HasSuffix(file, "_test.go") {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), ".mountAccessSurface()") {
			t.Errorf("%s mounts the access surface; it belongs on the bff and no internal node should carry it", file)
		}
	}
}

// A declared-but-unserved route, and a served-but-undeclared one, are the two ways
// the surface and its declaration drift, and this is the boot assertion looking at
// exactly what the mount registers.
func TestAccessRoutesSurviveTheUnauthenticatedSurfaceAssertion(t *testing.T) {
	t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
	a := &App{mux: http.NewServeMux(), Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}

	a.mountAccessSurface()

	if want := []string{"GET /access/", "POST /access/"}; !reflect.DeepEqual(a.registeredRoutes, want) {
		t.Fatalf("registered = %v, want %v", a.registeredRoutes, want)
	}
	if err := server.AssertUnauthenticatedSurfaceDeclared(a.unauthenticatedSurfaceRoutes(true)); err != nil {
		t.Fatalf("the access surface must be declared in HandlerAuthorizedPaths(): %v", err)
	}
	if err := server.AssertSelfAuthenticatedRoutesFailClosed(); err != nil {
		t.Fatalf("the verifier steps aside for a route nothing certifies: %v", err)
	}
}

// A GET to a route that only takes POST is the handler's 404 and not the mux's 405,
// for the reason written on the mount.
func TestTheMountLetsTheHandlerAnswerEveryMethod(t *testing.T) {
	t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
	a := &App{mux: http.NewServeMux(), Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	a.mountAccessSurface()

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/access/unlock"},
		{http.MethodPost, "/access/session"},
		{http.MethodGet, "/access/nope"},
		{http.MethodPost, "/access/nope"},
	} {
		rec := httptest.NewRecorder()
		a.mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusNotFound || rec.Body.String() != `{"error":"not_found"}` {
			t.Errorf("%s %s = %d %q", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s %s: Cache-Control = %q, so this was not the handler's answer", tc.method, tc.path, got)
		}
	}
}
