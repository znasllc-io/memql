package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/provenance"
)

func TestAppSessionScopePersistsAndCannotBeRewrittenByItsOwner(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	owner := "scope-owner-" + uniqueSuffix("app")
	sessionID := "scope-session-" + uniqueSuffix("app")
	internal := appSessionOwnerCtx(owner)
	caller := auth.ContextWithUserActor(context.Background(), owner)
	scope := map[string]any{"agentId": "agent-one", "agentRole": "assistant", "tools": []any{map[string]any{"name": "discoverCapabilities"}}, "defaults": map[string]any{"discoverCapabilities": map[string]any{"runId": "run-one"}}}
	id := runMutation(t, internal, eng, "createAppSession", map[string]any{
		"sessionId": sessionID, "workerId": "worker-one", "app": "codex", "kind": "run", "runId": "run-one", "stepId": "step-one", "toolScope": scope, "startedAt": "2026-10-20T12:00:00Z",
	})
	const concept = "v1:worker:appSession"
	require.Equal(t, scope, latestPayload(t, internal, db, concept, id)["toolScope"])
	read, err := eng.Execute(caller, fmt.Sprintf("query appSessionById(sessionId:%q)", sessionID))
	require.NoError(t, err)
	require.Equal(t, scope, read.Bundle.Nodes[0].Payload.AsMap()["toolScope"], "the MCP replica reads the shaped durable contract")
	for field, value := range map[string]any{"toolScope": map[string]any{"agentRole": "owner"}, "status": "ended", "runId": "different", "stepId": "different", "sessionRunId": "different", "workerId": "different"} {
		t.Run(field, func(t *testing.T) {
			before := latestPayload(t, internal, db, concept, id)
			raw, err := json.Marshal(map[string]any{field: value})
			require.NoError(t, err)
			_, err = eng.Execute(caller, fmt.Sprintf(`insert(%q,id=%q,payload=%s)`, concept, id, raw))
			require.ErrorContains(t, err, "server-managed")
			updateCtx := provenance.ContextWithProvenance(caller, provenance.Direct("rawUpdate:"+concept))
			_, err = eng.executeMutation(updateCtx, MutationNode{Kind: ast.MutationKindUpdate, Concept: concept, ID: id, PayloadRaw: string(raw)})
			require.ErrorContains(t, err, "server-managed")
			require.Equal(t, before, latestPayload(t, internal, db, concept, id), "a refusal must not persist")
		})
	}
	// Structured results are still the app's writable back-channel.
	runMutation(t, caller, eng, "submitAppSessionResult", map[string]any{"sessionId": sessionID, "result": map[string]any{"ok": true}, "submittedAt": "2026-10-20T12:01:00Z"})
	require.Equal(t, scope, latestPayload(t, internal, db, concept, id)["toolScope"])
	runMutation(t, internal, eng, "recordAppSessionProgress", map[string]any{"sessionId": sessionID, "status": "running"})
	require.Equal(t, "running", latestPayload(t, internal, db, concept, id)["status"])
}
