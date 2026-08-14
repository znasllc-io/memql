package sense

import (
	"sort"
	"strings"
	"testing"
)

// funcCallSource builds a logic body whose cursor sits just after `name(` on
// line 3, so analyzeCursorContext reports ContextFuncCallArgs with ParentFunc
// == name. Returns the source, the 1-based cursor line, and column.
func funcCallSource(name string) (string, int, int) {
	line3 := "    return " + name + "("
	src := "logic wp8 {\n  body {\n" + line3 + "\n  }\n}"
	// Column is 1-based and points just past the '(' (== len(line3)+1).
	return src, 3, len(line3) + 1
}

func TestSignatureHelp_UserFunctionFromArgsSchema(t *testing.T) {
	reg := &stubRegistry{functions: map[string]*FunctionInfo{
		"createSpace": {
			Name:        "createSpace",
			Description: "Create a cognition space.",
			Kind:        "mutation",
			Args: []ArgInfo{
				{Name: "spaceId", Type: "string", Required: true},
				{Name: "name", Type: "string", Required: true},
				{Name: "description", Type: "string", Required: false},
			},
		},
	}}
	s := New(reg)

	src, line, col := funcCallSource("createSpace")
	res := s.SignatureHelp(src, line, col)
	if res == nil {
		t.Fatal("expected a signature result for a user function call")
	}
	if len(res.Signatures) != 1 {
		t.Fatalf("expected 1 signature, got %d", len(res.Signatures))
	}
	sig := res.Signatures[0]
	wantLabel := "createSpace(spaceId string, name string, description string?)"
	if sig.Label != wantLabel {
		t.Errorf("signature label = %q; want %q", sig.Label, wantLabel)
	}
	if len(sig.Parameters) != 3 {
		t.Fatalf("expected 3 parameters (from the args schema), got %d", len(sig.Parameters))
	}
	if sig.Parameters[0].Label != "spaceId string" || sig.Parameters[2].Label != "description string?" {
		t.Errorf("parameter labels wrong: %q ... %q", sig.Parameters[0].Label, sig.Parameters[2].Label)
	}
	if res.ActiveParameter != 0 {
		t.Errorf("active parameter = %d; want 0 (cursor at first arg)", res.ActiveParameter)
	}
}

func TestSignatureHelp_ActiveParameterTracksComma(t *testing.T) {
	reg := &stubRegistry{functions: map[string]*FunctionInfo{
		"f": {Name: "f", Args: []ArgInfo{{Name: "a", Type: "string", Required: true}, {Name: "b", Type: "string", Required: true}}},
	}}
	s := New(reg)
	// Cursor after `f(x, ` -> second argument (index 1).
	line3 := "    return f(x, "
	src := "logic wp8 {\n  body {\n" + line3 + "\n  }\n}"
	res := s.SignatureHelp(src, 3, len(line3)+1)
	if res == nil {
		t.Fatal("expected a signature result")
	}
	if res.ActiveParameter != 1 {
		t.Errorf("active parameter = %d; want 1 (after one comma)", res.ActiveParameter)
	}
}

// TestSignatureHelp_Builtin sweeps every builtin carrying a signature, in
// sorted order -- no randomness to flake on, and every builtin actually
// checked, one named subtest each.
//
// It used to pick ONE builtin at random -- `for n, def := range
// BuiltinFunctions { ... break }` over a map, which Go iterates in a
// randomised order -- and assert that was the only signature offered. Two
// things were wrong with that: the subject changed on every run, so a
// failure never said which of the 34 builtins it was (or that the other 33
// went untested that run); and the assertion itself is untrue for
// `contains`, which is also a relationship-traversal wrapper and so offers
// TWO readings (see TestSignatureHelpContainsOffersBothReadings) -- the
// random pick surfaced that as a flake on roughly one run in 34 instead of a
// reliable, attributable failure (memql#3735). The sibling handler test,
// TestSignatureHelpHandler_Builtin in cmd/memql-lsp/signaturehelp_test.go,
// hit the same bug and was fixed the same way in memql#3779 (commit
// 884655f2); this mirrors that shape rather than inventing a second one.
//
// The property asserted is the one true of all 34: the builtin's own
// reading is AMONG the signatures offered, rather than "is the only one",
// which is exactly what `contains` breaks.
func TestSignatureHelp_Builtin(t *testing.T) {
	names := make([]string, 0, len(BuiltinFunctions))
	for name, def := range BuiltinFunctions {
		if def.Signature != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no builtin carries a signature; the projection from dslspec is empty")
	}

	s := New(&stubRegistry{})
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			src, line, col := funcCallSource(name)
			res := s.SignatureHelp(src, line, col)
			if res == nil || len(res.Signatures) == 0 {
				t.Fatalf("no signature offered for builtin %q", name)
			}

			want := BuiltinFunctions[name].Signature
			var labels []string
			for _, sig := range res.Signatures {
				if sig.Label == want {
					return
				}
				labels = append(labels, sig.Label)
			}
			t.Errorf("the builtin's own reading %q is not among the signatures offered: %v", want, labels)
		})
	}
}

func TestSignatureHelp_NoneOutsideCall(t *testing.T) {
	s := New(&stubRegistry{})
	// Cursor in a plain body, not inside a call.
	if res := s.SignatureHelp("logic x {\n  body {\n    return 1\n  }\n}", 3, 5); res != nil {
		t.Errorf("expected no signature outside a call, got %+v", res)
	}
}

func TestFormatArgList(t *testing.T) {
	got := formatArgList([]ArgInfo{
		{Name: "id", Type: "string", Required: true},
		{Name: "limit", Type: "number", Required: false},
	})
	if want := "id string, limit number?"; got != want {
		t.Errorf("formatArgList = %q; want %q", got, want)
	}
	if got := formatArgList(nil); got != "" {
		t.Errorf("formatArgList(nil) = %q; want empty", got)
	}
	if !strings.Contains(argLabel(ArgInfo{Name: "x", Required: false}), "?") {
		t.Error("optional arg label should carry a trailing ?")
	}
}

// TestWrapperBuiltinOverlapIsPinned pins the names carried by BOTH
// relationshipWrapperArities and BuiltinFunctions.
//
// The overlap is what made memql#3779 a random failure rather than a visible
// one. relationshipWrapperSignatures now DERIVES the second reading for such a
// name from BuiltinFunctions, which is right for any collision -- but a
// collision is still a decision about what a name means in two grammars at
// once, and deriving it silently is how the next one arrives as a flake in
// somebody else's PR. Pinning the set makes it arrive as this diff.
//
// Adding a name here is fine. Doing it deliberately is the point.
func TestWrapperBuiltinOverlapIsPinned(t *testing.T) {
	want := map[string]bool{"contains": true}

	got := map[string]bool{}
	for name := range relationshipWrapperArities {
		if _, ok := BuiltinFunctions[name]; ok {
			got[name] = true
		}
	}

	for name := range got {
		if !want[name] {
			t.Errorf("%q is now both a relationship wrapper and a builtin. Both readings are real, so "+
				"SignatureHelp will offer both -- confirm that is what you meant and add it to `want`", name)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("%q is no longer in both tables; drop it from `want`", name)
		}
	}
}

// TestSignatureHelpContainsOffersBothReadings: `contains` is the one name the
// DSL gives two grammars -- a one-argument collection traversal and the
// two-argument substring search -- and signature help has to offer both.
//
// It offered only the traversal until memql#3779, because the wrapper branch
// runs first and returns before the builtin branch is reached. The substring
// reading was sitting in BuiltinFunctions the whole time, with parameter docs.
func TestSignatureHelpContainsOffersBothReadings(t *testing.T) {
	s := New(nil)
	src, line, col := funcCallSource("contains")

	res := s.SignatureHelp(src, line, col)
	if res == nil {
		t.Fatal("no signature help for contains(")
	}
	if len(res.Signatures) != 2 {
		t.Fatalf("expected both readings, got %d: %+v", len(res.Signatures), res.Signatures)
	}
	if res.Signatures[0].Label != "contains(expr)" {
		t.Errorf("first reading = %q; want the traversal, which is the default a reader should see",
			res.Signatures[0].Label)
	}
	if want := BuiltinFunctions["contains"].Signature; res.Signatures[1].Label != want {
		t.Errorf("second reading = %q; want the builtin's own %q", res.Signatures[1].Label, want)
	}
	if len(res.Signatures[1].Parameters) != len(BuiltinFunctions["contains"].Parameters) {
		t.Error("the substring reading lost its parameter docs on the way through")
	}
	if res.ActiveSignature != 0 {
		t.Errorf("before the first comma the traversal is active, got index %d", res.ActiveSignature)
	}
}

// Past the first comma `contains` can only be the two-argument substring
// search: the traversal takes no label and no second argument. The existing
// activeRelationshipSignature rule already says so -- this pins that it keeps
// meaning the right thing now that index 1 is the builtin rather than a
// label-scoped form.
func TestSignatureHelpContainsHighlightsSubstringPastTheComma(t *testing.T) {
	s := New(nil)
	const line3 = `    return contains("haystack", `
	src := "logic x {\n  body {\n" + line3 + "\n  }\n}"

	res := s.SignatureHelp(src, 3, len(line3)+1)
	if res == nil {
		t.Fatal("no signature help past the comma")
	}
	if res.ActiveSignature != 1 {
		t.Errorf("ActiveSignature = %d; past a comma contains can only be the substring search", res.ActiveSignature)
	}
}
