//go:build agent

package worker

import (
	"errors"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/common"
	"testing"
)

func TestAppQuotaFallbackCrossesReplicaWithContextIntact(t *testing.T) {
	assertFallbackCrossesReplica(t, workerservice.AppSessionOutcome{ExitCode: 1, Error: "You've hit your weekly limit", Usage: workerservice.AppSessionUsage{Known: true}})
}

// The two refusals the owner met on 2026-10-30, each across the hop: the
// machine's stream on the holder, the call on a replica without it.
func TestAppFallbackCrossesReplicaForARefusalReportingNoUsage(t *testing.T) {
	assertFallbackCrossesReplica(t, workerservice.AppSessionOutcome{ExitCode: 0, Error: measuredCodexQuota.ErrorMessage})
}

func TestAppFallbackCrossesReplicaForAnExpiredLogin(t *testing.T) {
	assertFallbackCrossesReplica(t, workerservice.AppSessionOutcome{ExitCode: 1, Error: measuredClaudeExpired.ErrorMessage, Usage: workerservice.AppSessionUsage{Known: true}})
}

func assertFallbackCrossesReplica(t *testing.T, refusal workerservice.AppSessionOutcome) {
	t.Helper()
	h := newAppHop(t)
	both := []workerservice.AppInfo{{Id: "claude-code", Allowed: true, SignedIn: true}, {Id: "codex", Allowed: true, SignedIn: true}}
	h.senderStore.machines[0].Apps = both
	h.holderStore.machines[0].Apps = both
	h.registry.WorkerById(hopMachine).SetApps(both)
	h.harness.refusals = map[string]workerservice.AppSessionOutcome{"claude-code": refusal}
	providers := memqlengine.NewProviderRegistryForTest()
	providers.SetAppInference(h.remote)
	ctx, cancel := plannerCtx(t)
	defer cancel()
	entry, _ := providers.EntryForUser(ctx, hopOwner, memqlengine.AppWildcard)
	_, err := entry.Client.(common.ChatStructuredProvider).CallChatStructured(ctx, []common.ChatMessage{{Role: "system", Content: "Keep all constraints"}, {Role: "user", Content: "Initial request"}, {Role: "assistant", Content: "Earlier result"}, {Role: "user", Content: "Correction: integer cents"}}, triageSchema)
	if err != nil {
		t.Fatal(err)
	}
	starts, _ := h.harness.sessions()
	if len(starts) != 2 || starts[0].App != "claude-code" || starts[1].App != "codex" {
		t.Fatalf("no app fallback: %+v", starts)
	}
	if starts[0].Prompt != starts[1].Prompt || starts[0].ResponseSchema != starts[1].ResponseSchema || starts[0].RunId != starts[1].RunId {
		t.Fatal("context changed across fallback")
	}
}

// The refusal each app really reports, measured on 2026-10-29: a Codex turn
// refused by its usage limit states no usage at all, and a Claude Code turn
// refused at sign-in states zero usage and repeats the refusal as its only text.
var (
	measuredCodexQuota = workerservice.RunResult{
		Status:       workerservice.AppSessionStatusFailed,
		ErrorMessage: "codex app-server: the turn failed: You\u2019ve hit your usage limit. Visit https://chatgpt.com/settings/usage to purchase more credits or try again at Nov 3rd, 2026 11:52 PM.",
	}
	measuredClaudeExpired = workerservice.RunResult{
		Status:       workerservice.AppSessionStatusFailed,
		ErrorMessage: "harness: the claude turn exited 1: Failed to authenticate: OAuth session expired and could not be refreshed",
		Answer:       "Failed to authenticate: OAuth session expired and could not be refreshed",
		Usage:        workerservice.AppSessionUsage{Known: true},
	}
)

func TestPreWorkRefusalNamesWhatTheAppRefused(t *testing.T) {
	var quota *memqlengine.AppQuotaRefused
	if err := appPreWorkRefusal(measuredCodexQuota); !errors.As(err, &quota) {
		t.Fatalf("a Codex refusal that reported no usage was lost: %v", err)
	}
	var signedOut *memqlengine.AppAuthRefused
	if err := appPreWorkRefusal(measuredClaudeExpired); !errors.As(err, &signedOut) {
		t.Fatalf("an expired login was not classified: %v", err)
	}
}

// Only a refusal proven to be pre-work may move to another app: anything else
// may have caused effects, and running it again would repeat them.
func TestPreWorkRefusalRequiresNoModelWorkOrRecordedActions(t *testing.T) {
	for name, base := range map[string]workerservice.RunResult{"quota": measuredCodexQuota, "auth": measuredClaudeExpired} {
		for why, change := range map[string]func(*workerservice.RunResult){
			"a recorded action": func(r *workerservice.RunResult) { r.RecordedActions = 1 },
			"a dropped action":  func(r *workerservice.RunResult) { r.DroppedActions = 1 },
			"input tokens":      func(r *workerservice.RunResult) { r.Usage.InputTokens = 1 },
			"output tokens":     func(r *workerservice.RunResult) { r.Usage.OutputTokens = 1 },
			"a served model":    func(r *workerservice.RunResult) { r.Model = "served-model" },
			"a result":          func(r *workerservice.RunResult) { r.Result = []byte(`{"a":1}`) },
			"model prose":       func(r *workerservice.RunResult) { r.Answer = "Here is the first half of the answer" },
			"another failure":   func(r *workerservice.RunResult) { r.ErrorMessage = "connection lost" },
			"a finished run":    func(r *workerservice.RunResult) { r.Status = workerservice.AppSessionStatusEnded },
		} {
			r := base
			change(&r)
			if err := appPreWorkRefusal(r); err != nil {
				t.Fatalf("%s with %s became retryable: %v", name, why, err)
			}
		}
	}
}
