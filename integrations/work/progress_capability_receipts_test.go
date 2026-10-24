package work

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

func capabilityProgressReceipt(t *testing.T, search, content string) map[string]any {
	t.Helper()
	args, err := json.Marshal(map[string]any{"search": search})
	require.NoError(t, err)
	messages, err := json.Marshal([]common.ChatMessage{
		{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "discovery", Name: "workCapabilities", Arguments: string(args)}}},
		{Role: "tool", Name: "workCapabilities", ToolCallId: "discovery", Content: content},
	})
	require.NoError(t, err)
	return map[string]any{"createdAt": rfc(testNow.Add(time.Second)), "runId": ceilingRunId, "ownerUserId": ceilingOwner, "kind": "note", "data": map[string]any{
		"capabilityEvidence": true, "messagesEncoding": "json-base64-v1", "messages": base64.StdEncoding.EncodeToString(messages),
	}}
}

const capabilityProgressFacts = `{"capabilities":[{"name":"library.createFile","kind":"mutation","app":"files"}],"fleet":{"status":"observed","checkedAt":"first","modalities":{"imageGen":{"advertised":1,"online":1,"idle":1}}}}`

func TestCapabilityProgressCreditsFactsOnceAcrossReplicas(t *testing.T) {
	i, e, run, rc := progressFixture(t)
	first := capabilityProgressReceipt(t, "files", capabilityProgressFacts)
	e.reply("workProgressReceiptsForOwnerRun", first)
	continued, err := i.ContinueWorkProgress(context.Background(), rc, workstate.Spent{WallClockMs: 650000}, 0)
	require.NoError(t, err)
	require.True(t, continued, "completed discovery must not ask for feedback before the first tool")
	run["classification"] = e.callsTo("updateWorkRun")[0].Args(t)["classification"]
	peer := New(e, testLogger())
	peer.decisionGate = i.decisionGate
	// A different search, timestamp, description or temporary Fleet status is
	// not a new capability fact, even after moving the work to another replica.
	repeated := capabilityProgressReceipt(t, "images and files", `{"fleet":{"checkedAt":"later","status":"observed","modalities":{"imageGen":{"idle":0,"online":0,"advertised":1}}},"capabilities":[{"description":"new prose","app":"files","kind":"mutation","name":"library.createFile"}]}`)
	require.Equal(t, progressReceiptKeys(first), progressReceiptKeys(repeated))
	e.reply("workProgressReceiptsForOwnerRun", repeated)
	continued, err = peer.ContinueWorkProgress(context.Background(), rc, workstate.Spent{WallClockMs: 1250000}, 0)
	require.NoError(t, err)
	require.False(t, continued)
	// A newly resolved operation contract is real new evidence. The existing
	// evidence and monetary-ceiling tests apply to this same path.
	e.reply("workProgressReceiptsForOwnerRun", first, capabilityProgressReceipt(t, "image", `{"capabilities":[{"name":"library.generateImage","kind":"mutation","app":"files"}]}`))
	continued, err = peer.ContinueWorkProgress(context.Background(), rc, workstate.Spent{WallClockMs: 1250000}, 0)
	require.NoError(t, err)
	require.True(t, continued)
	require.Len(t, e.callsTo("updateWorkRun"), 2)
	require.Empty(t, e.callsTo("createWorkApproval"))
	require.Empty(t, e.callsTo("updateWorkGoal"))
}

func TestCapabilityProgressRejectsIncompleteOrUnrelatedReceipts(t *testing.T) {
	for _, content := range []string{
		`broken`, `{}`, `{"capabilities":[{"name":"missing-kind"}]}`,
		`{"fleet":{"status":"unavailable","modalities":{"imageGen":{"advertised":1}}}}`,
		`{"fleet":{"status":"observed","modalities":{"imageGen":{}}}}`,
		`{"fleet":{"status":"observed","modalities":{"imageGen":{"advertised":-1}}}}`,
	} {
		require.Empty(t, progressReceiptKeys(capabilityProgressReceipt(t, "images", content)), content)
	}
	for _, mutate := range []func(map[string]any){
		func(row map[string]any) { row["kind"] = "decision" },
		func(row map[string]any) { rowMap(row, "data")["capabilityEvidence"] = false },
		func(row map[string]any) { rowMap(row, "data")["messagesEncoding"] = "unknown" },
		func(row map[string]any) { rowMap(row, "data")["messages"] = "malformed base64" },
		func(row map[string]any) {
			rowMap(row, "data")["messages"] = base64.StdEncoding.EncodeToString([]byte(`[{"Role":"assistant","Content":"I made progress"}]`))
		},
	} {
		row := capabilityProgressReceipt(t, "images", capabilityProgressFacts)
		mutate(row)
		require.Empty(t, progressReceiptKeys(row))
	}
}

func TestCapabilityProgressPreservesHardCeilings(t *testing.T) {
	for _, spent := range []workstate.Spent{{Cost: 0.5}, {Tokens: 1001}} {
		i, e, _, rc := progressFixture(t)
		e.reply("workProgressReceiptsForOwnerRun", capabilityProgressReceipt(t, "files", capabilityProgressFacts))
		continued, err := i.ContinueWorkProgress(context.Background(), rc, spent, 0)
		require.NoError(t, err)
		require.False(t, continued, "spent: %+v", spent)
		require.Empty(t, e.callsTo("updateWorkRun"))
	}
}
