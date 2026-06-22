package memql

import "testing"

// pagination_backstop_test.go covers the runtime default-cap backstop
// (epic 5, issue 5.1 / memql#1965): an unmarked list query -- no
// explicit paginate (plan.Limit nil) and no sort (plan.Sort empty) --
// is capped at config.DefaultListCap (50) instead of MaxResults (500),
// while every explicitly-windowed query keeps the wider default.

func backstopEngine() *MemQLEngine {
	return &MemQLEngine{config: engineConfig{
		MaxResults:     500,
		MaxWindow:      5000,
		DefaultListCap: 50,
	}}
}

func intptr(v int) *int { return &v }

func TestDefaultListLimit_UnmarkedListCappedAt50(t *testing.T) {
	e := backstopEngine()
	plan := &QueryPlan{} // no Limit, no Sort -> unmarked list read
	if got := e.defaultListLimit(plan); got != 50 {
		t.Fatalf("defaultListLimit(unmarked) = %d, want 50 (the implicit list cap)", got)
	}
}

func TestDefaultListLimit_PaginatedKeepsWideDefault(t *testing.T) {
	e := backstopEngine()
	// An explicit paginate sets plan.Limit -- the query stated its own
	// window, so the backstop must not engage.
	plan := &QueryPlan{Limit: intptr(200)}
	if got := e.defaultListLimit(plan); got != 500 {
		t.Fatalf("defaultListLimit(paginated) = %d, want 500 (MaxResults)", got)
	}
}

func TestDefaultListLimit_SortedKeepsWideDefault(t *testing.T) {
	e := backstopEngine()
	// A sort directive marks the query as explicitly ordered/bounded.
	plan := &QueryPlan{Sort: []SortField{{Field: "createdAt", Direction: SortDirectionDesc}}}
	if got := e.defaultListLimit(plan); got != 500 {
		t.Fatalf("defaultListLimit(sorted) = %d, want 500 (MaxResults)", got)
	}
}

// TestEffectiveWindow_UnmarkedListBackstop: the capped default flows
// through effectiveWindow to the realized window.
func TestEffectiveWindow_UnmarkedListBackstop(t *testing.T) {
	e := backstopEngine()
	plan := &QueryPlan{}
	limit, offset := e.effectiveWindow(plan.Limit, plan.Offset, e.defaultListLimit(plan))
	if limit != 50 {
		t.Errorf("effectiveWindow limit = %d, want 50", limit)
	}
	if offset != 0 {
		t.Errorf("effectiveWindow offset = %d, want 0", offset)
	}
}

// TestEffectiveWindow_ExplicitLimitWins: an explicit paginate window
// overrides the default entirely (it is neither raised to MaxResults nor
// lowered to the cap).
func TestEffectiveWindow_ExplicitLimitWins(t *testing.T) {
	e := backstopEngine()
	plan := &QueryPlan{Limit: intptr(7)}
	limit, _ := e.effectiveWindow(plan.Limit, plan.Offset, e.defaultListLimit(plan))
	if limit != 7 {
		t.Errorf("effectiveWindow limit = %d, want 7 (explicit paginate window)", limit)
	}
}

// TestDefaultListCapClampedToMaxResults: a misconfigured cap wider than
// MaxResults is clamped down -- the backstop is a tighter default, never
// a wider one.
func TestDefaultListCapClampedToMaxResults(t *testing.T) {
	t.Setenv("MEMORY_ENGINE_DEFAULT_LIST_CAP", "100000")
	cfg := loadEngineConfigFromEnv()
	if cfg.DefaultListCap != cfg.MaxResults {
		t.Fatalf("DefaultListCap = %d, want clamped to MaxResults=%d", cfg.DefaultListCap, cfg.MaxResults)
	}
}

// TestDefaultListCapHonoursOverride: a valid override below MaxResults
// is honoured.
func TestDefaultListCapHonoursOverride(t *testing.T) {
	t.Setenv("MEMORY_ENGINE_DEFAULT_LIST_CAP", "25")
	cfg := loadEngineConfigFromEnv()
	if cfg.DefaultListCap != 25 {
		t.Fatalf("DefaultListCap = %d, want 25 (honoured override)", cfg.DefaultListCap)
	}
}

// TestDefaultListCapDefaultsTo50: with no override the backstop defaults
// to 50.
func TestDefaultListCapDefaultsTo50(t *testing.T) {
	t.Setenv("MEMORY_ENGINE_DEFAULT_LIST_CAP", "")
	cfg := loadEngineConfigFromEnv()
	if cfg.DefaultListCap != defaultListCap {
		t.Fatalf("DefaultListCap = %d, want %d", cfg.DefaultListCap, defaultListCap)
	}
}
