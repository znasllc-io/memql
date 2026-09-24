package edge

// edge.go -- the engine seam. resolve.go declares QueryExecutor as the
// narrow interface the resolver needs; this file is the one implementation
// that actually asks the live engine, as opposed to the stubExec the tests
// use.
//
// # Why this runs under a synthetic actor
//
// Every concept this file reads declares @rowAuthz(clusterOwner), and every
// named query it issues carries an explicit actor.isClusterOwner==true
// conjunct on top of that declared tier -- so a caller without a
// cluster-owner actor is refused TWICE: once by the textual conjunct, once by
// the engine's own row-authz enforcement on the read path. The edge is a
// service, not a user, so each read stamps a synthetic cluster-owner identity
// onto the ctx it hands the engine.
//
// This mirrors component/campaigns/worker.go's systemCampaignsActor
// precedent exactly, including the reasoning: the identity is only as
// powerful as what it is asked to do, not as powerful as the role it
// carries. The scope of that power here is bounded to the named reads below
// -- v1:platform:site, v1:platform:customDomain, v1:platform:accountFrontDoor
// and, since epic memql#5530, the v1:shopify:store a storefront's binding
// NAMES. Nothing in this file reaches anything else.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/frontdoor"
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

// systemEdgeActor is the engine's own operator identity for the
// clusterOwner-tier reads this package issues. A synthetic cluster owner,
// scoped by what it is used for: the handful of named queries in this file.
// Never used for anything else.
//
// The literal is kept rather than composed from systemActorName below, so a
// test asserting the stored `createdBy` keeps reading the exact string it
// asserts; auth.SystemActor builds the same value from the name, and
// TestSystemEdgeActorMatchesTheSharedHelper holds the two together.
const systemEdgeActor = "system:edge"

// systemActorName is what this package calls itself to auth.SystemActor --
// the part after `system:` in the id above.
const systemActorName = "edge"

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

// SiteForAccountFrontDoor resolves an `app.<reservedName>` host through a LIVE
// v1:platform:accountFrontDoor row to the OS site (epic memql#5168, design
// D4/F).
//
// # ONE READ, NOT TWO
//
// SiteForCustomDomain needs two named reads because a binding names an
// arbitrary site. This needs one, because a front door always resolves to the
// SAME site: the shell's row id is frontdoor.OsSite, a constant known before
// any operator creates anything. So the site is fetched by that id and the
// account context comes out of the door row in the same pass.
//
// # THE HOST IS STRIPPED, THE NAME IS QUERIED
//
// A door row stores `memql.acme.com`; the request arrived for
// `app.memql.acme.com`. A DSL filter compiles to SQL over stored fields and
// cannot prepend a label, so the strip happens here, through
// frontdoor.AccountReservedNameFromAppHost -- the pinned inverse of the
// composition, so the label is spelled in exactly one place. A host that is
// not an `app.` host never reaches the engine at all, which is also what keeps
// `api.` and `id.` (routed straight to the bff and identity by Ingress) from
// costing a query if one ever arrives here by mistake.
//
// # A MISS AT EITHER STEP IS (nil, nil)
//
// The second step missing means a live door names an OS site that is deleted,
// draft or gone, and the honest answer is the same 404 an unknown hostname
// gets -- SiteForCustomDomain's reasoning, and the same reason: nobody outside
// this cluster can see the row that would explain an error page.
func (e *engineExecutor) SiteForAccountFrontDoor(ctx context.Context, hostname string) (*Site, error) {
	reserved, ok := frontdoor.AccountReservedNameFromAppHost(hostname)
	if !ok {
		return nil, nil
	}
	ctx = systemActorContext(ctx)
	q := fmt.Sprintf("query liveAccountFrontDoorByReservedName(reservedName: %s)",
		langparser.QuoteString(reserved))
	res, err := e.engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("edge: query liveAccountFrontDoorByReservedName for %q: %w", hostname, err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil, nil
	}
	accountId := memql.BareShortId(rowString(rows[0], "accountId"))
	if accountId == "" {
		// A door with no account is a row that should not exist -- accountId
		// is required by the concept. Treated as a miss rather than an error
		// for the reason above: there is no useful page to render about it.
		return nil, nil
	}

	sq := fmt.Sprintf("query siteById(siteId: %s)", langparser.QuoteString(frontdoor.OsSite))
	sres, err := e.engine.Execute(ctx, sq)
	if err != nil {
		return nil, fmt.Errorf("edge: query siteById for account front door %q: %w", hostname, err)
	}
	srows := memql.MaterializeRows(sres)
	if len(srows) == 0 {
		return nil, nil
	}
	site := siteFromRow(srows[0])
	site.Account = &SiteAccount{
		ID: accountId,
		// FROM THE ROW, NOT FROM THE HOST. They agree today, and the row is
		// the one that stays right: a door whose reservation was cleared is
		// torn down by name, and the name it is torn down by is this one.
		ReservedName: rowString(rows[0], "reservedName"),
	}
	return site, nil
}

// StoreByID resolves the v1:shopify:store row a storefront's binding names
// (epic memql#5530, issue memql#5538), under the same synthetic cluster-owner
// actor the site reads use -- see the file-level note for why, and note that
// v1:shopify:store's @rowAuthz(clusterOwner, rankFloor="developer") tier
// answers a caller below developer with zero rows, so an unstamped read would
// find no store at all.
//
// THREE FIELDS OF A ROW THAT CARRIES MORE. `adminTokenRef` and
// `webhookSecretRef` are deliberately NOT projected: the Admin API token is
// the credential that can read orders and customers and mutate the store, and
// the serving path cannot leak a reference it was never handed. Only
// `storefrontTokenRef` comes across, because the runtime-config document
// resolves it -- Shopify designs that one to be published to the shopper's own
// browser.
//
// A miss (zero rows) is (nil, nil): a store that is gone is not a query
// failure, it is a storefront with nothing to talk to, and the resolver
// depends on that distinction to keep serving the bundle.
func (e *engineExecutor) StoreByID(ctx context.Context, storeId string) (*BoundStore, error) {
	ctx = systemActorContext(ctx)
	q := fmt.Sprintf("query storeById(storeId: %s)", langparser.QuoteString(storeId))
	res, err := e.engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("edge: query storeById %q: %w", storeId, err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil, nil
	}
	return &BoundStore{
		ID:                 memql.BareShortId(rowString(rows[0], "id")),
		Domain:             rowString(rows[0], "domain"),
		StorefrontTokenRef: rowString(rows[0], "storefrontTokenRef"),
	}, nil
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
//
// THE DEFINITION MOVED (memql#5574). Everything above is still true and is
// now true in one place: auth.ContextWithSystemActor. This had been
// hand-rolled here, hand-rolled again in component/datasync with only ONE of
// the three surfaces, and not written at all in integrations/shopify -- which
// reached for a connector actor instead and read zero rows. The wrapper stays
// so every call site in this file keeps reading as the edge's own decision.
//
// Two flags come with the shared helper that this copy did not set, and both
// are corrections rather than changes: Unranked (the rank rules must not read
// RoleOwner above as rank 400) and Synthetic (the cluster can never be a
// row's owner). Every use here is a READ, so neither alters what this file
// does today; they close the hole a future write through it would fall into.
func systemActorContext(ctx context.Context) context.Context {
	return auth.ContextWithSystemActor(ctx, systemActorName)
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
		ID:       memql.BareShortId(rowString(r, "id")),
		Hostname: rowString(r, "hostname"),
		Kind:     rowString(r, "kind"),
		// Absent on every row written before memql#5535, which is the
		// default and means Kind decides -- see Site.ResolutionTail.
		ResolutionTail: rowString(r, "resolutionTail"),
		BundleRef:      rowString(r, "bundleRef"),
		Status:         rowString(r, "status"),
		Title:          rowString(r, "title"),
		APIProxy:       rowBool(r, "apiProxy"),
		ShopperForms:   rowBool(r, "shopperForms"),
		OwnerUserID:    rowString(r, "ownerUserId"),
		SystemOwned:    rowBool(r, "systemOwned"),
		Binding:        rowObject(r, "binding"),
		CandidateRef:   rowString(r, "candidateRef"),
		PreviewBinding: rowObject(r, "previewBinding"),
		Settings:       rowStringMap(r, "settings"),
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

// PreviewGrantByToken resolves a preview grant by the SHA-256 of the token a
// request presented (epic memql#5531, issue memql#5545).
//
// UNDER THE SAME SYNTHETIC CLUSTER-OWNER ACTOR as every other read in this
// file, and here it is what makes the feature work at all:
// v1:platform:sitePreviewGrant declares the composite owner tier, so a read as
// anybody else would answer only that person's own grants -- and the edge is
// nobody. It holds no operator's credential and never learns who is asking; it
// holds a DIGEST, and the row it finds is the whole authorization decision,
// made earlier by the mint under the caller's own actor.
//
// A MISS IS (nil, nil). A cookie naming no grant is not a query failure, it is
// a request that gets exactly what an unauthenticated one gets, and the
// resolver above depends on that distinction to cache the negative answer.
func (e *engineExecutor) PreviewGrantByToken(ctx context.Context, tokenHash string) (*PreviewGrant, error) {
	ctx = systemActorContext(ctx)
	q := fmt.Sprintf("query sitePreviewGrantByToken(tokenHash: %s)", langparser.QuoteString(tokenHash))
	res, err := e.engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("edge: query sitePreviewGrantByToken: %w", err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]
	return &PreviewGrant{
		ID:           memql.BareShortId(rowString(r, "id")),
		SiteID:       memql.BareShortId(rowString(r, "siteId")),
		CandidateRef: rowString(r, "candidateRef"),
		RevokedAt:    rowString(r, "revokedAt"),
		// AN UNPARSEABLE OR ABSENT STAMP IS THE ZERO TIME, which preview.go
		// reads as EXPIRED. Failing toward the refusal is the only defensible
		// direction for the one field bounding a bearer credential.
		ExpiresAt:  rowTime(r, "expiresAt"),
		LastSeenAt: rowTime(r, "lastSeenAt"),
	}, nil
}

// TouchPreviewGrant records that the edge honoured a grant. Throttled by the
// caller (preview.go) to at most one write per grant per minute, and run
// detached from the request, so nothing a visitor waits for happens here.
func (e *engineExecutor) TouchPreviewGrant(ctx context.Context, grantId string, at time.Time) error {
	ctx = systemActorContext(ctx)
	q := fmt.Sprintf("mutation touchSitePreviewGrant(grantId: %s, lastSeenAt: %s)",
		langparser.QuoteString(grantId), langparser.QuoteString(at.UTC().Format(time.RFC3339)))
	// INTERNAL ORIGIN, because touchSitePreviewGrant is @serverOnly: it is the
	// deployment's own observation of its own serving path, and a
	// client-writable "last seen" is a field that says what its writer wanted
	// it to say. Origin defaults to CLIENT, and a missed stamp is refused with
	// only a WARN in the log.
	if _, err := e.engine.Execute(auth.ContextWithInternalOrigin(ctx), q); err != nil {
		return fmt.Errorf("edge: mutation touchSitePreviewGrant: %w", err)
	}
	return nil
}

// rowTime reads an RFC3339 stamp off a row. ABSENT AND UNPARSEABLE ARE ONE
// ANSWER -- the zero time -- and every caller here treats that as "expired" or
// "never", both of which are the safe reading.
func rowTime(m map[string]any, key string) time.Time {
	raw := strings.TrimSpace(rowString(m, key))
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
