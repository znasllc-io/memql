package pipelines

import (
	"strconv"
	"strings"
	"time"
)

// A run's wall-clock ceiling (design record D11, ruling R31b): how long one
// pipeline run may take, end to end, whatever its steps declare. A step's own
// timeout runs from its Job's creation; the time it spends queued for a free
// slot under the cluster's ceiling is bounded by this alone.
//
// Named, defaulted and parsed here, at the seam's leaf, so the places that
// bound a run by it read one value through one parse: the driver that waits
// on a run's steps (component/pipelinerun), and the agent's executor and the
// workbench's runner that hold each step to it (integrations/pipelinesteps).
// Each reads the variable itself, by EnvRunCeiling, where cmd/envscan sees
// the read.

// EnvRunCeiling names the ceiling, in whole minutes. It is registered in
// scripts/secrets/manifest.yaml (component pipelines).
const EnvRunCeiling = "MEMQL_PIPELINES_RUN_MAX_MINUTES"

// DefaultRunCeiling is the ceiling where EnvRunCeiling names none: two hours.
const DefaultRunCeiling = 120 * time.Minute

// The bounds a ceiling the variable names is clamped to, in minutes.
const (
	minRunCeilingMinutes = 5
	maxRunCeilingMinutes = 1440
)

// ParseRunCeiling is the run's ceiling a value of EnvRunCeiling names.
//
// A value that is not a positive whole number of minutes -- unset included --
// is the DEFAULT, never a bound and never "no limit": zero minutes is not a
// request for the shortest ceiling, and an unbounded run is the one outcome a
// misconfigured ceiling must not produce. A value past a bound is clamped to
// it.
func ParseRunCeiling(minutes string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(minutes))
	switch {
	case err != nil || n <= 0:
		return DefaultRunCeiling
	case n < minRunCeilingMinutes:
		n = minRunCeilingMinutes
	case n > maxRunCeilingMinutes:
		// Compared in minutes, before any Duration is made: a value this
		// large would overflow one.
		n = maxRunCeilingMinutes
	}
	return time.Duration(n) * time.Minute
}
