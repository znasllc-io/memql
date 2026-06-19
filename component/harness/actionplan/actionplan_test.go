package actionplan

import "testing"

func TestDecide_Thresholds(t *testing.T) {
	th := DefaultThresholds()
	cases := []struct {
		name string
		fit  Fit
		want Decision
	}{
		{"no hit", Fit{}, Synthesize},
		{"high fit + bindable -> reuse", Fit{ActionID: "a", Score: 0.9, CanBind: true}, Reuse},
		{"high fit but cannot bind -> adapt", Fit{ActionID: "a", Score: 0.9, CanBind: false}, Adapt},
		{"mid fit -> adapt", Fit{ActionID: "a", Score: 0.7, CanBind: true}, Adapt},
		{"low fit -> synthesize", Fit{ActionID: "a", Score: 0.4, CanBind: true}, Synthesize},
		{"exactly reuse bar -> reuse", Fit{ActionID: "a", Score: 0.82, CanBind: true}, Reuse},
		{"exactly adapt bar -> adapt", Fit{ActionID: "a", Score: 0.60, CanBind: true}, Adapt},
	}
	for _, c := range cases {
		if got := Decide(c.fit, th); got != c.want {
			t.Errorf("%s: want %s, got %s", c.name, c.want, got)
		}
	}
}

func TestShouldDedup(t *testing.T) {
	th := DefaultThresholds()
	if !ShouldDedup(Fit{ActionID: "a", Score: 0.95}, th) {
		t.Fatalf("0.95 must dedup")
	}
	if ShouldDedup(Fit{ActionID: "a", Score: 0.85}, th) {
		t.Fatalf("0.85 (below 0.90) must not dedup")
	}
	if ShouldDedup(Fit{}, th) {
		t.Fatalf("empty fit must not dedup")
	}
}

func TestBestFit_PrefersCompositeAtOrAboveReuse(t *testing.T) {
	th := DefaultThresholds()
	hits := []RankedFit{
		{Fit: Fit{ActionID: "prim", Score: 0.95}, IsComposite: false},
		{Fit: Fit{ActionID: "comp", Score: 0.85}, IsComposite: true},
	}
	// Composite clears the reuse bar -> it wins even though the primitive
	// scores higher (reuse the highest-level prior composition).
	if got := BestFit(hits, th); got.ActionID != "comp" {
		t.Fatalf("want composite preferred, got %v", got.ActionID)
	}
}

func TestBestFit_FallsBackToHighestWhenNoCompositeClearsBar(t *testing.T) {
	th := DefaultThresholds()
	hits := []RankedFit{
		{Fit: Fit{ActionID: "prim", Score: 0.95}, IsComposite: false},
		{Fit: Fit{ActionID: "comp", Score: 0.5}, IsComposite: true}, // below reuse bar
	}
	if got := BestFit(hits, th); got.ActionID != "prim" {
		t.Fatalf("want highest-scoring primitive, got %v", got.ActionID)
	}
}
