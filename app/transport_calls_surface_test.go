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

// transport_calls_surface_test.go -- what the mount and the adapter promise, with no
// database. What the graph really answers the site read, reached through the edge, is the
// edge half's end-to-end test; what the calls rows answer is integrations/calls'.

// callsFakeGraph answers the one read the adapter makes itself, and records who it ran as.
type callsFakeGraph struct {
	site  map[string]any
	err   error
	reads []accessRecordedRead
}

func (f *callsFakeGraph) Execute(ctx context.Context, query string) (any, error) {
	actor := ""
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		actor = ac.UserId
	}
	f.reads = append(f.reads, accessRecordedRead{query, actor, memql.FreshReadFromContext(ctx), auth.OriginFromContext(ctx)})
	if f.err != nil {
		return nil, f.err
	}
	if f.site == nil {
		return memql.NewResultWithOutput([]any{}), nil
	}
	return memql.NewResultWithOutput([]any{f.site}), nil
}

func callsFakeSiteRow() map[string]any {
	return map[string]any{
		"id":                    "v1:platform:site:site1",
		"ownerUserId":           "v1:identity:user:owner1",
		"accountId":             "v1:accounts:account:acct1",
		"packageDeployableName": "meetings",
		"calls":                 true,
		// Fields the projection leaves out, so a handler cannot leak them.
		"bundleRef": "blob://secret",
	}
}

// THE READ IS THE CHECK ON THE STAMP, so it runs as the stamped owner, unstamped, and
// answered by the database: the switch and the owner are what the decision rests on, and a
// sibling replica's eviction of a cached answer is asynchronous.
func TestCallsSiteIsReadFreshUnderTheNamedOwner(t *testing.T) {
	graph := &callsFakeGraph{site: callsFakeSiteRow()}
	surface := &callsSurface{engine: graph}

	site, err := surface.SiteAsOwner(context.Background(), "site1", "v1:identity:user:owner1")
	if err != nil {
		t.Fatal(err)
	}
	want := &server.CallsSite{ID: "site1", OwnerUserID: "owner1", DeployableName: "meetings", Calls: true}
	if !reflect.DeepEqual(site, want) {
		t.Fatalf("site = %+v, want %+v: ids bare, because the handler compares them", site, want)
	}
	if len(graph.reads) != 1 {
		t.Fatalf("reads = %+v, want the site and nothing else", graph.reads)
	}
	read := graph.reads[0]
	if !strings.Contains(read.query, `query siteById(siteId: "site1")`) {
		t.Fatalf("read = %s", read.query)
	}
	if read.actor != "owner1" {
		t.Errorf("the site was read as %q: read as anybody else it is not a check on the stamp", read.actor)
	}
	if !read.fresh {
		t.Error("the site may be answered from the result cache, and a switch turned off must count from the next request")
	}
	if read.origin == auth.OriginInternal {
		t.Error("the read carries internal origin: it IS the authorization check, and the stamp would be the engine's escape for trusted Go")
	}
}

func TestCallsSiteReadMisses(t *testing.T) {
	// ContextWithUserActor leaves a context unchanged for a blank id, so this read would
	// otherwise run as whoever the request happened to be.
	for name, tc := range map[string][2]string{"a blank owner": {"site1", "  "}, "a blank site": {"  ", "owner1"}} {
		t.Run(name, func(t *testing.T) {
			graph := &callsFakeGraph{site: callsFakeSiteRow()}
			site, err := (&callsSurface{engine: graph}).SiteAsOwner(context.Background(), tc[0], tc[1])
			if err != nil || site != nil || len(graph.reads) != 0 {
				t.Fatalf("site = %+v, err = %v, reads = %+v", site, err, graph.reads)
			}
		})
	}

	t.Run("a site the owner cannot read", func(t *testing.T) {
		site, err := (&callsSurface{engine: &callsFakeGraph{}}).SiteAsOwner(context.Background(), "site1", "owner1")
		if err != nil || site != nil {
			t.Fatalf("site = %+v, err = %v", site, err)
		}
	})

	// ABSENT IS OFF, the concept's own rule: a row written before the switch existed has no
	// calls surface.
	t.Run("a row with no switch is off", func(t *testing.T) {
		row := callsFakeSiteRow()
		delete(row, "calls")
		site, err := (&callsSurface{engine: &callsFakeGraph{site: row}}).SiteAsOwner(context.Background(), "site1", "owner1")
		if err != nil || site == nil || site.Calls {
			t.Fatalf("site = %+v, err = %v", site, err)
		}
	})

	t.Run("a failed read is an error and not a miss", func(t *testing.T) {
		failure := errors.New("the site read failed")
		site, err := (&callsSurface{engine: &callsFakeGraph{err: failure}}).SiteAsOwner(context.Background(), "site1", "owner1")
		if site != nil || !errors.Is(err, failure) {
			t.Fatalf("site = %+v, err = %v", site, err)
		}
	})
}

// THE ROUTE IS MOUNTED BY ONE CALL ON ONE NODE, and nothing else proves it: every test here
// drives mountCallsSurface itself, so a transportBFF that lost the line would serve no call
// with the whole suite green. It also has to run ahead of createHTTPServer, or
// requireUnsealedSurface refuses the boot.
func TestTheBFFMountsTheCallsSurface(t *testing.T) {
	source, err := os.ReadFile("transport_bff.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	mount, seal := strings.Index(text, "a.mountCallsSurface()"), strings.Index(text, "a.createHTTPServer()")
	if mount < 0 {
		t.Fatal("transportBFF does not mount the calls surface")
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
		if strings.Contains(string(body), ".mountCallsSurface()") {
			t.Errorf("%s mounts the calls surface; it belongs on the bff and no internal node should carry it", file)
		}
	}
}

// A declared-but-unserved route, and a served-but-undeclared one, are the two ways the
// surface and its declaration drift, and this is the boot assertion looking at exactly what
// the mount registers.
func TestCallsRoutesSurviveTheUnauthenticatedSurfaceAssertion(t *testing.T) {
	t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
	a := &App{mux: http.NewServeMux(), Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}

	a.mountCallsSurface()

	if want := []string{"GET /calls/", "POST /calls/"}; !reflect.DeepEqual(a.registeredRoutes, want) {
		t.Fatalf("registered = %v, want %v", a.registeredRoutes, want)
	}
	if err := server.AssertUnauthenticatedSurfaceDeclared(a.unauthenticatedSurfaceRoutes(true)); err != nil {
		t.Fatalf("the calls surface must be declared in HandlerAuthorizedPaths(): %v", err)
	}
	if err := server.AssertSelfAuthenticatedRoutesFailClosed(); err != nil {
		t.Fatalf("the verifier steps aside for a route nothing certifies: %v", err)
	}
}

// A GET to a route that only takes POST is the handler's 404 and not the mux's 405, for the
// reason written on the mount. A node whose calls integration is missing mounts the routes
// anyway and answers the one 404, and says why at boot.
func TestTheMountLetsTheCallsHandlerAnswerEveryMethod(t *testing.T) {
	t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
	logs := &bytes.Buffer{}
	a := &App{mux: http.NewServeMux(), Logger: slog.New(slog.NewTextHandler(logs, nil))}
	a.mountCallsSurface()
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "calls surface") {
		t.Fatalf("a node with no calls integration said nothing at boot:\n%s", logs.String())
	}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/calls/unlock"},
		{http.MethodPost, "/calls/session"},
		{http.MethodGet, "/calls/nope"},
		{http.MethodPost, "/calls/nope"},
		{http.MethodPost, "/calls/join"},
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
