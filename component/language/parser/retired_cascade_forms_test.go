package parser

import (
	"github.com/znasllc-io/memql/component/language/annotations"
	"strings"
	"testing"
)

// TestRetiredCascadeFormsRefuse is one negative cell per form D17 removes
// from the parse cascade (issue #5376's acceptance criterion).
//
// `,` AS OR IS DEFERRED, not retired, so it has no cell here: it is still
// live grammar (two filters folding into one traversal argument) and its
// codemod is `--rewrite=expressions`, which epic memql#5363 owns. See
// parseLogicalOr.
//
// Two of the three ALREADY failed before memql#5375, and that is the point of
// testing all four together: `?.` failed as "unexpected token" and
// `import (...)` as "expected import path string", neither of which names the
// form or the fix -- and the second actively misleads, reading as though the
// block were valid with different contents. A refusal that does not say what
// to write instead is a retirement an author cannot act on.
func TestRetiredCascadeFormsRefuse(t *testing.T) {
	for _, tc := range []struct {
		name, source, wants string
	}{
		{
			name:   "semicolon-as-AND",
			source: "active==true ; deleted==false",
			wants:  "`&&`",
		},
		{
			name:   "optional-chain",
			source: "owner?.id == actor.userId",
			wants:  "`??`",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseExpression(tc.source)
			if err == nil {
				t.Fatalf("%s should be refused", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("the refusal should name the surviving form %s.\n  got: %v", tc.wants, err)
			}
			if !strings.Contains(err.Error(), annotations.AttributeRewriteHint) {
				t.Errorf("the refusal should name the rewrite.\n  got: %v", err)
			}
		})
	}

	t.Run("import-block", func(t *testing.T) {
		_, err := ParseFile("import (\n  common.concepts.{ node }\n)\n")
		if err == nil {
			t.Fatal("`import (...)` should be refused")
		}
		if !strings.Contains(err.Error(), "use <domain>") {
			t.Errorf("the refusal should name the `use` form.\n  got: %v", err)
		}
		if !strings.Contains(err.Error(), annotations.AttributeRewriteHint) {
			t.Errorf("the refusal should name the rewrite.\n  got: %v", err)
		}
	})
}

// TestCommaStaysASeparator is the over-rejection guard, and the one this
// change could plausibly break. Retiring `,` as a boolean OR must not touch
// the comma in an argument list, a sort clause, a field list or an @enum --
// suppressCommaOr is what tells the two apart, and refusing outside that
// guard would reject most of the tree.
func TestCommaStaysASeparator(t *testing.T) {
	for _, src := range []string{
		`coalesce(args.a, args.b)`,
		`status in ["a", "b"]`,
	} {
		if _, err := ParseExpression(src); err != nil {
			t.Errorf("a comma separator must still parse: %q\n  got: %v", src, err)
		}
	}

	// The whole-construct path: a sort clause and an @enum both carry commas
	// through the rewriter, so a query exercises the separator in the
	// positions an author actually writes it.
	src := "@description(\"probe\")\nquery account q {\n  args {\n    kind  string  @enum(\"a\", \"b\")\n  }\n  filter  active==true && kind==args.kind\n  shape   accountFull\n  sort    \"name\", \"asc\"\n}\n"
	if _, err := NormaliseAll(src); err != nil {
		t.Errorf("a query with an @enum list and a sort clause must still lower: %v", err)
	}
}

// TestTrailingSemicolonIsStillTolerated guards the other legitimate `;`. The
// expression entry point permits trailing semicolons before EOF, which is a
// different position from the connective and must stay -- refusing it would
// reject the internal procedural form the rewriter emits.
func TestTrailingSemicolonIsStillTolerated(t *testing.T) {
	if _, err := ParseExpression("active==true;"); err != nil {
		t.Errorf("a trailing semicolon must still be tolerated: %v", err)
	}
}

// TestLoweredQueryUsesAndAnd pins the rewriter's own glue. It was the last
// producer of `;`-as-AND, and it is machine-generated, so a revert here would
// take the whole tree down at load while every .memql file still looked
// correct -- the failure would read as a parser defect rather than as this
// line.
func TestLoweredQueryUsesAndAnd(t *testing.T) {
	src := "@description(\"probe\")\nquery account q {\n  filter  active==true\n  shape   accountFull\n}\n"
	out, err := NormaliseAll(src)
	if err != nil {
		t.Fatalf("NormaliseAll: %v", err)
	}
	if strings.Contains(out, ";active==true") {
		t.Errorf("the lowering still glues the concept term with `;`, which the parser now refuses:\n%s", out)
	}
	if !strings.Contains(out, "&&active==true") {
		t.Errorf("the lowering should glue the concept term with `&&`:\n%s", out)
	}
}
