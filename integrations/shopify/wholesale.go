package shopify

import (
	"context"
	"fmt"
	"strings"
)

// wholesale.go -- THE B2B WRITES THE PUSH CHANNEL ANTICIPATED (epic
// memql#5533, issues memql#5558 and memql#5559).
//
// propagate.go's own header named this file before it existed: "B2B
// catalogs and price lists are the next flip, and they get their own review
// when the wholesale application asks -- not a quiet extension of this one."
// This is that flip, and it is deliberately a separate file with its own
// argument rather than more cases in the metafield projection.
//
// # What makes these different from every other write this connector makes
//
// Everything propagate.go pushes is ADDITIVE and invisible to a shopper: a
// metafield in a namespace nobody else uses, changing no price, no
// inventory and no order. These calls create real B2B objects in a
// merchant's live store, and one of them -- the catalog with its price list
// -- CHANGES WHAT SOMEBODY PAYS. So three rules apply here and nowhere else
// in the package:
//
//   - NOTHING IS DELETED, EVER. Revoking an entitlement sets the catalog to
//     DRAFT; it does not delete the catalog, the price list, the company or
//     the contact. A company carries the buyer's order history, and a
//     merchant who revokes trade terms is ending a discount, not erasing a
//     customer. Draft is reversible in one call and destroys nothing.
//   - THE CATALOG CEILING IS REPORTED, NOT DISCOVERED. Since 2026-04-02
//     companies, payment terms, volume pricing and up to three catalogs are
//     available BELOW Plus (the connector record, 1.3). An adapter that
//     found that ceiling by failing at the fourth grant would find it in a
//     merchant's live store, with an approved application and nothing to
//     show for it. Headroom is read first.
//   - EVERY OBJECT CARRIES ITS EXTERNAL ID. A company created here is
//     stamped with the MemQL application id, so a second call for the same
//     application finds the first rather than making a twin. That is what
//     makes Provision idempotent, which the pack's seam requires because it
//     retries after a failure.
//
// # Verification status, stated plainly
//
// The mutation documents below are written against Admin API 2026-07 (the
// pinned generated.APIVersion) and are NOT verified by live introspection
// the way the connector record's claims are. They are exercised against a
// development store by wholesale_live_test.go, which self-skips without
// credentials. Anything this file asserts about Shopify's behaviour beyond
// the shape of these documents is UNVERIFIED, by the same convention the
// connector record uses.

// WholesaleTagDefault is the customer tag the plan-independent adapter
// writes when the merchant names none.
//
// PREFIXED WITH OURS so a merchant can tell at a glance in the Shopify
// admin which tags MemQL maintains, and so this connector never removes a
// tag somebody else put there.
const WholesaleTagDefault = "memql-wholesale"

// wholesaleExternalIDPrefix stamps a company with the application it came
// from. It is what makes a second Provision find the first company.
const wholesaleExternalIDPrefix = "memql-wholesale:"

// maxCatalogsBelowPlus is Shopify's ceiling for a store that is not on
// Plus, verified in the connector record (1.3) on 2026-08-23: "companies,
// payment terms, volume pricing and up to three catalogs (via Markets) are
// available below Plus; Plus keeps unlimited catalogs".
const maxCatalogsBelowPlus = 3

// WholesaleAccountInput is one buyer to entitle.
type WholesaleAccountInput struct {
	// ApplicationID is the MemQL row this came from. It becomes the
	// company's externalId, which is how a retry finds the first company.
	ApplicationID string
	CompanyName   string
	BuyerName     string
	BuyerEmail    string
	// CatalogTitle names the catalog created for this company's location.
	// Empty means one derived from the company name.
	CatalogTitle string
	// PriceAdjustmentPercent is how much off the published price this
	// buyer gets, as a whole-number percentage. Zero means the catalog is
	// created with no adjustment -- a catalog that exists and prices
	// nothing differently, which is a legitimate starting point for a
	// merchant who sets their own prices afterwards.
	PriceAdjustmentPercent int
	CurrencyCode           string
}

// WholesaleAccountRefs is everything the pack must hold to undo this.
//
// FIVE GIDS AND NOT A CREDENTIAL. The pack stores this as an opaque
// reference string and never parses it; this struct exists so THIS package
// can.
type WholesaleAccountRefs struct {
	CompanyGID         string
	CompanyLocationGID string
	CompanyContactGID  string
	CatalogGID         string
	PriceListGID       string
	CustomerGID        string
}

// Encode renders the refs as the opaque string the pack stores.
func (r WholesaleAccountRefs) Encode() string {
	parts := []string{
		"company=" + r.CompanyGID,
		"location=" + r.CompanyLocationGID,
		"contact=" + r.CompanyContactGID,
		"catalog=" + r.CatalogGID,
		"priceList=" + r.PriceListGID,
		"customer=" + r.CustomerGID,
	}
	return strings.Join(parts, ";")
}

// DecodeWholesaleRefs reads back what Encode wrote.
//
// A MISSING KEY IS EMPTY, NOT AN ERROR. A reference written by an older
// build carries fewer keys, and refusing to parse it would make an
// entitlement unrevokable through the pack -- which is a worse outcome than
// revoking what can be found.
func DecodeWholesaleRefs(encoded string) WholesaleAccountRefs {
	var refs WholesaleAccountRefs
	for _, part := range strings.Split(encoded, ";") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch key {
		case "company":
			refs.CompanyGID = value
		case "location":
			refs.CompanyLocationGID = value
		case "contact":
			refs.CompanyContactGID = value
		case "catalog":
			refs.CatalogGID = value
		case "priceList":
			refs.PriceListGID = value
		case "customer":
			refs.CustomerGID = value
		}
	}
	return refs
}

// PlanHasUnlimitedCatalogs reports whether this plan escapes the ceiling.
//
// A SUBSTRING MATCH ON "plus", and the looseness is deliberate:
// v1:shopify:store.plan is a free string mirrored from whatever Shopify's
// shop.plan.displayName says, which has been "Shopify Plus", "Plus" and
// "Plus Partner Sandbox" at different times. Matching loosely errs toward
// letting a Plus store past a ceiling it does not have; matching strictly
// would err toward stopping one at three catalogs it is entitled to
// exceed, and a merchant paying for Plus being told they have hit a limit
// that does not apply to them is the worse failure.
//
// An EMPTY plan is not Plus, which is the fail-closed reading: an unknown
// plan gets the ceiling.
func PlanHasUnlimitedCatalogs(plan string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(plan)), "plus")
}

// WholesaleCatalogHeadroom is how many more company-location catalogs this
// store may hold.
//
// -1 MEANS UNLIMITED, which is a sentinel rather than a large number
// because "unlimited" is a different fact from "a lot" and a caller that
// treated a large number as unlimited would be wrong exactly once.
func (c *Connector) WholesaleCatalogHeadroom(ctx context.Context, store Store) (int, error) {
	if PlanHasUnlimitedCatalogs(store.Plan) {
		return -1, nil
	}
	resp, err := c.adminCall(ctx, store, catalogsCountQuery, "ShopifyCatalogsCount", nil)
	if err != nil {
		return 0, err
	}
	var decoded struct {
		CatalogsCount struct {
			Count int `json:"count"`
		} `json:"catalogsCount"`
	}
	if err := resp.DecodeInto(&decoded); err != nil {
		return 0, err
	}
	headroom := maxCatalogsBelowPlus - decoded.CatalogsCount.Count
	if headroom < 0 {
		headroom = 0
	}
	return headroom, nil
}

const catalogsCountQuery = `query ShopifyCatalogsCount {
  catalogsCount(type: COMPANY_LOCATION) { count }
}`

// ProvisionWholesaleAccount creates or finds this buyer's B2B account.
//
// IDEMPOTENT BY EXTERNAL ID. The pack calls this again after a failure, so
// every step first looks for what a previous run may have left: a company
// stamped with this application id, a customer with this email, a catalog
// already on the location. Making a second company for one application
// would be the kind of mess a merchant has to clean up by hand.
func (c *Connector) ProvisionWholesaleAccount(ctx context.Context, store Store,
	in WholesaleAccountInput) (WholesaleAccountRefs, error) {
	var refs WholesaleAccountRefs
	if strings.TrimSpace(in.ApplicationID) == "" {
		return refs, fmt.Errorf("shopify: an application id is required; it is the company's externalId and the only thing that makes this call idempotent")
	}
	if strings.TrimSpace(in.CompanyName) == "" || strings.TrimSpace(in.BuyerEmail) == "" {
		return refs, fmt.Errorf("shopify: a company name and a buyer email are required to provision a B2B account")
	}

	// THE CEILING FIRST, before anything is created. Reaching it halfway
	// through would leave a company and a contact with no catalog -- an
	// account that looks provisioned and prices nothing.
	headroom, err := c.WholesaleCatalogHeadroom(ctx, store)
	if err != nil {
		return refs, err
	}
	if headroom == 0 {
		return refs, &WholesaleCeilingError{StoreID: store.ID, Plan: store.Plan, Limit: maxCatalogsBelowPlus}
	}

	customerGID, err := c.customerByEmail(ctx, store, in.BuyerEmail)
	if err != nil {
		return refs, err
	}
	if customerGID == "" {
		customerGID, err = c.createCustomer(ctx, store, in.BuyerName, in.BuyerEmail)
		if err != nil {
			return refs, err
		}
	}
	refs.CustomerGID = customerGID

	externalID := wholesaleExternalIDPrefix + in.ApplicationID
	company, location, err := c.companyByExternalID(ctx, store, externalID)
	if err != nil {
		return refs, err
	}
	if company == "" {
		company, location, err = c.createCompany(ctx, store, in, externalID)
		if err != nil {
			return refs, err
		}
	}
	refs.CompanyGID = company
	refs.CompanyLocationGID = location

	contact, err := c.assignCustomerAsContact(ctx, store, company, customerGID)
	if err != nil {
		return refs, err
	}
	refs.CompanyContactGID = contact

	title := strings.TrimSpace(in.CatalogTitle)
	if title == "" {
		title = in.CompanyName + " wholesale"
	}
	catalog, err := c.createCatalog(ctx, store, title, location)
	if err != nil {
		return refs, err
	}
	refs.CatalogGID = catalog

	priceList, err := c.createPriceList(ctx, store, title, catalog,
		in.CurrencyCode, in.PriceAdjustmentPercent)
	if err != nil {
		return refs, err
	}
	refs.PriceListGID = priceList
	return refs, nil
}

// RevokeWholesaleAccount ends the entitlement WITHOUT deleting anything.
//
// The catalog goes to DRAFT and that is the whole act. A draft catalog
// prices nothing, so the buyer pays published prices from their next visit
// -- and the company, its location, its contact, the price list and every
// order placed under them are untouched. A merchant who revokes trade terms
// is ending a discount, not erasing a customer, and a connector that
// deleted a company would take the buyer's order history's association with
// it.
func (c *Connector) RevokeWholesaleAccount(ctx context.Context, store Store,
	refs WholesaleAccountRefs) error {
	if strings.TrimSpace(refs.CatalogGID) == "" {
		return fmt.Errorf("shopify: this entitlement holds no catalog reference, so there is nothing to draft; revoke it in the Shopify admin and record the decision in MemQL")
	}
	resp, err := c.adminCall(ctx, store, catalogDraftMutation, "ShopifyCatalogDraft",
		map[string]any{"id": refs.CatalogGID, "input": map[string]any{"status": "DRAFT"}})
	if err != nil {
		return err
	}
	return userErrorsFrom(resp, "catalogUpdate")
}

// TagWholesaleCustomer is the plan-independent grant: one tag, one call.
//
// IT WRITES A TAG AND NOTHING ELSE. What the tag DOES is the merchant's own
// automatic discount, which this connector deliberately does not create --
// a discount is a price change and creating one on a merchant's behalf from
// an approval is a much larger act than the approval was.
func (c *Connector) TagWholesaleCustomer(ctx context.Context, store Store,
	email, tag string) (string, error) {
	customerGID, err := c.requireCustomer(ctx, store, email)
	if err != nil {
		return "", err
	}
	resp, err := c.adminCall(ctx, store, tagsAddMutation, "ShopifyTagsAdd",
		map[string]any{"id": customerGID, "tags": []string{normaliseTag(tag)}})
	if err != nil {
		return "", err
	}
	if ueErr := userErrorsFrom(resp, "tagsAdd"); ueErr != nil {
		return "", ueErr
	}
	return customerGID, nil
}

// UntagWholesaleCustomer is the plan-independent revoke.
func (c *Connector) UntagWholesaleCustomer(ctx context.Context, store Store,
	email, tag string) error {
	customerGID, err := c.requireCustomer(ctx, store, email)
	if err != nil {
		return err
	}
	resp, err := c.adminCall(ctx, store, tagsRemoveMutation, "ShopifyTagsRemove",
		map[string]any{"id": customerGID, "tags": []string{normaliseTag(tag)}})
	if err != nil {
		return err
	}
	return userErrorsFrom(resp, "tagsRemove")
}

// normaliseTag bounds what this connector will write as a tag.
//
// A TAG IS NOT FREE TEXT HERE. It reaches a merchant's live store and is
// visible in their admin, so it is trimmed and defaulted rather than
// passed through: an empty one would remove every tag on a tagsRemove, and
// a whitespace-only one would create a tag nobody can see or search for.
func normaliseTag(tag string) string {
	t := strings.TrimSpace(tag)
	if t == "" {
		return WholesaleTagDefault
	}
	return t
}

// requireCustomer resolves an email to a Shopify customer, or refuses.
//
// REFUSED RATHER THAN CREATED, unlike the B2B path. A tag on a customer
// that did not exist a moment ago entitles nobody: the buyer has not
// shopped here, so there is no account for the tag to attach to and no
// checkout for a discount to apply at. The merchant is told to invite them.
func (c *Connector) requireCustomer(ctx context.Context, store Store, email string) (string, error) {
	gid, err := c.customerByEmail(ctx, store, email)
	if err != nil {
		return "", err
	}
	if gid == "" {
		return "", fmt.Errorf("shopify: no customer on %s has the email %q, so there is no account to entitle -- invite them to create one, then provision again",
			store.Domain, email)
	}
	return gid, nil
}

// WholesaleCeilingError is the refusal a store below Plus gets at its
// fourth catalog.
//
// A TYPED ERROR rather than a string, because the pack has to tell it apart
// from a transient push failure: a ceiling is a REFUSAL -- it will fail
// identically for ever until the merchant upgrades or frees a catalog --
// and the pack records a refusal as `failed` while it records a transient
// failure as `pending` and retries.
type WholesaleCeilingError struct {
	StoreID string
	Plan    string
	Limit   int
}

func (e *WholesaleCeilingError) Error() string {
	plan := e.Plan
	if plan == "" {
		plan = "an unknown plan"
	}
	return fmt.Sprintf("shopify: store %s is on %s and already holds its %d company-location catalogs; "+
		"Shopify allows unlimited catalogs on Plus only. Free a catalog or upgrade the plan, then provision again",
		e.StoreID, plan, e.Limit)
}

// Refused reports that this will fail identically for ever.
func (e *WholesaleCeilingError) Refused() bool { return true }
