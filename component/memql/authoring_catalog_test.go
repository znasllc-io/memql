package memql

import "testing"

// TestCatalogKey_SameModuloName: two specs with the same predicate but
// different names + whitespace + comments hash identically.
func TestCatalogKey_SameModuloName(t *testing.T) {
	a, err := CatalogKey("spec", `// the admin check
spec actorEnvelope foo = actor => actor.role == "admin"`)
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := CatalogKey("spec", `spec actorEnvelope bar = actor => actor.role == "admin"`)
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if a != b {
		t.Errorf("expected same catalog key modulo name/whitespace/comments:\n a=%s\n b=%s", a, b)
	}
}

// TestCatalogKey_DifferentBody: different predicate -> different key.
func TestCatalogKey_DifferentBody(t *testing.T) {
	a, _ := CatalogKey("spec", `spec actorEnvelope foo = actor => actor.role == "admin"`)
	b, _ := CatalogKey("spec", `spec actorEnvelope foo = actor => actor.role == "owner"`)
	if a == b {
		t.Errorf("expected different keys for different bodies, both = %s", a)
	}
}

// TestCatalogKey_KindMatters: same body text under different kinds -> different
// key (the kind is part of the signature).
func TestCatalogKey_KindMatters(t *testing.T) {
	a, _ := CatalogKey("spec", `spec actorEnvelope foo = actor => actor.role == "admin"`)
	b, _ := CatalogKey("trait", `trait foo = row => row.active == true`)
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
	existingKey, _ := CatalogKey("spec", `spec actorEnvelope isAdmin = actor => actor.role == "admin"`)
	catalog := []CatalogEntry{
		{Name: "isAdmin", Kind: "spec", CatalogKey: existingKey},
		{Name: "other", Kind: "spec", CatalogKey: "spec:deadbeef"},
	}

	// Same predicate, different name -> reuse the cataloged "isAdmin".
	m, err := FindCatalogMatch("spec", `spec actorEnvelope needAdminCheck = actor => actor.role == "admin"`, catalog)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if m == nil || m.Name != "isAdmin" {
		t.Errorf("expected reuse of cataloged isAdmin, got %+v", m)
	}

	// Different predicate -> no match.
	m2, err := FindCatalogMatch("spec", `spec actorEnvelope needOwner = actor => actor.role == "owner"`, catalog)
	if err != nil {
		t.Fatalf("match2: %v", err)
	}
	if m2 != nil {
		t.Errorf("expected no match for a different predicate, got %+v", m2)
	}

	// Same body but a different kind -> no match (kind is part of identity).
	m3, err := FindCatalogMatch("trait", `trait needAdminCheck = row => row.role == "admin"`, catalog)
	if err != nil {
		t.Fatalf("match3: %v", err)
	}
	if m3 != nil {
		t.Errorf("expected no cross-kind match, got %+v", m3)
	}
}

// TestCatalogKey_EditionTwentySix: a brace-less spec or trait (epic
// memql#5363) keys by its predicate, whatever it calls the row -- a legacy
// predicate named no parameter, so the parameter is a degree of freedom the
// dedup layer must not read as a difference -- and still keys differently when
// the predicate differs.
func TestCatalogKey_EditionTwentySix(t *testing.T) {
	key := func(kind, src string) string {
		t.Helper()
		k, err := CatalogKey(kind, src)
		if err != nil {
			t.Fatalf("CatalogKey(%q): %v", src, err)
		}
		return k
	}
	a := key("spec", `spec actorEnvelope foo = actor => actor.role == "admin"`)
	b := key("spec", "// the admin check\nspec actorEnvelope bar = a =>\n  a.role==\"admin\"")
	if a != b {
		t.Errorf("same predicate, different name, parameter and layout -> different keys:\n a=%s\n b=%s", a, b)
	}
	if c := key("spec", `spec actorEnvelope foo = actor => actor.role == "owner"`); c == a {
		t.Error("a different predicate keyed the same")
	}
	// A string that spells the parameter is data, not the parameter.
	d := key("trait", `trait t = row => row.kind == "row.kind"`)
	e := key("trait", `trait t = r => r.kind == "row.kind"`)
	f := key("trait", `trait t = r => r.kind == "r.kind"`)
	if d != e {
		t.Errorf("renaming the parameter changed the key:\n d=%s\n e=%s", d, e)
	}
	if d == f {
		t.Error("a string literal's contents were renamed with the parameter")
	}
}
