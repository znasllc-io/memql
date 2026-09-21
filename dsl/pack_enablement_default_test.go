package dsl

import "testing"

// A pack that declares nothing must behave exactly as it did before
// declared defaults existed, which is what keeps this change additive.
func TestAnUndeclaredPackDefaultsToEnabled(t *testing.T) {
	t.Cleanup(ResetPackDefaultsForTest)
	if !PackDefaultEnabled("nobody-declared-me") {
		t.Fatal("an undeclared pack must default to enabled")
	}
	if len(PackDefaults()) != 0 {
		t.Fatalf("PackDefaults() = %v, want empty when nothing is declared", PackDefaults())
	}
}

func TestADeclaredDefaultIsReported(t *testing.T) {
	t.Cleanup(ResetPackDefaultsForTest)
	RegisterPackDefault("reviews", false)
	if PackDefaultEnabled("reviews") {
		t.Fatal("a pack declaring defaultEnabled=false must report disabled")
	}
	if got, ok := PackDefaults()["reviews"]; !ok || got {
		t.Fatalf("PackDefaults()[reviews] = (%v, %v), want (false, true)", got, ok)
	}
}

// A declared TRUE is a declaration too: it must appear in PackDefaults()
// so the module inventory can tell "declared enabled" from "declared
// nothing", even though both load.
func TestADeclaredTrueIsStillADeclaration(t *testing.T) {
	t.Cleanup(ResetPackDefaultsForTest)
	RegisterPackDefault("harness", true)
	if !PackDefaultEnabled("harness") {
		t.Fatal("a pack declaring defaultEnabled=true must report enabled")
	}
	if _, ok := PackDefaults()["harness"]; !ok {
		t.Fatal("a declared true must still appear in PackDefaults()")
	}
}

func TestADeclaredDefaultIsTrimmedAndAnEmptyDomainIsIgnored(t *testing.T) {
	t.Cleanup(ResetPackDefaultsForTest)
	RegisterPackDefault("  reviews  ", false)
	RegisterPackDefault("   ", false)
	if PackDefaultEnabled("reviews") {
		t.Fatal("a padded domain must register under its trimmed name")
	}
	if len(PackDefaults()) != 1 {
		t.Fatalf("PackDefaults() = %v, want exactly the one real domain", PackDefaults())
	}
}
