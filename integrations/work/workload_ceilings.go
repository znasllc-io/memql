package work

import (
	"fmt"
	"github.com/znasllc-io/memql/component/work"
)

// Preserve previously recorded workload allowances; no new budget decisions are accepted. Keep
// every unapproved estimate and always intersect with the current hard limit;
// a later tighter goal limit must win over an older approval.
func approvedWorkloadCeilings(goal map[string]any, declared work.Ceilings, workload string) (work.Ceilings, error) {
	effective := work.EffectiveWorkloadCeilings(declared, workload)
	approved, err := ceilingsOf(map[string]any{"ceilings": rowMap(rowMap(goal, "ceilings"), "workloadOverrides")})
	if err != nil {
		return work.Ceilings{}, fmt.Errorf("invalid approved workload ceilings: %w", err)
	}
	if declared.WallClockMs > 0 && approved.WallClockMs > effective.WallClockMs {
		effective.WallClockMs = min(declared.WallClockMs, approved.WallClockMs)
	}
	if declared.MaxModelCalls > 0 && approved.MaxModelCalls > effective.MaxModelCalls {
		effective.MaxModelCalls = min(declared.MaxModelCalls, approved.MaxModelCalls)
	}
	if declared.MaxRetries > 0 && approved.MaxRetries > effective.MaxRetries {
		effective.MaxRetries = min(declared.MaxRetries, approved.MaxRetries)
	}
	return effective, nil
}
