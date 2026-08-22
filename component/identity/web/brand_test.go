package web

import (
	"strings"
	"testing"
)

// The brand endpoint's contract, and the CSP directive it bought back.

func TestBrandCSSEmitsNoRuleWithoutAnOverride(t *testing.T) {
	css := brandCSS(Settings{})
	if strings.Contains(css, ":root") {
		t.Errorf("an unset brand emitted a :root rule:\n%s\n\n"+
			"No override must mean NO RULE. Emitting the brand's own default here\n"+
			"would make every cluster look overridden, and would defeat the point of\n"+
			"tokens.css being the single source.", css)
	}
}

func TestBrandCSSOverridesTheAccentRole(t *testing.T) {
	css := brandCSS(Settings{BrandPrimaryColor: "#0433ff"})
	for _, want := range []string{":root", "--memql-accent: #0433ff", "--memql-accent-deep:", "--memql-accent-fg:"} {
		if !strings.Contains(css, want) {
			t.Errorf("brand CSS is missing %q:\n%s", want, css)
		}
	}
	// The override targets the ROLE, not a parallel --brand-* name. A second
	// name would let the sign-in button and the links on the same page
	// disagree about what the brand colour is.
	if strings.Contains(css, "--brand-primary") {
		t.Errorf("brand CSS still emits a --brand-* name:\n%s", css)
	}
}

// A colour that does not parse is REFUSED, not sanitised. Anything else is a
// CSS injection: the value goes into a stylesheet verbatim.
func TestBrandCSSRefusesAnythingThatIsNotAHexColour(t *testing.T) {
	for _, bad := range []string{
		"red",
		"#fff",
		"#0433ff;} body{display:none",
		"var(--x)",
		"#gggggg",
		"  ",
		"#0433ff extra",
	} {
		css := brandCSS(Settings{BrandPrimaryColor: bad})
		if strings.Contains(css, ":root") {
			t.Errorf("brandCSS(%q) produced a rule; malformed colours must yield none:\n%s", bad, css)
		}
	}
}

// The fingerprint is what makes a brand change reach a browser holding a
// cached stylesheet. Two different brands must not share a cache key.
func TestBrandFingerprintChangesWithTheBrand(t *testing.T) {
	a := brandFingerprint(brandCSS(Settings{BrandPrimaryColor: "#0433ff"}))
	b := brandFingerprint(brandCSS(Settings{BrandPrimaryColor: "#b3362a"}))
	none := brandFingerprint(brandCSS(Settings{}))
	if a == b || a == none || b == none {
		t.Errorf("brand fingerprints collide: blue=%s red=%s unset=%s", a, b, none)
	}
	if got := brandFingerprint(brandCSS(Settings{BrandPrimaryColor: "#0433ff"})); got != a {
		t.Errorf("fingerprint is not stable for the same brand: %s then %s", a, got)
	}
}

// Text on the accent is computed, not taken from the palette, so an operator
// who picks a pale colour does not get white-on-pale.
func TestReadableOnPicksAContrastingForeground(t *testing.T) {
	cases := map[string]string{
		"#0433ff": "#ffffff", // deep blue -> light text
		"#07090a": "#ffffff", // near-black -> light text
		"#f5e6a8": "#07090a", // pale yellow -> dark text
		"#ffffff": "#07090a", // white -> dark text
	}
	for colour, want := range cases {
		if got := readableOn(colour); got != want {
			t.Errorf("readableOn(%s) = %s, want %s", colour, got, want)
		}
	}
}

// The directive this endpoint exists to buy back. If an inline style attribute
// ever comes back, this is what says so.
func TestCSPDoesNotAllowInlineStyles(t *testing.T) {
	if strings.Contains(cspBase, "'unsafe-inline'") {
		t.Errorf("the CSP allows inline styles again:\n%s\n\n"+
			"That concession existed only for the brand colour, and memql#4269 replaced\n"+
			"it with /static/brand.css. Serve values as CSS rather than re-weakening the\n"+
			"policy for every page.", cspBase)
	}
	if !strings.Contains(cspBase, "style-src 'self'") {
		t.Errorf("the CSP no longer carries style-src 'self':\n%s", cspBase)
	}
}
