package memql

// authoring_capability_store.go -- the engine-backed CapabilityStore (epic
// memql#954, issue #961, increment 2).
//
// Resolves the author's standing envelope from the graph, reusing the EXACT
// queries the worker dispatcher uses so the authored-automation gate and the
// agent gate read the same source of truth:
//
//   - userById -> preferences.computerUseEnabled (the kill switch);
//   - agentAuthorizationsForUser -> the BROADEST standing
//     computerUseScope the author has granted (the author's envelope ceiling).
//
// An authored automation runs under the author's envelope, so its scope ceiling
// is whatever scope the author already granted -- it can never widen it.

import (
	"context"
	"encoding/json"
	"fmt"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// capNodePayloadJSON renders a query result node's structpb payload as JSON
// bytes. Local to the capability gate so it stays independent of the activation
// increment's identical helper (both land via separate PRs; keeping a private
// copy avoids a cross-PR symbol collision).
func capNodePayloadJSON(node *memqlv1.MemoryNode) ([]byte, error) {
	if node == nil || node.GetPayload() == nil {
		return nil, nil
	}
	return node.GetPayload().MarshalJSON()
}

// engineCapabilityStore is the production CapabilityStore over a live engine.
type engineCapabilityStore struct {
	engine *MemQLEngine
}

// LoadEnvelope resolves the author's kill-switch flag (from user preferences)
// and the broadest standing computer-use scope they have granted.
func (s *engineCapabilityStore) LoadEnvelope(ctx context.Context, ownerUserId string) (AuthoredEnvelope, error) {
	env := AuthoredEnvelope{OwnerUserId: ownerUserId}

	// Kill switch: v1:identity:user.preferences.computerUseEnabled.
	userRes, err := s.engine.Execute(ctx, fmt.Sprintf(`query userById(userId:%q)`, ownerUserId))
	if err != nil {
		return env, fmt.Errorf("query user: %w", err)
	}
	if userRes != nil && userRes.Bundle != nil && len(userRes.Bundle.Nodes) > 0 {
		payload, perr := capNodePayloadJSON(userRes.Bundle.Nodes[0])
		if perr != nil {
			return env, perr
		}
		env.KillSwitchEnabled = userPrefComputerUseEnabled(payload)
	}

	// Standing scope: the broadest active computerUseScope the author granted.
	authRes, err := s.engine.Execute(ctx, fmt.Sprintf(`query agentAuthorizationsForUser(userId:%q)`, ownerUserId))
	if err != nil {
		return env, fmt.Errorf("query authorizations: %w", err)
	}
	env.Scope = broadestScope(authRes)
	return env, nil
}

// userPrefComputerUseEnabled extracts preferences.computerUseEnabled from a user
// row payload JSON. Defaults to false (fail-closed: no preference row, or the
// flag absent, halts authored automations rather than silently allowing them).
func userPrefComputerUseEnabled(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	var row struct {
		Preferences struct {
			ComputerUseEnabled bool `json:"computerUseEnabled"`
		} `json:"preferences"`
	}
	if err := json.Unmarshal(payload, &row); err != nil {
		return false
	}
	return row.Preferences.ComputerUseEnabled
}

// broadestScope returns the widest active computerUseScope across the author's
// standing authorizations. Empty when the author has granted none -- which the
// gate treats as "no computer-use scope" (observe/full denied; mutations still
// flow under per-row authz once the kill switch is on).
func broadestScope(res *ExecuteResult) string {
	if res == nil {
		return ""
	}
	// authorizations land on the shaped Data axis (OutputPayload) OR the bundle
	// nodes depending on the query path; read both, preferring whichever carries
	// the rows.
	best := ""
	consider := func(scope string) {
		if scopeOrder[scope] > scopeOrder[best] {
			best = scope
		}
	}
	if res.Bundle != nil {
		for _, node := range res.Bundle.Nodes {
			payload, err := capNodePayloadJSON(node)
			if err != nil || len(payload) == 0 {
				continue
			}
			var row struct {
				ComputerUseScope string `json:"computerUseScope"`
			}
			if err := json.Unmarshal(payload, &row); err == nil {
				consider(row.ComputerUseScope)
			}
		}
	}
	return best
}
