package memql

import "testing"

// TestCatalogKey_SameModuloName: two specs with the same predicate but
// different names + whitespace + comments hash identically.
func TestCatalogKey_SameModuloName(t *testing.T) {
	a, err := CatalogKey("spec", `// the admin check
spec actorEnvelope foo {
  return role == "admin"
}`)
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := CatalogKey("spec", `spec actorEnvelope bar { return role=="admin" }`)
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if a != b {
		t.Errorf("expected same catalog key modulo name/whitespace/comments:\n a=%s\n b=%s", a, b)
	}
}

// TestCatalogKey_DifferentBody: different predicate -> different key.
func TestCatalogKey_DifferentBody(t *testing.T) {
	a, _ := CatalogKey("spec", `spec actorEnvelope foo { return role == "admin" }`)
	b, _ := CatalogKey("spec", `spec actorEnvelope foo { return role == "owner" }`)
	if a == b {
		t.Errorf("expected different keys for different bodies, both = %s", a)
	}
}

// TestCatalogKey_KindMatters: same body text under different kinds -> different
// key (the kind is part of the signature).
func TestCatalogKey_KindMatters(t *testing.T) {
	a, _ := CatalogKey("spec", `spec actorEnvelope foo { return role == "admin" }`)
	b, _ := CatalogKey("trait", `trait foo { return active == true }`)
	if a == b {
		t.Errorf("spec and trait with the same body must differ, both = %s", a)
	}
}

// TestCatalogKey_Shape: shape keys are name-independent too.
func TestCatalogKey_Shape(t *testing.T) {
	a, err := CatalogKey("shape", "@actor\nshape envA {\n  actor.userId\n  actor.role\n}")
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := CatalogKey("shape", "@actor\nshape envB { actor.userId actor.role }")
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if a != b {
		t.Errorf("shape keys should match modulo name/whitespace:\n a=%s\n b=%s", a, b)
	}
}

// TestCatalogKey_BadSourceErrors: unparseable source returns an error.
func TestCatalogKey_BadSourceErrors(t *testing.T) {
	if _, err := CatalogKey("spec", `spec actorEnvelope broken { return role == }`); err == nil {
		t.Errorf("expected an error for unparseable spec source")
	}
	if _, err := CatalogKey("automation", `automation a { }`); err == nil {
		t.Errorf("expected an unsupported-kind error for automation")
	}
}

// TestFindCatalogMatch: exact-key match reused; non-match returns nil; kind is
// part of the match.
func TestFindCatalogMatch(t *testing.T) {
	existingKey, _ := CatalogKey("spec", `spec actorEnvelope isAdmin { return role == "admin" }`)
	catalog := []CatalogEntry{
		{Name: "isAdmin", Kind: "spec", CatalogKey: existingKey},
		{Name: "other", Kind: "spec", CatalogKey: "spec:deadbeef"},
	}

	// Same predicate, different name -> reuse the cataloged "isAdmin".
	m, err := FindCatalogMatch("spec", `spec actorEnvelope needAdminCheck { return role=="admin" }`, catalog)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if m == nil || m.Name != "isAdmin" {
		t.Errorf("expected reuse of cataloged isAdmin, got %+v", m)
	}

	// Different predicate -> no match.
	m2, err := FindCatalogMatch("spec", `spec actorEnvelope needOwner { return role=="owner" }`, catalog)
	if err != nil {
		t.Fatalf("match2: %v", err)
	}
	if m2 != nil {
		t.Errorf("expected no match for a different predicate, got %+v", m2)
	}

	// Same body but a different kind -> no match (kind is part of identity).
	m3, err := FindCatalogMatch("trait", `trait needAdminCheck { return role=="admin" }`, catalog)
	if err != nil {
		t.Fatalf("match3: %v", err)
	}
	if m3 != nil {
		t.Errorf("expected no cross-kind match, got %+v", m3)
	}
}
