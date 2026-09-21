package sitepreview

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// store.go -- the graph seam. Every read and write is a NAMED construct
// rendered as MemQL text and handed to the engine, the shape component/edge,
// component/campaigns and integrations/customdomain all use. Nothing here
// reaches the database.
//
// # Two actors, and which is which is the whole of the authorization story
//
// THE SITE AND THE STORE ARE READ UNDER THE CALLER. That is what makes them
// authorization checks rather than lookups: `siteById` carries
// v1:platform:site's composite tier and `storeById` carries
// v1:shopify:store's clusterOwner tier, so a caller who may not see either
// resolves zero rows and is refused BY NAME, before anything is minted or
// written. Reading them under a system actor would turn both gates off while
// leaving the code looking identical, which is the failure
// integrations/customdomain's releaseForSite note describes from the other
// direction.
//
// THE THREE @serverOnly WRITES RUN UNDER THE CALLER TOO, with internal origin
// stamped inline (integrations/work and integrations/compose set that
// precedent). A grant belongs to the operator who asked for it, so writing it
// under a synthetic actor would make the row the DEPLOYMENT's -- and then the
// composite owner tier would hide every operator's own previews from them.
// Call origin defaults to CLIENT, so without the stamp each of the three is
// refused at runtime with only a WARN in the log and the row silently not
// appearing.
//
// The ONE system-actor read is the storefront token's secret resolution, which
// is not in this file at all: it is ResolveSystemSecret, handed in from the
// plugin context, and it is a system read because a globalSecret is nobody's
// to own.

// Engine is the narrow engine surface this package needs.
type Engine interface {
	Execute(ctx context.Context, query string) (any, error)
}

// Site is the projection of v1:platform:site this package works with: the
// serving version, the candidate beside it, and the two bindings.
type Site struct {
	ID             string
	Hostname       string
	Kind           string
	Status         string
	BundleRef      string
	CandidateRef   string
	SystemOwned    bool
	StoreID        string // binding.storeId -- the store shoppers reach
	PreviewStoreID string // previewBinding.storeId -- the development store
}

// HasCandidate reports whether there is a version to exercise.
func (s Site) HasCandidate() bool { return strings.TrimSpace(s.CandidateRef) != "" }

// Store is the projection of v1:shopify:store the guard and the probe need.
type Store struct {
	ID                 string
	Domain             string
	APIVersion         string
	StorefrontTokenRef string
	IsDevelopment      bool
	Plan               string
}

// Grant is one v1:platform:sitePreviewGrant row.
type Grant struct {
	ID             string
	SiteID         string
	OwnerUserID    string
	CandidateRef   string
	PreviewStoreID string
	IssuedAt       time.Time
	ExpiresAt      time.Time
	RevokedAt      string
	LastSeenAt     string
}

// Observation is one v1:platform:sitePreviewObservation row.
type Observation struct {
	ID          string
	GrantID     string
	SiteID      string
	OwnerUserID string
	StoreID     string
	Kind        string
	ObservedAt  time.Time
	OK          bool
	Detail      string
	Failure     string
	DurationMs  int
}

// The four things the engine can observe, and the closed set is the point --
// see v1:platform:sitePreviewObservation. The payment walk is not one of them.
const (
	ObservationCatalogRead   = "catalog_read"
	ObservationCartAccepted  = "cart_accepted"
	ObservationCheckoutURL   = "checkout_url"
	ObservationOrderMirrored = "order_mirrored"
)

// Store reads and writes preview state through the engine.
type GraphStore struct{ engine Engine }

// NewGraphStore wraps an engine.
func NewGraphStore(engine Engine) *GraphStore { return &GraphStore{engine: engine} }

// SiteByID resolves a deployable UNDER THE CALLER. A miss is (Site{}, false,
// nil): a site this caller cannot read and a site that does not exist are the
// same answer here, deliberately, because telling them apart would tell a
// caller which ids exist.
func (s *GraphStore) SiteByID(ctx context.Context, siteID string) (Site, bool, error) {
	rows, err := s.rows(ctx, fmt.Sprintf("query siteById(siteId: %s)", langparser.QuoteString(siteID)))
	if err != nil {
		return Site{}, false, fmt.Errorf("sitepreview: read deployable %q: %w", siteID, err)
	}
	if len(rows) == 0 {
		return Site{}, false, nil
	}
	r := rows[0]
	return Site{
		ID:             memql.BareShortId(rowString(r, "id")),
		Hostname:       rowString(r, "hostname"),
		Kind:           rowString(r, "kind"),
		Status:         rowString(r, "status"),
		BundleRef:      rowString(r, "bundleRef"),
		CandidateRef:   rowString(r, "candidateRef"),
		SystemOwned:    rowBool(r, "systemOwned"),
		StoreID:        memql.BareShortId(objectString(r, "binding", "storeId")),
		PreviewStoreID: memql.BareShortId(objectString(r, "previewBinding", "storeId")),
	}, true, nil
}

// StoreByID resolves a v1:shopify:store UNDER THE CALLER.
//
// A MISS IS NOT AN ERROR, and both of its meanings matter to the callers here.
// The readiness read uses it to answer "can this caller see the store this
// deployable is bound to", where a miss is a legitimate no; the guard path uses
// it to decide whether a store is a development store, where a miss must NOT be
// read as "it is one" -- see Readiness, which treats an unreadable store as a
// refusal rather than as an answer.
func (s *GraphStore) StoreByID(ctx context.Context, storeID string) (Store, bool, error) {
	storeID = strings.TrimSpace(storeID)
	if storeID == "" {
		return Store{}, false, nil
	}
	rows, err := s.rows(ctx, fmt.Sprintf("query storeById(storeId: %s)", langparser.QuoteString(storeID)))
	if err != nil {
		return Store{}, false, fmt.Errorf("sitepreview: read store %q: %w", storeID, err)
	}
	if len(rows) == 0 {
		return Store{}, false, nil
	}
	r := rows[0]
	return Store{
		ID:                 memql.BareShortId(rowString(r, "id")),
		Domain:             rowString(r, "domain"),
		APIVersion:         rowString(r, "apiVersion"),
		StorefrontTokenRef: rowString(r, "storefrontTokenRef"),
		IsDevelopment:      rowBool(r, "isDevelopment"),
		Plan:               rowString(r, "plan"),
	}, true, nil
}

// CreateGrant writes a minted grant.
//
// @serverOnly, so internal origin is stamped here -- and the CALLER's identity
// is KEPT rather than replaced, which is what makes the row the operator's own:
// createSitePreviewGrant stamps ownerUserId from actor.userId, so writing this
// under a synthetic actor would produce a grant owned by the deployment and
// hide every operator's own previews from them.
//
// Grant.OwnerUserID is therefore NOT sent. It is carried on the value for the
// reply and for the observation rows, and the row's own owner is the engine's
// to decide.
func (s *GraphStore) CreateGrant(ctx context.Context, g Grant, tokenDigest string) error {
	var q strings.Builder
	q.WriteString("mutation createSitePreviewGrant(grantId: ")
	q.WriteString(langparser.QuoteString(g.ID))
	q.WriteString(", siteId: ")
	q.WriteString(langparser.QuoteString(g.SiteID))
	q.WriteString(", tokenHash: ")
	q.WriteString(langparser.QuoteString(tokenDigest))
	q.WriteString(", candidateRef: ")
	q.WriteString(langparser.QuoteString(g.CandidateRef))
	q.WriteString(", previewStoreId: ")
	q.WriteString(langparser.QuoteString(g.PreviewStoreID))
	q.WriteString(", issuedAt: ")
	q.WriteString(langparser.QuoteString(g.IssuedAt.UTC().Format(time.RFC3339)))
	q.WriteString(", expiresAt: ")
	q.WriteString(langparser.QuoteString(g.ExpiresAt.UTC().Format(time.RFC3339)))
	q.WriteString(")")
	if _, err := s.engine.Execute(auth.ContextWithInternalOrigin(ctx), q.String()); err != nil {
		return fmt.Errorf("sitepreview: write the preview grant: %w", err)
	}
	return nil
}

// GrantByID resolves one grant under the caller. A miss is (Grant{}, false,
// nil) -- a grant this caller may not read and a grant that never existed are
// the same answer, for SiteByID's reason.
func (s *GraphStore) GrantByID(ctx context.Context, grantID string) (Grant, bool, error) {
	rows, err := s.rows(ctx, fmt.Sprintf("query sitePreviewGrantById(grantId: %s)", langparser.QuoteString(grantID)))
	if err != nil {
		return Grant{}, false, fmt.Errorf("sitepreview: read preview %q: %w", grantID, err)
	}
	if len(rows) == 0 {
		return Grant{}, false, nil
	}
	return grantFromRow(rows[0]), true, nil
}

// GrantsForSite lists every grant ever issued against a deployable, newest
// first, under the caller.
func (s *GraphStore) GrantsForSite(ctx context.Context, siteID string) ([]Grant, error) {
	rows, err := s.rows(ctx, fmt.Sprintf("query sitePreviewGrantsForSite(siteId: %s)", langparser.QuoteString(siteID)))
	if err != nil {
		return nil, fmt.Errorf("sitepreview: list previews of %q: %w", siteID, err)
	}
	out := make([]Grant, 0, len(rows))
	for _, r := range rows {
		out = append(out, grantFromRow(r))
	}
	return out, nil
}

// grantFromRow projects one sitePreviewGrantFull row. The ids are bare-ified:
// every field with a @relationship comes back canonicalized, and the bare-ids
// wire contract is what every other named-query reader in this tree follows.
func grantFromRow(r map[string]any) Grant {
	return Grant{
		ID:             memql.BareShortId(rowString(r, "id")),
		SiteID:         memql.BareShortId(rowString(r, "siteId")),
		OwnerUserID:    memql.BareShortId(rowString(r, "ownerUserId")),
		CandidateRef:   rowString(r, "candidateRef"),
		PreviewStoreID: memql.BareShortId(rowString(r, "previewStoreId")),
		RevokedAt:      rowString(r, "revokedAt"),
		LastSeenAt:     rowString(r, "lastSeenAt"),
		IssuedAt:       parseTime(rowString(r, "issuedAt")),
		ExpiresAt:      parseTime(rowString(r, "expiresAt")),
	}
}

// RecordObservation writes one of the four.
//
// @serverOnly, so internal origin is stamped, and the caller's identity is kept
// for CreateGrant's reason: recordSitePreviewObservation stamps ownerUserId
// from actor.userId, so the row belongs to whoever ran the exercise.
// Observation.OwnerUserID is not sent, for the same reason Grant.OwnerUserID is
// not.
func (s *GraphStore) RecordObservation(ctx context.Context, o Observation) error {
	var q strings.Builder
	q.WriteString("mutation recordSitePreviewObservation(observationId: ")
	q.WriteString(langparser.QuoteString(o.ID))
	q.WriteString(", grantId: ")
	q.WriteString(langparser.QuoteString(o.GrantID))
	q.WriteString(", siteId: ")
	q.WriteString(langparser.QuoteString(o.SiteID))
	q.WriteString(", storeId: ")
	q.WriteString(langparser.QuoteString(o.StoreID))
	q.WriteString(", kind: ")
	q.WriteString(langparser.QuoteString(o.Kind))
	q.WriteString(", observedAt: ")
	q.WriteString(langparser.QuoteString(o.ObservedAt.UTC().Format(time.RFC3339)))
	q.WriteString(", ok: ")
	if o.OK {
		q.WriteString("true")
	} else {
		q.WriteString("false")
	}
	q.WriteString(", detail: ")
	q.WriteString(langparser.QuoteString(o.Detail))
	q.WriteString(", failure: ")
	q.WriteString(langparser.QuoteString(o.Failure))
	q.WriteString(fmt.Sprintf(", durationMs: %d)", o.DurationMs))
	if _, err := s.engine.Execute(auth.ContextWithInternalOrigin(ctx), q.String()); err != nil {
		return fmt.Errorf("sitepreview: record the %s observation: %w", o.Kind, err)
	}
	return nil
}

func (s *GraphStore) rows(ctx context.Context, query string) ([]map[string]any, error) {
	if s == nil || s.engine == nil {
		return nil, fmt.Errorf("sitepreview: no engine")
	}
	res, err := s.engine.Execute(ctx, query)
	if err != nil {
		return nil, err
	}
	return memql.MaterializeRows(res), nil
}

// parseTime reads an RFC3339 stamp, answering the zero time for anything else.
// ABSENT AND UNPARSEABLE ARE ONE ANSWER and callers test IsZero, because a
// grant with no readable expiry must be treated as expired rather than as
// eternal -- fail toward the refusal.
func parseTime(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func rowString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
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

// objectString reads a leaf out of a nested object field -- binding.storeId and
// previewBinding.storeId, the only two this package needs.
func objectString(m map[string]any, field, key string) string {
	obj, ok := m[field].(map[string]any)
	if !ok {
		return ""
	}
	if v, ok := obj[key].(string); ok {
		return v
	}
	return ""
}
