package annotations

import (
	"strings"
	"testing"
)

// retiredNames is the memql#5375 set, restated here rather than imported:
// this package is a LEAF that imports nothing inside the repo (see the
// package doc), and importing core/baseparser for its ledger would end
// that. The two lists agreeing is guarded from the other side by
// TestMatrixListsNoRetiredAttribute, which reads both.
var retiredNames = map[string]bool{
	"enabled": true, "latestMode": true, "nocache": true,
	"schedule": true, "rateLimit": true, "scopes": true,
	"namespace": true, "deprecated": true, "timeout": true,
	"retry": true, "idempotent": true, "audit": true,
	"unique": true, "immutable": true,
}

// TestNoReceiverOffersARetiredName is the other half of the ledger's gate.
// A name the loader refuses and the registry still OFFERS puts the
// editor's completion list at war with the load gate, so an author would
// be completed straight into a refusal -- which reads as the editor being
// right and the engine being broken.
func TestNoReceiverOffersARetiredName(t *testing.T) {
	for receiver, names := range ByReceiver {
		for _, n := range names {
			if retiredNames[n] {
				label := receiver
				if label == "" {
					label = "<concept>"
				}
				t.Errorf("receiver %s still offers retired @%s", label, n)
			}
		}
	}
}

// TestNoDocsEntryForARetiredName keeps the hover surface in step. A doc
// entry with no receiver offering it is dead weight the editor still
// renders on hover over an author's refused annotation.
func TestNoDocsEntryForARetiredName(t *testing.T) {
	for name := range Docs {
		if retiredNames[name] {
			t.Errorf("Docs still carries retired @%s", name)
		}
	}
}

// TestProviderVendorReplacedType pins the rename. @type survives on a
// concept, where it is the row kind, and must not be collateral damage --
// the two annotations shared a spelling and nothing else.
func TestProviderVendorReplacedType(t *testing.T) {
	has := func(receiver, name string) bool {
		for _, a := range ByReceiver[receiver] {
			if a == name {
				return true
			}
		}
		return false
	}
	if !has("Provider", "vendor") {
		t.Error("Provider should accept @vendor")
	}
	if has("Provider", "type") {
		t.Error("Provider should no longer accept @type -- @vendor is the one spelling")
	}
	if !has("", "type") {
		t.Error("a concept's @type (the row kind) is a different annotation and must survive")
	}
	if _, ok := Docs["vendor"]; !ok {
		t.Error("@vendor has no Docs entry")
	}
}

// TestKeptAnnotationsNameTheirReader is issue #5378's deliverable turned
// into a gate. These three survive BECAUSE something reads them; a doc
// that does not say WHERE means the next audit has to re-run the grep to
// find out whether it is still true, which is how @rateLimit and @scopes
// survived as long as they did.
func TestKeptAnnotationsNameTheirReader(t *testing.T) {
	for _, tc := range []struct{ name, reader string }{
		{"displayCard", "clients/os"},
		{"composable", "clients/os"},
		{"allowedRoles", "tool_types.go"},
	} {
		doc, ok := Docs[tc.name]
		if !ok {
			t.Errorf("@%s has no doc entry", tc.name)
			continue
		}
		if !strings.Contains(doc, tc.reader) {
			t.Errorf("@%s doc must name its reader %q, or the next audit cannot tell it is still read.\n  got: %s",
				tc.name, tc.reader, doc)
		}
	}
}

// TestEveryOfferedNameHasADoc keeps the pair complete after the deletions.
// The same invariant is asserted from the sense side; asserting it here
// too means a receiver edit in this file fails in this file.
func TestEveryOfferedNameHasADoc(t *testing.T) {
	for receiver, names := range ByReceiver {
		for _, n := range names {
			if _, ok := Docs[n]; !ok {
				label := receiver
				if label == "" {
					label = "<concept>"
				}
				t.Errorf("receiver %s offers @%s with no Docs entry", label, n)
			}
		}
	}
}

// TestKeywordArgsNameAKnownAnnotation guards the direction the deletions
// could break: dropping a receiver entry while leaving its argument list
// behind would offer arguments for an annotation nothing accepts.
func TestKeywordArgsNameAKnownAnnotation(t *testing.T) {
	known := map[string]bool{}
	for _, names := range ByReceiver {
		for _, n := range names {
			known[n] = true
		}
	}
	for name := range KeywordArgs {
		if !known[name] {
			t.Errorf("KeywordArgs carries @%s, which no receiver offers", name)
		}
	}
}
