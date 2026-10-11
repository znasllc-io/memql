package memql

import "testing"

// Plan expansion resolves construct names through lazyLookup instead of
// cloning the registry with LookupIndex, so the two must agree on every name
// expansion can ask for: qualified, real bare, unambiguous bare alias, and an
// ambiguous bare name that has no answer. A disagreement would change which
// construct a query expands, or turn a working reference into "not found".
// (Names arrive trimmed -- expandFunctionCall trims before it asks -- and
// Lookup trims too, where a map key would not; that difference is unreachable.)
func TestLazyLookupAnswersEveryNameAsLookupIndexDoes(t *testing.T) {
	reg := newFunctionRegistry()
	for _, fn := range []*Function{
		{Name: "shared", Origin: "alpha/queries.memql", FunctionKind: "query"},
		{Name: "shared", Origin: "beta/queries.memql", FunctionKind: "query"},
		{Name: "unique", Origin: "alpha/queries.memql", FunctionKind: "query"},
		{Name: "internalOnly", FunctionKind: "query"},
		{Name: "internalOnly", Origin: "alpha/queries.memql", FunctionKind: "query"},
	} {
		if err := reg.add(fn); err != nil {
			t.Fatalf("add %s from %q: %v", fn.Name, fn.Origin, err)
		}
	}
	index := reg.LookupIndex()
	lookup := reg.lazyLookup()

	names := []string{"shared", "unique", "internalOnly", "absent", "gamma.unique", ""}
	for key := range index {
		names = append(names, key)
	}
	for _, name := range names {
		want, inIndex := index[name]
		got, ok := lookup(name)
		if ok != inIndex {
			t.Errorf("%q: lazyLookup found=%v, LookupIndex found=%v", name, ok, inIndex)
			continue
		}
		if ok && (got.Name != want.Name || got.Origin != want.Origin) {
			t.Errorf("%q: lazyLookup gave %s from %q, LookupIndex gave %s from %q",
				name, got.Name, got.Origin, want.Name, want.Origin)
		}
	}
}

// One expansion asking twice gets the same copy, and that copy is its own: a
// change to it must not reach the registry or the next query's expansion.
func TestLazyLookupKeepsOneCopyPerNameAndNeverAliasesTheRegistry(t *testing.T) {
	reg := newFunctionRegistry()
	if err := reg.add(&Function{Name: "unique", Origin: "alpha/queries.memql", FunctionKind: "query"}); err != nil {
		t.Fatal(err)
	}
	lookup := reg.lazyLookup()
	first, ok := lookup("unique")
	if !ok {
		t.Fatal("unique did not resolve")
	}
	if again, _ := lookup("unique"); again != first {
		t.Error("a second ask for the same name made a second copy")
	}
	first.FunctionKind = "changed by an expansion"
	if fresh, _ := reg.Lookup("unique"); fresh.FunctionKind != "query" {
		t.Errorf("a change to the expansion's copy reached the registry: %q", fresh.FunctionKind)
	}
	if next, _ := reg.lazyLookup()("unique"); next.FunctionKind != "query" {
		t.Errorf("a change to one expansion's copy reached the next expansion: %q", next.FunctionKind)
	}
}
