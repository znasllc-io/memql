package memql

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

func TestScopeCheckpointRequiresCurrentUnexecutedRequest(t *testing.T) {
	for _, mode := range []string{"pending", "session-pending", "prior-result", "later-call", "already-executed", "later-assistant", "unpaired", "duplicate", "wrong-name", "typed", "changed-summary", "changed-scope", "changed-session-kind", "changed-labels", "wrong-owner", "wrong-agent", "changed-call", "changed-hash", "missing-proof", "forged-proof", "malformed-typed"} {
		t.Run(mode, func(t *testing.T) {
			run := common.RunContext{RunId: "run", OwnerUserId: "owner", StepKey: "step", RerunRequestId: "attempt"}
			args := map[string]any{"intent": " inspect ", "requestedScope": "full", "summary": "Inspect selected material", "requireLabels": map[string]any{" kind ": "test", "ready": true}}
			if mode == "session-pending" {
				args["sessionKind"] = "desktop"
			}
			raw, err := json.Marshal(args)
			require.NoError(t, err)
			call := common.ToolCall{ID: "pending", Name: "requestComputerUseScope", Arguments: string(raw)}
			messages := []common.ChatMessage{{Role: "system"}, {Role: "assistant", ToolCalls: []common.ToolCall{call}}, {Role: "tool", ToolCallId: call.ID, Name: call.Name, Content: common.UnexecutedWorkToolResult}}
			subject := map[string]any{"agentId": "agent", "scope": "full", "intent": "inspect", "summary": "Inspect selected material. Runs Test computer.", "target": "Test computer", "requireLabels": map[string]string{"kind": "test", "ready": "true"}}
			if mode == "session-pending" {
				delete(subject, "scope")
				subject["sessionScope"] = "full"
				subject["sessionKind"] = "desktop"
			}
			witness := map[string]any{"version": 1, "continuationId": "checkpoint", "toolCallId": call.ID, "requestHash": work.ArtifactHash(args)}
			subject["nativeScopeCheckpoint"] = witness
			switch mode {
			case "prior-result", "later-call":
				messages[1].ToolCalls = append([]common.ToolCall{{ID: "earlier", Name: "other", Arguments: `{}`}}, call)
				content := "completed earlier effect"
				if mode == "later-call" {
					content = common.UnexecutedWorkToolResult
				}
				messages = append(messages, common.ChatMessage{Role: "tool", ToolCallId: "earlier", Name: "other", Content: content})
			case "already-executed":
				messages[2].Content = `{"status":"approved"}`
			case "later-assistant":
				messages = append(messages, common.ChatMessage{Role: "assistant", Content: "Later reply"})
			case "unpaired":
				messages = messages[:2]
			case "duplicate":
				messages = append(messages, messages[2])
			case "wrong-name":
				messages[1].ToolCalls[0].Name = "different"
			case "typed":
				args["operation"] = map[string]any{}
			case "changed-summary":
				subject["summary"] = "Another request"
			case "changed-scope":
				subject["scope"] = "observe"
			case "changed-session-kind":
				subject["sessionKind"] = "desktop"
			case "changed-labels":
				subject["requireLabels"] = map[string]string{"kind": "elsewhere"}
			case "wrong-owner":
				args["ownerUserId"] = "other"
			case "wrong-agent":
				args["agentId"] = "other"
			case "changed-call":
				witness["toolCallId"] = "different"
			case "changed-hash":
				witness["requestHash"] = "different"
			case "missing-proof":
				delete(subject, "nativeScopeCheckpoint")
			case "forged-proof":
				witness["continuationId"] = "different"
			case "malformed-typed":
				subject["computerPermission"] = map[string]any{}
			}
			if mode == "typed" || mode == "wrong-owner" || mode == "wrong-agent" {
				raw, err = json.Marshal(args)
				require.NoError(t, err)
				messages[1].ToolCalls[0].Arguments = string(raw)
				witness["requestHash"] = work.ArtifactHash(args) // A forged marker cannot repair a mismatched request.
			}
			want := mode == "pending" || mode == "prior-result" || mode == "session-pending"
			require.Equal(t, want, scopeCheckpointMatches(run, subject, "checkpoint", messages))
			if want || mode == "missing-proof" || mode == "malformed-typed" {
				require.Equal(t, want, WorkPermissionGuidance(work.ApprovalKindScopeElevation, subject, map[string]any{"feedback": "Request a narrower operation."}, work.ArtifactHash(subject)))
			}
		})
	}
}
