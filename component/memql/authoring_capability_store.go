package memql

// authoring_capability_store.go -- the engine-backed CapabilityStore (epic
// memql#954, issue #961, increment 2).
//
// Resolves the author's standing envelope from the graph, reusing the EXACT
// queries the worker dispatcher uses so the authored-automation gate and the
// agent gate read the same source of truth:
//
//   - userByIdSystem -> preferences.computerUseEnabled (the kill switch);
//   - agentAuthorizationsForSelf -> the BROADEST standing
//     computerUseScope the author has granted (the author's envelope ceiling).
//
// An authored automation runs under the author's envelope, so its scope ceiling
// is whatever scope the author already granted -- it can never widen it.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"

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
	if strings.TrimSpace(ownerUserId) == "" {
		// AuthorizeCapabilityWithStore already refuses a blank author, so this
		// is defence rather than a live path -- but it has to be here too:
		// auth.ContextWithUserActor below is a NO-OP on a blank id (see
		// actor_user_context_test.go, "a blank owner is a no-op, so call sites
		// must refuse first"), which would silently resolve the grant read
		// against whatever actor the inbound context carries.
		return env, fmt.Errorf("authoring: capability envelope requires an author")
	}

	// Kill switch: v1:identity:user.preferences.computerUseEnabled.
	// #2800: server-side read of another user's row (the computer-use kill
	// switch is checked for the workspace OWNER, not the caller), so it uses
	// the @serverOnly construct with an explicit internal stamp.
	userRes, err := s.engine.Execute(auth.ContextWithInternalOrigin(ctx),
		fmt.Sprintf(`query userByIdSystem(userId:%q)`, ownerUserId))
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
	//
	// #3177: the construct is `agentAuthorizationsForSelf` now -- self-scoped
	// on actor.userId, no userId argument -- because the concept declares
	// `@rowAuthz(owner="userId")` and an `args.userId` read of a declared
	// concept is exactly what #3172's land gate refuses. The AUTHOR is not
	// necessarily the caller here (an authored automation runs under its
	// author's envelope no matter who fired the triggering event), so the
	// owner's actor envelope is supplied for this ONE Execute -- built inline
	// as the argument, in the memql#3072 shape epic decision C blesses, never
	// stamped onto the request's own context (memql#2989 refuted that).
	authRes, err := s.engine.Execute(
		auth.ContextWithUserActor(ctx, ownerUserId),
		`query agentAuthorizationsForSelf()`)
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
