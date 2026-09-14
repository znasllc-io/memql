package baseparser

import (
	"strings"
	"testing"
)

// preAttributeEpochRetirements are the ledger entries that predate epic
// memql#5375. Each was retired with no codemod behind it -- deleting the
// annotation is the whole migration -- so naming a rewrite in their hints
// would point an author at a command with nothing to do for them.
var preAttributeEpochRetirements = map[string]bool{
	"internal":   true,
	"role":       true,
	"permission": true,
}

// TestEveryRetiredFormNamesTheRewrite is the D17 acceptance gate: "Every
// retired form refuses with the migrator's name." A hint that explains the
// retirement without naming the rewrite leaves the author to go and find
// the migrator, which is exactly the step the sentence exists to remove.
func TestEveryRetiredFormNamesTheRewrite(t *testing.T) {
	for name, hint := range retiredConstructAnnotations {
		if preAttributeEpochRetirements[name] {
			continue
		}
		if !strings.Contains(hint, AttributeRewriteHint) {
			t.Errorf("@%s: hint does not name the rewrite.\n  got:  %s\n  want: a hint containing %q",
				name, hint, AttributeRewriteHint)
		}
	}
	for name, hint := range retiredFieldAnnotations {
		if !strings.Contains(hint, AttributeRewriteHint) {
			t.Errorf("field @%s: hint does not name the rewrite.\n  got:  %s\n  want: a hint containing %q",
				name, hint, AttributeRewriteHint)
		}
	}
}

// TestRetiredSetIsTheD17Set pins the ledger against the spec's list so a
// retirement cannot be quietly dropped or quietly added.
//
// The three exceptions are asserted ABSENT by name. Each has a live reader
// found when the spec's own section 10 re-verification was run, so
// retiring one is a behaviour regression rather than a cleanup -- and the
// regression is silent, which is why it needs a gate rather than a note.
func TestRetiredSetIsTheD17Set(t *testing.T) {
	for _, n := range []string{
		"deprecated", "timeout", "retry", "idempotent", "audit",
		"latestMode", "enabled", "nocache", "schedule",
		"rateLimit", "scopes", "namespace",
	} {
		if _, ok := RetiredConstructAnnotation(n); !ok {
			t.Errorf("@%s should be retired (D17) but is not in the construct ledger", n)
		}
	}
	for _, n := range []string{"unique", "immutable"} {
		if _, ok := RetiredFieldAnnotation(n); !ok {
			t.Errorf("field @%s should be retired (D17) but is not in the field ledger", n)
		}
	}
	for _, tc := range []struct{ name, reader string }{
		{"displayCard", "clients/os reads it (src/apps/concepts/displayCard.ts)"},
		{"composable", "clients/os reads it (src/apps/materializer/useCompose.ts)"},
		{"allowedRoles", "the agent-role gate enforces it (tool_types.go, grpc/server.go, tool_execution.go)"},
	} {
		if hint, ok := RetiredConstructAnnotation(tc.name); ok {
			t.Errorf("@%s must NOT be retired -- %s.\n  ledger wrongly says: %s", tc.name, tc.reader, hint)
		}
		if hint, ok := RetiredFieldAnnotation(tc.name); ok {
			t.Errorf("field @%s must NOT be retired -- %s.\n  ledger wrongly says: %s", tc.name, tc.reader, hint)
		}
	}
}

// TestConceptVersionIsNotRetired guards the one name where the construct
// and field surfaces disagree. A function's @version was read by nothing;
// a concept's @version is the "v1" of every canonical id it declares, so a
// single shared ledger would have taken concept ids down with it.
func TestConceptVersionIsNotRetired(t *testing.T) {
	if hint, ok := RetiredConstructAnnotation("version"); ok {
		t.Errorf("@version must not be in the construct ledger -- a concept's and a seed's @version are id-bearing, and the ledger is consulted for every construct kind.\n  got: %s", hint)
	}
}
