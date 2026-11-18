package app

import (
	"context"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/calls"
)

// transport_calls_surface.go wires a deployable's CALLS SURFACE onto the bff's mux, and
// adapts the calls integration's service to the handler (component/server
// calls_handler.go). The owner approved this HTTP route family on 2026-11-17.
//
// THE BFF ONLY, for the access surface's reason: this is the node the edge forwards to, and
// the caller is a person with a key, a call's link or the session one of them opened, and no
// MemQL identity. No internal node should carry it. Only transportBFF calls
// mountCallsSurface, which TestTheBFFMountsTheCallsSurface pins.
//
// NO BUILD TAG ON THIS FILE, for transport_access_surface.go's reason: the db-gated lane
// compiles this package with no tags, and an end-to-end test through the edge can only run
// against an adapter that compiles there.
//
// IT ALWAYS MOUNTS. A deployable whose calls switch is off answers every request the way it
// answers a stranger, and a node with no calls integration answers the one 404 too, said
// once at boot, so "is calls on here" and "does the route exist" are never two questions
// with one answer.

func (a *App) mountCallsSurface() {
	handler := server.NewCallsHandler(a.callsSurface(), a.Logger, time.Now)
	for _, path := range server.CallsSurfacePaths() {
		// Both methods on the one base: the handler decides which is legal for which, so a
		// GET to /calls/join is a 404 from the handler rather than a 405 from the mux. One
		// answer for "there is nothing here" beats two a prober can tell apart.
		a.handleRoute("GET "+path, handler)
		a.handleRoute("POST "+path, handler)
	}
}

// callsSurface is the calls service in the handler's terms, or nil for a node with no calls
// integration. The integration registers on every node type (integrations/calls plugin.go),
// so a bff without it is a broken build rather than a configuration: said at ERROR, and the
// node still boots and serves everything else.
func (a *App) callsSurface() server.CallsService {
	integration, ok := a.engine.IntegrationByName("calls").(*calls.Integration)
	if !ok || integration.Service() == nil {
		a.Logger.Error("calls surface: this node has no calls integration, so every calls route answers 404",
			"component", memql.ComponentName)
		return nil
	}
	return &callsSurface{Service: integration.Service(), engine: &callsEngineAdapter{engine: a.engine}}
}

// callsSurface adapts integrations/calls to server.CallsService.
//
// THE SERVICE IS EMBEDDED, so its acts are the handler's acts with nothing in between: the
// two speak component/calls' types, and a signature that drifted on either side stops this
// compiling rather than adapting around it. The one method it adds is the read the handler
// makes itself.
type callsSurface struct {
	*calls.Service
	engine server.MemQLExecutor
}

var _ server.CallsService = (*callsSurface)(nil)

// callsEngineAdapter narrows the engine to the one call the site read makes. Its own, for
// AccessEngineAdapter's reason: this is reached by somebody who is nobody to MemQL.
type callsEngineAdapter struct {
	engine *memql.MemQLEngine
}

func (a *callsEngineAdapter) Execute(ctx context.Context, query string) (any, error) {
	return a.engine.Execute(ctx, query)
}

// SiteAsOwner resolves a site UNDER the named owner's own actor, as shopperSiteReader does:
// v1:platform:site is composite-owner tier, so somebody who does not own it, and is not a
// member of its organization, reads no row. A miss is (nil, nil).
//
// UNSTAMPED, because this read IS the check on the edge's stamp: internal origin would be
// the engine's escape for trusted Go, and would let the stamp name anybody.
//
// FRESH, because the switch and the owner are what the decision rests on, and a sibling
// replica's eviction of a cached answer is asynchronous: a switch turned off, or a site
// handed to a new owner, has to count from the next request rather than when an entry ages
// out. THE ROW AND NOTHING ELSE: what the row is related to adds nothing to the decision.
func (s *callsSurface) SiteAsOwner(ctx context.Context, siteID, owner string) (*server.CallsSite, error) {
	siteID, owner = memql.BareShortId(strings.TrimSpace(siteID)), memql.BareShortId(strings.TrimSpace(owner))
	if siteID == "" || owner == "" || s.engine == nil {
		// A blank owner returns ctx unchanged from ContextWithUserActor, so this read would
		// otherwise run as whoever the request happened to be.
		return nil, nil
	}
	call, err := langparser.RenderCall("siteById", map[string]any{"siteId": siteID})
	if err != nil {
		return nil, err
	}
	asOwner := memql.ContextWithRootsOnlyRead(memql.ContextWithFreshRead(auth.ContextWithUserActor(ctx, owner)))
	result, err := s.engine.Execute(asOwner, "query "+call)
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(result)
	if len(rows) == 0 {
		// NOT AN ERROR. A site this user cannot read gets what a stranger gets.
		return nil, nil
	}
	return callsSiteFromRow(rows[0]), nil
}

// callsSiteFromRow projects a site row onto the four fields the handler decides on, and
// nothing else of it. Ids bare, because the handler compares them and hands them on.
func callsSiteFromRow(row map[string]any) *server.CallsSite {
	deployable, _ := row["packageDeployableName"].(string)
	// ABSENT IS OFF, the concept's own rule, and so is anything but a boolean: a row written
	// before the switch existed has no calls surface.
	on, _ := row["calls"].(bool)
	return &server.CallsSite{
		ID:             accessBareField(row, "id"),
		OwnerUserID:    accessBareField(row, "ownerUserId"),
		DeployableName: strings.TrimSpace(deployable),
		Calls:          on,
	}
}
