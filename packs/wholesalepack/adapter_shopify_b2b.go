package wholesalepack

import (
	"context"
	"fmt"
	"strings"
)

// adapter_shopify_b2b.go -- THE NATIVE-B2B ADAPTER (epic memql#5533, issue
// memql#5558).
//
// # Every noun this needs was already mirrored
//
// company, companyContact, companyLocation, catalog, priceList,
// priceListPrice and quantityRule are all in dsl/shopify/generated. What
// was missing was never the model; it was the act of CREATING one from an
// approval, and that is all this adapter is.
//
// # Native B2B is not Plus-only, and the ceiling is what matters instead
//
// The connector record verified on 2026-08-23 that since 2026-04-02
// "companies, payment terms, volume pricing and up to three catalogs (via
// Markets) are available below Plus; Plus keeps unlimited catalogs, direct
// catalog assignment, deposits, order-review Functions." So the interesting
// number is three, not a tier gate -- and an adapter that found it by
// failing at the fourth grant would find it in a merchant's live store,
// with an application already approved and nothing to show the buyer.
// integrations/shopify reads the headroom before it creates anything.
//
// # WHY AN UNREADABLE PLAN IS FATAL *HERE*
//
// v1:shopify:store is @rowAuthz(clusterOwner, rankFloor="developer"): a
// cluster owner or a developer reads it (Connect Shopify, D3), and the pack's
// plan read answers nothing for anyone below that -- an ordinary merchant. This adapter REFUSES in that case, and the refusal is
// the honest answer rather than a limitation: without the plan it cannot
// tell a Plus store with forty catalogs from a Basic store with three, and
// guessing wrong means either refusing a merchant who is entitled to
// proceed or attempting a write that will fail inside somebody else's live
// store. customerTag has no such problem, which is exactly why this pack
// ships two adapters.

// shopifyB2BAdapter grants through Shopify's own B2B objects.
//
// IT HOLDS NO STATE AND NO CONNECTOR. Everything it does is a call to a
// named construct through the Caller it is handed, which is what lets this
// package stay ignorant of integrations/shopify -- see Caller's own
// comment for why that is load-bearing rather than tidy.
type shopifyB2BAdapter struct{}

// NewShopifyB2BAdapter builds the native-B2B adapter.
func NewShopifyB2BAdapter() Adapter { return &shopifyB2BAdapter{} }

func (a *shopifyB2BAdapter) Name() string { return AdapterShopifyB2B }

// Available refuses when the plan is unknown, and otherwise admits every
// plan.
//
// NO TIER GATE, deliberately. The obvious implementation refuses below Plus
// and it would be WRONG since 2026-04-02: a Basic store can hold companies,
// payment terms, volume pricing and three catalogs. The real constraint is
// the catalog count, which is a live fact about the store rather than a
// fact about its plan, so it is checked where it can be measured --
// integrations/shopify's headroom read, before anything is created.
func (a *shopifyB2BAdapter) Available(plan string) error {
	if strings.TrimSpace(plan) == "" {
		return &AdapterRefusal{
			Adapter: AdapterShopifyB2B,
			Reason: "this store's plan is not readable by the caller, so the catalog ceiling " +
				"cannot be established. v1:shopify:store is readable by a cluster owner or a " +
				"developer only. Provision as one of them, or choose the " +
				AdapterCustomerTag + " adapter, which needs no plan at all",
		}
	}
	return nil
}

func (a *shopifyB2BAdapter) Provision(ctx context.Context, call Caller, g Grant) (Outcome, error) {
	if call == nil {
		return Outcome{}, fmt.Errorf("wholesale: the %s adapter has no caller", AdapterShopifyB2B)
	}
	reply, err := call.Call(ctx, "shopifyProvisionWholesale", map[string]any{
		"storeId":       g.StoreID,
		"applicationId": g.ApplicationID,
		"companyName":   g.CompanyName,
		"buyerName":     g.BuyerName,
		"buyerEmail":    g.BuyerEmail,
	})
	if err != nil {
		return Outcome{}, err
	}
	return outcomeFromReply(AdapterShopifyB2B, reply)
}

func (a *shopifyB2BAdapter) Revoke(ctx context.Context, call Caller, g Grant) (Outcome, error) {
	if call == nil {
		return Outcome{}, fmt.Errorf("wholesale: the %s adapter has no caller", AdapterShopifyB2B)
	}
	if strings.TrimSpace(g.Reference) == "" {
		// A REFUSAL, not a retry. There is no reference to undo and there
		// never will be one, so recording this as pending would have the
		// pack try again for ever against nothing.
		return Outcome{}, &AdapterRefusal{
			Adapter: AdapterShopifyB2B,
			Reason: "this entitlement carries no Shopify reference, so there is nothing to " +
				"draft. Revoke the catalog in the Shopify admin; the decision is already recorded here",
		}
	}
	reply, err := call.Call(ctx, "shopifyRevokeWholesale", map[string]any{
		"storeId":   g.StoreID,
		"reference": g.Reference,
	})
	if err != nil {
		return Outcome{}, err
	}
	out, err := outcomeFromReply(AdapterShopifyB2B, reply)
	if err != nil {
		return Outcome{}, err
	}
	// THE REFERENCE SURVIVES A REVOKE. A drafted catalog can be made active
	// again, and a merchant who re-approves the same buyer should reach the
	// same company rather than a second one.
	if out.Reference == "" {
		out.Reference = g.Reference
	}
	return out, nil
}

// outcomeFromReply reads a connector reply, turning a refusal into one.
//
// THE STATUS FIELD IS THE CONTRACT. An integration capability answers
// {status: "refused", reason} with a NIL error when a store simply cannot
// do the thing, precisely so the pack can tell that apart from an outage --
// the first is recorded `failed` and final, the second `pending` and
// retried. A reply with no status at all is treated as a failure rather
// than a success, which is the fail-closed reading: recording an
// entitlement as granted on the strength of a reply nobody could parse is
// how a buyer is told they have trade prices they do not have.
func outcomeFromReply(adapter string, reply map[string]any) (Outcome, error) {
	status := trimmed(reply["status"])
	switch status {
	case "ok":
		return Outcome{
			Reference: trimmed(reply["reference"]),
			Detail:    trimmed(reply["detail"]),
		}, nil
	case "refused":
		return Outcome{}, &AdapterRefusal{Adapter: adapter, Reason: trimmed(reply["reason"])}
	case "unconfigured":
		return Outcome{}, &AdapterRefusal{
			Adapter: adapter,
			Reason: "the Shopify connector is not configured on this cluster, so there is no " +
				"store to provision against",
		}
	case "":
		return Outcome{}, fmt.Errorf("wholesale: %s got a reply with no status; the "+
			"entitlement is not recorded as granted on the strength of an answer nobody could read", adapter)
	default:
		return Outcome{}, fmt.Errorf("wholesale: %s got status %q: %s",
			adapter, status, trimmed(reply["reason"]))
	}
}
