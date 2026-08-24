package parser

import (
	"strings"
	"testing"
)

// conceptAttr parses a single annotation line above a stub concept and
// returns the attribute with the given name.
func conceptAttr(t *testing.T, line, name string) *Attribute {
	t.Helper()
	src := line + "\nconcept probe {\n  handle string\n}\n"
	for _, a := range parseConceptAttrs(t, src) {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no @%s attribute parsed from %q", name, line)
	return nil
}

func TestParseOriginAcceptsAConnectorNameAndTheMemQLDefault(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{`@origin("shopify")`, "shopify"},
		{`@origin("memql")`, OriginMemQL},
		{`@origin("quickBooks")`, "quickBooks"},
	}
	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			got, err := ParseOrigin(conceptAttr(t, tc.line, OriginAnnotation))
			if err != nil {
				t.Fatalf("ParseOrigin(%s): %v", tc.line, err)
			}
			if got != tc.want {
				t.Errorf("ParseOrigin(%s) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

func TestParseOriginRefusesMalformedDeclarations(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{"empty name", `@origin("")`, "is empty"},
		{"two origins", `@origin("shopify", "quickBooks")`, "exactly one name"},
		{"keyword args", `@origin(name="shopify")`, "not keyword arguments"},
		{"upper camel", `@origin("Shopify")`, "lowerCamelCase"},
		{"snake case", `@origin("quick_books")`, "lowerCamelCase"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseOrigin(conceptAttr(t, tc.line, OriginAnnotation))
			if err == nil {
				t.Fatalf("ParseOrigin(%s) accepted a malformed declaration", tc.line)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ParseOrigin(%s) error = %q, want it to contain %q", tc.line, err, tc.want)
			}
		})
	}
}

func TestParseMirroredToAcceptsOneOrManyTargets(t *testing.T) {
	cases := []struct {
		line string
		want []string
	}{
		{`@mirroredTo("shopify")`, []string{"shopify"}},
		{`@mirroredTo("shopify", "quickBooks")`, []string{"shopify", "quickBooks"}},
	}
	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			got, err := ParseMirroredTo(conceptAttr(t, tc.line, MirroredToAnnotation))
			if err != nil {
				t.Fatalf("ParseMirroredTo(%s): %v", tc.line, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseMirroredTo(%s) = %v, want %v", tc.line, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("ParseMirroredTo(%s)[%d] = %q, want %q -- authored order is the outbox append order",
						tc.line, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseMirroredToRefusesMalformedDeclarations(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{"empty name", `@mirroredTo("")`, "is empty"},
		{"memql as a target", `@mirroredTo("memql")`, "not a mirror target"},
		{"repeated target", `@mirroredTo("shopify", "shopify")`, "twice"},
		{"keyword args", `@mirroredTo(target="shopify")`, "not keyword arguments"},
		{"malformed name", `@mirroredTo("Shopify")`, "lowerCamelCase"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseMirroredTo(conceptAttr(t, tc.line, MirroredToAnnotation))
			if err == nil {
				t.Fatalf("ParseMirroredTo(%s) accepted a malformed declaration", tc.line)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ParseMirroredTo(%s) error = %q, want it to contain %q", tc.line, err, tc.want)
			}
		})
	}
}

// The three states, derived from the two declarations. This is the D2
// table, executable.
func TestDataStateDerivesFromTheTwoDeclarations(t *testing.T) {
	cases := []struct {
		name string
		decl OriginDecl
		want DataState
	}{
		{"no declaration at all", OriginDecl{}, DataStateNative},
		{"explicit memql, no targets", OriginDecl{Origin: OriginMemQL}, DataStateNative},
		{"external origin", OriginDecl{Origin: "shopify"}, DataStateMirror},
		{"memql with one target", OriginDecl{Origin: OriginMemQL, MirroredTo: []string{"shopify"}}, DataStateOrigin},
		{"absent origin with a target", OriginDecl{MirroredTo: []string{"shopify"}}, DataStateOrigin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.decl.State(); got != tc.want {
				t.Errorf("%+v.State() = %q, want %q", tc.decl, got, tc.want)
			}
		})
	}
}

// A mirror may not also be mirrored onward from here: re-mirroring is
// the origin's job (D2/D3).
func TestMirroredToOnAnExternalOriginIsRefused(t *testing.T) {
	err := ValidateOriginDecl(OriginDecl{Origin: "shopify", MirroredTo: []string{"quickBooks"}})
	if err == nil {
		t.Fatal("ValidateOriginDecl admitted @mirroredTo on a mirror concept -- a mirror that also publishes is a second origin wearing the first one's badge")
	}
	for _, want := range []string{"MIRROR", "shopify", "@mirroredTo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q -- the message has to name the way out", err, want)
		}
	}
	if err := ValidateOriginDecl(OriginDecl{Origin: OriginMemQL, MirroredTo: []string{"shopify"}}); err != nil {
		t.Errorf("ValidateOriginDecl refused a legitimate origin declaration: %v", err)
	}
	if err := ValidateOriginDecl(OriginDecl{Origin: "shopify"}); err != nil {
		t.Errorf("ValidateOriginDecl refused a plain mirror: %v", err)
	}
}

// Connectors() is what the boot check resolves against the registry:
// every name the declaration mentions, and nothing else. "memql" is
// never in it -- MemQL is not a connector.
func TestConnectorsNamesEveryDeclaredConnectorAndNeverMemQL(t *testing.T) {
	cases := []struct {
		name string
		decl OriginDecl
		want []string
	}{
		{"native", OriginDecl{}, nil},
		{"explicit memql origin", OriginDecl{Origin: OriginMemQL}, nil},
		{"mirror", OriginDecl{Origin: "shopify"}, []string{"shopify"}},
		{"origin with two targets", OriginDecl{Origin: OriginMemQL, MirroredTo: []string{"shopify", "quickBooks"}}, []string{"quickBooks", "shopify"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.decl.Connectors()
			if len(got) != len(tc.want) {
				t.Fatalf("Connectors() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("Connectors()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Anything the renderers emit must read back to the same declaration.
func TestOriginDeclarationsRoundTripThroughTheRenderers(t *testing.T) {
	cases := []OriginDecl{
		{Origin: "shopify"},
		{Origin: OriginMemQL, MirroredTo: []string{"shopify"}},
		{Origin: OriginMemQL, MirroredTo: []string{"shopify", "quickBooks"}},
	}
	for _, decl := range cases {
		t.Run(string(decl.State())+"/"+decl.Origin, func(t *testing.T) {
			var got OriginDecl
			if line := FormatOrigin(decl); line != "" {
				name, err := ParseOrigin(conceptAttr(t, line, OriginAnnotation))
				if err != nil {
					t.Fatalf("FormatOrigin(%+v) = %q, which ParseOrigin refuses: %v", decl, line, err)
				}
				got.Origin = name
			}
			if line := FormatMirroredTo(decl); line != "" {
				names, err := ParseMirroredTo(conceptAttr(t, line, MirroredToAnnotation))
				if err != nil {
					t.Fatalf("FormatMirroredTo(%+v) = %q, which ParseMirroredTo refuses: %v", decl, line, err)
				}
				got.MirroredTo = names
			}
			if got.State() != decl.State() {
				t.Errorf("round trip of %+v produced %+v -- state %q became %q", decl, got, decl.State(), got.State())
			}
		})
	}
	// A native concept renders to nothing: there is no annotation that
	// carries "the default", and emitting one would be noise on 100+
	// concepts.
	native := OriginDecl{}
	if line := FormatOrigin(native); line != "" {
		t.Errorf("FormatOrigin(native) = %q, want \"\" -- native is the default and needs no annotation", line)
	}
}
