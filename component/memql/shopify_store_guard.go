package memql

import (
	"fmt"
	"regexp"
	"strings"
)

// The Storefront token a store row names (memql#5626).
//
// component/edge publishes a store's Storefront token -- a public credential,
// by Shopify's design -- to every visitor of every storefront bound to that
// store, and it publishes it from ONE secret only: the one the store's id
// names, SHOPIFY_<STOREID>_STOREFRONT_TOKEN. Any other reference is refused
// there, before the lookup, so the edge never depends on the writer having
// named a Storefront token. This file is the write-side half: a store row may
// not be given a reference the edge would refuse, so a store cannot read
// "connected" while serving nothing.
//
// # Why the rule lives in the engine
//
// It is the one place every writer converges -- createStore and updateStore
// from the console, Connect Shopify, the first-boot seed and a raw insert()
// -- and the name has to be known on both sides of the store row: here, and
// at the edge, which imports this package. integrations/shopify seals the
// token under its own spelling of the same name, and the parity test in app/
// (the module importing all three) holds the spellings together.
//
// # The store id
//
// The id names the store's secrets and its webhook URL, and Connect derives it
// from the myshopify.com domain (integrations/shopify's StoreIDForDomain):
// lower-case letters, digits, '_' and '-', starting with a letter or digit, at
// most 56 characters. A hand-made store with any other id -- "Acme" beside
// "acme", which would share one upper-cased secret name, or one carrying a
// quote -- is refused at creation, and names no token at the edge.

// conceptShopifyStore is the store row this file guards.
const conceptShopifyStore = "v1:shopify:store"

// shopifyStoreIDForm is the form of a store id: what Connect's
// StoreIDForDomain produces, and nothing else.
var shopifyStoreIDForm = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,55}$`)

// ValidShopifyStoreID reports whether id -- bare or canonical -- is a store id
// that can name the store's secrets.
func ValidShopifyStoreID(id string) bool {
	return shopifyStoreIDForm.MatchString(BareShortId(strings.TrimSpace(id)))
}

// StorefrontTokenSecretName is the v1:platform:globalSecret name a store's
// Storefront API token is sealed under, SHOPIFY_<STOREID>_STOREFRONT_TOKEN, or
// "" for an id that is not a valid store id. It is the ONLY secret the edge
// publishes for a store.
func StorefrontTokenSecretName(storeID string) string {
	id := BareShortId(strings.TrimSpace(storeID))
	if !shopifyStoreIDForm.MatchString(id) {
		return ""
	}
	return "SHOPIFY_" + strings.ToUpper(id) + "_STOREFRONT_TOKEN"
}

// NamesItsOwnStorefrontToken reports whether ref is the store's own Storefront
// token name -- whether the edge will publish what it names. This, not a
// non-empty reference, is what "the store's Storefront token is connected"
// means, and it is what the readiness answer hands MemQL OS so the console
// never needs a copy of the naming rule.
func NamesItsOwnStorefrontToken(storeID, ref string) bool {
	ref = strings.TrimSpace(ref)
	return ref != "" && ref == StorefrontTokenSecretName(storeID)
}

// validateShopifyStoreWrite refuses a store row the edge could never serve a
// token from: a NEW store whose id cannot name a token, and a storefrontTokenRef
// set to anything but the store's own token name.
//
// ONLY A NEW OR CHANGED REFERENCE IS JUDGED. payload is the merged row, so
// every write -- the connector's health and status writes included -- carries
// the stored reference; a store written before this rule with another name
// must stay writable, and it is told about instead (the edge warns when it
// resolves one). Clearing the reference is always allowed: a store with no
// token is a store whose token has not been set yet.
//
// No actor is exempt. The connector and the seed name the token correctly, and
// a server-side writer naming another secret is the case this exists for.
func validateShopifyStoreWrite(payload map[string]any, id string, priorExisted bool, priorStorefrontTokenRef string) error {
	if payload == nil {
		return nil
	}
	storeID := BareShortId(strings.TrimSpace(id))
	// A create with no id yet gets a generated one after this point, which
	// cannot be judged here; the cluster-owner create floor already decides
	// who may create a store at all (create_rank_floor.go).
	if !priorExisted && storeID != "" && !shopifyStoreIDForm.MatchString(storeID) {
		return fmt.Errorf(
			"v1:shopify:store: %q is not a store id -- a store id is the shop's handle as Connect Shopify derives it from the myshopify.com domain (acme-widgets for acme-widgets.myshopify.com): lower-case letters, digits, '_' and '-', starting with a letter or digit, at most 56 characters. It names the store's secrets, so a second spelling of one store would share them",
			storeID,
		)
	}
	ref := strings.TrimSpace(stringFromAny(payload["storefrontTokenRef"]))
	if ref == "" || ref == strings.TrimSpace(priorStorefrontTokenRef) {
		return nil
	}
	want := StorefrontTokenSecretName(storeID)
	if ref == want {
		return nil
	}
	if want == "" {
		return fmt.Errorf(
			"v1:shopify:store: %q cannot name a Storefront token, so its storefrontTokenRef stays empty -- the store id is not one Connect Shopify would derive (lower-case letters, digits, '_' and '-'). Attach the store through Connect Shopify, which registers it under its own id",
			storeID,
		)
	}
	return fmt.Errorf(
		"v1:shopify:store: storefrontTokenRef %q is not this store's Storefront token. The edge publishes a store's Storefront token only from the secret %q -- the name Connect Shopify seals it under -- and would serve no token from any other. Name %q, or leave the reference empty and set the token from the store's panel, which seals it there",
		ref, want, want,
	)
}
