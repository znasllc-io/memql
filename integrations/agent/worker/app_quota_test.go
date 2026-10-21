//go:build agent

package worker

import (
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/common"
	"testing"
)

func TestAppQuotaFallbackCrossesReplicaWithContextIntact(t *testing.T) {
	h := newAppHop(t)
	both := []workerservice.AppInfo{{Id: "claude-code", Allowed: true, SignedIn: true}, {Id: "codex", Allowed: true, SignedIn: true}}
	h.senderStore.machines[0].Apps = both
	h.holderStore.machines[0].Apps = both
	h.registry.WorkerById(hopMachine).SetApps(both)
	h.harness.quotaApp = "claude-code"
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

func TestQuotaFallbackRequiresNoModelWorkOrRecordedActions(t *testing.T) {
	base := workerservice.RunResult{Status: workerservice.AppSessionStatusFailed, ErrorMessage: "You've hit your weekly limit", Usage: workerservice.AppSessionUsage{Known: true}}
	if appQuotaRefusal(base) == nil {
		t.Fatal("pre-work quota refusal was lost")
	}
	for _, change := range []func(*workerservice.RunResult){
		func(r *workerservice.RunResult) { r.RecordedActions = 1 },
		func(r *workerservice.RunResult) { r.DroppedActions = 1 },
		func(r *workerservice.RunResult) { r.Usage.InputTokens = 1 },
		func(r *workerservice.RunResult) { r.Usage.Known = false },
		func(r *workerservice.RunResult) { r.Model = "served-model" },
		func(r *workerservice.RunResult) { r.ErrorMessage = "connection lost" },
	} {
		r := base
		change(&r)
		if appQuotaRefusal(r) != nil {
			t.Fatalf("uncertain work became retryable: %+v", r)
		}
	}
}
