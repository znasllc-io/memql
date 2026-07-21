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
		src := "@namespace(\"cognition\")\n@version(\"1.0.0\")\nconcept space {\n}\n"
		got := strip(t, "cognition", src)
		if strings.Contains(got, "@namespace") {
			t.Errorf("directory-equal @namespace must strip:\n%s", got)
		}
		if !strings.Contains(got, "@version") {
			t.Errorf("neighboring annotations stay:\n%s", got)
		}
	})
	t.Run("colon-scoped-stays", func(t *testing.T) {
		src := "@namespace(\"cognition:client:tool\")\nconcept tool {\n}\n"
		if got := strip(t, "cognition", src); !strings.Contains(got, "@namespace(\"cognition:client:tool\")") {
			t.Errorf("colon-scoped sub-namespace is load-bearing:\n%s", got)
		}
	})
	t.Run("divergent-stays", func(t *testing.T) {
		src := "@namespace(\"cluster\")\nconcept deployment {\n}\n"
		if got := strip(t, "deployment", src); !strings.Contains(got, "@namespace(\"cluster\")") {
			t.Errorf("pinned divergence is load-bearing:\n%s", got)
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
