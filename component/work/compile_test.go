package work

import "testing"

func TestGoalSignature_NormalisesStatementAndInputShape(t *testing.T) {
	a := GoalSignature("  Summarise   Yesterday's TICKETS! ", []string{"day", "team"})
	b := GoalSignature("summarise yesterday's tickets", []string{"team", "day"})
	if a != b {
		t.Fatalf("signature must be insensitive to case, punctuation, spacing and arg ORDER:\n a=%s\n b=%s", a, b)
	}
	if GoalSignature("summarise tickets", []string{"day"}) == a {
		t.Fatal("a different input shape is a different signature: the same words with different arguments is a different template")
	}
	if GoalSignature("", nil) == "" {
		t.Fatal("a signature is always computable; an empty goal still hashes")
	}
}

func TestGoalSignature_IsStableAcrossCalls(t *testing.T) {
	for i := 0; i < 8; i++ {
		if GoalSignature("do the thing", []string{"b", "a", "c"}) != GoalSignature("do the thing", []string{"c", "b", "a"}) {
			t.Fatal("signature is not stable; a catalog keyed on it would miss its own entries")
		}
	}
}
