package edge

// edge.go -- the engine seam. resolve.go declares QueryExecutor as the
// narrow interface the resolver needs; this file is the one implementation
// that actually asks the live engine, as opposed to the stubExec the tests
// use.
//
// # Why this runs under a synthetic actor
//
// The bound concept declares @rowAuthz(clusterOwner), and its named query
// carries an explicit actor.isClusterOwner==true conjunct on top of that
// declared tier -- so a caller without a cluster-owner actor is refused
// TWICE: once by the textual conjunct, once by the engine's own row-authz
// enforcement on the read path. The edge is a service, not a user, so
// SiteByHostname stamps a synthetic cluster-owner identity onto the ctx it
// hands the engine.
//
// This mirrors component/campaigns/worker.go's systemCampaignsActor
// precedent exactly, including the reasoning: the identity is only as
// powerful as what it is asked to do, not as powerful as the role it
// carries. The scope of that power here is bounded to ONE named query over
// ONE concept -- nothing in this file reaches anything else.

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// Engine is the narrow engine surface the edge needs to issue its one named
// query. Kept to one method, the same shape every other Go component in
// this tree uses to talk to the engine (component/campaigns, component/
// outbound, component/inbound, ...).
type Engine interface {
	Execute(ctx context.Context, query string) (any, error)
}

// systemEdgeActor is the engine's own operator identity for the one
// clusterOwner-tier read this package issues. A synthetic cluster owner,
// scoped by what it is used for: one named query over one concept. Never
// used for anything else.
const systemEdgeActor = "system:edge"

// engineExecutor is the QueryExecutor that asks the live engine.
type engineExecutor struct {
	engine Engine
}

// NewEngineExecutor wraps engine as a QueryExecutor. The concrete
// counterpart to the stubExec the resolver tests use.
func NewEngineExecutor(engine Engine) QueryExecutor {
	return &engineExecutor{engine: engine}
}

// SiteByHostname asks the engine's named siteByHostname query under a
// synthetic cluster-owner actor -- see the file-level note for why. A miss
// (zero rows) is (nil, nil): an unknown hostname is not a query failure, and
// the resolver above depends on that distinction to cache a miss instead of
// treating it as an error.
func (e *engineExecutor) SiteByHostname(ctx context.Context, hostname string) (*Site, error) {
	ctx = systemActorContext(ctx)
	q := fmt.Sprintf("query siteByHostname(hostname: %s)", langparser.QuoteString(hostname))
	res, err := e.engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("edge: query siteByHostname: %w", err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil, nil
	}
	return siteFromRow(rows[0]), nil
}

// SiteForCustomDomain resolves a hostname through a LIVE custom-domain binding
// to the deployable it names (epic memql#4805, design D8).
//
// TWO NAMED READS, and the first one carries the whole security property.
// `liveCustomDomainByHostname` filters `status=="live"`, and a binding reaches
// `live` only after BOTH DNS checks passed and its certificate came back Ready
// -- so a hostname answering here is one whose owner proved control of the name
// and whose traffic this cluster can terminate TLS for. Every other status
// resolves to nothing, which is why a removal takes effect at the speed of a
// row write rather than at the speed of an Ingress deletion.
//
// A MISS AT EITHER STEP IS (nil, nil). The second step missing is the
// interesting one: it means a live binding names a site that is deleted, draft
// or gone, and the honest answer is the same 404 an unknown hostname gets
// rather than an error page about a row nobody outside this cluster can see.
func (e *engineExecutor) SiteForCustomDomain(ctx context.Context, hostname string) (*Site, error) {
	ctx = systemActorContext(ctx)
	q := fmt.Sprintf("query liveCustomDomainByHostname(hostname: %s)", langparser.QuoteString(hostname))
	res, err := e.engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("edge: query liveCustomDomainByHostname: %w", err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil, nil
	}
	siteId := memql.BareShortId(rowString(rows[0], "siteId"))
	if siteId == "" {
		return nil, nil
	}
	sq := fmt.Sprintf("query siteById(siteId: %s)", langparser.QuoteString(siteId))
	sres, err := e.engine.Execute(ctx, sq)
	if err != nil {
		return nil, fmt.Errorf("edge: query siteById for custom domain %q: %w", hostname, err)
	}
	srows := memql.MaterializeRows(sres)
	if len(srows) == 0 {
		return nil, nil
	}
	return siteFromRow(srows[0]), nil
}

// systemActorContext stamps the synthetic cluster-owner identity onto ctx.
//
// It sets the same three surfaces auth.ContextWithUserActor sets for a real
// user -- claims, TokenInfo, AccessContext -- because createdBy and
// actor.userId read different ones (memql#2989), and a mutation or filter
// reading the wrong one is how a synthetic actor silently resolves to
// nobody. The one difference from auth.ContextWithUserActor is the Role:
// RoleOwner here, not RoleWriter, because AccessContext.IsClusterOwner()
// reads Role == RoleOwner, and that bit is what the read's
// actor.isClusterOwner==true conjunct is checking. That is precisely why
// auth.ContextWithUserActor is NOT a substitute for this -- it hardcodes
// RoleWriter.
func systemActorContext(ctx context.Context) context.Context {
	claims := map[string]any{"sub": systemEdgeActor, "role": "owner"}
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	return auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: systemEdgeActor,
		Role:   auth.RoleOwner,
	})
}

// siteFromRow projects one engine row (the siteFull shape's output) onto
// Site. id is bare-ified to the short form: every id field on a
// @relationship concept comes back canonicalized
// (v1:<ns>:<concept>:<shortId>), and the BARE-ids wire contract
// (docs/public/concepts/identifiers.md) is what every other named-query
// reader in the tree already follows (see component/campaigns/store.go's
// bare helper).
func siteFromRow(r map[string]any) *Site {
	return &Site{
		ID:          memql.BareShortId(rowString(r, "id")),
		Hostname:    rowString(r, "hostname"),
		Kind:        rowString(r, "kind"),
		BundleRef:   rowString(r, "bundleRef"),
		Status:      rowString(r, "status"),
		Title:       rowString(r, "title"),
		APIProxy:    rowBool(r, "apiProxy"),
		SystemOwned: rowBool(r, "systemOwned"),
		Binding:     rowObject(r, "binding"),
		Settings:    rowStringMap(r, "settings"),
	}
}

// rowStringMap projects an object field whose values are meant to be plain
// strings (v1:platform:site.settings) onto a string map, keeping only the
// entries that ARE strings. The write guard admits nothing else, so a
// non-string here is a raw write that bypassed it; dropping the entry rather
// than stringifying it keeps the document honest about what the guard
// promises a bundle -- a value typed as a string, never one coerced into one.
// Absent, null or a non-object yields an empty map, never nil.
func rowStringMap(m map[string]any, key string) map[string]string {
	out := map[string]string{}
	obj, ok := m[key].(map[string]any)
	if !ok {
		return out
	}
	for k, v := range obj {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func rowString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func rowBool(m map[string]any, key string) bool {
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	default:
		return false
	}
}

// rowObject projects a nested object field (v1:platform:site.binding) as a
// plain map. A payload arrives here through structpb's AsMap, so a stored
// object is already a map[string]any; anything else -- absent, null, or a
// scalar somebody wrote into an object field -- yields nil rather than a
// partially-populated map, so a caller's "is there a binding" check is a
// single nil test.
func rowObject(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok && len(v) > 0 {
		return v
	}
	return nil
}
