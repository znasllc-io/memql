package actiontrust

import (
	"math"
	"testing"
)

func TestGate_SurfaceAware(t *testing.T) {
	cases := []struct {
		se, surface string
		want        GateDecision
	}{
		{"read", "computer-use", AutoPromote}, // reads reversible everywhere
		{"read", "workbench", AutoPromote},
		{"write", "workbench", AutoPromote},  // sandbox write auto-promotes
		{"exec", "workbench", AutoPromote},   // sandbox exec auto-promotes
		{"write", "", AutoPromote},           // unset defaults to sandbox
		{"write", "computer-use", SuggestConfirm}, // real machine always confirms
		{"exec", "computer-use", SuggestConfirm},
		{"write", "mcp", SuggestConfirm}, // external side effects confirm
	}
	for _, c := range cases {
		if got := Gate(c.se, c.surface); got != c.want {
			t.Errorf("Gate(%q,%q): want %s, got %s", c.se, c.surface, c.want, got)
		}
	}
}

func TestInitialStatus(t *testing.T) {
	if InitialStatus(AutoPromote) != "active" {
		t.Fatalf("auto-promote must mint active")
	}
	if InitialStatus(SuggestConfirm) != "candidate" {
		t.Fatalf("confirm-gated must mint candidate")
	}
}

func TestClassifySideEffect(t *testing.T) {
	reads := []string{"fs.readFile", "fs.list", "fs.stat", "http.get", "db.query", "search.find"}
	for _, c := range reads {
		if ClassifySideEffect(c) != "read" {
			t.Errorf("%q should classify read", c)
		}
	}
	writes := []string{"fs.writeFile", "fs.mkdir", "shell.exec", "fs.copy"}
	for _, c := range writes {
		if ClassifySideEffect(c) != "write" {
			t.Errorf("%q should classify write", c)
		}
	}
}

func TestDecay_HalfLife(t *testing.T) {
	// After exactly one half-life, reliability halves.
	got := Decay(0.8, 3600, 3600)
	if math.Abs(got-0.4) > 1e-9 {
		t.Fatalf("one half-life: want 0.4, got %v", got)
	}
	// No age / no half-life leaves it unchanged.
	if Decay(0.8, 0, 3600) != 0.8 {
		t.Fatalf("zero age must not decay")
	}
	if Decay(0.8, 3600, 0) != 0.8 {
		t.Fatalf("zero half-life must not decay")
	}
	// Two half-lives -> quarter.
	if g := Decay(0.8, 7200, 3600); math.Abs(g-0.2) > 1e-9 {
		t.Fatalf("two half-lives: want 0.2, got %v", g)
	}
}

func TestShouldDeprecate(t *testing.T) {
	if !ShouldDeprecate(0.1, DefaultDeprecateFloor) {
		t.Fatalf("0.1 below floor must deprecate")
	}
	if ShouldDeprecate(0.5, DefaultDeprecateFloor) {
		t.Fatalf("0.5 above floor must not deprecate")
	}
}

func TestReadyToSuggest(t *testing.T) {
	if !ReadyToSuggest(3, 0.8, DefaultSuggestRuns, DefaultSuggestRel) {
		t.Fatalf("3 runs + 0.8 reliability should be ready")
	}
	if ReadyToSuggest(2, 0.9, DefaultSuggestRuns, DefaultSuggestRel) {
		t.Fatalf("too few runs must not be ready")
	}
	if ReadyToSuggest(5, 0.5, DefaultSuggestRuns, DefaultSuggestRel) {
		t.Fatalf("too-low reliability must not be ready")
	}
}
