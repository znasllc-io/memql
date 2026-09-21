package memql

import (
	"testing"
)

// The development-store fields are the half of design D8 the engine holds
// (epic memql#5530, issue memql#5539): a development store is a SECOND
// v1:shopify:store row, flagged, and paired to the live store it stands in
// for. Epic 3's go-live guard (memql#5546) reads the flag off the store a
// site names, which is why it lives here and not on the site.
func TestTheStoreConceptCarriesTheDevelopmentStoreFields(t *testing.T) {
	c := conceptFieldsFor(t, "v1:shopify:store")
	// The TYPE is half the contract. A field declared under the wrong type
	// satisfies an existence check and then answers the go-live guard with
	// something it cannot compare.
	for name, want := range map[string]string{
		"isDevelopment":        "bool",
		"developmentOfStoreId": "string",
	} {
		got, ok := c[name]
		if !ok {
			t.Errorf("v1:shopify:store declares no %q field; the go-live guard has nothing to ask", name)
			continue
		}
		if got.Type != want {
			t.Errorf("v1:shopify:store.%s is %q, want %q", name, got.Type, want)
		}
		if got.Required {
			t.Errorf("v1:shopify:store.%s is @required; every store row written before this field "+
				"existed would be unwritable on its next write (memql#5199)", name)
		}
	}
	// THE REACHABLE POSITIVE: a field the concept has always had, so a
	// helper that returned an empty map would fail here rather than pass.
	if _, ok := c["domain"]; !ok {
		t.Fatal("read no fields off v1:shopify:store; the helper is not reading the concept")
	}
}

func TestTheDevelopmentStoreReadIsRegistered(t *testing.T) {
	fn := registeredFunctionNamed(t, "developmentStoresFor")
	if fn == nil {
		t.Fatal("developmentStoresFor is not registered; a storefront cannot show its development store")
	}
	// The PAIRING is what the read is for. Asked of the parsed filter rather
	// than of the source text, so a mention in a doc comment cannot answer
	// for a term that is not in the boolean tree.
	fields := map[string]struct{}{}
	conjuncts, decidable := topLevelConjunctsOf(unwrapToFilter(fn.Expr))
	for _, c := range conjuncts {
		cmp, ok := c.(*ComparisonExpression)
		if !ok {
			continue
		}
		if name := topLevelPayloadField(cmp.Field); name != "" {
			fields[name] = struct{}{}
		}
	}
	if !decidable {
		t.Fatal("developmentStoresFor's filter is a top-level disjunction; a store's development " +
			"stores are not an alternative to anything and the pairing term could be skipped")
	}
	if _, ok := fields["developmentOfStoreId"]; !ok {
		t.Errorf("developmentStoresFor does not filter on developmentOfStoreId; it filters on %v", sortedKeys(fields))
	}
	if _, ok := fields["isDevelopment"]; !ok {
		t.Errorf("developmentStoresFor does not filter on isDevelopment, so a live store pointing at "+
			"another store would be returned as its development store; it filters on %v", sortedKeys(fields))
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

// conceptFieldsFor answers one concept's declared fields, keyed by dotted
// path, off the package-shared db-less engine (memql#4569). Read-only, so it
// borrows rather than booting: flattenConceptFields is the same walk the
// concept-field snapshot takes, so what is asserted here is what the snapshot
// records.
func conceptFieldsFor(t *testing.T, conceptName string) map[string]conceptFieldShape {
	t.Helper()
	eng := sharedDblessEngine(t)
	c, err := eng.concepts.Get(conceptName)
	if err != nil {
		t.Fatalf("concept %s is not registered: %v", conceptName, err)
	}
	fields, err := flattenConceptFields(c)
	if err != nil {
		t.Fatalf("flatten %s: %v", conceptName, err)
	}
	return fields
}

// registeredFunctionNamed answers a loaded construct by name, or nil when the
// registry does not hold one. Nil rather than a fatal, so the caller says what
// the absence MEANS.
func registeredFunctionNamed(t *testing.T, name string) *Function {
	t.Helper()
	fn, err := sharedDblessEngine(t).functions.Get(name)
	if err != nil {
		return nil
	}
	return fn
}
