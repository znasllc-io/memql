package worker

import (
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql/cockpit/internal/worker/hardware"
	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
	"github.com/znasllc-io/memql/cockpit/internal/worker/tools"
)

// appdescriptors_test.go pins Register.app_descriptors (memql-cockpit#444):
// HOW this machine drives each app it reported. The field has been on the
// pinned proto since 2026-09-08 and this cockpit never set it, and the
// engine reads an absent descriptor as "assume capable" -- right for a
// cockpit that predates the field, wrong for one that simply forgot it. The
// cost was concrete: a machine whose Codex only has the mcp-server tools
// cannot constrain an answer, and without a descriptor saying so the engine
// sent it structured calls anyway.

func TestAppDescriptorsToProto(t *testing.T) {
	got := appDescriptorsToProto([]apps.Info{
		{Id: apps.IDClaudeCode, Harness: apps.HarnessClaudeHeadless, StructuredResult: true, FollowUps: true},
		{Id: apps.IDCodex, Harness: apps.HarnessCodexMCP, StructuredResult: false, FollowUps: true},
	})
	if len(got) != 2 {
		t.Fatalf("descriptors = %d, want one per app: %+v", len(got), got)
	}
	claude, codex := got[0], got[1]
	if claude.GetId() != apps.IDClaudeCode || claude.GetHarness() != apps.HarnessClaudeHeadless ||
		!claude.GetStructuredResult() || !claude.GetFollowUps() {
		t.Errorf("claude-code descriptor = %+v", claude)
	}
	if codex.GetId() != apps.IDCodex || codex.GetHarness() != apps.HarnessCodexMCP {
		t.Errorf("codex descriptor = %+v", codex)
	}
	// The one flag that matters most: the mcp-server fallback cannot
	// constrain an answer, and saying so is what keeps structured calls
	// away from it.
	if codex.GetStructuredResult() {
		t.Error("the codex-mcp descriptor claims structured answers it cannot give")
	}
}

// The inference-only claim rides the descriptor, so the engine can tell a
// machine that runs a tool-free turn without tools from one that cannot.
func TestDescriptorsCarryInferenceOnly(t *testing.T) {
	got := appDescriptorsToProto([]apps.Info{
		{Id: apps.IDClaudeCode, Harness: apps.HarnessClaudeHeadless, StructuredResult: true, FollowUps: true, InferenceOnly: true},
		{Id: apps.IDCodex, Harness: apps.HarnessCodexMCP, FollowUps: true},
	})
	if len(got) != 2 || !got[0].GetInferenceOnly() || got[1].GetInferenceOnly() {
		t.Fatalf("descriptors = %+v, want inference-only on claude-code only", got)
	}
}

// An inventory entry with no harness word is left out rather than sent
// with an empty one: the engine drops a descriptor whose harness it does
// not know, but an empty word is a claim this machine never meant to make.
func TestAppDescriptorsToProto_SkipsAnAppWithNoHarness(t *testing.T) {
	got := appDescriptorsToProto([]apps.Info{
		{Id: apps.IDClaudeCode, Harness: apps.HarnessClaudeHeadless, StructuredResult: true, FollowUps: true},
		{Id: apps.IDCodex},
	})
	if len(got) != 1 || got[0].GetId() != apps.IDClaudeCode {
		t.Errorf("descriptors = %+v, want only the app with a harness", got)
	}
	if appDescriptorsToProto(nil) != nil {
		t.Error("no inventory is no descriptors")
	}
}

// The descriptor is resolved for a BLOCKED app too (the detector says so),
// and it rides Register beside that app's inventory entry.
func TestBuildRegister_CarriesAppDescriptors(t *testing.T) {
	register := buildRegister(Config{
		Name:         "test-worker",
		Capabilities: []string{"HEADLESS"},
	}, []apps.Info{
		{Id: apps.IDClaudeCode, Version: "2.1.270", SignedIn: true, Allowed: true,
			Harness: apps.HarnessClaudeHeadless, StructuredResult: true, FollowUps: true},
		{Id: apps.IDCodex, Version: "codex-cli 0.153.4", Allowed: false,
			Harness: apps.HarnessCodexAppServer, StructuredResult: true, FollowUps: true},
	}, models.Inventory{}, hardware.Inventory{}, tools.PipelinesPolicy{Allow: false})

	descs := register.GetAppDescriptors()
	if len(descs) != 2 {
		t.Fatalf("app_descriptors = %d, want 2: %+v", len(descs), descs)
	}
	if descs[1].GetId() != apps.IDCodex || descs[1].GetHarness() != apps.HarnessCodexAppServer {
		t.Errorf("codex descriptor = %+v, want the harness this machine resolved", descs[1])
	}
	if len(register.GetApps()) != 2 {
		t.Error("the descriptors ride beside the inventory, not instead of it")
	}
}
