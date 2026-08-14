package memql

// construct_catalog_key_test.go pins the three things the language server reads
// out of this package to compute training state (memql#3759):
// ConstructKindForKeyword, CatalogConstructKey and DocumentConstructKey.
//
// The corpus parity gate (construct_source_hash_parity_test.go) already runs all
// three over every construct in the tree, and it is the stronger test. What it
// cannot cover is the case where a construct exists on NEITHER side: `action`
// and `capability` are authored kinds this catalog deliberately does not carry,
// so the parity gate skips them by design and would never notice if
// ConstructKindForKeyword started claiming them.
//
// That claim is not a cosmetic one. A caller that reads "no kind" as "not in the
// catalog" reports every action in an open file as UNTRAINED against a cluster
// that was never asked about it -- the same false alarm as reading a
// disconnected cluster as untrained, arriving through a door the connection
// checks do not cover.

import "testing"

// TestConstructKindForKeywordTranslatesEveryCatalogedKind: the map is the
// engine's, so a kind added to constructKeyword must reach the language server
// without anyone editing the language server.
func TestConstructKindForKeywordTranslatesEveryCatalogedKind(t *testing.T) {
	for kind, keyword := range constructKeyword {
		got, ok := ConstructKindForKeyword(keyword)
		if !ok {
			t.Errorf("ConstructKindForKeyword(%q) reported the kind is not cataloged, but the "+
				"catalog reports it as %q: every construct of that kind would read as unknown", keyword, kind)
			continue
		}
		if got != kind {
			t.Errorf("ConstructKindForKeyword(%q) = %q; want %q", keyword, got, kind)
		}
	}
	// The one keyword whose two halves differ at all. Spelled out because it is
	// the whole reason the translation exists, and a loop over an inverted map
	// would pass just as happily if both halves were `mutate`.
	if got, _ := ConstructKindForKeyword("mutate"); got != ConstructKindMutation {
		t.Errorf("ConstructKindForKeyword(\"mutate\") = %q; want %q", got, ConstructKindMutation)
	}
}

// TestConstructKindForKeywordRefusesKindsTheCatalogDoesNotCarry: the false
// return is load-bearing, and this is the only test that covers it.
func TestConstructKindForKeywordRefusesKindsTheCatalogDoesNotCarry(t *testing.T) {
	for _, keyword := range []string{
		// Authored kinds loaded by component/actions, not by any registry this
		// catalog reads. See the file header.
		"action", "capability",
		// Not a declaration at all.
		"use",
		// Not a keyword.
		"queries", "",
	} {
		if kind, ok := ConstructKindForKeyword(keyword); ok {
			t.Errorf("ConstructKindForKeyword(%q) = (%q, true); want false -- a caller that "+
				"reads a cataloged kind here will report %q constructs as untrained against a "+
				"catalog that structurally cannot carry them", keyword, kind, keyword)
		}
	}
}

// TestConstructKeysAgreeAcrossTheRegistryFileBoundary: the two halves of the one
// matching rule must land on the same key for the same construct, or nothing the
// language server computes can be matched to anything the catalog reports.
//
// The concept case is the whole point. A registry names it by canonical id; a
// file names it by short name plus the directory it sits in. Those are different
// inputs and must produce one key.
func TestConstructKeysAgreeAcrossTheRegistryFileBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		// what a registry entry carries
		kind, registryName string
		// what a file declares
		declaredName, domain string
	}{
		{"concept", ConstructKindConcept, "v1:cognition:space", "space", "cognition"},
		{"nested-namespace concept", ConstructKindConcept, "v1:cognition:turn:state", "state", "cognition"},
		{"query", ConstructKindQuery, "spaceParticipants", "spaceParticipants", "cognition"},
		{"mutation", ConstructKindMutation, "joinSpaceAsHuman", "joinSpaceAsHuman", "cognition"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fromCatalog := CatalogConstructKey(tc.kind, tc.registryName)
			fromDocument := DocumentConstructKey(tc.kind, tc.declaredName, tc.domain)
			if fromCatalog != fromDocument {
				t.Errorf("CatalogConstructKey(%q, %q) = %q but DocumentConstructKey(%q, %q, %q) = %q; "+
					"the two halves of one rule must agree",
					tc.kind, tc.registryName, fromCatalog, tc.kind, tc.declaredName, tc.domain, fromDocument)
			}
		})
	}
}

// TestConstructKeysSeparateConceptsByDomain: two domains may each declare a
// `state`, which is exactly why a concept keys on its domain and every other
// kind does not.
func TestConstructKeysSeparateConceptsByDomain(t *testing.T) {
	cognition := CatalogConstructKey(ConstructKindConcept, "v1:cognition:state")
	knowledge := CatalogConstructKey(ConstructKindConcept, "v1:knowledge:state")
	if cognition == knowledge {
		t.Fatalf("v1:cognition:state and v1:knowledge:state share the key %q: one would be "+
			"reported with the other's training state", cognition)
	}
	if got := DocumentConstructKey(ConstructKindConcept, "state", "knowledge"); got != knowledge {
		t.Errorf("a `state` declared under knowledge/ keys as %q; want %q", got, knowledge)
	}

	// A non-concept kind lives in a flat, process-wide registry, so the domain
	// is not part of its identity -- and must not become part of it, or a
	// construct in a file the language server sees at one path would stop
	// matching the same construct in the catalog.
	if DocumentConstructKey(ConstructKindQuery, "someQuery", "cognition") !=
		DocumentConstructKey(ConstructKindQuery, "someQuery", "knowledge") {
		t.Error("a query's key varies with its domain; the registry it lives in does not")
	}
}
