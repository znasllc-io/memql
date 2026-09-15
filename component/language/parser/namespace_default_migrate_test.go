package parser

import (
	"strings"
	"testing"
)

// #2614 codemod fixtures: directory-equal @namespace strips; colon-scoped
// and divergent (pinned) values stay; idempotent.
func TestRewriteRedundantNamespace(t *testing.T) {
	strip := func(t *testing.T, domain, src string) string {
		t.Helper()
		out, err := RewriteRedundantNamespace(domain, []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}

	t.Run("directory-equal-strips", func(t *testing.T) {
		src := "@version(\"1.0.0\")\n@namespace(\"cognition\")\nconcept space {\n}\n"
		got := strip(t, "cognition", src)
		if strings.Contains(got, "@namespace") {
			t.Errorf("directory-equal @namespace must strip:\n%s", got)
		}
		if !strings.Contains(got, "@version") {
			t.Errorf("neighboring annotations stay:\n%s", got)
		}
	})
	// WHAT "STAYS" MEANS CHANGED. These two cases asserted that a
	// colon-scoped sub-namespace and a pinned divergence were LOAD-BEARING,
	// so the #2614 strip had to leave them alone. Epic memql#5375 retired the
	// annotation, so an annotation the strip leaves standing is no longer
	// load-bearing -- it is a REFUSAL waiting at load, and the migrator's
	// job is to say so rather than to strip it.
	//
	// The strip itself is unchanged and still removes only the case it can
	// prove redundant, which is why these fixtures still exercise it: what
	// they now pin is that it does NOT quietly remove an annotation whose
	// namespace it cannot reproduce. `--rewrite=attributes-namespace` is
	// what turns each survivor into a named refusal, tested in
	// cmd/memqlmigrate.
	t.Run("colon-scoped-is-not-stripped", func(t *testing.T) {
		src := "@namespace(\"cognition:client:tool\")\nconcept tool {\n}\n"
		if got := strip(t, "cognition", src); !strings.Contains(got, "@namespace(\"cognition:client:tool\")") {
			t.Errorf("a colon-scoped namespace the strip cannot reproduce must be left for a human:\n%s", got)
		}
	})
	t.Run("divergent-is-not-stripped", func(t *testing.T) {
		src := "@namespace(\"cluster\")\nconcept deployment {\n}\n"
		if got := strip(t, "deployment", src); !strings.Contains(got, "@namespace(\"cluster\")") {
			t.Errorf("a divergence the strip cannot reproduce must be left for a human:\n%s", got)
		}
	})
	t.Run("idempotent", func(t *testing.T) {
		src := "@namespace(\"cognition\")\nconcept space {\n}\n"
		once := strip(t, "cognition", src)
		if twice := strip(t, "cognition", once); twice != once {
			t.Errorf("must converge")
		}
	})
}
