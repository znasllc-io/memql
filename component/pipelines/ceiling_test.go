package pipelines

import (
	"testing"
	"time"
)

// TestParseRunCeiling (ruling R31b): the run's wall-clock ceiling is named,
// defaulted and parsed in one place, the seam's leaf, so the driver that
// waits on a run's steps and the substrate that holds each step to it read
// the same value.
//
// The variable's name is spelled out here rather than taken from the
// constant, so a typo in the constant fails this test instead of being read
// back through itself.
func TestParseRunCeiling(t *testing.T) {
	if EnvRunCeiling != "MEMQL_PIPELINES_RUN_MAX_MINUTES" {
		t.Fatalf("EnvRunCeiling = %q, want MEMQL_PIPELINES_RUN_MAX_MINUTES: the variable the registry names", EnvRunCeiling)
	}
	if DefaultRunCeiling != 120*time.Minute {
		t.Fatalf("DefaultRunCeiling = %v, want 2h0m0s (design record D11)", DefaultRunCeiling)
	}

	// A value the operator set that makes no sense as a whole number of
	// minutes is the DEFAULT, never a bound: zero minutes is not a request for
	// a five-minute ceiling, and no value means "no limit".
	for _, c := range []struct {
		raw  string
		want time.Duration
	}{
		{"", 120 * time.Minute},
		{"90", 90 * time.Minute},
		{" 90 ", 90 * time.Minute},
		{"5", 5 * time.Minute},
		{"1440", 1440 * time.Minute},
		{"1", 5 * time.Minute},
		{"4", 5 * time.Minute},
		{"1441", 1440 * time.Minute},
		{"100000", 1440 * time.Minute},
		{"99999999999999999999", 120 * time.Minute}, // not an int: the default
		{"9223372036854775807", 1440 * time.Minute}, // an int whose minutes overflow a Duration: the bound
		{"0", 120 * time.Minute},
		{"-30", 120 * time.Minute},
		{"ninety", 120 * time.Minute},
		{"1.5", 120 * time.Minute},
		{"90m", 120 * time.Minute},
	} {
		t.Run("MEMQL_PIPELINES_RUN_MAX_MINUTES="+c.raw, func(t *testing.T) {
			if got := ParseRunCeiling(c.raw); got != c.want {
				t.Errorf("ParseRunCeiling(%q) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}
