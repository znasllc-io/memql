package planner

import (
	"testing"

	"github.com/znasllc-io/memql/core/id"
)

// TestSystemPlanIdsAreValid is the planner-side guard for issue #1712:
// every plan id minted by a system automation in this package must pass
// the engine's shortId validator (id.ValidateShortId -- the same rule
// memory-nodes Concept.validateShortId delegates to). Each case mirrors
// exactly how a live call site builds its id, so reintroducing a
// colon-concatenated id at any of these sites fails this test.
//
// Audited system minting sites routed through the central generator:
//   - reactive_loop.go      createResponsibilityPlan  ("proactive", respId)
//   - refresh_cron.go        spawnRefreshPlan          ("refresh", domainId)
//   - agent_loop_training_mint.go  ("train", parentPlanId) -- deterministic
func TestSystemPlanIdsAreValid(t *testing.T) {
	const planConcept = "v1:planner:plan"

	// Realistic colon-laden source ids drawn from live rows.
	respId := "v1:planner:responsibility:d6ac657f-1234-5678-9abc-def012345678"
	domainId := "v1:knowledge:knowledgeDomain:sales-playbook"
	parentPlanId := "v1:planner:plan:abc-123"

	cases := map[string]string{
		"reactive-loop proactive plan": id.NewSystemNodeShortId("proactive", respId),
		"refresh-cron refresh plan":    id.NewSystemNodeShortId("refresh", domainId),
		"training mint child plan":     id.SystemNodeShortId("train", parentPlanId),
	}

	for name, planId := range cases {
		t.Run(name, func(t *testing.T) {
			if err := id.ValidateShortId(planConcept, planId); err != nil {
				t.Fatalf("system-generated plan id %q invalid: %v", planId, err)
			}
		})
	}
}
