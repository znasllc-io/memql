package shopify

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
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
//
// # Who may call them
//
// The declarations carry no @requiresCapability, and a builtin without one
// has no rank floor: every signed-in caller of any role reaches the handler.
// So the FIRST STATEMENT of each handler is requireWholesaleCaller: the call
// must carry internal origin and the wholesale pack's mark, which the pack's
// engineCaller sets on its one call AFTER the pack's own checks have decided
// who the caller is and what they may do. Everybody else -- the store's owner,
// a cluster owner and a registered automation's step included -- is refused before the store is read or Shopify is asked, and
// reaches a grant through the pack (an approval, Retry access), never around
// it. A refusal here is an error, not a refused reply: nothing was decided
// about the store.

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
				"three company-location catalogs. " + wholesaleCallers,
			Handler: i.handleProvisionWholesale,
			ArgsSchema: map[string]string{
				"storeId":       "string - the store to provision on",
				"applicationId": "string - the MemQL application; becomes the company externalId",
				"companyName":   "string - the business applying",
				"buyerName":     "string (optional) - the applicant",
				"buyerEmail":    "string - the applicant's email; the customer to entitle",
				"catalogTitle":  "string (optional) - defaults to '<company> wholesale'",
				"percentOff":    "integer (optional) - whole-number percentage off published prices",
				"currencyCode":  "string (optional) - the price list currency",
			},
		},
		{
			Name: "revokeWholesale",
			Description: "End a B2B entitlement by setting its catalog to DRAFT. DELETES " +
				"NOTHING: the company, its location, its contact, the price list and every " +
				"order placed under them are untouched, because revoking trade terms is " +
				"ending a discount rather than erasing a customer. A reference that names no " +
				"catalog is a success that sends nothing, since there is no catalog it can name to " +
				"draft; a B2B grant that failed partway can still have left an active catalog, which " +
				"the merchant drafts in the Shopify admin. " + wholesaleCallers,
			Handler: i.handleRevokeWholesale,
			ArgsSchema: map[string]string{
				"storeId":   "string - the store",
				"reference": "string - the encoded refs a provision returned",
			},
		},
		{
			Name: "tagWholesaleCustomer",
			Description: "The plan-independent grant: add one tag to the buyer's customer " +
				"record, creating the customer (the address and the name, no marketing consent) " +
				"when the store has none for the address. Writes the customer and the tag and " +
				"nothing else -- what the tag DOES is the merchant's own automatic discount, " +
				"which this connector deliberately does not create on their behalf. A tag the " +
				"customer already carries is not sent again. " + wholesaleCallers,
			Handler: i.handleTagWholesaleCustomer,
			ArgsSchema: map[string]string{
				"storeId":    "string - the store",
				"buyerEmail": "string - the customer to tag; created when the store has none",
				"buyerName":  "string (optional) - the applicant, for a customer created here",
				"tag":        "string (optional) - defaults to memql-wholesale",
			},
		},
		{
			Name: "untagWholesaleCustomer",
			Description: "The plan-independent revoke: remove that one tag from the customer the " +
				"grant recorded (customerId), read by id, when it carries it. Only when no customer " +
				"is named is the address used, and then the tag comes off every customer holding the " +
				"address that carries it. A customer that no longer exists or no longer carries the " +
				"tag, or an address nobody holds, is a success that sends nothing more. " + wholesaleCallers,
			Handler: i.handleUntagWholesaleCustomer,
			ArgsSchema: map[string]string{
				"storeId":    "string - the store",
				"customerId": "string (optional) - the customer the grant recorded",
				"buyerEmail": "string - the address, used only when no customerId is named",
				"tag":        "string (optional) - defaults to memql-wholesale",
			},
		},
		{
			Name: "wholesaleCatalogHeadroom",
			Description: "How many more company-location catalogs this store may hold: -1 on " +
				"Plus, which is unlimited, and otherwise three minus what it already has. The " +
				"read that lets an adapter REPORT the ceiling instead of discovering it at the " +
				"fourth grant, in a merchant's live store. " + wholesaleCallers,
			Handler: i.handleWholesaleCatalogHeadroom,
			ArgsSchema: map[string]string{
				"storeId": "string - the store to measure",
			},
		},
	}
}

// wholesaleCallers is who may call a wholesale builtin, as each description
// says it.
const wholesaleCallers = "Called by the wholesale pack alone: the call must carry internal origin AND the " +
	"wholesale pack's mark, which only the pack's Go sets, on its one call, after its own checks. Any other call -- " +
	"a client's, or a registered automation's step, which carries internal origin and no mark -- is refused " +
	"before the store is read or Shopify is asked."

// wholesalePackCaller is the pack whose mark these builtins admit
// (auth.ContextWithPackCaller).
const wholesalePackCaller = "wholesale"

// requireWholesaleCaller is the rule for all five wholesale builtins and the
// first statement of each handler: internal origin AND the wholesale pack's
// mark, or the refusal. Nothing is read before it answers -- not the store row
// -- and nothing is sent to Shopify.
//
// INTERNAL ORIGIN ALONE IS NOT THE PACK (ruling 57). The automation executor
// stamps it on every step of every registered automation, a client package's
// deployed domain included, so an automation of any package could otherwise
// tag any customer of any store the connector holds, around the pack's checks.
// The mark is a context value only Go sets (a root test allowlists who), so a
// DSL step cannot carry it; and it is no authority without internal origin.
//
// ONE FUNCTION, so the rule is in one place, and THE ONLY READER OF THE MARK:
// the root gate fails if anything else reads it, so nothing these handlers
// call can treat the mark as authority.
func requireWholesaleCaller(ctx context.Context, construct string) error {
	if auth.OriginFromContext(ctx).IsInternal() && auth.PackCallerFromContext(ctx) == wholesalePackCaller {
		return nil
	}
	return fmt.Errorf("shopify: %s is called by the wholesale pack, never by a client", construct)
}

func (i *Integration) handleProvisionWholesale(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireWholesaleCaller(ctx, "shopifyProvisionWholesale"); err != nil {
		return nil, err
	}
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
	if err := requireWholesaleCaller(ctx, "shopifyRevokeWholesale"); err != nil {
		return nil, err
	}
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
	detail := "catalog " + refs.CatalogGID + " set to DRAFT; nothing was deleted"
	if strings.TrimSpace(refs.CatalogGID) == "" {
		detail = "this entitlement names no catalog on " + store.Domain + ", so nothing was drafted; a grant that " +
			"failed partway may have left a catalog, which is drafted in the Shopify admin"
	}
	return resultNode("shopify", map[string]any{
		"status":    "ok",
		"storeId":   store.ID,
		"reference": reference,
		"detail":    detail,
	})
}

func (i *Integration) handleTagWholesaleCustomer(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireWholesaleCaller(ctx, "shopifyTagWholesaleCustomer"); err != nil {
		return nil, err
	}
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	store, err := c.resolveStoreArg(ctx, argString(args, "storeId"))
	if err != nil {
		return nil, err
	}
	tag := normaliseTag(argString(args, "tag"))
	res, err := c.tagWholesaleCustomer(ctx, store, argString(args, "buyerEmail"), argString(args, "buyerName"), tag)
	if err != nil {
		if node, refused := refusalNode(err); refused {
			return node, nil
		}
		return nil, err
	}
	detail := "tagged " + tag + " on " + store.Domain
	switch {
	case res.created:
		detail = "created the buyer's customer and tagged " + tag + " on " + store.Domain
	case !res.tagged:
		detail = "the buyer's customer already carries " + tag + " on " + store.Domain
	}
	if res.customer.matches > 1 {
		detail += fmt.Sprintf("; %d customers on the store have this address, and the one Shopify lists first was used",
			res.customer.matches)
	}
	return resultNode("shopify", map[string]any{
		"status":  "ok",
		"storeId": store.ID,
		// THE REFERENCE CARRIES BOTH, because a revoke needs to know which
		// tag was written as well as to whom: a merchant who changed the
		// tag in settings after granting would otherwise have this
		// entitlement revoke the new tag off a customer who never had it.
		"reference":       "customer=" + res.customer.id + ";tag=" + tag,
		"customerId":      res.customer.id,
		"customerCreated": res.created,
		"detail":          detail,
	})
}

func (i *Integration) handleUntagWholesaleCustomer(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireWholesaleCaller(ctx, "shopifyUntagWholesaleCustomer"); err != nil {
		return nil, err
	}
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	store, err := c.resolveStoreArg(ctx, argString(args, "storeId"))
	if err != nil {
		return nil, err
	}
	tag := normaliseTag(argString(args, "tag"))
	if err := c.UntagWholesaleCustomer(ctx, store, argString(args, "customerId"), argString(args, "buyerEmail"), tag); err != nil {
		if node, refused := refusalNode(err); refused {
			return node, nil
		}
		return nil, err
	}
	return resultNode("shopify", map[string]any{
		"status":  "ok",
		"storeId": store.ID,
		"detail":  tag + " is off the buyer's customer on " + store.Domain,
		// customerId is not echoed: the pack keeps the reference it sent.
	})
}

func (i *Integration) handleWholesaleCatalogHeadroom(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireWholesaleCaller(ctx, "shopifyWholesaleCatalogHeadroom"); err != nil {
		return nil, err
	}
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

// codedRefusal is a refusal that names its cause for the pack's entitlement
// row: customer_refused, scope_missing or plan_ceiling.
type codedRefusal interface {
	RefusalCode() string
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
	reply := map[string]any{
		"status": "refused",
		"reason": refused.Error(),
	}
	if coded, ok := refused.(codedRefusal); ok && coded.RefusalCode() != "" {
		reply["code"] = coded.RefusalCode()
	}
	node, buildErr := resultNode("shopify", reply)
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
