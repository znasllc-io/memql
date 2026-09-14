package sense

// relationship_label_tooling_3661_test.go covers the editor surface for the
// `as` domain label on @relationship (memql#3652) and the label-scoped
// traversal form (memql#3656).
//
// The completion half was already correct by construction: kwarg names come
// from annotations.KeywordArgs, so adding `as` to the registry surfaced it
// with no change here. The tests below pin that it stays that way, and that
// the kwarg VALUE positions do not start leaking concept names.
//
// The hover half was NOT correct, and not only for the new argument. Hover
// resolves a bare token through a first-match-wins ladder with no idea of its
// surroundings, so two of @relationship's own argument names were already
// being answered by unrelated entries:
//
//   - `as` is a lexer keyword (`forEach ... as x`), so it matched KeywordDocs
//     and hovered as the forEach alias.
//   - `type` is an annotation name, so it matched AnnotationDocs and hovered
//     as the @type concept/provider annotation.
//
// and `field` / `target` / `direction` matched nothing at all. So this is a
// repair of a surface that never worked, not a feature bolted onto a working
// one -- which is why the fix is one kwarg-aware branch above the ladder
// rather than a special case for `as`.

import (
	"strings"
	"testing"
)

// kwargCompletions runs completion at the end of src and returns the labels.
func kwargCompletions(t *testing.T, src string) map[string]bool {
	t.Helper()
	return completeAtEnd(New(refShapeRegistry()), src)
}

// TestRelationshipAsKwargIsOffered pins that `as` reaches completion. It is
// registry-driven, so this fails if the ArgSpec is dropped from
// annotations.KeywordArgs -- which is the only place it is declared.
func TestRelationshipAsKwargIsOffered(t *testing.T) {
	got := kwargCompletions(t, `concept ticket {
  @relationship(`)
	for _, want := range []string{"type", "field", "target", "direction", "as"} {
		if !got[want] {
			t.Errorf("@relationship( should offer the %q argument, got %v", want, got)
		}
	}
}

// TestRelationshipAsKwargPrefixFilter pins the prefix path for the new
// argument specifically -- `as` is the only @relationship argument starting
// with "a", so a prefix of "a" must narrow to exactly it.
func TestRelationshipAsKwargPrefixFilter(t *testing.T) {
	got := kwargCompletions(t, `concept ticket {
  @relationship(type="references", a`)
	if !got["as"] {
		t.Errorf("prefix `a` inside @relationship( should offer `as`, got %v", got)
	}
	for _, unwanted := range []string{"type", "field", "target", "direction"} {
		if got[unwanted] {
			t.Errorf("prefix `a` should not offer %q, got %v", unwanted, got)
		}
	}
}

// TestRelationshipAsValueOffersNoClosedList is the load-bearing one.
//
// `as` is an OPEN vocabulary by design: it is validated for form only and
// never for membership, because a closed list is exactly what made an
// unrecognised verb a boot refusal that took the mesh down twice. Completion
// offering a fixed set of values would rebuild that closed list in the
// editor, and an author would reasonably read it as the allowed set.
//
// So the value position must stay empty. This asserts it offers no concepts
// (the noise the sibling `target=` position deliberately does offer) and no
// invented label vocabulary.
func TestRelationshipAsValueOffersNoClosedList(t *testing.T) {
	got := kwargCompletions(t, `concept ticket {
  @relationship(type="references", as="`)

	if got["v1:reference:node"] || got["v1:reference:order"] {
		t.Errorf("as= must not offer concepts -- that is the target= position, got %v", got)
	}
	for _, invented := range []string{"assignedTo", "respondsAs", "actsFor", "belongsToSpace"} {
		if got[invented] {
			t.Errorf("as= offered %q: the label vocabulary is OPEN and must never be "+
				"completed from a fixed list (memql#3652), got %v", invented, got)
		}
	}
}

// hoverAt returns the hover contents at the end of the given single-line
// source, positioned on the last identifier.
func hoverContainsAt(t *testing.T, src string, line, col int) string {
	t.Helper()
	res := New(refShapeRegistry()).Hover(src, line, col, "")
	if res == nil {
		return ""
	}
	return res.Contents
}

// TestRelationshipKwargHover covers the repair. Each argument must hover as
// what it is inside @relationship(...), not as whatever the bare token happens
// to match elsewhere in the language.
func TestRelationshipKwargHover(t *testing.T) {
	// One line, so the columns below are unambiguous.
	const src = `  @relationship(type="references", as="assignedTo", field="agentId", target=agent, direction="outgoing")`

	cases := []struct {
		name        string
		kwarg       string
		mustHave    []string
		mustNotHave []string
	}{
		{
			name:     "type is the structural axis, not the @type annotation",
			kwarg:    "type",
			mustHave: []string{"@relationship", "STRUCTURAL"},
			// The @type annotation doc is about concept/provider kinds.
			mustNotHave: []string{"collection", "provider"},
		},
		{
			name:        "as is the domain axis, not the forEach alias",
			kwarg:       "as",
			mustHave:    []string{"@relationship", "DOMAIN"},
			mustNotHave: []string{"forEach", "iteration"},
		},
		{name: "field", kwarg: "field", mustHave: []string{"@relationship", "foreign key"}},
		{name: "target", kwarg: "target", mustHave: []string{"@relationship", "concept"}},
		{name: "direction", kwarg: "direction", mustHave: []string{"@relationship", "outgoing"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// +2 lands INSIDE the token: a column equal to a token's start is also
			// the end column of the one before it, and tokenAtPosition returns the
			// first match.
			col := strings.Index(src, tc.kwarg+"=") + 2
			if col <= 0 {
				t.Fatalf("fixture does not contain %q=", tc.kwarg)
			}
			contents := hoverContainsAt(t, src, 1, col)
			if contents == "" {
				t.Fatalf("no hover for %q inside @relationship(...)", tc.kwarg)
			}
			for _, want := range tc.mustHave {
				if !strings.Contains(contents, want) {
					t.Errorf("hover for %q should mention %q, got:\n%s", tc.kwarg, want, contents)
				}
			}
			for _, unwanted := range tc.mustNotHave {
				if strings.Contains(contents, unwanted) {
					t.Errorf("hover for %q leaked the unrelated %q entry, got:\n%s",
						tc.kwarg, unwanted, contents)
				}
			}
		})
	}
}

// TestKwargHoverDoesNotHijackTheLanguage is the other half of the guard: the
// branch sits above the whole ladder, so it must fire ONLY inside an
// annotation's parentheses. `as` outside one is still the forEach alias, and
// `type` outside one is still the annotation.
func TestKwargHoverDoesNotHijackTheLanguage(t *testing.T) {
	// `as` in its forEach sense, with no annotation in sight.
	const loop = `  forEach items as item {`
	col := strings.Index(loop, "as ") + 2
	if contents := hoverContainsAt(t, loop, 1, col); contents != "" &&
		!strings.Contains(contents, "forEach") {
		t.Errorf("`as` outside an annotation must keep its keyword hover, got:\n%s", contents)
	}

	// A CLOSED annotation before the cursor must not put us "inside" it.
	const closed = `  @relationship(type="parent") type`
	col = strings.LastIndex(closed, "type") + 2
	contents := hoverContainsAt(t, closed, 1, col)
	if strings.Contains(contents, "@relationship argument") {
		t.Errorf("a closed annotation must not capture a later token, got:\n%s", contents)
	}
}

// TestKwargHoverIgnoresAnUnknownArgument pins that an argument the registry
// does not model falls THROUGH to the ladder rather than being answered with
// an empty doc -- so a typo does not get a confident-looking hover.
func TestKwargHoverIgnoresAnUnknownArgument(t *testing.T) {
	const src = `  @relationship(tipe="parent")`
	col := strings.Index(src, "tipe") + 2
	if contents := hoverContainsAt(t, src, 1, col); strings.Contains(contents, "argument") {
		t.Errorf("an unmodelled argument must not hover as one, got:\n%s", contents)
	}
}

// TestRelationshipAxesDiagnostics covers the editor half of the load gate: a
// malformed `as` and an unknown structural `type` must squiggle with the same
// wording the engine refuses boot with, so an author is not told two different
// things by two surfaces.
func TestRelationshipAxesDiagnostics(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		wantCode string
		wantIn   string
	}{
		{
			name:     "an invented structural type names the way out",
			src:      `concept t {` + "\n" + `  @relationship(type="assignedTo", field="a", target=b, direction="outgoing")` + "\n" + `}`,
			wantCode: "relationship-type-unknown",
			wantIn:   `type="references", as="assignedTo"`,
		},
		{
			name:     "an upper-case as label is malformed",
			src:      `concept t {` + "\n" + `  @relationship(type="references", as="AssignedTo", field="a", target=b, direction="outgoing")` + "\n" + `}`,
			wantCode: "relationship-as-malformed",
			wantIn:   "lowerCamelCase",
		},
		{
			name:     "an underscored as label is malformed",
			src:      `concept t {` + "\n" + `  @relationship(type="references", as="assigned_to", field="a", target=b, direction="outgoing")` + "\n" + `}`,
			wantCode: "relationship-as-malformed",
			wantIn:   "lowerCamelCase",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var found *Diagnostic
			for _, d := range New(nil).Diagnose(tc.src, "concepts.memql") {
				if d.Code == tc.wantCode {
					found = &d
					break
				}
			}
			if found == nil {
				t.Fatalf("expected a %q diagnostic for:\n%s", tc.wantCode, tc.src)
			}
			if !strings.Contains(found.Message, tc.wantIn) {
				t.Errorf("message should contain %q, got: %s", tc.wantIn, found.Message)
			}
			if found.Severity != SeverityError {
				t.Errorf("expected an error severity, got %v", found.Severity)
			}
		})
	}
}

// TestRelationshipAxesDiagnosticsStayQuietOnValidInput is the false-positive
// guard. `as` is an OPEN vocabulary: any well-formed identifier is legal, and
// a diagnostic that second-guessed the value would rebuild the closed list
// this whole epic removed.
func TestRelationshipAxesDiagnosticsStayQuietOnValidInput(t *testing.T) {
	valid := []string{
		`  @relationship(type="parent", field="spaceId", target=space, direction="outgoing")`,
		`  @relationship(type="references", as="respondsAs", field="agentId", target=agent, direction="outgoing")`,
		`  @relationship(type="parent", as="belongsToSpace", field="spaceId", target=space, direction="outgoing")`,
		// A verb nobody has ever used before is still legal -- that is the point.
		`  @relationship(type="references", as="frobnicatesWith2", field="x", target=y, direction="outgoing")`,
		// Case / underscore insensitivity on the TYPE mirrors
		// canonicalRelationshipType's normalisation. Uses createdBy rather
		// than the retired dependsOn: this list is "legal input", and
		// memql#3655 made that one illegal.
		`  @relationship(type="CREATED_BY", field="authorId", target=user, direction="outgoing")`,
		// A commented-out declaration is not a declaration.
		`  // @relationship(type="references", as="Bad_Label", field="x", target=y, direction="outgoing")`,
	}
	for _, line := range valid {
		src := "concept t {\n" + line + "\n}"
		for _, d := range New(nil).Diagnose(src, "concepts.memql") {
			if strings.HasPrefix(d.Code, "relationship-") {
				t.Errorf("false positive %s on a legal declaration:\n  %s\n  %s", d.Code, line, d.Message)
			}
		}
	}
}

// TestRelationshipWrapperSignatureHelp covers the traversal half. Before
// memql#3661 the wrappers were invisible to sense entirely -- absent from
// BuiltinFunctions, so SignatureHelp returned nil on `references(`.
//
// memql#3661 then modelled each traversal as TWO hand-kept readings, because
// nothing could say "optional". The v1 catalog (memql#5365) says it: every
// traversal is ONE signature whose leading `as` label is an optional
// parameter, and the active parameter skips the label when the call leaves it
// out. The properties below are the ones #3661 pinned, restated for that shape.
func TestRelationshipWrapperSignatureHelp(t *testing.T) {
	svc := New(nil)

	t.Run("the signature states the optional leading label", func(t *testing.T) {
		src := "references("
		res := svc.SignatureHelp(src, 1, len(src)+1)
		if res == nil {
			t.Fatal("no signature help for references( -- the traversal surface is invisible again")
		}
		if len(res.Signatures) != 1 {
			t.Fatalf("expected the catalog's one signature, got %d: %+v", len(res.Signatures), res.Signatures)
		}
		sig := res.Signatures[0]
		if sig.Label != "references(label? string, match lambda) rows" {
			t.Errorf("signature = %q, want the catalog's", sig.Label)
		}
		if len(sig.Parameters) != 2 || sig.Parameters[0].Label != "label? string" || sig.Parameters[1].Label != "match lambda" {
			t.Errorf("parameters = %+v, want the optional label then the match", sig.Parameters)
		}
		if res.ActiveParameter != 0 {
			t.Errorf("before anything is typed the (optional) label is active, got %d", res.ActiveParameter)
		}
	})

	t.Run("past a label the match is active", func(t *testing.T) {
		src := `references("assignedTo", `
		res := svc.SignatureHelp(src, 1, len(src)+1)
		if res == nil {
			t.Fatal("no signature help past the comma")
		}
		if res.ActiveParameter != 1 {
			t.Errorf("expected the match to be active, got %d", res.ActiveParameter)
		}
	})

	t.Run("without a label the first argument is the match", func(t *testing.T) {
		src := `references(r => r.`
		res := svc.SignatureHelp(src, 1, len(src)+1)
		if res == nil {
			t.Fatal("no signature help inside the match lambda")
		}
		if res.ActiveParameter != 1 {
			t.Errorf("a first argument that is not a string is the match, got parameter %d", res.ActiveParameter)
		}
	})

	t.Run("every traversal but ids takes the optional label", func(t *testing.T) {
		// contains is in this list since memql#5365: its two-argument slot was
		// the substring search, which is string.includes now, so the reason it
		// could not take a label is gone.
		for _, fn := range []string{"parentOf", "childOf", "aliasOf", "equals",
			"references", "owns", "createdBy", "contains"} {
			src := fn + "("
			res := svc.SignatureHelp(src, 1, len(src)+1)
			if res == nil || len(res.Signatures) != 1 || len(res.Signatures[0].Parameters) != 2 ||
				res.Signatures[0].Parameters[0].Label != "label? string" {
				t.Errorf("%s should take an optional leading label, got %+v", fn, res)
			}
		}
	})

	// `ids` follows no relationship, so it takes no label, and says so rather
	// than leaving the absence unexplained.
	t.Run("ids takes no label", func(t *testing.T) {
		src := "ids("
		res := svc.SignatureHelp(src, 1, len(src)+1)
		if res == nil || len(res.Signatures) != 1 {
			t.Fatalf("ids should have exactly one signature, got %+v", res)
		}
		if n := len(res.Signatures[0].Parameters); n != 1 {
			t.Errorf("ids takes exactly one parameter, got %d", n)
		}
		if !strings.Contains(res.Signatures[0].Documentation, "takes no label") {
			t.Errorf("ids should say WHY it takes no label, got: %s", res.Signatures[0].Documentation)
		}
	})

	t.Run("the label parameter names no closed set", func(t *testing.T) {
		src := "owns("
		res := svc.SignatureHelp(src, 1, len(src)+1)
		if res == nil || len(res.Signatures) != 1 {
			t.Fatal("expected the signature for owns(")
		}
		doc := res.Signatures[0].Parameters[0].Documentation
		if !strings.Contains(doc, "open") {
			t.Errorf("the label parameter should say the vocabulary is open, got: %s", doc)
		}
		for _, invented := range []string{"assignedTo\", \"", "one of:"} {
			if strings.Contains(doc, invented) {
				t.Errorf("signature help must not enumerate label values, found %q in: %s",
					invented, doc)
			}
		}
	})
}

// TestRelationshipTypeValueCompletion covers the asymmetry that IS the design:
// `type` is closed so its values are offered, `as` is open so they are not.
func TestRelationshipTypeValueCompletion(t *testing.T) {
	t.Run("the structural set is offered", func(t *testing.T) {
		got := kwargCompletions(t, `concept ticket {
  @relationship(type="`)
		for _, want := range []string{"parent", "references", "owns", "createdBy",
			"contains", "alias", "equals"} {
			if !got[want] {
				t.Errorf("type= should offer the structural type %q, got %v", want, got)
			}
		}
		// Not a dump: concept names belong to the target= position.
		if got["v1:reference:node"] {
			t.Errorf("type= must not offer concepts, got %v", got)
		}
	})

	t.Run("prefix filters", func(t *testing.T) {
		got := kwargCompletions(t, `concept ticket {
  @relationship(type="cr`)
		if !got["createdBy"] {
			t.Errorf("prefix `cr` should offer createdBy, got %v", got)
		}
		if got["parent"] || got["owns"] {
			t.Errorf("prefix `cr` should not offer unrelated types, got %v", got)
		}
	})

	t.Run("a retired type is not taught", func(t *testing.T) {
		// memql#3655 retired dependsOn / formedFrom to `as` labels, so they
		// are no longer structural types and completion must not offer them.
		got := kwargCompletions(t, `concept ticket {
  @relationship(type="`)
		for _, retired := range []string{"dependsOn", "formedFrom"} {
			if got[retired] {
				t.Errorf("type= should not offer %q, retired by memql#3655, got %v",
					retired, got)
			}
		}
	})
}
