package app

import (
	"context"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/siteaccess"
)

// transport_access_surface.go wires a deployable's ACCESS SURFACE onto the bff's mux
// and adapts the graph to what the handler needs (design record
// 2026-10-29-deployable-access-codes, section 6).
//
// THE BFF ONLY, for the reason the shopper surface is: this is the node the edge
// forwards to, and the caller is a visitor with a code and no MemQL identity. No
// internal node should carry it. Only transportBFF calls mountAccessSurface.
//
// NO BUILD TAG ON THIS FILE, unlike transport_shopper_surface.go, and the reason is
// where the test of it can run. The db-gated lane compiles this package with no tags,
// and the bff-tagged lane has no database, so a tagged adapter could only ever be
// tested against a real engine on somebody's own machine. What keeps it off the other
// nodes is the one call, which is what TestTheBFFMountsTheAccessSurface pins.
//
// IT ALWAYS MOUNTS, even on a cluster with no gate declared: the reason on
// mountShopperSurface exactly, and a deployable with no codes answers every call the
// way it answers a stranger.

func (a *App) mountAccessSurface() {
	engine := &AccessEngineAdapter{Engine: a.engine}
	handler := server.NewAccessHandler(engine, newAccessStore(engine), a.Logger, time.Now)
	for _, path := range server.AccessSurfacePaths() {
		// Both methods on the one base: the handler decides which is legal for which, so
		// a GET to /access/unlock is a 404 from the handler rather than a 405 from the
		// mux. One answer for "there is nothing here" beats two a prober can tell apart.
		a.handleRoute("GET "+path, handler)
		a.handleRoute("POST "+path, handler)
	}
}

// AccessEngineAdapter wraps MemQLEngine to satisfy server.MemQLExecutor for the access
// surface, and siteaccess.Engine for the store under it.
//
// ITS OWN ADAPTER, for ShopperEngineAdapter's reason: this is reached by somebody who
// is nobody to MemQL, and a shared adapter would make narrowing it later a change to
// everybody's path.
type AccessEngineAdapter struct {
	Engine *memql.MemQLEngine
}

func (a *AccessEngineAdapter) Execute(ctx context.Context, query string) (any, error) {
	return a.Engine.Execute(ctx, query)
}

// accessStore is the graph, in the handler's terms.
//
// Everything but one read goes through integrations/siteaccess, which reads and updates
// the rows as the cluster acting, with its own internal origin: component/server may not
// stamp one and may not import integrations/, code and session rows answer nothing to a
// client (D14), and a row's current owner must not decide whether a code opens. Its
// errors are scrubbed of digests before they get here, which is what AccessStore asks of
// them.
//
// The read under the owner is made here, UNSTAMPED, and that is the difference between
// the two halves. A read as that person is what says they can still see what a call is
// about to lend, and it costs what resolving them costs, so the handler asks it only just
// before a construct runs.
type accessStore struct {
	engine server.MemQLExecutor
	graph  *siteaccess.GraphStore
}

func newAccessStore(engine server.MemQLExecutor) *accessStore {
	return &accessStore{engine: engine, graph: siteaccess.NewGraphStore(engine)}
}

var _ server.AccessStore = (*accessStore)(nil)

// SiteAsCluster is the site as the cluster reads it. A conversion rather than a copy
// field by field, so a field added to one side and not the other stops this compiling,
// with the one field only the read under the owner fills left empty.
func (s *accessStore) SiteAsCluster(ctx context.Context, siteID string) (*server.AccessSite, error) {
	site, err := s.graph.SiteForAccess(ctx, siteID)
	if site == nil {
		return nil, err
	}
	return &server.AccessSite{
		ID: site.ID, OwnerUserID: site.OwnerUserID, AccountID: site.AccountID,
		PackageID: site.PackageID, DeployableName: site.DeployableName,
	}, err
}

// PackageDomainsAsCluster is the DSL domains a package ships, as the cluster reads them.
func (s *accessStore) PackageDomainsAsCluster(ctx context.Context, packageID string) ([]string, error) {
	return s.graph.PackageDomainsForAccess(ctx, packageID)
}

// SiteAsOwner resolves a site UNDER the named owner's own actor, as shopperSiteReader
// does. A miss is (nil, nil). A site with no package deployable is not one: it was read,
// it has no gates to open, and it comes back with a blank name.
//
// FRESH, both reads, because nothing in the access decision is cached (D11). siteById
// and packageById are ordinary cached reads, and the handler compares what comes back:
// a move of the site's organization, a change of owner, or a package's domains changing
// on a deploy has to count from the next request, not from when the entry ages out. THE
// ROWS AND NOTHING ELSE, for the reason on the store's own reads: what the owner's
// authority costs is who they are, and what the row is related to adds nothing to that.
func (s *accessStore) SiteAsOwner(ctx context.Context, siteID, owner string) (*server.AccessSite, error) {
	siteID, owner = strings.TrimSpace(siteID), memql.BareShortId(owner)
	if siteID == "" || owner == "" {
		return nil, nil
	}
	// A blank owner returns ctx unchanged from ContextWithUserActor, which is why it was
	// refused above: this read must never run as whoever the request happened to be.
	asOwner := memql.ContextWithRootsOnlyRead(memql.ContextWithFreshRead(auth.ContextWithUserActor(ctx, owner)))
	rows, err := s.read(asOwner, "siteById", map[string]any{"siteId": siteID})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	row := rows[0]
	deployable, _ := row["packageDeployableName"].(string)
	site := &server.AccessSite{
		ID:             accessBareField(row, "id"),
		OwnerUserID:    accessBareField(row, "ownerUserId"),
		AccountID:      accessBareField(row, "accountId"),
		PackageID:      accessBareField(row, "packageId"),
		DeployableName: strings.TrimSpace(deployable),
	}
	if site.DeployableName != "" {
		if site.PackageDomains, err = s.packageDomains(asOwner, site.PackageID); err != nil {
			return nil, err
		}
	}
	return site, nil
}

// packageDomains reads the DSL domains a site's package ships, under whoever asOwner is.
//
// A package the owner cannot read and one with no list are the same answer, no domains,
// which reaches no gate: the handler has already seen the list as the cluster, so here it
// means the owner cannot see it. A failed read is an error, and the handler refuses the
// call: this read is only made on the way to a construct, so there is nothing else in
// the request left to answer.
func (s *accessStore) packageDomains(asOwner context.Context, packageID string) ([]string, error) {
	if packageID == "" {
		return nil, nil
	}
	rows, err := s.read(asOwner, "packageById", map[string]any{"packageId": packageID})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return accessStringList(rows[0]["dslDomains"]), nil
}

// read runs one named query. The arguments here are ids and nothing else, so an error
// repeating its statement says nothing a log may not.
func (s *accessStore) read(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		return nil, err
	}
	result, err := s.engine.Execute(ctx, "query "+call)
	if err != nil {
		return nil, err
	}
	return memql.MaterializeRows(result), nil
}

func (s *accessStore) CodeByHash(ctx context.Context, siteID, codeHash string) (*server.AccessCode, error) {
	code, err := s.graph.CodeByHash(ctx, siteID, codeHash)
	return accessCodeOf(code), err
}

func (s *accessStore) CodeByID(ctx context.Context, codeID string) (*server.AccessCode, error) {
	code, err := s.graph.CodeByID(ctx, codeID)
	return accessCodeOf(code), err
}

func (s *accessStore) SessionByTokenHash(ctx context.Context, tokenHash string) (*server.AccessSession, error) {
	session, err := s.graph.SessionByTokenHash(ctx, tokenHash)
	if session == nil {
		return nil, err
	}
	converted := server.AccessSession(*session)
	return &converted, err
}

func (s *accessStore) CreateSession(ctx context.Context, owner string, in server.AccessSession, tokenHash string) error {
	return s.graph.CreateSession(ctx, owner, siteaccess.Session(in), tokenHash)
}

func (s *accessStore) EndSession(ctx context.Context, sessionID string, at time.Time) error {
	return s.graph.EndSession(ctx, sessionID, at)
}

// accessCodeOf converts a store's row. A conversion rather than a copy field by field,
// so a field added to one side and not the other stops this compiling.
func accessCodeOf(code *siteaccess.Code) *server.AccessCode {
	if code == nil {
		return nil
	}
	converted := server.AccessCode(*code)
	return &converted
}

// accessBareField reads an id off a shaped row. Bare because the shape hands
// relationship fields back canonical, and everything the handler does with these is
// compare them.
func accessBareField(row map[string]any, key string) string {
	value, _ := row[key].(string)
	return memql.BareShortId(value)
}

// accessStringList reads a decoded list of strings, whichever way the read carried it.
func accessStringList(value any) []string {
	switch list := value.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}
