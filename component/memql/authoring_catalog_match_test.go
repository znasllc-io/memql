package memql

import (
	"strings"
	"testing"
)

// TestCatalogMatchText_NameIndependent: same kind + description + body but
// different names produce the same match text.
func TestCatalogMatchText_NameIndependent(t *testing.T) {
	a, err := CatalogMatchText("spec", `@description("Caller is admin")
spec actorEnvelope isAdmin { return role == "admin" }`)
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := CatalogMatchText("spec", `@description("Caller is admin")
spec actorEnvelope adminCheck { return role=="admin" }`)
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if a != b {
		t.Errorf("match text should be name-independent:\n a=%q\n b=%q", a, b)
	}
	if !strings.Contains(a, "kind:spec") || !strings.Contains(a, "intent:Caller is admin") || !strings.Contains(a, "form:") {
		t.Errorf("match text missing expected parts: %q", a)
	}
}

// TestCatalogMatchText_IntentMatters: same body, different description -> the
// text differs (intent is part of the semantic signal).
func TestCatalogMatchText_IntentMatters(t *testing.T) {
	a, _ := CatalogMatchText("spec", `@description("admin only")
spec actorEnvelope s { return role == "admin" }`)
	b, _ := CatalogMatchText("spec", `@description("owner only")
spec actorEnvelope s { return role == "admin" }`)
	if a == b {
		t.Errorf("different descriptions should yield different match text, both = %q", a)
	}
}

// TestCatalogMatchText_NoDescription: works (form only) when there is no
// @description.
func TestCatalogMatchText_NoDescription(t *testing.T) {
	got, err := CatalogMatchText("spec", `spec actorEnvelope s { return role == "admin" }`)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Contains(got, "intent:") {
		t.Errorf("no description -> no intent part, got %q", got)
	}
	if !strings.Contains(got, "form:") {
		t.Errorf("expected a form part, got %q", got)
	}
}

// TestCatalogMatchText_BadSource: unparseable source errors.
func TestCatalogMatchText_BadSource(t *testing.T) {
	if _, err := CatalogMatchText("spec", `spec actorEnvelope broken { return role == }`); err == nil {
		t.Errorf("expected an error for unparseable source")
	}
}

// TestDecideReuse_ExactMatch: a same-behavior cataloged construct (identical
// CatalogKey -- same body + docs, different name) short-circuits to direct
// reuse; MatchText stays empty. CatalogKey is description-sensitive, so this
// uses matching descriptions (here: none); a same-predicate construct with a
// DIFFERENT description is intentionally a fuzzy-tier match, not exact.
func TestDecideReuse_ExactMatch(t *testing.T) {
	key, _ := CatalogKey("spec", `spec actorEnvelope isAdmin { return role == "admin" }`)
	catalog := []CatalogEntry{{Name: "isAdmin", Kind: "spec", CatalogKey: key}}

	d, err := DecideReuse("spec", `spec actorEnvelope wantAdmin { return role=="admin" }`, catalog)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if d.ExactMatch == nil || d.ExactMatch.Name != "isAdmin" {
		t.Fatalf("expected exact reuse of isAdmin, got %+v", d)
	}
	if d.MatchText != "" {
		t.Errorf("exact match should not produce MatchText, got %q", d.MatchText)
	}
}

// TestDecideReuse_DifferentDescriptionIsFuzzy: same predicate but a different
// @description is NOT an exact match -> it returns MatchText for the fuzzy
// similarTo tier (which will recognize the high semantic similarity).
func TestDecideReuse_DifferentDescriptionIsFuzzy(t *testing.T) {
	key, _ := CatalogKey("spec", `@description("is administrator")
spec actorEnvelope isAdmin { return role == "admin" }`)
	catalog := []CatalogEntry{{Name: "isAdmin", Kind: "spec", CatalogKey: key}}

	d, err := DecideReuse("spec", `@description("admin gate")
spec actorEnvelope wantAdmin { return role=="admin" }`, catalog)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if d.ExactMatch != nil {
		t.Errorf("different description should not be an exact match, got %+v", d.ExactMatch)
	}
	if !strings.Contains(d.MatchText, "form:") {
		t.Errorf("expected MatchText for the fuzzy tier, got %q", d.MatchText)
	}
}

// TestDecideReuse_Gap: no exact match -> MatchText for the similarTo layer,
// ExactMatch nil.
func TestDecideReuse_Gap(t *testing.T) {
	key, _ := CatalogKey("spec", `spec actorEnvelope isAdmin { return role == "admin" }`)
	catalog := []CatalogEntry{{Name: "isAdmin", Kind: "spec", CatalogKey: key}}

	d, err := DecideReuse("spec", `@description("owner gate")
spec actorEnvelope wantOwner { return role=="owner" }`, catalog)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if d.ExactMatch != nil {
		t.Errorf("expected no exact match, got %+v", d.ExactMatch)
	}
	if !strings.Contains(d.MatchText, "intent:owner gate") {
		t.Errorf("expected MatchText with the intent, got %q", d.MatchText)
	}
}
