package apps

import "testing"

// TestHarnessWordsMirrorTheWire pins the spelling of the three harness
// words against literals rather than against the constants that produce
// them.
//
// The words are a CONTRACT with two other places: internal/worker/harness
// dispatches on them, and the engine reads them off the registration
// (memql#5096) so it never asks a machine for a protocol it cannot speak.
// A rename on one side alone compiles everywhere and fails at the only
// moment that matters -- the session's Start, with "no client for harness
// <word>" -- so the literal is written out here where a rename has to
// stop and think.
func TestHarnessWordsMirrorTheWire(t *testing.T) {
	cases := map[string]string{
		HarnessClaudeHeadless: "claude-headless",
		HarnessCodexAppServer: "codex-app-server",
		HarnessCodexMCP:       "codex-mcp",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("harness word = %q, want %q", got, want)
		}
	}
	if len(cases) != 3 {
		t.Fatalf("two harness words collided into one: %v", cases)
	}
}

// TestSpecs_EveryAppDeclaresADrivableHarness. A spec with no harness word
// reaches the runner as an empty string, which no harness answers to; the
// session then fails after the engine has already committed a turn to
// this machine. Every entry in the closed set carries a word that is
// drivable on any machine that has the binary at all.
func TestSpecs_EveryAppDeclaresADrivableHarness(t *testing.T) {
	known := map[string]bool{
		HarnessClaudeHeadless: true,
		HarnessCodexAppServer: true,
		HarnessCodexMCP:       true,
	}
	for _, s := range Specs() {
		if s.Harness == "" {
			t.Errorf("%s: no harness word", s.ID)
			continue
		}
		if !known[s.Harness] {
			t.Errorf("%s: harness %q is not one of the three words", s.ID, s.Harness)
		}
	}
}

// TestSpecs_ClaudeCodeIsAlwaysHeadless. Claude Code has exactly one
// protocol, so its descriptor is a constant and there is nothing to probe
// -- a probe would be a subprocess per beat answering a question whose
// answer is written down.
func TestSpecs_ClaudeCodeIsAlwaysHeadless(t *testing.T) {
	spec, ok := SpecFor(IDClaudeCode)
	if !ok {
		t.Fatal("claude-code is not in the closed set")
	}
	if spec.Harness != HarnessClaudeHeadless {
		t.Errorf("harness = %q, want %q", spec.Harness, HarnessClaudeHeadless)
	}
	if !spec.StructuredResult {
		t.Error("claude -p --json-schema returns a schema'd final answer; claiming otherwise costs the engine the app door for every structured call")
	}
	if !spec.FollowUps {
		t.Error("claude --resume continues a session by id, which is what a follow-up is")
	}
	if _, _, ok := spec.harnessUpgrade(); ok {
		t.Error("claude-code offers a harness upgrade to probe for, and it has none")
	}
}

// TestSpecs_CodexFloorIsTheFallbackHarness is the fail-closed direction,
// and it is the whole reason the descriptor exists.
//
// Two harnesses answer to the id `codex`. The spec carries the one that
// works on EVERY Codex (the mcp-server tool pair); the machine's own
// binary is what upgrades it to the app-server. Carrying the app-server
// here instead would have a cockpit whose Codex predates it advertise a
// protocol nothing on that machine speaks, and the session would die at
// Start after the engine had already routed a turn to it.
//
// StructuredResult is false at the floor for the same reason: the
// mcp-server tools return a transcript, not an answer against a schema.
// An over-claim here surfaces as a parse failure in the engine's
// structured path, three layers away, naming nothing on this machine.
func TestSpecs_CodexFloorIsTheFallbackHarness(t *testing.T) {
	spec, ok := SpecFor(IDCodex)
	if !ok {
		t.Fatal("codex is not in the closed set")
	}
	if spec.Harness != HarnessCodexMCP {
		t.Errorf("floor harness = %q, want the fallback %q", spec.Harness, HarnessCodexMCP)
	}
	if spec.StructuredResult {
		t.Error("the mcp-server tool pair returns no schema'd answer; claiming one is an over-claim the engine cannot detect")
	}
	if !spec.FollowUps {
		t.Error("codex-reply continues a thread by id, which is what a follow-up is")
	}
}

// TestSpec_HarnessUpgrade_NamesTheProbeAndTheAnswer. The upgrade is data
// in this file rather than in detect.go so the two halves -- which word
// the machine may earn, and which command decides -- cannot drift apart:
// a probe wired to the wrong word reports a harness the binary does not
// have, which is the failure this whole descriptor exists to prevent.
func TestSpec_HarnessUpgrade_NamesTheProbeAndTheAnswer(t *testing.T) {
	spec, _ := SpecFor(IDCodex)
	upgraded, probe, ok := spec.harnessUpgrade()
	if !ok {
		t.Fatal("codex must offer an upgrade to the app-server")
	}
	if upgraded.Harness != HarnessCodexAppServer {
		t.Errorf("upgraded harness = %q, want %q", upgraded.Harness, HarnessCodexAppServer)
	}
	if !upgraded.StructuredResult || !upgraded.FollowUps {
		t.Errorf("the app-server carries both a schema'd result and thread continuation: %+v", upgraded)
	}
	if len(probe) == 0 {
		t.Fatal("an upgrade with no probe would be taken on faith")
	}
	if probe[0] != "app-server" {
		t.Errorf("probe = %v, want the app-server subcommand first", probe)
	}
	// Everything else about the spec survives the upgrade. The upgrade
	// answers three questions and must not quietly re-point the binary
	// or the version flag on its way through.
	if upgraded.ID != spec.ID || upgraded.Binary != spec.Binary {
		t.Errorf("the upgrade altered identity: %+v -> %+v", spec, upgraded)
	}
}

// TestSpec_InteractiveArgsSurviveTheHarnesses.
//
// RunArgs and AttachArgs were DELETED when the harnesses took over the
// headless argv (memql-cockpit#386), and this is the one that stayed:
// the `open` kind hands the app to a HUMAN with the prompt loaded, which
// is not a harness turn and has no protocol to speak. A later tidy-up
// that removed it alongside the other two would leave `open` with no way
// to pass the prompt at all -- and an `open` that launched the app with
// an empty first turn looks, to the person watching the window, exactly
// like one that worked.
func TestSpec_InteractiveArgsSurviveTheHarnesses(t *testing.T) {
	for _, id := range []string{IDClaudeCode, IDCodex} {
		spec, ok := SpecFor(id)
		if !ok {
			t.Fatalf("%s is not in the closed set", id)
		}
		args := spec.InteractiveArgs("write the release notes")
		if len(args) == 0 {
			t.Errorf("%s: the open kind has no argv to load the prompt with", id)
			continue
		}
		found := false
		for _, a := range args {
			if a == "write the release notes" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: the prompt is not in the argv: %v", id, args)
		}
		// An empty prompt yields no argv, so the launcher opens the app
		// with nothing rather than with an empty first turn.
		if got := spec.InteractiveArgs("  "); len(got) != 0 {
			t.Errorf("%s: a blank prompt produced argv %v", id, got)
		}
	}
}
