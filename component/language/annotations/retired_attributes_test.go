package annotations

import (
	"strings"
	"testing"
)

// retired_attributes_test.go is epic memql#5375's acceptance gate. It moved
// here from core/baseparser when #5359 folded that package's hand-rolled
// ledger into the one registry check; the assertions are unchanged in
// substance, re-expressed against retiredEverywhere / retiredOn.

// d17Everywhere is the epic's retirement set that applies to every receiver.
var d17Everywhere = []string{
	"deprecated", "timeout", "retry", "idempotent", "audit",
	"latestMode", "enabled", "nocache", "schedule",
	"rateLimit", "scopes", "namespace",
	// No surviving home, so they are retired everywhere rather than on the
	// concept field alone -- otherwise @unique written anywhere else reads
	// as "unknown annotation" instead of "retired".
	"unique", "immutable",
}

// d17OnConceptField is the epic's retirement set scoped to a concept field.
// @default is here rather than above on purpose: a tool / prompt / builtin
// field body IS the JSON schema handed to the model, so `default` there is a
// value the model reads.
var d17OnConceptField = []string{"default"}

// TestEveryRetiredFormNamesTheRewrite is the D17 acceptance gate: "Every
// retired form refuses with the migrator's name." A hint that explains the
// retirement without naming the rewrite leaves the author to go and find the
// migrator, which is exactly the step the sentence exists to remove.
//
// Scoped to this epic's own set: the retirements that predate it are migrated
// by deleting the annotation, so naming a codemod would point an author at a
// command with nothing to do for them.
func TestEveryRetiredFormNamesTheRewrite(t *testing.T) {
	for _, name := range d17Everywhere {
		hint, ok := retiredEverywhere[name]
		if !ok {
			t.Errorf("@%s should be retired everywhere (D17) but is not in retiredEverywhere", name)
			continue
		}
		if !strings.Contains(hint, AttributeRewriteHint) {
			t.Errorf("@%s: hint does not name the rewrite.\n  got:  %s\n  want: a hint containing %q",
				name, hint, AttributeRewriteHint)
		}
	}
	for _, name := range d17OnConceptField {
		hint, ok := retiredOn[retiredKey{ConceptField, name}]
		if !ok {
			t.Errorf("concept-field @%s should be retired (D17) but is not in retiredOn", name)
			continue
		}
		if !strings.Contains(hint, AttributeRewriteHint) {
			t.Errorf("concept-field @%s: hint does not name the rewrite.\n  got:  %s\n  want: a hint containing %q",
				name, hint, AttributeRewriteHint)
		}
	}
}

// TestRetiredSetIsTheD17Set pins the tables against the spec's list so a
// retirement cannot be quietly dropped or quietly added.
//
// The three exceptions are asserted ABSENT by name. Each has a live reader,
// found when the spec's own section 10 re-verification was run, so retiring
// one is a behaviour regression rather than a cleanup -- and the regression is
// silent, which is why it needs a gate rather than a note.
func TestRetiredSetIsTheD17Set(t *testing.T) {
	for _, name := range d17Everywhere {
		if _, ok := retiredEverywhere[name]; !ok {
			t.Errorf("@%s should be retired (D17) but is not in retiredEverywhere", name)
		}
	}
	for _, name := range d17OnConceptField {
		if _, ok := retiredOn[retiredKey{ConceptField, name}]; !ok {
			t.Errorf("concept-field @%s should be retired (D17) but is not in retiredOn", name)
		}
	}
	for _, tc := range []struct{ name, reader string }{
		{"displayCard", "clients/os reads it (src/apps/concepts/displayCard.ts, RowsPanel.tsx)"},
		{"composable", "clients/os reads it (src/apps/materializer/useCompose.ts via integration.compose.composableConcepts)"},
		{"allowedRoles", "the agent-role gate enforces it (tool_types.go, grpc/server.go, tool_execution.go)"},
	} {
		for _, r := range Retirements() {
			if r.Name == tc.name {
				t.Errorf("@%s must NOT be retired -- %s.\n  tables wrongly say: %s", tc.name, tc.reader, r.Hint)
			}
		}
	}
}

// TestConceptVersionIsNotRetired guards the one name where the construct and
// field surfaces disagree. A function's @version was read by nothing; a
// concept's and a seed's @version is the "v1" of every canonical id it
// declares, so retiring the name outright would take concept ids down with it.
func TestConceptVersionIsNotRetired(t *testing.T) {
	if hint, ok := retiredEverywhere["version"]; ok {
		t.Errorf("@version must not be retired everywhere -- a concept's and a seed's @version are id-bearing.\n  got: %s", hint)
	}
	for _, r := range []Receiver{Concept, Seed} {
		if _, ok := Lookup(r, "version"); !ok {
			t.Errorf("@version must stay live on %s -- it is id-bearing there", r)
		}
	}
}
