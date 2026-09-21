package sitepreview

import (
	"context"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/num"
)

// probe.go -- the three things the engine can ACTIVELY observe of a preview
// (issue memql#5547). The fourth, an order arriving in the mirror, is observed
// passively by the connector doing its ordinary job and is written by the
// automation in dsl/platform/automations.memql.
//
// # The Storefront call is not made here
//
// It is made by integrations/shopify, reached through the `storefrontProbe`
// builtin over the engine -- the seam component/packages already uses to reach
// customDomainAdd, and for the same reason: having this package dial Shopify
// would put a second Storefront caller in a package the module taxonomy
// classifies as a component, which is the drift that table exists to stop.
//
// What this package owns is the ROWS: one v1:platform:sitePreviewObservation
// per step, attributed to the grant, the site, the operator and the development
// store the exercise ran against.
//
// # Three rows every time, including the failures
//
// A step that did not answer gets a row with ok=false and a reason. A step
// nobody ran gets NO ROW -- and that is the whole distinction the OS draws as
// "unmeasured" rather than as a failure. So a probe that could not even reach
// the store writes three failures (each saying so), while a deployable nobody
// has exercised has nothing at all.

// handleProbe runs the active observations for one preview and records them.
func (i *Integration) handleProbe(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	grantID := strings.TrimSpace(asString(args["grantId"]))
	if grantID == "" {
		return nil, fmt.Errorf("sitePreviewProbe: grantId is required -- an observation with no exercise behind it is a measurement of nothing")
	}

	grant, ok, err := i.store.GrantByID(ctx, grantID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("sitePreviewProbe: no preview %q is readable by this caller", grantID)
	}

	site, siteOK, err := i.store.SiteByID(ctx, grant.SiteID)
	if err != nil {
		return nil, err
	}
	if !siteOK {
		return nil, fmt.Errorf("sitePreviewProbe: preview %q names deployable %q, which is not readable by this caller", grantID, grant.SiteID)
	}

	// THE PREVIEW STORE IS RE-CHECKED, not trusted from the grant. A preview
	// binding can be re-pointed after a grant is minted, and exercising a
	// storefront is the one act whose whole safety is which store it reaches --
	// so the question is asked again, against the row as it stands now.
	preview, err := i.boundStore(ctx, site.PreviewStoreID)
	if err != nil {
		return nil, err
	}
	if refusal := memql.SitePreviewBindingRefusal(site.Kind == storefrontKind, preview); !refusal.Empty() {
		return nil, fmt.Errorf("sitePreviewProbe: %s %s", refusal.Message, refusal.Remedy)
	}
	if site.Kind != storefrontKind {
		return nil, fmt.Errorf(
			"sitePreviewProbe: %q is a %q deployable, and the four things this observes are all questions about a Shopify store",
			site.Hostname, site.Kind)
	}

	report, probeErr := i.storefrontProbe(ctx, preview.ID)
	observedAt := time.Now().UTC()

	// A PROBE THAT COULD NOT RUN AT ALL STILL RECORDS THREE FAILURES. The
	// operator asked a question and the honest answer is "the store could not
	// be asked, here is why" -- writing nothing would leave the panel reading
	// `unmeasured`, which is the answer for a deployable nobody exercised.
	if probeErr != nil {
		reason := probeErr.Error()
		report = map[string]stepResult{
			memql.PreviewStepCatalog:  {Failure: reason},
			memql.PreviewStepCart:     {Failure: "not attempted: " + reason},
			memql.PreviewStepCheckout: {Failure: "not attempted: " + reason},
		}
	}

	written := make([]map[string]any, 0, 3)
	for _, step := range []struct {
		kind string
		from string
	}{
		{ObservationCatalogRead, memql.PreviewStepCatalog},
		{ObservationCartAccepted, memql.PreviewStepCart},
		{ObservationCheckoutURL, memql.PreviewStepCheckout},
	} {
		r := report[step.from]
		obs := Observation{
			ID:          id.NewShortId(),
			GrantID:     grant.ID,
			SiteID:      site.ID,
			OwnerUserID: grant.OwnerUserID,
			StoreID:     preview.ID,
			Kind:        step.kind,
			ObservedAt:  observedAt,
			OK:          r.OK,
			Detail:      r.Detail,
			Failure:     r.Failure,
			DurationMs:  r.DurationMs,
		}
		if err := i.store.RecordObservation(ctx, obs); err != nil {
			return nil, err
		}
		written = append(written, map[string]any{
			"kind":       obs.Kind,
			"ok":         obs.OK,
			"detail":     obs.Detail,
			"failure":    obs.Failure,
			"durationMs": obs.DurationMs,
		})
	}

	return i.node("probe:"+grant.ID, map[string]any{
		"grantId":            grant.ID,
		"siteId":             site.ID,
		"hostname":           site.Hostname,
		"storeId":            preview.ID,
		"storeDomain":        preview.Domain,
		"observedAt":         observedAt.Format(time.RFC3339),
		"observations":       written,
		"orderObservedWhen":  "an order placed on this development store is recorded on this preview when it reaches the mirror",
		"paymentNotObserved": "the payment walk happens in a browser on Shopify's hosted checkout and is not something this cluster can observe",
	})
}

// stepResult is one step of the Storefront report, as it crosses the builtin
// boundary.
type stepResult struct {
	OK         bool
	Detail     string
	Failure    string
	DurationMs int
}

// storefrontProbe asks integrations/shopify, through the engine.
func (i *Integration) storefrontProbe(ctx context.Context, storeID string) (map[string]stepResult, error) {
	call := fmt.Sprintf("builtin storefrontProbe(storeId: %s)", langparser.QuoteString(storeID))
	res, err := i.engine.Execute(ctx, call)
	if err != nil {
		return nil, fmt.Errorf("the store could not be asked: %w", err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil, fmt.Errorf("the store probe answered nothing, which this build cannot read as either outcome")
	}
	row := rows[0]
	out := map[string]stepResult{}
	for _, key := range []string{memql.PreviewStepCatalog, memql.PreviewStepCart, memql.PreviewStepCheckout} {
		out[key] = stepFromPayload(row[key])
	}
	return out, nil
}

func stepFromPayload(v any) stepResult {
	m, ok := v.(map[string]any)
	if !ok {
		return stepResult{Failure: "the store probe did not report this step"}
	}
	out := stepResult{}
	if b, ok := m["ok"].(bool); ok {
		out.OK = b
	}
	if s, ok := m["detail"].(string); ok {
		out.Detail = s
	}
	if s, ok := m["failure"].(string); ok {
		out.Failure = s
	}
	// SATURATE, and the answer is named rather than assumed (core/num). A bare
	// int(x) from a float64 is implementation-defined out of range and answers
	// with the integer indefinite value on amd64, so a nonsense duration on the
	// wire would land on the row as a hugely NEGATIVE one -- which the OS would
	// then print beside "answered" as though something had been measured.
	// Clamping keeps a wrong number wrong in the direction a reader can see.
	switch n := m["durationMs"].(type) {
	case float64:
		out.DurationMs = num.ClampFloat64(n)
	case int:
		out.DurationMs = n
	case int64:
		out.DurationMs = num.ClampInt64(n)
	}
	return out
}
