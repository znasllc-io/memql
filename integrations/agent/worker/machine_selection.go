package worker

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

// A machine selector must match the immutable enrollment, even when an
// operator label happens to use the same key. Other labels retain their
// ordinary policy semantics.
func satisfiesMachineRequirements(candidate Candidate, require map[string]string) bool {
	for key, value := range require {
		if key == "machineId" {
			if candidate.MachineId == "" || candidate.MachineId != value {
				return false
			}
		} else if got, ok := candidate.Labels[key]; !ok || got != value {
			return false
		}
	}
	return true
}

// Permission cannot silently choose between distinct personal machines. Check
// before filtering version support, so an old requested machine never turns
// into permission for a different, updated one. Choosing is human feedback;
// the subsequent scoped approval remains a separate gate.
func requireSingleComputerTarget(plan RoutePlan) error {
	candidates := append(append([]Candidate(nil), plan.Candidates...), plan.OfflineCandidates...)
	if len(candidates) <= 1 {
		return nil
	}
	choices := make([]map[string]any, 0, len(candidates))
	names := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		choices = append(choices, map[string]any{"name": candidate.Label(), "requireLabels": map[string]string{"machineId": candidate.MachineId}})
		names = append(names, candidate.Label())
	}
	return memql.NewStructuredError(memql.ErrorCodeInvalidArgument,
		fmt.Sprintf("Choose which computer to use: %s. Ask with requestUserFeedback, then request access separately for each selected machine using its workerStatus requireLabels. A machine choice does not grant access.", strings.Join(names, ", "))).
		WithDetails(map[string]any{"field": "requireLabels", "reason": "machine_selection_required", "machines": choices})
}
