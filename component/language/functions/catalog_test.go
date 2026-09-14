package functions

import (
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"

	"github.com/znasllc-io/memql/component/language/ast"
)

// identifier is the spelling rule for a function, method or parameter name: a
// lowerCamel identifier. One spelling each (D10) is only checkable when the one
// spelling is a plain identifier the parser and Sense both see the same way.
var identifier = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)

// paramTypes is the vocabulary a Param's Type is spelled in. returnTypes adds
// TypeRows, which a traversal returns and no parameter takes.
var (
	paramTypes = map[string]bool{
		TypeString: true, TypeNumber: true, TypeBool: true, TypeDatetime: true,
		TypeDuration: true, TypeList: true, TypeMap: true, TypeAny: true,
		TypeLambda: true, TypeRow: true,
	}
	returnTypes = func() map[string]bool {
		out := map[string]bool{TypeRows: true}
		for k := range paramTypes {
			out[k] = true
		}
		return out
	}()
	// receivers is what a Function's Receiver may be: "" for a function, or the
	// type word of the value a method is called on.
	receivers = map[string]bool{"": true, TypeString: true, TypeList: true, TypeAny: true}
)

// One entry per key, and -- because the pre-v1 grammar dispatched on the
// LOWERCASED name -- no two keys that differ only in case either: `orderBy` and
// `orderby` would be two spellings of one method.
func TestCatalogKeysAreUnique(t *testing.T) {
	fns := Catalog()
	if len(fns) == 0 {
		t.Fatal("Catalog() is empty: this test examined nothing, which is not a pass")
	}
	seen := map[string]string{}
	for _, f := range fns {
		folded := strings.ToLower(f.Key())
		if prev, dup := seen[folded]; dup {
			t.Errorf("catalog keys %q and %q are one spelling twice: D10 allows one entry per function", prev, f.Key())
			continue
		}
		seen[folded] = f.Key()
	}
}

func TestEveryEntryHasADocAndATier(t *testing.T) {
	for _, f := range Catalog() {
		key := f.Key()
		if !identifier.MatchString(f.Name) {
			t.Errorf("%s: name %q is not a lowerCamel identifier", key, f.Name)
		}
		if !receivers[f.Receiver] {
			t.Errorf("%s: receiver %q is not one of \"\", string, list, any", key, f.Receiver)
		}
		if f.Tier != TierP && f.Tier != TierM {
			t.Errorf("%s: tier %q is neither P nor M", key, f.Tier)
		}
		if !returnTypes[f.Returns] {
			t.Errorf("%s: returns %q, which is not a type word", key, f.Returns)
		}
		checkDoc(t, key, f.Doc)

		names := map[string]bool{}
		for i, p := range f.Params {
			if !identifier.MatchString(p.Name) {
				t.Errorf("%s: parameter %d name %q is not a lowerCamel identifier", key, i, p.Name)
			}
			if names[p.Name] {
				t.Errorf("%s: parameter name %q is used twice", key, p.Name)
			}
			names[p.Name] = true
			if !paramTypes[p.Type] {
				t.Errorf("%s: parameter %s has type %q, which is not a parameter type word", key, p.Name, p.Type)
			}
			if p.Variadic && i != len(f.Params)-1 {
				t.Errorf("%s: parameter %s is variadic but not last", key, p.Name)
			}
		}
		for _, r := range f.Retired {
			if strings.TrimSpace(r) == "" {
				t.Errorf("%s: a Retired spelling is blank", key)
			}
		}
	}
}

// checkDoc holds a Doc to the catalog's rule: one to three sentences, starting
// with a capital (the Docs open on their verb: Returns, Reports, Raises), ending
// with a period. The sentence count is exact because no Doc uses an
// abbreviation such as "e.g." -- keep it that way.
func checkDoc(t *testing.T, key, doc string) {
	t.Helper()
	if strings.TrimSpace(doc) == "" {
		t.Errorf("%s: Doc is empty", key)
		return
	}
	if !strings.HasSuffix(doc, ".") {
		t.Errorf("%s: Doc does not end with a period: %q", key, doc)
	}
	if first := []rune(doc)[0]; !unicode.IsUpper(first) {
		t.Errorf("%s: Doc does not start with a capital: %q", key, doc)
	}
	if n := strings.Count(doc, ". ") + 1; n > 3 {
		t.Errorf("%s: Doc has %d sentences; the rule is one to three", key, n)
	}
}

func TestSignatureRendering(t *testing.T) {
	cases := []struct {
		fn   func() (Function, bool)
		want string
	}{
		{func() (Function, bool) { return Lookup("addDuration") }, "addDuration(ts datetime, dur duration) datetime"},
		{func() (Function, bool) { return Lookup("canonicalId") }, "canonicalId(value string, concept string) string"},
		{func() (Function, bool) { return Lookup("parentOf") }, "parentOf(label? string, match lambda) rows"},
		{func() (Function, bool) { return Lookup("ids") }, "ids(match lambda) rows"},
		{func() (Function, bool) { return Method(TypeList, "any") }, "list.any(pred lambda) bool"},
		{func() (Function, bool) { return Method(TypeList, "count") }, "list.count() number"},
		{func() (Function, bool) { return Method(TypeList, "first") }, "list.first() any"},
		{func() (Function, bool) { return Method(TypeList, "take") }, "list.take(n number) list"},
		{func() (Function, bool) { return Method(TypeList, "reduce") }, "list.reduce(seed any, fn lambda) any"},
		{func() (Function, bool) { return Method(TypeString, "includes") }, "string.includes(sub string) bool"},
		{func() (Function, bool) { return Method(TypeString, "count") }, "string.count() number"},
	}
	for _, c := range cases {
		f, ok := c.fn()
		if !ok {
			t.Errorf("no catalog entry for the signature %q", c.want)
			continue
		}
		if got := f.Signature(); got != c.want {
			t.Errorf("Signature() = %q, want %q", got, c.want)
		}
	}

	// No entry is variadic today; the rendering is pinned on a hand-built one
	// so the first variadic entry does not also have to invent the spelling.
	variadic := Function{Name: "join", Params: []Param{
		{Name: "sep", Type: TypeString},
		{Name: "parts", Type: TypeString, Variadic: true},
	}, Returns: TypeString}
	if got, want := variadic.Signature(), "join(sep string, parts ...string) string"; got != want {
		t.Errorf("variadic Signature() = %q, want %q", got, want)
	}
}

// The two retired maps are what Sense and the docs print as "was: ... write:
// ...", so a blank on either side is a hover card with a hole in it. The key
// set is pinned too: the parser must refuse every one of these names, so a key
// that silently left this map is a refusal nobody asks for any more.
func TestRetiredMapsNameReplacements(t *testing.T) {
	fns := RetiredFunctions()
	var keys []string
	for name, repl := range fns {
		keys = append(keys, name)
		if !identifier.MatchString(name) {
			t.Errorf("retired function %q is not a bare function name", name)
		}
		if strings.TrimSpace(repl) == "" {
			t.Errorf("retired function %q names no replacement", name)
		}
	}
	sort.Strings(keys)
	want := []string{
		"and", "coalesce", "concat", "cond", "count", "exists", "first", "gt", "gte",
		"last", "len", "lt", "lte", "mean", "not", "now", "or", "timestamp",
	}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("RetiredFunctions() keys = %v, want %v", keys, want)
	}

	methods := RetiredMethods()
	if len(methods) == 0 {
		t.Error("RetiredMethods() is empty; list.contains is retired in favour of `in`")
	}
	for key, repl := range methods {
		recv, name, ok := strings.Cut(key, ".")
		if !ok || recv == "" || !receivers[recv] || !identifier.MatchString(name) {
			t.Errorf("retired method %q is not spelled <receiver>.<name>", key)
		}
		if strings.TrimSpace(repl) == "" {
			t.Errorf("retired method %q names no replacement", key)
		}
	}
}

// One spelling each: a name cannot be both offered by the catalog and refused
// as retired. `contains` is the case this guards -- the traversal stays, only
// the two-argument substring form retired -- so it must never become a
// RetiredFunctions key.
func TestRetiredSpellingsAreNotLive(t *testing.T) {
	for name := range RetiredFunctions() {
		if f, ok := Lookup(name); ok {
			t.Errorf("%q is both a catalog function (%s) and a retired function spelling", name, f.Signature())
		}
	}
	for key := range RetiredMethods() {
		recv, name, _ := strings.Cut(key, ".")
		if f, ok := Method(recv, name); ok {
			t.Errorf("%q is both a catalog method (%s) and a retired method spelling", key, f.Signature())
		}
	}
}

// Catalog() is in a stable order a docs table and a completion list can print
// as-is: functions first by name, then methods by receiver and name.
func TestCatalogOrder(t *testing.T) {
	fns := Catalog()
	seenMethod := false
	for i, f := range fns {
		if f.Receiver != "" {
			seenMethod = true
		} else if seenMethod {
			t.Errorf("function %s comes after a method", f.Key())
		}
		if i == 0 {
			continue
		}
		prev := fns[i-1]
		if prev.Receiver == f.Receiver && prev.Name >= f.Name {
			t.Errorf("%s is listed before %s", prev.Key(), f.Key())
		}
		if prev.Receiver != "" && f.Receiver != "" && prev.Receiver > f.Receiver {
			t.Errorf("method receiver %q is listed before %q", prev.Receiver, f.Receiver)
		}
	}
}

func TestLookupFindsFunctionsOnly(t *testing.T) {
	if f, ok := Lookup("lower"); !ok || f.Receiver != "" || f.Name != "lower" {
		t.Errorf("Lookup(lower) = %+v, %v; want the lower function", f, ok)
	}
	// `contains` is the graph traversal; the substring test is string.includes.
	if f, ok := Lookup("contains"); !ok || f.Returns != TypeRows {
		t.Errorf("Lookup(contains) = %+v, %v; want the traversal returning rows", f, ok)
	}
	for _, name := range []string{
		"any",        // a list method, not a function
		"includes",   // a string method
		"list.any",   // a method key is not a function name
		"shortid",    // one spelling each: case is part of the spelling
		"cond",       // retired: `p ? a : b`
		"notDefined", // never existed
	} {
		if f, ok := Lookup(name); ok {
			t.Errorf("Lookup(%q) found %s; want no function", name, f.Key())
		}
	}
}

func TestMethodResolvesItsReceiver(t *testing.T) {
	if f, ok := Method(TypeList, "count"); !ok || f.Receiver != TypeList {
		t.Errorf("Method(list, count) = %+v, %v; want the list method", f, ok)
	}
	if f, ok := Method(TypeString, "count"); !ok || f.Receiver != TypeString {
		t.Errorf("Method(string, count) = %+v, %v; want the string method", f, ok)
	}
	for _, c := range []struct{ recv, name string }{
		{"", "lower"},           // a function is never a method
		{TypeString, "any"},     // any is a list method only
		{TypeList, "contains"},  // retired: `v in list`
		{TypeMap, "count"},      // no method on a map
		{TypeList, "notAThing"}, // never existed
	} {
		if f, ok := Method(c.recv, c.name); ok {
			t.Errorf("Method(%q, %q) found %s; want nothing", c.recv, c.name, f.Key())
		}
	}
}

// A method declared on receiver "any" answers for every receiver that has no
// method of that name of its own. No entry is an "any" method today, so the
// rule is pinned on a hand-built table.
func TestMethodFallsBackToAnyReceiver(t *testing.T) {
	tbl := newTable([]Function{
		{Name: "size", Receiver: TypeList, Doc: "Returns the list size.", Returns: TypeNumber, Tier: TierM},
		{Name: "size", Receiver: TypeAny, Doc: "Returns a size.", Returns: TypeNumber, Tier: TierM},
		{Name: "describe", Receiver: TypeAny, Doc: "Returns a description.", Returns: TypeString, Tier: TierM},
		{Name: "size", Doc: "Returns a size.", Returns: TypeNumber, Tier: TierM},
	})
	cases := []struct {
		recv, name string
		want       string // the key found, "" for none
	}{
		{TypeList, "size", "list.size"},       // its own method wins over the any fallback
		{TypeString, "size", "any.size"},      // no string.size: the any method answers
		{TypeMap, "describe", "any.describe"}, // any receiver at all
		{"", "size", ""},                      // "" asks for a method on nothing: the function is not one
		{TypeList, "absent", ""},
	}
	for _, c := range cases {
		f, ok := tbl.method(c.recv, c.name)
		got := ""
		if ok {
			got = f.Key()
		}
		if got != c.want {
			t.Errorf("method(%q, %q) = %q, want %q", c.recv, c.name, got, c.want)
		}
	}
}

// The P set is the record's (D11): the traversals, count / any / all over a row
// array, and includes. A tier flag decides what Lower may push down against the
// row, so a flag that drifted to P would let the load accept an expression the
// lowering has no SQL for, and one that drifted to M would refuse a filter that
// lowers today.
func TestPushdownFunctionsAreTheRecordsSet(t *testing.T) {
	var got []string
	for _, f := range Catalog() {
		if f.Tier == TierP {
			got = append(got, f.Key())
		}
	}
	sort.Strings(got)
	want := []string{
		"aliasOf", "childOf", "contains", "createdBy", "equals", "ids",
		"list.all", "list.any", "list.count", "owns", "parentOf", "references",
		"string.includes",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("TierP entries = %v, want %v", got, want)
	}
}

// The traversals are the AST's relationship functions, spelled the same way,
// each P and each returning rows. All but ids take an optional leading label;
// ids follows no edge, so a label on it would always be a mistake.
func TestTraversalsAreTheRelationshipFunctions(t *testing.T) {
	rels := []ast.RelationshipFunction{
		ast.RelParentOf, ast.RelChildOf, ast.RelAliasOf, ast.RelEquals, ast.RelReferences,
		ast.RelContains, ast.RelOwns, ast.RelCreatedBy, ast.RelIds,
	}
	isRel := map[string]bool{}
	for _, rel := range rels {
		name := string(rel)
		isRel[name] = true
		f, ok := Lookup(name)
		if !ok {
			t.Errorf("relationship function %s has no catalog entry", name)
			continue
		}
		if f.Returns != TypeRows || f.Tier != TierP {
			t.Errorf("%s returns %q in tier %q; a traversal returns rows in tier P", name, f.Returns, f.Tier)
		}
		if len(f.Params) == 0 || f.Params[len(f.Params)-1].Type != TypeLambda {
			t.Errorf("%s does not end on its match lambda: %s", name, f.Signature())
			continue
		}
		switch {
		case rel == ast.RelIds:
			if len(f.Params) != 1 {
				t.Errorf("ids takes no label: %s", f.Signature())
			}
		case len(f.Params) != 2 || !f.Params[0].Optional || f.Params[0].Type != TypeString:
			t.Errorf("%s does not take an optional leading label: %s", name, f.Signature())
		}
	}
	for _, f := range Catalog() {
		if f.Returns == TypeRows && !isRel[f.Key()] {
			t.Errorf("%s returns rows but is not a relationship function", f.Key())
		}
	}
}

// Catalog, Lookup and Method hand out copies: a caller that edits what it got
// back (a Sense hover appending to a Doc, a test trimming Params) cannot change
// what the next caller reads.
func TestCatalogReturnsCopies(t *testing.T) {
	first := Catalog()
	idx := -1
	for i, f := range first {
		if len(f.Params) > 0 {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("no catalog entry has a parameter: this test examined nothing")
	}
	key, param := first[idx].Key(), first[idx].Params[0].Name
	first[idx].Params[0].Name = "mutated"
	first[idx].Doc = "Mutated."
	if again := Catalog()[idx]; again.Params[0].Name != param || again.Doc == "Mutated." {
		t.Errorf("editing Catalog()'s result changed the catalog entry %s", key)
	}

	got, _ := Lookup("addDuration")
	got.Params[0].Name = "mutated"
	if again, _ := Lookup("addDuration"); again.Params[0].Name == "mutated" {
		t.Error("editing Lookup's result changed the catalog entry addDuration")
	}
}

// includes() refuses to widen a selection the way startsWith does (authoring
// rule 32): a blank needle matches nothing, since "" occurs in every string.
// The doc is what Sense and the generated docs print, so it must say so.
func TestIncludesDocStatesTheBlankRule(t *testing.T) {
	m, ok := Method(TypeString, "includes")
	if !ok {
		t.Fatal("string.includes is not in the catalog")
	}
	if !strings.Contains(m.Doc, "blank") || !strings.Contains(m.Doc, "matches nothing") {
		t.Errorf("includes doc %q must say that a blank sub matches nothing", m.Doc)
	}
}
