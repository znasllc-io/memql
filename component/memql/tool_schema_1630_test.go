package memql

import (
	"log/slog"
	"os"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// Test #1630: for several mutations the reflected tool argsSchema declared
// a field as one JSON type while the bound concept required another, so NO
// input satisfied both -- the typed-create / typed-update path was
// unsatisfiable via the MCP connector and the agent tool loop.
//
// Affected (all array concept fields mis-declared as `object` in the
// mutation args block, plus the delegation payload/field mismatches):
//   - mutationRecordLegalAcceptance.legalAcceptance  (user.legalAcceptance []object)
//   - mutationPersistTaskState.toolCallHistory       (taskState.toolCallHistory []object)
//   - mutationPersistTaskState.pendingSubPlanIds     (taskState.pendingSubPlanIds []string)
//   - mutationCreateDomainEntitySchema.keyFields/displayFields (domainEntitySchema []string)
//   - mutationCreateDelegation.scopes (delegation.scopes []string),
//     roleCeiling enum, missing createdBySubject, spurious agentSubject.
//
// This loads the real DSL, reflects each mutation's tool InputSchema the
// same way registerFunctionTools does, and validates the CONCEPT-VALID
// shape against it. Before the DSL fix these mutations declared `object`
// args, so an array (the concept-valid value) fails validation; after the
// fix the array validates.
func TestToolSchema1630_ArgConceptTypesReconciled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(logger, registry, memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	snapshot := registry.Snapshot()

	schemaFor := func(t *testing.T, mutation string) interface {
		Validate(any) error
	} {
		t.Helper()
		fn := snapshot[mutation]
		if fn == nil {
			t.Fatalf("mutation %q not found in function registry", mutation)
		}
		raw, err := toolInputSchemaFromArgs(fn.ArgsSchema)
		if err != nil {
			t.Fatalf("%s: toolInputSchemaFromArgs: %v", mutation, err)
		}
		return compileSchema(t, raw)
	}

	t.Run("recordLegalAcceptance accepts legalAcceptance array", func(t *testing.T) {
		s := schemaFor(t, "mutationRecordLegalAcceptance")
		args := map[string]any{
			"userId": "v1:identity:user:abc",
			"legalAcceptance": []any{
				map[string]any{"documentType": "tos", "version": "1.0", "acceptedAt": "2026-01-01T00:00:00Z", "sourceIP": "1.2.3.4"},
			},
		}
		if err := s.Validate(args); err != nil {
			t.Fatalf("concept-valid legalAcceptance array rejected: %v", err)
		}
	})

	t.Run("persistTaskState accepts toolCallHistory + pendingSubPlanIds arrays", func(t *testing.T) {
		s := schemaFor(t, "mutationPersistTaskState")
		args := map[string]any{
			"taskId":            "v1:planner:task:abc",
			"workingMemory":     map[string]any{"step": 1},
			"toolCallHistory":   []any{map[string]any{"toolName": "x", "args": map[string]any{}, "result": "ok"}},
			"pendingSubPlanIds": []any{"v1:planner:plan:a", "v1:planner:plan:b"},
		}
		if err := s.Validate(args); err != nil {
			t.Fatalf("concept-valid taskState arrays rejected: %v", err)
		}
	})

	t.Run("createDelegation typed-create is self-consistent", func(t *testing.T) {
		s := schemaFor(t, "mutationCreateDelegation")
		// scopes is an array (concept []string), roleCeiling is a
		// concept-enum value, createdBySubject is present, and
		// agentSubject is NOT a field (it is not on the concept and was
		// rejected by additionalProperties:false in the payload).
		args := map[string]any{
			"identityId":       "v1:identity:identity:abc",
			"identitySubject":  "user:abc",
			"identityType":     "human",
			"agentId":          "v1:agents:agent:xyz",
			"roleCeiling":      "writer",
			"scopes":           []any{"query:*", "mutation:cognition.*"},
			"createdBySubject": "user:abc",
		}
		if err := s.Validate(args); err != nil {
			t.Fatalf("concept-valid delegation create rejected: %v", err)
		}

		// roleCeiling must reject a value outside the concept enum
		// (the old arg had no enum and accepted "member").
		bad := map[string]any{
			"identityId":       "v1:identity:identity:abc",
			"identitySubject":  "user:abc",
			"identityType":     "human",
			"agentId":          "v1:agents:agent:xyz",
			"roleCeiling":      "member",
			"scopes":           []any{"query:*"},
			"createdBySubject": "user:abc",
		}
		if err := s.Validate(bad); err == nil {
			t.Fatalf("roleCeiling=member should be rejected by the concept enum, but validated")
		}

		// agentSubject is no longer a declared field; passing it must be
		// rejected (additionalProperties:false on the tool schema).
		extra := map[string]any{
			"identityId":       "v1:identity:identity:abc",
			"identitySubject":  "user:abc",
			"identityType":     "human",
			"agentId":          "v1:agents:agent:xyz",
			"roleCeiling":      "writer",
			"scopes":           []any{"query:*"},
			"createdBySubject": "user:abc",
			"agentSubject":     "agent:xyz",
		}
		if err := s.Validate(extra); err == nil {
			t.Fatalf("agentSubject should be rejected (not a declared arg), but validated")
		}
	})
}
