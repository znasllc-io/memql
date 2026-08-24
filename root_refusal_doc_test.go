package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// root_refusal_doc_test.go -- every release-cut refusal code is documented
// (epic memql#4434).
//
// The runbook's "What each refusal means" table is a TABLE: it reads as
// complete, so an operator who does not find their code there concludes they
// are looking at something else. Two codes were already missing from it within
// an hour of it being written, which is the argument for a gate rather than
// for care.
//
// It checks ONE direction on purpose -- every code in the Go source appears in
// the runbook. The converse (every documented code still exists) is not
// checked, because a retired code whose row survives is a stale sentence, and a
// NEW code with no row is an operator holding an error nothing explains. Only
// the second is the failure this guards.
func TestEveryReleaseRefusalCodeIsDocumented(t *testing.T) {
	src, err := os.ReadFile("integrations/release/refusals.go")
	if err != nil {
		t.Fatalf("read refusals.go: %v. If the package moved, update this path -- do not "+
			"delete the test: the drift it catches has no other detector.", err)
	}
	doc, err := os.ReadFile("docs/public/operate/release-cutting.md")
	if err != nil {
		t.Fatalf("read the runbook: %v", err)
	}

	codes := regexp.MustCompile(`Code[A-Za-z]+\s*=\s*"([a-z_]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(codes) == 0 {
		t.Fatal("found no refusal codes at all, so this test would pass vacuously -- the const " +
			"block's shape must have changed")
	}
	for _, m := range codes {
		code := m[1]
		// Backticked, which is how the table spells them. A bare mention
		// in prose is not a row.
		if !strings.Contains(string(doc), "`"+code+"`") {
			t.Errorf("the refusal code %q is not in docs/public/operate/release-cutting.md. "+
				"An operator who hits it finds a table that looks complete and does not "+
				"explain their error, so they conclude they are reading the wrong page. "+
				"Add a row saying what happened and what to do -- or, if it is unreachable "+
				"from the portal, say so, which is what the not_owner and invalid_bump rows do.",
				code)
		}
	}
	t.Logf("checked %d refusal codes against the runbook", len(codes))
}
