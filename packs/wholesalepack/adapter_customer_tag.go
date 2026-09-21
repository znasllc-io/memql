package wholesalepack

import (
	"context"
	"fmt"
	"strings"
)

// adapter_customer_tag.go -- THE PLAN-INDEPENDENT ADAPTER (epic memql#5533,
// issue memql#5559).
//
// ===========================================================================
// THE DECISION THE ISSUE ASKS FOR, AND THE ARGUMENT FOR IT
// ===========================================================================
//
// "A pack that requires one Shopify tier is not client-agnostic" (design
// record, D4). This adapter grants wholesale prices on a store with no B2B
// at all. The record put two candidates up and asked that the choice be
// weighed and RECORDED:
//
//	(a) a customer TAG that price rules or a discount function key on --
//	    "cheap, and leaks if the tag is guessable";
//	(b) DRAFT ORDERS priced in MemQL and sent through the push channel,
//	    which dsl/commerce's quote already does on acceptance, and which
//	    "puts every wholesale order through a person".
//
// CHOSEN: (a), the customer tag. The argument is not that it is cheaper.
// It is that (b) IS NOT AN ENTITLEMENT ADAPTER AT ALL.
//
// Issue memql#5557 fixes what an adapter has to be able to do: "a
// provisioning failure leaves the application approved and the entitlement
// pending with its error". An adapter is called AT APPROVAL TIME and must
// report success or failure THEN. The tag adapter can: writing the tag is
// one call, it is idempotent, revoking is removing it, and the pack's
// granted/revoked state maps one-to-one onto the tag's presence.
//
// Draft orders cannot. Nothing is provisioned at approval time; the
// entitlement is only realised later, when an order happens. An entitlement
// "provisioned" through a draft-order adapter would sit `pending` for ever
// with no error and no success -- and `pending` is precisely the state this
// pack uses to mean "the push has not landed yet", so every wholesale buyer
// would look like a failed grant. Draft orders are an ORDER-PLACEMENT
// mechanism, they belong to the client's own process, and dsl/commerce's
// quote plus integrations/shopify's CreateDraftOrderFromQuote already carry
// them end to end. Nothing is lost by declining to call that an entitlement.
//
// # THE LEAK IS ANSWERED, NOT ACCEPTED
//
// The record's worry about (a) is that it "leaks if the tag is guessable".
// Taken literally that worry is misplaced, and saying why matters because
// somebody will re-raise it: A SHOPPER CANNOT ASSIGN THEMSELVES A TAG.
// Customer tags are written through the Admin API under the store's own
// credentials, which is what integrations/shopify does here; there is no
// storefront surface that sets one. Guessing the tag's NAME gets an
// attacker nothing, because knowing it does not let them have it.
//
// What CAN leak is a DISCOUNT CODE. If a merchant implements the price side
// as a code-based discount and hands the code out, the code travels and the
// tag becomes decorative. So this adapter states the constraint it cannot
// enforce, plainly and in one place:
//
//	THE DISCOUNT KEYED ON THIS TAG MUST BE AN AUTOMATIC DISCOUNT
//	CONDITIONED ON THE CUSTOMER. NEVER A CODE.
//
// The pack cannot enforce that -- creating a discount on a merchant's
// behalf is a far larger act than an approval, and this adapter
// deliberately does not do it (integrations/shopify/wholesale.go says so
// too). So it is documented here, in the pack guide, and in the adapter's
// own Detail string, which is the honest treatment of a constraint that
// lives outside the software.

// customerTagAdapter grants by tagging the buyer's customer record.
type customerTagAdapter struct {
	// tag is what this adapter writes. Held on the adapter rather than read
	// from settings so a cluster's operator chooses it once at registration
	// -- a per-store tag would mean a merchant could rename it between a
	// grant and a revoke, and the revoke would then remove a tag the buyer
	// never had. The entitlement's reference carries the tag that was
	// actually written, which is what a revoke uses.
	tag string
}

// DefaultWholesaleTag is what the adapter writes when none is named.
//
// PREFIXED WITH OURS, so a merchant can tell at a glance in the Shopify
// admin which tags MemQL maintains, and so a revoke can never remove a tag
// somebody else put there.
const DefaultWholesaleTag = "memql-wholesale"

// NewCustomerTagAdapter builds the plan-independent adapter. An empty tag
// means DefaultWholesaleTag.
func NewCustomerTagAdapter(tag string) Adapter {
	t := strings.TrimSpace(tag)
	if t == "" {
		t = DefaultWholesaleTag
	}
	return &customerTagAdapter{tag: t}
}

func (a *customerTagAdapter) Name() string { return AdapterCustomerTag }

// Available admits every plan, including an unknown one.
//
// THIS METHOD RETURNING NIL UNCONDITIONALLY IS THE WHOLE POINT OF THE
// ADAPTER, and it is why the seam takes a plan at all: shopifyB2B refuses
// when it cannot read one, this one does not care, and a merchant on any
// tier -- or an operator who cannot read the store row -- can still entitle
// a buyer. "Plan-independent" is this function.
func (a *customerTagAdapter) Available(string) error { return nil }

func (a *customerTagAdapter) Provision(ctx context.Context, call Caller, g Grant) (Outcome, error) {
	if call == nil {
		return Outcome{}, fmt.Errorf("wholesale: the %s adapter has no caller", AdapterCustomerTag)
	}
	reply, err := call.Call(ctx, "shopifyTagWholesaleCustomer", map[string]any{
		"storeId":    g.StoreID,
		"buyerEmail": g.BuyerEmail,
		"tag":        a.tag,
	})
	if err != nil {
		return Outcome{}, err
	}
	out, err := outcomeFromReply(AdapterCustomerTag, reply)
	if err != nil {
		return Outcome{}, err
	}
	// THE CONSTRAINT TRAVELS WITH THE ROW. An operator reading this
	// entitlement in six months is the person who needs to know that the
	// tag alone prices nothing and that a code-based discount would leak --
	// and a sentence on the row reaches them where a comment in this file
	// does not.
	out.Detail = strings.TrimSpace(out.Detail + " -- the tag entitles nobody on its own: " +
		"key an AUTOMATIC discount on it, conditioned on the customer, never a discount code")
	return out, nil
}

func (a *customerTagAdapter) Revoke(ctx context.Context, call Caller, g Grant) (Outcome, error) {
	if call == nil {
		return Outcome{}, fmt.Errorf("wholesale: the %s adapter has no caller", AdapterCustomerTag)
	}
	reply, err := call.Call(ctx, "shopifyUntagWholesaleCustomer", map[string]any{
		"storeId":    g.StoreID,
		"buyerEmail": g.BuyerEmail,
		// THE TAG THAT WAS GRANTED, read back off the reference, not the
		// one this adapter is configured with today. An operator who
		// re-registered the adapter with a different tag between the grant
		// and the revoke would otherwise remove a tag this buyer never had
		// and leave the one they do have in place -- a revoke that reports
		// success and revokes nothing.
		"tag": tagFromReference(g.Reference, a.tag),
	})
	if err != nil {
		return Outcome{}, err
	}
	return outcomeFromReply(AdapterCustomerTag, reply)
}

// tagFromReference reads the tag out of what a provision recorded.
//
// The reference is "customer=<gid>;tag=<tag>". A reference from before that
// shape, or one that cannot be read, falls back to the adapter's configured
// tag -- removing the likely tag is better than removing none, and the
// alternative is an entitlement nobody can revoke through the pack.
func tagFromReference(reference, fallback string) string {
	for _, part := range strings.Split(reference, ";") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if found && key == "tag" && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return fallback
}
