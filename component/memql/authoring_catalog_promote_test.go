package memql

import (
	"strings"
	"testing"
)

// TestPlanCatalogPromotion_DerivesKeyAndText: the promotion plan carries the
// SAME CatalogKey + CatalogMatchText the deterministic + fuzzy tiers compute,
// so a promoted construct is findable by both an exact-key match and a
// similarTo near-match.
func TestPlanCatalogPromotion_DerivesKeyAndText(t *testing.T) {
	src := `@description("Caller is admin")
spec isAdmin { actor.role == "admin" }`

	wantKey, _ := CatalogKey("spec", src)
	wantText, _ := CatalogMatchText("spec", src)

	p, err := PlanCatalogPromotion("v1:authoring:construct:c1", "spec", "isAdmin", src, "v1:authoring:bundle:b1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p.CatalogKey != wantKey {
		t.Errorf("CatalogKey mismatch: got %q want %q", p.CatalogKey, wantKey)
	}
	if p.MatchText != wantText {
		t.Errorf("MatchText mismatch: got %q want %q", p.MatchText, wantText)
	}
	if p.ConstructID != "v1:authoring:construct:c1" || p.Kind != "spec" || p.Name != "isAdmin" {
		t.Errorf("identity not carried through: %+v", p)
	}
	if p.FromBundleID != "v1:authoring:bundle:b1" {
		t.Errorf("provenance bundle not carried through: %+v", p)
	}
}

// TestPlanCatalogPromotion_MatchesDeterministicReuse: a construct promoted with
// PlanCatalogPromotion produces a CatalogEntry whose key an identical
// (name-different) construct reuses exactly -- the promote -> reuse round-trip.
func TestPlanCatalogPromotion_MatchesDeterministicReuse(t *testing.T) {
	p, err := PlanCatalogPromotion("c1", "spec", "isAdmin", `spec isAdmin { actor.role == "admin" }`, "b1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	catalog := []CatalogEntry{{Name: p.Name, Kind: p.Kind, CatalogKey: p.CatalogKey}}

	// A same-behavior construct under a different name reuses the promoted one.
	d, err := DecideReuse("spec", `spec wantAdmin { actor.role=="admin" }`, catalog)
	if err != nil {
		t.Fatalf("DecideReuse: %v", err)
	}
	if d.ExactMatch == nil || d.ExactMatch.Name != "isAdmin" {
		t.Fatalf("promoted construct should be an exact reuse, got %+v", d)
	}
}

// TestPlanCatalogPromotion_RequiresIDAndBundle: promotion needs the construct id
// and the source bundle (provenance) -- both are required.
func TestPlanCatalogPromotion_RequiresIDAndBundle(t *testing.T) {
	if _, err := PlanCatalogPromotion("", "spec", "s", `spec s { actor.role == "admin" }`, "b1"); err == nil {
		t.Errorf("expected an error for empty constructId")
	}
	if _, err := PlanCatalogPromotion("c1", "spec", "s", `spec s { actor.role == "admin" }`, ""); err == nil {
		t.Errorf("expected an error for empty fromBundleId (provenance)")
	}
}

// TestPlanCatalogPromotion_BadSource: an unparseable construct cannot be
// promoted (you only promote constructs that compiled in the sandbox).
func TestPlanCatalogPromotion_BadSource(t *testing.T) {
	_, err := PlanCatalogPromotion("c1", "spec", "broken", `spec broken { actor.role == }`, "b1")
	if err == nil {
		t.Errorf("expected an error for unparseable source")
	}
}

// TestPlanCatalogPromotion_TextIsEmbeddable: the MatchText carries the semantic
// signal (kind + intent + form) so the embedding has something to bite on.
func TestPlanCatalogPromotion_TextIsEmbeddable(t *testing.T) {
	p, err := PlanCatalogPromotion("c1", "spec", "isAdmin", `@description("admin gate")
spec isAdmin { actor.role == "admin" }`, "b1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	for _, want := range []string{"kind:spec", "intent:admin gate", "form:"} {
		if !strings.Contains(p.MatchText, want) {
			t.Errorf("MatchText missing %q: %q", want, p.MatchText)
		}
	}
}
