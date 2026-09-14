package annotations

import (
	"sort"
	"strings"
	"testing"
	"unicode"
)

// useFromText reads one written annotation -- `@cache(300)`,
// `@rowAuthz(owner="ownerUserId", clusterOwner)` -- into the Use the parser
// would hand the check. It is a TEST helper and deliberately small: this
// package is a leaf and cannot import the parser, so the tests here hold the
// examples to a reading of the same argument forms the parser distinguishes,
// and component/memql's consistency test holds them to the real parser.
func useFromText(t *testing.T, text string) Use {
	t.Helper()
	if !strings.HasPrefix(text, "@") {
		t.Fatalf("example %q does not start with @", text)
	}
	rest := text[1:]
	end := strings.IndexFunc(rest, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
	})
	if end < 0 {
		return Use{Name: rest, Form: FormFlag}
	}
	name := rest[:end]
	args := strings.TrimSpace(rest[end:])
	if !strings.HasPrefix(args, "(") || !strings.HasSuffix(args, ")") {
		t.Fatalf("example %q: arguments must be one parenthesised list", text)
	}
	inner := strings.TrimSpace(args[1 : len(args)-1])
	switch {
	case inner == "":
		return Use{Name: name, Form: FormEmpty}
	case name == "filter" && !strings.HasPrefix(inner, `"`) && !strings.HasPrefix(inner, "{"):
		return Use{Name: name, Form: FormExpression}
	case strings.HasPrefix(inner, "{"):
		return Use{Name: name, Form: FormObject}
	case strings.HasPrefix(inner, "!"):
		return Use{Name: name, Form: FormExclude}
	case strings.HasPrefix(inner, `"`):
		if len(splitTopLevel(inner)) > 1 {
			return Use{Name: name, Form: FormStrings}
		}
		return Use{Name: name, Form: FormString}
	case inner[0] == '-' || (inner[0] >= '0' && inner[0] <= '9'):
		return Use{Name: name, Form: FormNumber}
	case inner == "true" || inner == "false":
		return Use{Name: name, Form: FormBool}
	}
	var keys []WrittenKey
	for _, part := range splitTopLevel(inner) {
		key := strings.TrimSpace(part)
		bare := true
		if i := strings.IndexAny(key, "=:"); i >= 0 {
			key, bare = strings.TrimSpace(key[:i]), false
		}
		keys = append(keys, WrittenKey{Name: key, Bare: bare})
	}
	return Use{Name: name, Form: FormKeywords, Keys: keys}
}

// splitTopLevel splits an argument list on the commas outside string literals.
func splitTopLevel(s string) []string {
	var out []string
	inString := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if inString {
				i++
			}
		case '"':
			inString = !inString
		case ',':
			if !inString {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// TestEveryExampleIsAcceptedByItsOwnPlacement is the first contract a
// placement makes: the example it shows an author is a use the check accepts.
// An example the check refuses would teach the one spelling that fails.
func TestEveryExampleIsAcceptedByItsOwnPlacement(t *testing.T) {
	if len(Placements()) < 150 {
		t.Fatalf("only %d placements -- the registry is not describing every receiver", len(Placements()))
	}
	for _, p := range Placements() {
		if p.Example == "" {
			t.Errorf("%s @%s has no Example", p.Receiver, p.Name)
			continue
		}
		u := useFromText(t, p.Example)
		if u.Name != p.Name {
			t.Errorf("%s @%s: Example %q names @%s", p.Receiver, p.Name, p.Example, u.Name)
			continue
		}
		if r := Check(p.Receiver, u); r != nil {
			t.Errorf("%s @%s: Example %q is refused by its own placement: %v", p.Receiver, p.Name, p.Example, r)
		}
	}
}

// wrongForm picks a form the placement does NOT accept, so the refusal a
// variant draws is always about the form rather than the name.
func wrongForm(p Placement) Form {
	for _, f := range []Form{FormNumber, FormString, FormFlag, FormObject, FormStrings, FormEmpty, FormExclude, FormBool, FormExpression, FormKeywords} {
		if p.Forms&f == 0 {
			return f
		}
	}
	return 0
}

// TestEveryPlacementRefusesAWrongForm derives, for every placement, one use
// in a form the placement does not take and one keyword use naming a key it
// does not have. Each is refused with its own code, and the message says what
// to write -- the placement's example.
func TestEveryPlacementRefusesAWrongForm(t *testing.T) {
	for _, p := range Placements() {
		f := wrongForm(p)
		if f == 0 {
			t.Errorf("%s @%s accepts every form -- a placement that takes anything checks nothing", p.Receiver, p.Name)
			continue
		}
		u := Use{Name: p.Name, Form: f}
		if f == FormKeywords {
			u.Keys = []WrittenKey{{Name: "zz"}}
		}
		r := Check(p.Receiver, u)
		if r == nil || r.Code != CodeForm {
			t.Errorf("%s @%s written as %s: got %v, want %s", p.Receiver, p.Name, f, r, CodeForm)
			continue
		}
		if !strings.Contains(r.Message, p.Example) {
			t.Errorf("%s @%s: the form refusal does not show the example %q: %s", p.Receiver, p.Name, p.Example, r.Message)
		}
		if !strings.Contains(r.Message, p.Receiver.Phrase()) {
			t.Errorf("%s @%s: the form refusal does not name the receiver: %s", p.Receiver, p.Name, r.Message)
		}

		if p.Forms&FormKeywords == 0 {
			continue
		}
		bad := Use{Name: p.Name, Form: FormKeywords, Keys: []WrittenKey{{Name: "zzUnknownKey"}}}
		r = Check(p.Receiver, bad)
		if r == nil || r.Code != CodeKey {
			t.Errorf("%s @%s with key zzUnknownKey: got %v, want %s", p.Receiver, p.Name, r, CodeKey)
			continue
		}
		if !strings.Contains(r.Message, "zzUnknownKey") {
			t.Errorf("%s @%s: the key refusal does not name the key it refused: %s", p.Receiver, p.Name, r.Message)
		}
	}
}

// TestKeywordPlacementsDeclareTheirKeys: a keyword form with no key set would
// refuse every key an author writes.
func TestKeywordPlacementsDeclareTheirKeys(t *testing.T) {
	for _, p := range Placements() {
		hasKeywords := p.Forms&FormKeywords != 0
		if hasKeywords && len(p.Keys) == 0 {
			t.Errorf("%s @%s takes keyword arguments but declares no keys", p.Receiver, p.Name)
		}
		if !hasKeywords && len(p.Keys) > 0 {
			t.Errorf("%s @%s declares keys but does not take keyword arguments", p.Receiver, p.Name)
		}
		seen := map[string]bool{}
		for _, k := range p.Keys {
			if k.Name == "" || k.Type == "" || k.Doc == "" {
				t.Errorf("%s @%s: malformed key %+v", p.Receiver, p.Name, k)
			}
			if seen[k.Name] {
				t.Errorf("%s @%s: key %q declared twice", p.Receiver, p.Name, k.Name)
			}
			seen[k.Name] = true
		}
	}
}

// TestUnknownNameIsRefusedWithADidYouMean: a one-letter typo of a real name
// is refused as unknown, suggests the real name, and lists what the receiver
// takes.
func TestUnknownNameIsRefusedWithADidYouMean(t *testing.T) {
	for _, r := range Receivers() {
		names := ByReceiver[string(r)]
		if len(names) == 0 {
			t.Errorf("receiver %s has no placements", r)
			continue
		}
		// The longest name, with its last letter doubled: a typo no other
		// receiver accepts and no retired table names.
		target := names[0]
		for _, n := range names {
			if len(n) > len(target) {
				target = n
			}
		}
		typo := target + target[len(target)-1:]
		ref := Check(r, Use{Name: typo, Form: FormFlag})
		if ref == nil || ref.Code != CodeUnknown {
			t.Errorf("%s @%s: got %v, want %s", r, typo, ref, CodeUnknown)
			continue
		}
		if !strings.Contains(ref.Message, "did you mean @"+target) {
			t.Errorf("%s @%s: no did-you-mean for @%s: %s", r, typo, target, ref.Message)
		}
		if !strings.Contains(ref.Message, r.Phrase()) {
			t.Errorf("%s @%s: the refusal does not name the receiver: %s", r, typo, ref.Message)
		}
		for _, n := range names {
			if !strings.Contains(ref.Message, "@"+n) {
				t.Errorf("%s @%s: the refusal does not list @%s: %s", r, typo, n, ref.Message)
			}
		}
	}

	far := Check(Query, Use{Name: "zzzzzzzzqqq", Form: FormFlag})
	if far == nil || far.Code != CodeUnknown || strings.Contains(far.Message, "did you mean") {
		t.Errorf("a name near nothing must be unknown with no suggestion, got %v", far)
	}
}

// TestRetiredNamesRefuseWithTheirHint: every retired entry refuses with its
// own hint, on the receivers it is retired on, before any other reading.
func TestRetiredNamesRefuseWithTheirHint(t *testing.T) {
	if len(retiredEverywhere) == 0 || len(retiredOn) == 0 {
		t.Fatal("the retired tables are empty -- every retirement lost its hint")
	}
	for name, hint := range retiredEverywhere {
		for _, r := range Receivers() {
			if _, accepted := Lookup(r, name); accepted {
				continue // live on this receiver (e.g. @internal on a concept field)
			}
			ref := Check(r, Use{Name: name, Form: FormFlag})
			if ref == nil || ref.Code != CodeRetired {
				t.Errorf("%s @%s: got %v, want %s", r, name, ref, CodeRetired)
				continue
			}
			if !strings.Contains(ref.Message, hint) {
				t.Errorf("%s @%s: the refusal does not carry the hint: %s", r, name, ref.Message)
			}
		}
	}
	for key, hint := range retiredOn {
		if _, accepted := Lookup(key.receiver, key.name); accepted {
			t.Errorf("@%s is both retired on and accepted by %s", key.name, key.receiver)
			continue
		}
		ref := Check(key.receiver, Use{Name: key.name, Form: FormString})
		if ref == nil || ref.Code != CodeRetired {
			t.Errorf("%s @%s: got %v, want %s", key.receiver, key.name, ref, CodeRetired)
			continue
		}
		if !strings.Contains(ref.Message, hint) {
			t.Errorf("%s @%s: the refusal does not carry the hint: %s", key.receiver, key.name, ref.Message)
		}
	}

	// The retired @use* family is a prefix rule, not a list.
	for _, name := range []string{"useConcept", "useShape", "useQuery", "useAnythingNew"} {
		ref := Check(Query, Use{Name: name, Form: FormKeywords, Keys: []WrittenKey{{Name: "x", Bare: true}}})
		if ref == nil || ref.Code != CodeRetired || !strings.Contains(ref.Message, "file-top `use") {
			t.Errorf("@%s: got %v, want the retired @use* hint", name, ref)
		}
	}
	// ...which does not swallow a live name that merely starts with "use".
	if ref := Check(Query, Use{Name: "user", Form: FormFlag}); ref == nil || ref.Code != CodeUnknown {
		t.Errorf("@user: got %v, want %s", ref, CodeUnknown)
	}
}

// TestMisplacedNamesSayWhereTheyBelong: a name live on other receivers is
// refused as misplaced, and the message names those receivers.
func TestMisplacedNamesSayWhereTheyBelong(t *testing.T) {
	ref := Check(Query, Use{Name: "row", Form: FormFlag})
	if ref == nil || ref.Code != CodeMisplaced {
		t.Fatalf("@row on a query: got %v, want %s", ref, CodeMisplaced)
	}
	for _, want := range []string{"a shape", "SHAPE kind marker", "filter row.id"} {
		if !strings.Contains(ref.Message, want) {
			t.Errorf("@row on a query: the refusal does not say %q: %s", want, ref.Message)
		}
	}

	ref = Check(ArgsField, Use{Name: "unique", Form: FormFlag})
	if ref == nil || ref.Code != CodeMisplaced || !strings.Contains(ref.Message, "a concept field") {
		t.Errorf("@unique on an args field: got %v, want misplaced naming a concept field", ref)
	}
}

// TestRepeatedAnnotationIsRefused: a non-repeatable annotation written twice is
// refused -- a reader cannot tell which one the engine uses -- and a
// repeatable one accumulates.
func TestRepeatedAnnotationIsRefused(t *testing.T) {
	desc := Use{Name: "description", Form: FormString}
	ref := CheckAll(Query, []Use{desc, {Name: "actor", Form: FormFlag}, desc})
	if ref == nil || ref.Code != CodeRepeated || !strings.Contains(ref.Message, "@description") {
		t.Errorf("@description twice: got %v, want %s", ref, CodeRepeated)
	}
	fallback := Use{Name: "fallback", Form: FormString}
	if ref := CheckAll(Policy, []Use{fallback, fallback, fallback}); ref != nil {
		t.Errorf("@fallback is repeatable: got %v", ref)
	}
	// The first refusal wins, in the order written.
	ref = CheckAll(Query, []Use{{Name: "bogusFirst", Form: FormFlag}, desc, desc})
	if ref == nil || ref.Code != CodeUnknown {
		t.Errorf("an unknown name before a repeat: got %v, want %s", ref, CodeUnknown)
	}
	if ref := CheckAll(Query, nil); ref != nil {
		t.Errorf("no uses: got %v", ref)
	}
}

// TestMisplacedRefusalShowsAnExampleFromTheSameKindOfPlace: a misplaced name
// is shown written where it IS accepted, and in the same kind of place as
// where it was written -- an author who put @default on a builtin field is
// shown a field's `@default("...")`, not the provider's bare `@default` flag,
// which would teach a spelling every field refuses.
func TestMisplacedRefusalShowsAnExampleFromTheSameKindOfPlace(t *testing.T) {
	ref := Check(BuiltinField, Use{Name: "default", Form: FormString})
	if ref == nil || ref.Code != CodeMisplaced {
		t.Fatalf("@default on a builtin field: got %v, want %s", ref, CodeMisplaced)
	}
	if !strings.Contains(ref.Message, `as in @default("open")`) {
		t.Errorf("a field is shown a field's example: %s", ref.Message)
	}
	ref = Check(Tool, Use{Name: "default", Form: FormFlag})
	if ref == nil || ref.Code != CodeMisplaced {
		t.Fatalf("@default on a tool: got %v, want %s", ref, CodeMisplaced)
	}
	if !strings.HasSuffix(ref.Message, "as in @default") {
		t.Errorf("a construct is shown a construct's example (the provider's @default): %s", ref.Message)
	}
	// A name accepted only in the other kind of place still shows an example.
	ref = Check(Query, Use{Name: "pii", Form: FormFlag})
	if ref == nil || ref.Code != CodeMisplaced || !strings.Contains(ref.Message, "as in @pii") {
		t.Errorf("@pii on a query: got %v, want a misplaced refusal showing @pii", ref)
	}
}

// TestKeyShapeIsRefused: a flag key is written bare and every other key with
// a value; a key written in the other shape is refused with annotation_key,
// naming the key and how to write it. The parser stores a bare key as `true`,
// so a check that compared key NAMES only let `@rateLimit(maxCalls,
// periodSeconds)` through, and the tool registered with no rate limit at all.
func TestKeyShapeIsRefused(t *testing.T) {
	cases := []struct {
		name string
		r    Receiver
		use  Use
		want []string
	}{
		{
			name: "a valued key written bare",
			r:    Tool,
			use:  Use{Name: "rateLimit", Form: FormKeywords, Keys: []WrittenKey{{Name: "maxCalls", Bare: true}, {Name: "periodSeconds", Bare: true}}},
			want: []string{"@rateLimit on a tool: maxCalls takes a value", "write maxCalls=<number>", "as in @rateLimit(maxCalls=10, periodSeconds=60)"},
		},
		{
			name: "a string key written bare",
			r:    Query,
			use:  Use{Name: "cache", Form: FormKeywords, Keys: []WrittenKey{{Name: "ttl", Bare: true}}},
			want: []string{"@cache on a query: ttl takes a value", `write ttl="..."`},
		},
		{
			name: "a flag key given a value",
			r:    Concept,
			use:  Use{Name: "rowAuthz", Form: FormKeywords, Keys: []WrittenKey{{Name: "owner"}, {Name: "clusterOwner"}}},
			want: []string{"@rowAuthz on a concept: clusterOwner is a flag and takes no value", "write it bare (clusterOwner)", `as in @rowAuthz(owner="ownerUserId", clusterOwner)`},
		},
		{
			// The first offending key AS WRITTEN is the one named -- not the
			// first in some other order.
			name: "the first offending key in written order",
			r:    Automation,
			use:  Use{Name: "trigger", Form: FormKeywords, Keys: []WrittenKey{{Name: "schedule", Bare: true}, {Name: "concept", Bare: true}}},
			want: []string{"@trigger on an automation: schedule takes a value"},
		},
	}
	for _, tc := range cases {
		ref := Check(tc.r, tc.use)
		if ref == nil || ref.Code != CodeKey {
			t.Errorf("%s: got %v, want %s", tc.name, ref, CodeKey)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(ref.Message, w) {
				t.Errorf("%s: the refusal must say %q: %s", tc.name, w, ref.Message)
			}
		}
	}
	// Every key of every keyword placement is accepted in its own shape.
	for _, p := range Placements() {
		for _, k := range p.Keys {
			u := Use{Name: p.Name, Form: FormKeywords, Keys: []WrittenKey{{Name: k.Name, Bare: k.Type == "flag"}}}
			if ref := Check(p.Receiver, u); ref != nil {
				t.Errorf("%s @%s: key %s written in its own shape is refused: %v", p.Receiver, p.Name, k.Name, ref)
			}
		}
	}
}

// TestRefusalErrorEndsWithItsCode: the code is the stable part of a refusal
// and sits last, in brackets, so a wrapper that prefixes context keeps it
// findable.
func TestRefusalErrorEndsWithItsCode(t *testing.T) {
	ref := Check(Query, Use{Name: "bogus", Form: FormFlag})
	if ref == nil {
		t.Fatal("expected a refusal")
	}
	if got, want := ref.Error(), ref.Message+" ["+CodeUnknown+"]"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	var err error = ref
	if !strings.HasSuffix(err.Error(), "[annotation_unknown]") {
		t.Errorf("Error() does not end with its code: %q", err.Error())
	}
	for _, code := range []string{CodeUnknown, CodeRetired, CodeMisplaced, CodeForm, CodeKey, CodeRepeated} {
		if !strings.HasPrefix(code, "annotation_") {
			t.Errorf("code %q is not in the annotation_ family", code)
		}
	}
}

// TestByReceiverIsDerivedFromThePlacements: ByReceiver is a view, and a view
// that disagrees with what it views is the drift this package exists to end.
func TestByReceiverIsDerivedFromThePlacements(t *testing.T) {
	want := map[string][]string{}
	for _, p := range Placements() {
		want[string(p.Receiver)] = append(want[string(p.Receiver)], p.Name)
	}
	if len(ByReceiver) != len(want) {
		t.Errorf("ByReceiver has %d receivers, the placements %d", len(ByReceiver), len(want))
	}
	for r, names := range want {
		if got := strings.Join(ByReceiver[r], ","); got != strings.Join(names, ",") {
			t.Errorf("ByReceiver[%q] = %s, placements say %s", r, got, strings.Join(names, ","))
		}
	}
	if _, legacy := ByReceiver[""]; legacy {
		t.Error(`ByReceiver still carries the "" concept key -- it is "Concept" now`)
	}
	for _, r := range Receivers() {
		if len(ByReceiver[string(r)]) == 0 {
			t.Errorf("receiver %s carries no placement", r)
		}
	}
}

// TestPlacementsAreOrderedAndUnique: receiver order, then name, and no
// placement twice.
func TestPlacementsAreOrderedAndUnique(t *testing.T) {
	index := map[Receiver]int{}
	for i, r := range Receivers() {
		index[r] = i
	}
	all := Placements()
	seen := map[string]bool{}
	for i, p := range all {
		if _, ok := index[p.Receiver]; !ok {
			t.Errorf("placement @%s names an unknown receiver %q", p.Name, p.Receiver)
		}
		key := string(p.Receiver) + "/" + p.Name
		if seen[key] {
			t.Errorf("placement %s declared twice", key)
		}
		seen[key] = true
		if i == 0 {
			continue
		}
		prev := all[i-1]
		if index[prev.Receiver] > index[p.Receiver] ||
			(prev.Receiver == p.Receiver && prev.Name >= p.Name) {
			t.Errorf("placements out of order: %s/@%s before %s/@%s", prev.Receiver, prev.Name, p.Receiver, p.Name)
		}
	}
	// Placements hands out a copy.
	all[0].Name = "mutated"
	if Placements()[0].Name == "mutated" {
		t.Error("Placements returned the registry's own slice")
	}
}

// TestLookup finds a placement and misses one that is not there.
func TestLookup(t *testing.T) {
	p, ok := Lookup(Query, "cache")
	if !ok || p.Forms&FormNumber == 0 || p.Example == "" {
		t.Errorf("Lookup(Query, cache) = %+v, %v", p, ok)
	}
	if _, ok := Lookup(Query, "row"); ok {
		t.Error("Lookup(Query, row) found a placement")
	}
	if _, ok := Lookup(Receiver("Nope"), "description"); ok {
		t.Error("Lookup on an unknown receiver found a placement")
	}
}

// TestReceiversAndPhrases: the receiver list is the brief's, and every
// receiver reads as words with an article.
func TestReceiversAndPhrases(t *testing.T) {
	want := []string{"Query", "Mutation", "Logic", "Automation", "Action", "Capability", "Spec", "Tool", "Builtin", "Prompt", "Provider", "Shape", "Policy", "Rule", "Seed", "Concept", "ConceptBody", "ConceptField", "ArgsField", "ToolField", "PromptField", "BuiltinField"}
	var got []string
	for _, r := range Receivers() {
		got = append(got, string(r))
		phrase := r.Phrase()
		if !strings.HasPrefix(phrase, "a ") && !strings.HasPrefix(phrase, "an ") {
			t.Errorf("%s.Phrase() = %q, want an article", r, phrase)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Receivers() = %v, want %v", got, want)
	}
	for r, want := range map[Receiver]string{Query: "a query", ArgsField: "an args field", Automation: "an automation", ConceptField: "a concept field"} {
		if got := r.Phrase(); got != want {
			t.Errorf("%s.Phrase() = %q, want %q", r, got, want)
		}
	}
}

// TestFormMessagesAreGeneratedFromFormsAndExample pins the wording the brief
// fixed: the form refusal says what the placement takes, in words, and shows
// the example.
func TestFormMessagesAreGeneratedFromFormsAndExample(t *testing.T) {
	ref := Check(Query, Use{Name: "serverOnly", Form: FormString})
	if ref == nil || ref.Code != CodeForm {
		t.Fatalf("@serverOnly(\"x\"): got %v", ref)
	}
	if want := "@serverOnly on a query takes no arguments, as in @serverOnly"; !strings.HasPrefix(ref.Message, want) {
		t.Errorf("got %q, want prefix %q", ref.Message, want)
	}
	ref = Check(Rule, Use{Name: "precedence", Form: FormString})
	if want := "@precedence on a rule takes one number, as in @precedence(60)"; ref == nil || !strings.HasPrefix(ref.Message, want) {
		t.Errorf("got %v, want prefix %q", ref, want)
	}
	if !strings.Contains(ref.Message, "one string") {
		t.Errorf("the form refusal does not say what was written: %s", ref.Message)
	}
}

// TestEveryPlacementHasADoc: completion and hover show the doc, and the
// generated matrix prints it. Every NAME also carries a Docs entry, for the
// consumers that look a doc up by name alone (hover on an `@name`).
func TestEveryPlacementHasADoc(t *testing.T) {
	for _, p := range Placements() {
		if docFor(p) == "" {
			t.Errorf("%s @%s has no doc (neither Placement.Doc nor Docs[%q])", p.Receiver, p.Name, p.Name)
		}
		if Docs[p.Name] == "" {
			t.Errorf("@%s has no Docs entry, so a hover that knows only the name shows nothing", p.Name)
		}
	}
}

// TestFormsHaveWords: every form the parser can report reads as words, so no
// refusal prints a bit pattern.
func TestFormsHaveWords(t *testing.T) {
	for _, f := range allForms {
		if w := f.String(); w == "" || strings.Contains(w, "Form(") {
			t.Errorf("form %d has no words: %q", f, w)
		}
	}
	if got := (FormString | FormStrings).String(); got != "one or more strings" {
		t.Errorf("String|Strings reads %q", got)
	}
	names := make([]string, 0, len(allForms))
	for _, f := range allForms {
		names = append(names, f.String())
	}
	sort.Strings(names)
	for i := 1; i < len(names); i++ {
		if names[i] == names[i-1] {
			t.Errorf("two forms read the same: %q", names[i])
		}
	}
}
