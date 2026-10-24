package work

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/work"
)

func TestFailureRetryUnknownChronologyCannotReleaseABareRun(t *testing.T) {
	i, eng, store := newActsIntegration(t)
	addVersion(store, actRunId, "draft", 0, 2, "failed", nil, map[string]any{"override": map[string]any{"prompt": "Earlier owner direction"}, "startedAt": ""})
	req := work.FailureApproval(work.ApprovalKindPlanReview, actRunId, "draft", work.SymptomEnvironment, "retention failed", "Retry?", work.Evidence{}, testNow, time.Hour)
	raw, err := json.Marshal(map[string]any{"id": "v1:work:approval:a1", "runId": actRunId, "ownerUserId": actOwner, "stepKey": "draft", "kind": req.Kind, "subject": req.Subject, "artifactHash": req.ArtifactHash, "options": req.Options})
	require.NoError(t, err)
	var approval map[string]any
	require.NoError(t, json.Unmarshal(raw, &approval))
	eng.reply("workApprovalsForOwner", approval)
	run := actRunRow(runStatusWaiting, "draft")
	run["waitingOn"] = map[string]any{"kind": "approval", "subject": "v1:work:approval:a1", "approvalKind": req.Kind}
	eng.reply("workRunForOwner", run)
	nodes, err := i.handleDecideApproval(callerContext(actOwner), map[string]any{"approvalId": "v1:work:approval:a1", "decision": "answered", "answer": map[string]any{"value": work.FailureAnswerRetry}}, 0)
	require.NoError(t, err, "the answer is recorded even when recovery cannot proceed")
	result := decodeReply(t, nodes)
	require.Equal(t, false, result["runResumed"])
	require.Contains(t, result["resumeError"], "original time")
	require.Empty(t, eng.callsTo("updateWorkRun"), "a recorded answer cannot drop the override and release a bare execution")
}

func TestFailureRetryPreservesOnlyTheFailedVersionsOverride(t *testing.T) {
	for _, whole := range []bool{false, true} {
		for _, cleared := range []bool{false, true} {
			t.Run(testBoolName(whole)+"/cleared-"+testBoolName(cleared), func(t *testing.T) {
				i, _, store := newActsIntegration(t)
				old := map[string]any{"prompt": "Superseded instruction", "model": "fleet:old"}
				current := work.Override{Prompt: "Verify the saved document without desktop input.", WholePrompt: whole, Model: "app:codex:test-model", Effort: "xhigh", Inputs: map[string]any{"document": "reviewed-copy"}, RequestedBy: actOwner}.Object()
				if cleared {
					current = map[string]any{}
				}
				addVersion(store, actRunId, "fetch", 0, 9, "done", nil, map[string]any{"override": old})
				addVersion(store, actRunId, "draft", 1, 2, "failed", nil, map[string]any{"override": old})
				addVersion(store, actRunId, "draft", 1, 3, "failed", nil, map[string]any{"override": current, "startedAt": rfc(testNow.Add(-time.Hour))})
				run := actRunRow(runStatusWaiting, "fetch", "draft")
				// Neither a stale head nor a different step's rerun may supply it.
				run["head"] = map[string]any{"draft": map[string]any{"version": 2}}
				run["rerun"] = map[string]any{"stepKey": "fetch", "override": old, "requestedAt": rfc(testNow)}
				request, stale, ok, err := i.failureRetryRequest(callerContext(actOwner), run, actRunId, failureRetry{stepKey: "draft/nested", decidedBy: actOwner}, testNow)
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, current, rowMap(request, "override"))
				require.Equal(t, "draft", request["stepKey"])
				require.Equal(t, map[string]int{"draft": 4}, request["versions"])
				require.NotContains(t, stale, "fetch")
				require.NotContains(t, request, "snapshot", "retry must not restore a pre-effect workspace")
				if cleared {
					require.NotContains(t, request, "overrideRequestedAt")
				} else {
					require.Equal(t, rfc(testNow.Add(-time.Hour)), request["overrideRequestedAt"])
				}
			})
		}
	}
}

func TestFailureRetryDoesNotRenewInstructionChronology(t *testing.T) {
	i, _, store := newActsIntegration(t)
	override := work.Override{Prompt: "Original owner direction", Model: "fleet:chosen"}.Object()
	addVersion(store, actRunId, "draft", 0, 3, "failed", nil, map[string]any{"override": override, "startedAt": rfc(testNow)})
	run := actRunRow(runStatusWaiting, "draft")
	run["rerun"] = map[string]any{"stepKey": "draft", "override": override, "requestedAt": rfc(testNow.Add(-time.Minute)), "overrideRequestedAt": rfc(testNow.Add(-time.Hour))}
	request, _, ok, err := i.failureRetryRequest(callerContext(actOwner), run, actRunId, failureRetry{stepKey: "draft"}, testNow)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, rfc(testNow.Add(-time.Hour)), request["overrideRequestedAt"])
	require.Equal(t, rfc(testNow), request["requestedAt"], "the recovery request itself is new")
}

func testBoolName(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func TestFailureRetryRefusesUnknownInstructionChronology(t *testing.T) {
	for _, source := range []string{"failed-version", "inherited"} {
		for _, at := range []string{"", "invalid-time", rfc(testNow.Add(time.Minute))} {
			t.Run(source+"/"+at, func(t *testing.T) {
				i, _, store := newActsIntegration(t)
				override := work.Override{Prompt: "Earlier direction"}.Object()
				started := at
				run := actRunRow(runStatusWaiting, "draft")
				if source == "inherited" {
					started = rfc(testNow.Add(-time.Minute))
					run["rerun"] = map[string]any{"stepKey": "draft", "override": override, "overrideRequestedAt": at, "requestedAt": rfc(testNow)}
				}
				addVersion(store, actRunId, "draft", 0, 2, "failed", nil, map[string]any{"override": override, "startedAt": started})
				request, _, ok, err := i.failureRetryRequest(callerContext(actOwner), run, actRunId, failureRetry{stepKey: "draft"}, testNow)
				require.ErrorContains(t, err, "original time")
				require.False(t, ok)
				require.Nil(t, request, "unknown chronology cannot become a fresh instruction")
			})
		}
	}
}
