package shopify

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// wholesale_capabilities.go -- THE DSL SEAM FOR THE B2B WRITES (epic
// memql#5533, issues memql#5558 and memql#5559).
//
// # Why these are capabilities rather than a Go call from the pack
//
// The wholesale pack needs Shopify to grant an entitlement, and it must
// not import this package to get it. That is the three-word rule, not a
// preference: an INTEGRATION is Go that talks OUT to another service,
// "reached from DSL via @executor(\"integration.<name>.*\")"; a PACK is a
// client-agnostic product feature. A pack that imported an integration
// would be a pack that only works on Shopify, which is the one thing
// packs/wholesalepack exists not to be.
//
// So the seam is a construct NAME. The pack's adapters call
// shopifyProvisionWholesale through the engine, exactly as any .memql body
// would, and the pack's own Go knows nothing about this package -- it knows
// four builtin names and the shape of their replies. A client that wants a
// different commerce platform writes its own adapter against its own
// integration's builtins and registers it; nothing here has to change.
//
// # Every reply is a map, never an error, when the failure is the store's
//
// A capability that returned an error for "this store is on a plan without
// the catalogs you need" would make the pack unable to tell that apart from
// "Shopify is down", and the two get recorded differently: a refusal is
// `failed` and final, a transient failure is `pending` and retried. So a
// refusal comes back as {status: "refused", reason: ...} with a nil error,
// and a nil error with status != "ok" is what the pack reads as a refusal.

// wholesaleCapabilities is appended to the connector's DSL-callable
// surface by Capabilities().
func (i *Integration) wholesaleCapabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name: "provisionWholesale",
			Description: "Create or find this buyer's B2B account: a company stamped with the " +
				"MemQL application id, its location, the buyer as a company contact, and an " +
				"ACTIVE catalog with a price list. Idempotent -- a second call for one " +
				"application finds the first company rather than making a twin. Refuses " +
				"BEFORE creating anything when the store is below Plus and already holds its " +
				"three company-location catalogs.",
			Handler: i.handleProvisionWholesale,
			ArgsSchema: map[string]string{
				"storeId":         "string - the store to provision on",
				"applicationId":   "string - the MemQL application; becomes the company externalId",
				"companyName":     "string - the business applying",
				"buyerName":       "string (optional) - the applicant",
				"buyerEmail":      "string - the applicant's email; the customer to entitle",
				"catalogTitle":    "string (optional) - defaults to '<company> wholesale'",
				"percentOff":      "integer (optional) - whole-number percentage off published prices",
				"currencyCode":    "string (optional) - the price list currency",
			},
		},
		{
			Name: "revokeWholesale",
			Description: "End a B2B entitlement by setting its catalog to DRAFT. DELETES " +
				"NOTHING: the company, its location, its contact, the price list and every " +
				"order placed under them are untouched, because revoking trade terms is " +
				"ending a discount rather than erasing a customer.",
			Handler: i.handleRevokeWholesale,
			ArgsSchema: map[string]string{
				"storeId":   "string - the store",
				"reference": "string - the encoded refs a provision returned",
			},
		},
		{
			Name: "tagWholesaleCustomer",
			Description: "The plan-independent grant: add one tag to the buyer's customer " +
				"record. Writes the tag and nothing else -- what the tag DOES is the " +
				"merchant's own automatic discount, which this connector deliberately does " +
				"not create on their behalf.",
			Handler: i.handleTagWholesaleCustomer,
			ArgsSchema: map[string]string{
				"storeId":    "string - the store",
				"buyerEmail": "string - the customer to tag",
				"tag":        "string (optional) - defaults to memql-wholesale",
			},
		},
		{
			Name:        "untagWholesaleCustomer",
			Description: "The plan-independent revoke: remove that one tag.",
			Handler:     i.handleUntagWholesaleCustomer,
			ArgsSchema: map[string]string{
				"storeId":    "string - the store",
				"buyerEmail": "string - the customer to untag",
				"tag":        "string (optional) - defaults to memql-wholesale",
			},
		},
		{
			Name: "wholesaleCatalogHeadroom",
			Description: "How many more company-location catalogs this store may hold: -1 on " +
				"Plus, which is unlimited, and otherwise three minus what it already has. The " +
				"read that lets an adapter REPORT the ceiling instead of discovering it at the " +
				"fourth grant, in a merchant's live store.",
			Handler: i.handleWholesaleCatalogHeadroom,
			ArgsSchema: map[string]string{
				"storeId": "string - the store to measure",
			},
		},
	}
}

func (i *Integration) handleProvisionWholesale(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	store, err := c.resolveStoreArg(ctx, argString(args, "storeId"))
	if err != nil {
		return nil, err
	}
	refs, err := c.ProvisionWholesaleAccount(ctx, store, WholesaleAccountInput{
		ApplicationID:          argString(args, "applicationId"),
		CompanyName:            argString(args, "companyName"),
		BuyerName:              argString(args, "buyerName"),
		BuyerEmail:             argString(args, "buyerEmail"),
		CatalogTitle:           argString(args, "catalogTitle"),
		PriceAdjustmentPercent: argInt(args, "percentOff", 0),
		CurrencyCode:           argString(args, "currencyCode"),
	})
	if err != nil {
		if node, refused := refusalNode(err); refused {
			return node, nil
		}
		return nil, err
	}
	return resultNode("shopify", map[string]any{
		"status":    "ok",
		"storeId":   store.ID,
		"reference": refs.Encode(),
		"detail": fmt.Sprintf("company %s with catalog %s on %s",
			refs.CompanyGID, refs.CatalogGID, store.Domain),
	})
}

func (i *Integration) handleRevokeWholesale(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	store, err := c.resolveStoreArg(ctx, argString(args, "storeId"))
	if err != nil {
		return nil, err
	}
	reference := argString(args, "reference")
	refs := DecodeWholesaleRefs(reference)
	if err := c.RevokeWholesaleAccount(ctx, store, refs); err != nil {
		if node, refused := refusalNode(err); refused {
			return node, nil
		}
		return nil, err
	}
	return resultNode("shopify", map[string]any{
		"status":    "ok",
		"storeId":   store.ID,
		"reference": reference,
		"detail":    "catalog " + refs.CatalogGID + " set to DRAFT; nothing was deleted",
	})
}

func (i *Integration) handleTagWholesaleCustomer(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	store, err := c.resolveStoreArg(ctx, argString(args, "storeId"))
	if err != nil {
		return nil, err
	}
	tag := normaliseTag(argString(args, "tag"))
	customerGID, err := c.TagWholesaleCustomer(ctx, store, argString(args, "buyerEmail"), tag)
	if err != nil {
		if node, refused := refusalNode(err); refused {
			return node, nil
		}
		return nil, err
	}
	return resultNode("shopify", map[string]any{
		"status":  "ok",
		"storeId": store.ID,
		// THE REFERENCE CARRIES BOTH, because a revoke needs to know which
		// tag was written as well as to whom: a merchant who changed the
		// tag in settings after granting would otherwise have this
		// entitlement revoke the new tag off a customer who never had it.
		"reference": "customer=" + customerGID + ";tag=" + tag,
		"detail":    "tagged " + tag + " on " + store.Domain,
	})
}

func (i *Integration) handleUntagWholesaleCustomer(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	store, err := c.resolveStoreArg(ctx, argString(args, "storeId"))
	if err != nil {
		return nil, err
	}
	tag := normaliseTag(argString(args, "tag"))
	if err := c.UntagWholesaleCustomer(ctx, store, argString(args, "buyerEmail"), tag); err != nil {
		if node, refused := refusalNode(err); refused {
			return node, nil
		}
		return nil, err
	}
	return resultNode("shopify", map[string]any{
		"status":  "ok",
		"storeId": store.ID,
		"detail":  "removed " + tag + " on " + store.Domain,
	})
}

func (i *Integration) handleWholesaleCatalogHeadroom(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	store, err := c.resolveStoreArg(ctx, argString(args, "storeId"))
	if err != nil {
		return nil, err
	}
	headroom, err := c.WholesaleCatalogHeadroom(ctx, store)
	if err != nil {
		return nil, err
	}
	return resultNode("shopify", map[string]any{
		"status":    "ok",
		"storeId":   store.ID,
		"headroom":  headroom,
		"unlimited": headroom < 0,
		"plan":      store.Plan,
	})
}

// refusedError is an error that will fail identically for ever.
//
// AN INTERFACE RATHER THAN A TYPE SWITCH, so a second refusal kind is a
// method on that type rather than another case here.
type refusedError interface {
	error
	Refused() bool
}

// refusalNode turns a permanent refusal into a reply the pack can record.
//
// A REFUSAL IS NOT AN ERROR TO THE CALLER. The pack has to tell "this store
// cannot do this, and re-trying will never change that" apart from "the
// push failed, try again", because it records the first as `failed` and the
// second as `pending`. Returning a Go error for both would collapse them,
// and the pack would go on retrying a ceiling for ever.
func refusalNode(err error) ([]memorynodes.MemoryNode, bool) {
	var refused refusedError
	if !asRefused(err, &refused) {
		return nil, false
	}
	node, buildErr := resultNode("shopify", map[string]any{
		"status": "refused",
		"reason": refused.Error(),
	})
	if buildErr != nil {
		return nil, false
	}
	return node, true
}

func asRefused(err error, target *refusedError) bool {
	for err != nil {
		if r, ok := err.(refusedError); ok && r.Refused() {
			*target = r
			return true
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}
