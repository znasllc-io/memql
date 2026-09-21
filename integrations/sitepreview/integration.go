package sitepreview

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// integration.go -- the three DSL-callable capabilities:
//
//	integration.sitePreview.open       mint a grant  (dsl builtin sitePreviewOpen)
//	integration.sitePreview.readiness  what is legal (dsl builtin sitePreviewReadiness)
//	integration.sitePreview.probe      run the three observations (dsl builtin sitePreviewProbe)
//
// # Why `readiness` exists at all
//
// Issue memql#5546 asks that an act which is not legal be ABSENT in the OS
// rather than drawn and disabled, and DESIGN.md rule 12 says the same thing for
// every surface in the shell. An absent control needs the refusal to be
// knowable BEFORE the click, and no amount of client-side reasoning can know it:
// whether the bound store is a development store is a field on a row the browser
// cannot read. So the engine answers the question, in the same words the guard
// would refuse in -- literally the same function, component/sitepreview.
//
// # What it deliberately does not do
//
// It does not decide anything. Every act it describes is still refused by the
// Go guard in component/memql if it is attempted anyway, because a check only
// one caller runs is not a check (integrations/customdomain's handleAdd note
// gives the same reasoning for the same shape of split). This is a READ of what
// the guard would say.

// resultConcept is the synthetic MemoryNode concept these capabilities return:
// an in-flight integration result, never persisted.
const resultConcept = "integration:sitePreview:result"

// Integration exposes the preview capabilities.
type Integration struct {
	store  *GraphStore
	engine Engine
	cfg    Config
	logger *slog.Logger
	// orders is the cheap path behind the fourth observation: the set of
	// stores with a preview open, refreshed on a TTL, so the automation that
	// fires on every mirrored order costs a map lookup rather than a query.
	// See order.go.
	orders *openPreviewStores
}

// NewIntegration builds the integration over an engine.
func NewIntegration(engine Engine, cfg Config, logger *slog.Logger) *Integration {
	return &Integration{store: NewGraphStore(engine), engine: engine, cfg: cfg, logger: logger, orders: newOpenPreviewStores()}
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "sitePreview" }

// Capabilities implements memql.IntegrationProvider.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name: "open",
			Description: "Mint a short-lived preview of a deployable's candidate version, on the deployable's own origin. " +
				"Returns the URL once; the token is never readable again.",
			Handler:    i.handleOpen,
			ArgsSchema: map[string]string{"siteId": "string (required) -- the deployable to preview."},
		},
		{
			Name: "readiness",
			Description: "What is legal for a deployable right now: whether it may be previewed, promoted and taken live, " +
				"and the typed refusal with its remedy for each that is not.",
			Handler:    i.handleReadiness,
			ArgsSchema: map[string]string{"siteId": "string (required) -- the deployable to ask about."},
		},
		{
			Name: "noteOrder",
			Description: "Record that an order reached the mirror under a store a preview is open against -- the fourth and only passive observation. " +
				"Answers a count; an order on a store nobody is previewing records nothing and is not a failure.",
			Handler: i.handleNoteOrder,
			ArgsSchema: map[string]string{
				"storeId": "string (required) -- the v1:shopify:store the order was mirrored under.",
				"orderId": "string (optional) -- the mirrored order, for the observation's detail.",
			},
		},
		{
			Name: "probe",
			Description: "Run the three things the engine can observe of a preview against the development store the " +
				"preview binding names, and record each on the grant.",
			Handler:    i.handleProbe,
			ArgsSchema: map[string]string{"grantId": "string (required) -- the preview to record the observations on."},
		},
	}
}

// handleOpen mints a grant.
//
// EVERY GATE RUNS BEFORE THE TOKEN IS MINTED, in the order a person would ask
// them: is there a deployable I can see, is it one this applies to, is there
// something to preview, and is there a safe store to preview it against. A
// refusal at any of them has written nothing and minted nothing.
//
// THE SITE AND STORE READS RUN UNDER THE CALLER, which is what makes them
// authorization rather than lookup -- see store.go's file note.
func (i *Integration) handleOpen(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	siteID := strings.TrimSpace(asString(args["siteId"]))
	if siteID == "" {
		return nil, fmt.Errorf("sitePreviewOpen: siteId is required")
	}

	site, ok, err := i.store.SiteByID(ctx, siteID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("sitePreviewOpen: no deployable %q is readable by this caller", siteID)
	}
	if site.SystemOwned {
		return nil, fmt.Errorf(
			"sitePreviewOpen: %q is the surface this cluster is managed through and is exempt from the preview axis, as it is from the status and settings axes",
			site.Hostname)
	}
	if !site.HasCandidate() {
		return nil, fmt.Errorf(
			"sitePreviewOpen: %q has no candidate version, so there is nothing to preview. Publish a version and set it as the candidate first",
			site.Hostname)
	}

	storefront := site.Kind == storefrontKind
	preview, err := i.boundStore(ctx, site.PreviewStoreID)
	if err != nil {
		return nil, err
	}
	if refusal := memql.SitePreviewBindingRefusal(storefront, preview); !refusal.Empty() {
		return nil, fmt.Errorf("sitePreviewOpen: %s %s", refusal.Message, refusal.Remedy)
	}

	actor := memql.BareShortId(strings.TrimSpace(callerUserID(ctx)))
	if actor == "" {
		return nil, fmt.Errorf("sitePreviewOpen: a preview belongs to the person who opened it, and this call resolves to no user")
	}

	token, digest, err := memql.MintPreviewToken()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	grant := Grant{
		ID:             id.NewShortId(),
		SiteID:         site.ID,
		OwnerUserID:    actor,
		CandidateRef:   site.CandidateRef,
		PreviewStoreID: site.PreviewStoreID,
		IssuedAt:       now,
		ExpiresAt:      now.Add(i.cfg.GrantTTL),
	}
	if err := i.store.CreateGrant(ctx, grant, digest); err != nil {
		return nil, err
	}

	return i.node("open:"+grant.ID, map[string]any{
		"grantId":        grant.ID,
		"siteId":         grant.SiteID,
		"hostname":       site.Hostname,
		"candidateRef":   grant.CandidateRef,
		"previewStoreId": grant.PreviewStoreID,
		// The domain is named only when the caller could read the store. See
		// boundStore: naming it otherwise would hand a site owner a fact about
		// a cluster-owner-tier row.
		"previewStoreDomain": preview.Domain,
		"expiresAt":          grant.ExpiresAt.Format(time.RFC3339),
		"ttlMinutes":         int(i.cfg.GrantTTL / time.Minute),
		// THE ONE TIME THE TOKEN IS READABLE. It is in this reply and in the
		// operator's browser, and nowhere else ever again.
		"url": PreviewEnterURL(site.Hostname, token),
	})
}

// handleReadiness answers what is legal, in the guard's own words.
func (i *Integration) handleReadiness(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	siteID := strings.TrimSpace(asString(args["siteId"]))
	if siteID == "" {
		return nil, fmt.Errorf("sitePreviewReadiness: siteId is required")
	}
	site, ok, err := i.store.SiteByID(ctx, siteID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("sitePreviewReadiness: no deployable %q is readable by this caller", siteID)
	}

	storefront := site.Kind == storefrontKind
	serving, err := i.boundStore(ctx, site.StoreID)
	if err != nil {
		return nil, err
	}
	preview, err := i.boundStore(ctx, site.PreviewStoreID)
	if err != nil {
		return nil, err
	}

	goLive := memql.SiteGoLiveRefusal(storefront, serving)
	previewRefusal := memql.SitePreviewBindingRefusal(storefront, preview)
	promotion := memql.SitePromotionRefusal(site.CandidateRef, "")

	// PROMOTION IS REFUSED BY EITHER, and the order is the one an operator
	// would fix them in: there being nothing to promote is a simpler problem
	// than being bound to the wrong store, so it is reported first.
	promote := promotion
	if promote.Empty() {
		promote = goLive
	}
	// A SYSTEM-OWNED SITE IS EXEMPT FROM THE WHOLE AXIS, and saying so here is
	// what keeps the OS from drawing a preview panel on the console somebody is
	// reading it in.
	if site.SystemOwned {
		exempt := memql.PreviewRefusal{
			Code:    memql.PreviewRefusalSystemOwned,
			Message: "this is the surface the cluster is managed through, and it is exempt from the preview axis as it is from the status and settings axes.",
			Remedy:  "Nothing -- the platform's own site is deployed with the image.",
		}
		goLive, previewRefusal, promote = exempt, exempt, exempt
	}

	return i.node("readiness:"+site.ID, map[string]any{
		"siteId":             site.ID,
		"hostname":           site.Hostname,
		"kind":               site.Kind,
		"status":             site.Status,
		"storefront":         storefront,
		"bundleRef":          site.BundleRef,
		"candidateRef":       site.CandidateRef,
		"hasCandidate":       site.HasCandidate(),
		"storeId":            site.StoreID,
		"storeDomain":        serving.Domain,
		"storeReadable":      serving.Readable,
		"storeIsDevelopment": serving.Readable && serving.IsDevelopment,
		"previewStoreId":     site.PreviewStoreID,
		"previewStoreDomain": preview.Domain,
		"canPreview":         previewRefusal.Empty() && site.HasCandidate() && !site.SystemOwned,
		"canPromote":         promote.Empty(),
		"canGoLive":          goLive.Empty(),
		"previewRefusal":     refusalPayload(previewRefusal),
		"promoteRefusal":     refusalPayload(promote),
		"goLiveRefusal":      refusalPayload(goLive),
	})
}

// boundStore resolves one side's store UNDER THE CALLER and folds the three
// states the rules need into one value.
//
// AN EMPTY ID IS NOT A FAILED READ. `Readable` stays false and `ID` stays
// empty, which is how the rules tell "nothing is bound" from "something is
// bound and I cannot see it" -- and those get different answers in both
// directions of the guard.
func (i *Integration) boundStore(ctx context.Context, storeID string) (memql.PreviewBoundStore, error) {
	storeID = strings.TrimSpace(storeID)
	if storeID == "" {
		return memql.PreviewBoundStore{}, nil
	}
	store, ok, err := i.store.StoreByID(ctx, storeID)
	if err != nil {
		return memql.PreviewBoundStore{}, err
	}
	if !ok {
		return memql.PreviewBoundStore{ID: storeID}, nil
	}
	return memql.PreviewBoundStore{
		ID:            store.ID,
		Readable:      true,
		IsDevelopment: store.IsDevelopment,
		Domain:        store.Domain,
	}, nil
}

func refusalPayload(r memql.PreviewRefusal) map[string]any {
	return map[string]any{"code": r.Code, "message": r.Message, "remedy": r.Remedy}
}

// storefrontKind is the one site kind that carries a store binding. Spelled the
// same here as in component/edge and component/memql; it is the concept's own
// enum value.
const storefrontKind = "shopify_storefront"

func (i *Integration) node(suffix string, payload map[string]any) ([]memorynodes.MemoryNode, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("sitepreview: marshal result: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        "sitePreview:" + suffix,
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   raw,
	}}, nil
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
