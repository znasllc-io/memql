package dsl

import (
	"reflect"
	"testing"
)

// A STOREFRONT PACK IS A DECLARATION, NOT A DEFAULT (Connect Shopify design,
// section 9, D4). Shipping disabled is how a pack says enabling it is an
// exposure, so keying developer delegation on the default would make every
// future pack that ships off for safety developer-flippable without anybody
// deciding it. The two registries must stay independent in both directions.
func TestAStorefrontPackIsDeclaredNotInferredFromItsDefault(t *testing.T) {
	t.Cleanup(ResetPackDefaultsForTest)
	t.Cleanup(func() {
		UnregisterStorefrontPack("sfp-ships-off")
		UnregisterStorefrontPack("sfp-declared")
	})

	RegisterPackDefault("sfp-ships-off", false)
	if IsStorefrontPack("sfp-ships-off") {
		t.Fatal("a pack that only declared a disabled default was read as a storefront pack")
	}

	RegisterStorefrontPack("sfp-declared")
	RegisterPackDefault("sfp-declared", true)
	if !IsStorefrontPack("sfp-declared") {
		t.Fatal("a declared storefront pack stopped being one because its default is enabled")
	}
	if got := StorefrontPacks(); !reflect.DeepEqual(got, []string{"sfp-declared"}) {
		t.Fatalf("StorefrontPacks() = %v, want [sfp-declared]", got)
	}

	RegisterStorefrontPack("  ")
	if IsStorefrontPack("") {
		t.Fatal("an empty domain was stored as a storefront pack")
	}
}
