package app

import (
	"context"
	"log/slog"
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
	handler := server.NewAccessHandler(engine, newAccessStore(engine, a.Logger), a.Logger, time.Now)
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
// The code and session rows go through integrations/siteaccess, which reads and updates
// them as the cluster acting, with its own internal origin: component/server may not
// stamp one and may not import integrations/, those rows answer nothing to a client
// (D14), and a row's current owner must not decide whether a code opens. Its errors are
// scrubbed of digests before they get here, which is what AccessStore asks of them.
//
// The site and its package are read here, UNSTAMPED, under the named owner, and that is
// the difference between the two halves. A read as that person is what admits a real
// owner and turns away a stranger, and the handler then compares the stamp with the
// row, because the tier also admits a member of the organization.
type accessStore struct {
	engine server.MemQLExecutor
	graph  *siteaccess.GraphStore
	logger *slog.Logger
}

func newAccessStore(engine server.MemQLExecutor, logger *slog.Logger) *accessStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &accessStore{engine: engine, graph: siteaccess.NewGraphStore(engine), logger: logger}
}

var _ server.AccessStore = (*accessStore)(nil)

// AccessSite resolves a site UNDER the named owner's own actor, as shopperSiteReader
// does. A miss is (nil, nil). A site with no package deployable is not one: it was read,
// it has no gates to open, and it comes back with a blank name for the handler to refuse,
// because a miss is what the handler tells an operator is a site its owner cannot read.
//
// FRESH, both reads, because nothing in the access decision is cached (D11). siteById
// and packageById are ordinary cached reads, and the handler compares what comes back:
// a move of the site's organization, a change of owner, or a package's domains changing
// on a deploy has to count from the next request, not from when the entry ages out.
func (s *accessStore) AccessSite(ctx context.Context, siteID, owner string) (*server.AccessSite, error) {
	siteID, owner = strings.TrimSpace(siteID), memql.BareShortId(owner)
	if siteID == "" || owner == "" {
		return nil, nil
	}
	// A blank owner returns ctx unchanged from ContextWithUserActor, which is why it was
	// refused above: this read must never run as whoever the request happened to be.
	asOwner := memql.ContextWithFreshRead(auth.ContextWithUserActor(ctx, owner))
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
		DeployableName: strings.TrimSpace(deployable),
	}
	if site.DeployableName != "" {
		site.PackageDomains = s.packageDomains(asOwner, site.ID, accessBareField(row, "packageId"))
	}
	return site, nil
}

// packageDomains reads the DSL domains a site's package ships.
//
// A FAILED READ IS LOGGED AND READS AS NO DOMAINS. It opens nothing, since an empty
// list reaches no gate, and the rest of the request can still be answered: a visitor
// already inside is told they are inside and their calls 404 for the moment, rather
// than every poll failing over a read most of them do not need. Discarding the error
// silently would be a closed gate with nothing anywhere saying why.
//
// A package the owner cannot read and one with no list are the same answer, and neither
// is logged: a source no deploy has written a list for is an ordinary state, it would be
// said once per request, and the OS's gate list is where an operator sees it.
func (s *accessStore) packageDomains(asOwner context.Context, siteID, packageID string) []string {
	if packageID == "" {
		return nil
	}
	rows, err := s.read(asOwner, "packageById", map[string]any{"packageId": packageID})
	if err != nil {
		s.logger.Warn("access surface: reading the site's package failed, so its gates stay closed for this request",
			"component", "server", "site", siteID, "package", packageID, "err", err)
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	return accessStringList(rows[0]["dslDomains"])
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
