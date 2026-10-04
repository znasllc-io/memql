package memql

import (
	"context"
	"strings"
	"testing"
)

// THE STORE ROW NAMES ITS OWN STOREFRONT TOKEN, OR NONE (memql#5626).
//
// component/edge publishes a store's Storefront token only from the secret the
// store's id names, SHOPIFY_<STOREID>_STOREFRONT_TOKEN, and refuses any other
// reference. The write path used to accept any reference -- MemQL OS's
// "Register a store" form takes the secret's name as free text -- so a store
// could be created that read "connected" in the OS and served no token. These
// pin the write-side half: a new or changed reference must be the store's own
// name, and a new store's id must be one that can name a token at all.

func TestStorefrontTokenSecretNameIsDerivedFromAValidStoreIdOnly(t *testing.T) {
	for id, want := range map[string]string{
		"acme":                  "SHOPIFY_ACME_STOREFRONT_TOKEN",
		"acme-widgets":          "SHOPIFY_ACME-WIDGETS_STOREFRONT_TOKEN",
		"dev_store-2":           "SHOPIFY_DEV_STORE-2_STOREFRONT_TOKEN",
		"v1:shopify:store:acme": "SHOPIFY_ACME_STOREFRONT_TOKEN",
		" acme ":                "SHOPIFY_ACME_STOREFRONT_TOKEN",
	} {
		if got := StorefrontTokenSecretName(id); got != want {
			t.Errorf("StorefrontTokenSecretName(%q) = %q, want %q", id, got, want)
		}
	}
	// AN ID THAT CANNOT NAME A TOKEN NAMES NONE. Upper case would collide with
	// its lower-case twin (Acme and acme share SHOPIFY_ACME_...), and a quote
	// or a space has no place in a secret name.
	for _, id := range []string{"", "Acme", "acme widgets", `acme"x`, "-acme", "acme.dev", strings.Repeat("a", 57)} {
		if got := StorefrontTokenSecretName(id); got != "" {
			t.Errorf("StorefrontTokenSecretName(%q) = %q, want none", id, got)
		}
	}
}

func TestTheStoreGuardHoldsTheTokenReferenceToTheStoresOwnName(t *testing.T) {
	const id = "v1:shopify:store:acme"
	write := func(ref any) map[string]any {
		return map[string]any{"domain": "acme.myshopify.com", "storefrontTokenRef": ref}
	}

	for name, c := range map[string]struct {
		payload      map[string]any
		priorExisted bool
		priorRef     string
	}{
		"a new store with its own token":      {write("SHOPIFY_ACME_STOREFRONT_TOKEN"), false, ""},
		"a new store with no token yet":       {write(""), false, ""},
		"a new store with the key left out":   {map[string]any{"domain": "acme.myshopify.com"}, false, ""},
		"pointing an existing store at it":    {write("SHOPIFY_ACME_STOREFRONT_TOKEN"), true, ""},
		"clearing the reference":              {write(""), true, "SHOPIFY_ACME_STOREFRONT_TOKEN"},
		"an inherited legacy reference":       {write("ACME_STOREFRONT_TOKEN"), true, "ACME_STOREFRONT_TOKEN"},
		"a status write on the legacy row":    {map[string]any{"status": "paused", "storefrontTokenRef": "LEGACY"}, true, "LEGACY"},
		"a new store named canonically twice": {write("SHOPIFY_ACME_STOREFRONT_TOKEN"), true, "SHOPIFY_ACME_STOREFRONT_TOKEN"},
	} {
		t.Run(name+" passes", func(t *testing.T) {
			if err := validateShopifyStoreWrite(c.payload, id, c.priorExisted, c.priorRef); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}

	for name, c := range map[string]struct {
		payload      map[string]any
		priorExisted bool
		priorRef     string
	}{
		"a new store naming a free-text secret": {write("ACME_STOREFRONT_TOKEN"), false, ""},
		"a new store naming its admin token":    {write("SHOPIFY_ACME_ADMIN_TOKEN"), false, ""},
		"a new store naming another store's":    {write("SHOPIFY_BETA_STOREFRONT_TOKEN"), false, ""},
		"a near miss in case":                   {write("shopify_acme_storefront_token"), false, ""},
		"re-pointing an existing store":         {write("MEMQL_SMTP_PASSWORD"), true, "SHOPIFY_ACME_STOREFRONT_TOKEN"},
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			err := validateShopifyStoreWrite(c.payload, id, c.priorExisted, c.priorRef)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), "SHOPIFY_ACME_STOREFRONT_TOKEN") {
				t.Errorf("the refusal does not name the expected secret: %v", err)
			}
		})
	}
}

func TestTheStoreGuardRefusesANewStoreWhoseIdCannotNameAToken(t *testing.T) {
	for _, id := range []string{"v1:shopify:store:Acme", `v1:shopify:store:acme"x`, "v1:shopify:store:acme widgets", "v1:shopify:store:" + strings.Repeat("a", 57)} {
		err := validateShopifyStoreWrite(map[string]any{"domain": "acme.myshopify.com"}, id, false, "")
		if err == nil || !strings.Contains(err.Error(), "store id") {
			t.Errorf("a new store %q was not refused for its id: %v", id, err)
		}
	}
	// AN EXISTING STORE IS NOT REFUSED FOR ITS ID: refusing every write to a
	// row that already exists would brick it, and the connector writes its
	// health and status there. Its token is what goes unserved.
	if err := validateShopifyStoreWrite(map[string]any{"status": "paused"}, "v1:shopify:store:Acme", true, ""); err != nil {
		t.Errorf("an existing store was refused for an id it already has: %v", err)
	}
	if err := validateShopifyStoreWrite(map[string]any{"storefrontTokenRef": "SHOPIFY_ACME_STOREFRONT_TOKEN"}, "v1:shopify:store:Acme", true, ""); err == nil {
		t.Error("an existing store whose id names no token was pointed at a token")
	}
}

// THE STORE FACT THE OS READS IS THE EDGE'S RULE (memql#5626). HasStorefrontToken
// used to mean "the reference is non-empty", so a store naming a free-text
// secret read as connected while the edge served it nothing. It now means the
// store names its OWN token -- the one thing the edge will publish -- and the
// readiness answer carries it, so MemQL OS needs no copy of the naming rule.
func TestBoundStoreFactsSayWhetherTheEdgeWillServeTheToken(t *testing.T) {
	for ref, want := range map[string]bool{
		"SHOPIFY_ACME_STOREFRONT_TOKEN": true,
		"ACME_STOREFRONT_TOKEN":         false,
		"SHOPIFY_ACME_ADMIN_TOKEN":      false,
		"":                              false,
	} {
		execute := func(context.Context, string) (any, error) {
			return []map[string]any{{"id": "v1:shopify:store:acme", "domain": "acme.myshopify.com", "storefrontTokenRef": ref}}, nil
		}
		facts, err := BoundStoreAsDeployment(context.Background(), execute, "acme")
		if err != nil {
			t.Fatal(err)
		}
		if facts.HasStorefrontToken != want {
			t.Errorf("a store naming %q: HasStorefrontToken = %v, want %v", ref, facts.HasStorefrontToken, want)
		}
	}
}
