//go:build linux || darwin

package appsession

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// levels_test.go drives a whole session at a LEVEL (memql-cockpit#437,
// #438): the knobs the level becomes on this machine, the owner's
// policy.yaml overriding them, the refusals that stop a session before it
// touches anything, and the model the app REPORTED coming back on the End.
//
// The argv is read from what the fake app recorded, for the reason every
// app-session test does: a level gone wrong is silent -- an app run at the
// wrong model answers perfectly well.

// levelApp installs a fake `claude` that records its argv and prints one
// recorded 2.1.270 turn: an init naming Haiku, and a result whose
// modelUsage says Haiku spent the tokens.
func levelApp(t *testing.T) (argvFile string) {
	t.Helper()
	argvFile = filepath.Join(t.TempDir(), "argv")
	fakeApp(t, "claude", fmt.Sprintf(`
printf '%%s\n' "$@" > %q
echo '{"type":"system","subtype":"init","session_id":"app-level","model":"claude-haiku-4-5-20251001"}'
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"app-level","total_cost_usd":0.02,"usage":{"input_tokens":10,"output_tokens":273},"modelUsage":{"claude-haiku-4-5-20251001":{"outputTokens":284}},"result":"ok"}'
`, argvFile))
	return argvFile
}

// newLevelRig is newRig with the owner's apps.levels behind it.
func newLevelRig(t *testing.T, levels func(string) (harness.Table, map[string]string)) *rig {
	t.Helper()
	h := newRig(t)
	h.manager = NewManager(Options{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		StateDir:    h.state,
		LibraryBase: h.library.server.URL,
		HTTPClient:  preflightFixtureClient(h.library.server),
		Allowed:     func(id string) bool { return id == apps.IDClaudeCode },
		Levels:      levels,
	})
	return h
}

// policyLevels loads a real policy.yaml, so the path from the owner's file
// to the app's argv is the one the worker runs.
func policyLevels(t *testing.T, body string) func(string) (harness.Table, map[string]string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := tools.LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	return p.AppLevels
}

func stderrChunks(h *rig) string {
	var b strings.Builder
	for _, c := range h.sender.recorded() {
		if c.stream == StreamStderr {
			b.WriteString(c.data)
		}
	}
	return b.String()
}

func TestSession_LevelBecomesTheAppsKnobs(t *testing.T) {
	argvFile := levelApp(t)
	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) { s.Level = "reasoning" })

	if end.GetError() != "" {
		t.Fatalf("error = %q; transcript: %s", end.GetError(), h.sender.transcript())
	}
	argv := readArgv(t, argvFile)
	if argvValue(argv, "--model") != "opus" || argvValue(argv, "--effort") != "xhigh" {
		t.Errorf("argv = %v, want the built-in reasoning entry: --model opus --effort xhigh", argv)
	}
	// The person reading the session is the one who needs to know what
	// "reasoning" meant on this machine, and where that came from.
	const note = "[memql] level reasoning runs claude-code with --model opus --effort xhigh (the cockpit's built-in table)"
	if !strings.Contains(stderrChunks(h), note) {
		t.Errorf("the transcript does not say what the level became; stderr chunks:\n%s", stderrChunks(h))
	}
}

func TestSession_NoLevelPassesNoKnobsAndNoNote(t *testing.T) {
	argvFile := levelApp(t)
	h := newRig(t)
	end := h.start(t, nil)

	if end.GetError() != "" {
		t.Fatalf("error = %q", end.GetError())
	}
	argv := readArgv(t, argvFile)
	if argvValue(argv, "--model") != "" || argvValue(argv, "--effort") != "" {
		t.Errorf("argv = %v; a session with no level runs the app at its own defaults", argv)
	}
	if strings.Contains(stderrChunks(h), "[memql] level") {
		t.Error("a session with no level has no translation to announce")
	}
}

// memql-cockpit#438's acceptance, end to end: a policy.yaml with
// apps.levels changes the argv, and an absent block uses the built-in
// table.
func TestSession_PolicyLevelsChangeTheArgv(t *testing.T) {
	t.Run("an owner's entry", func(t *testing.T) {
		argvFile := levelApp(t)
		h := newLevelRig(t, policyLevels(t, `apps:
  allow: [claude-code]
  levels:
    claude-code:
      strong:
        model: opus
        effort: medium
`))
		end := h.start(t, func(s *memqlv1.AppSessionStart) { s.Level = "strong" })
		if end.GetError() != "" {
			t.Fatalf("error = %q", end.GetError())
		}
		argv := readArgv(t, argvFile)
		if argvValue(argv, "--model") != "opus" || argvValue(argv, "--effort") != "medium" {
			t.Errorf("argv = %v, want the owner's strong entry", argv)
		}
		if !strings.Contains(stderrChunks(h), "(this machine's policy.yaml apps.levels)") {
			t.Errorf("the note must say the entry was the owner's: %s", stderrChunks(h))
		}
	})

	t.Run("an absent block", func(t *testing.T) {
		argvFile := levelApp(t)
		h := newLevelRig(t, policyLevels(t, "apps:\n  allow: [claude-code]\n"))
		end := h.start(t, func(s *memqlv1.AppSessionStart) { s.Level = "strong" })
		if end.GetError() != "" {
			t.Fatalf("error = %q", end.GetError())
		}
		argv := readArgv(t, argvFile)
		if argvValue(argv, "--model") != "sonnet" || argvValue(argv, "--effort") != "high" {
			t.Errorf("argv = %v, want the built-in strong entry", argv)
		}
	})
}

// A refused level costs the refusal and nothing else: no inputs pulled, no
// transcript pushed, no app started. The level is settled before the
// session writes or fetches anything.
func TestSession_RefusesEmbeddingsBeforeTouchingTheMachine(t *testing.T) {
	argvFile := levelApp(t)
	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.Level = "embeddings"
		s.Inputs = []string{"artifact-that-must-not-be-fetched"}
	})

	if !strings.Contains(end.GetError(), `level "embeddings" never runs through an app`) {
		t.Errorf("error = %q, want the D10 sentence", end.GetError())
	}
	if end.GetExitCode() != -1 {
		t.Errorf("exit_code = %d, want -1 for a session that never ran a process", end.GetExitCode())
	}
	if _, err := os.Stat(argvFile); !os.IsNotExist(err) {
		t.Error("the app ran for a level no app serves")
	}
	if seen := h.library.seenBearers(); len(seen) != 0 {
		t.Errorf("the Library was called %d times for a session refused at its level", len(seen))
	}
}

func TestSession_RefusesAnUnknownLevel(t *testing.T) {
	levelApp(t)
	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) { s.Level = "turbo" })

	for _, want := range []string{"claude-code", `"turbo"`, "fast, strong, reasoning, embeddings"} {
		if !strings.Contains(end.GetError(), want) {
			t.Errorf("error = %q, want it to name %s", end.GetError(), want)
		}
	}
}

// An owner's entry the app would misread refuses ITS level only; the
// app's other levels keep running.
func TestSession_AnEntryTheAppWouldMisreadRefusesOnlyItsLevel(t *testing.T) {
	levels := policyLevels(t, `apps:
  levels:
    claude-code:
      strong:
        effort: extreme
`)
	t.Run("the refused level", func(t *testing.T) {
		levelApp(t)
		h := newLevelRig(t, levels)
		end := h.start(t, func(s *memqlv1.AppSessionStart) { s.Level = "strong" })
		for _, want := range []string{"apps.levels.claude-code.strong", "low, medium, high, xhigh, max"} {
			if !strings.Contains(end.GetError(), want) {
				t.Errorf("error = %q, want it to name %q", end.GetError(), want)
			}
		}
	})
	t.Run("another level", func(t *testing.T) {
		argvFile := levelApp(t)
		h := newLevelRig(t, levels)
		end := h.start(t, func(s *memqlv1.AppSessionStart) { s.Level = "fast" })
		if end.GetError() != "" {
			t.Fatalf("error = %q; one bad entry refused a level it does not name", end.GetError())
		}
		if got := argvValue(readArgv(t, argvFile), "--model"); got != "haiku" {
			t.Errorf("--model = %q, want the built-in fast entry", got)
		}
	})
}

// The open kind hands the app to a PERSON, who picks their own model, so it
// reads no level at all -- not even to refuse one.
//
// The app IS on PATH, so resolveApp passes and the level is the next thing
// a session would check. With no display the open then fails on its own
// terms -- which is also what keeps this test from opening a real window
// for a person who is not here. Had the open read the level, "turbo" would
// have refused it first, and the error would name the level instead.
func TestSession_OpenReadsNoLevel(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the no-display refusal that makes this safe is Linux's; elsewhere an open would launch a terminal")
	}
	levelApp(t)
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.Kind = KindOpen
		s.Level = "turbo"
	})
	if !strings.Contains(end.GetError(), "display") || strings.Contains(end.GetError(), "level") {
		t.Errorf("error = %q; an open session fails on its own terms, never over a level it never uses",
			end.GetError())
	}
}

// An attach through the Codex MCP fallback cannot take a level (codex-reply
// declares no configuration), and the session refuses it at its level --
// before a bearer is written or an mcp-server is started -- rather than run
// at whatever settings the thread already has while its transcript claimed
// otherwise.
func TestSession_ACodexMCPAttachCannotTakeALevel(t *testing.T) {
	dir := t.TempDir()
	invocations := filepath.Join(dir, "invocations")
	// A Codex without the app-server: the probe exits non-zero, so this
	// machine drives it through codex-mcp.
	fakeApp(t, "codex", fmt.Sprintf(`
echo "$*" >> %q
if [ "$1" = "app-server" ] && [ "$2" = "--help" ]; then exit 9; fi
exit 9
`, invocations))

	h := newRig(t, apps.IDCodex)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-codex-attach-level"
		s.App = apps.IDCodex
		s.Kind = KindAttach
		s.AppSessionRef = "019bbb20-bff6-7130-83aa-bf45ab33250e"
		s.Level = "strong"
	})
	if !strings.Contains(end.GetError(), "codex-reply takes no configuration") {
		t.Errorf("error = %q, want the attach refused at its level", end.GetError())
	}
	for _, line := range readArgv(t, invocations) {
		if strings.HasPrefix(line, "mcp-server") {
			t.Errorf("an mcp-server was started for an attach refused at its level: %v", readArgv(t, invocations))
		}
	}
	if seen := h.library.seenBearers(); len(seen) != 0 {
		t.Errorf("the Library was called %d times for a session refused at its level", len(seen))
	}
}

// A refused level's built-in row must not survive in the table the harness
// is handed: the harness resolves the level again, and a table that still
// held the row would run the default the owner's broken entry was written
// to replace, the moment anything skipped the session's own check.
func TestSession_ARefusedLevelLeavesTheTable(t *testing.T) {
	s := &session{
		start: &memqlv1.AppSessionStart{Level: "fast"},
		manager: &Manager{opts: Options{Levels: func(string) (harness.Table, map[string]string) {
			return nil, map[string]string{"strong": "apps.levels.claude-code.strong: refused"}
		}}},
	}
	spec, _ := apps.SpecFor(apps.IDClaudeCode)
	plan, err := s.resolveLevel(spec)
	if err != nil {
		t.Fatalf("fast is not refused: %v", err)
	}
	if _, ok := plan.table["strong"]; ok {
		t.Errorf("the refused strong row is still in the table the harness would resolve through: %v", plan.table)
	}
	if plan.table["fast"].Model != "haiku" {
		t.Errorf("fast = %+v, want the built-in row", plan.table["fast"])
	}
}

// memql-cockpit#438's other acceptance: the End carries the model from
// modelUsage -- the app's report. The session asked for Opus; the app says
// Haiku spent the tokens; Haiku is what goes back. No effort goes back,
// because Claude Code states none.
func TestSession_EndCarriesTheModelTheAppReported(t *testing.T) {
	levelApp(t)
	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) { s.Level = "reasoning" })

	if end.GetModel() != "claude-haiku-4-5-20251001" {
		t.Errorf("model = %q, want the model the app said spent the tokens", end.GetModel())
	}
	if end.GetEffort() != "" {
		t.Errorf("effort = %q, want empty: xhigh was the request, and a request is not a report", end.GetEffort())
	}
}

func TestSession_EndCarriesNoModelWhenTheAppSaidNothing(t *testing.T) {
	fakeApp(t, "claude", `
echo '{"type":"system","subtype":"init","session_id":"app-quiet","model":"claude-opus-5"}'
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"app-quiet","total_cost_usd":0.02,"usage":{"input_tokens":1,"output_tokens":2},"result":"ok"}'
`)
	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) { s.Level = "reasoning" })

	if end.GetModel() != "" {
		t.Errorf("model = %q, want empty: the result named no model that ran", end.GetModel())
	}
}

// The End reports the model and effort of the last turn that reported a
// model, as a PAIR: a later turn that said nothing does not erase what an
// earlier one said, and a later model is never joined to an earlier
// effort.
func TestSession_TheLastTurnThatReportedAModelWins(t *testing.T) {
	s := &session{}
	s.recordTurn(harness.TurnResult{Model: "gpt-6-astra", Effort: "low"})
	s.recordTurn(harness.TurnResult{})
	if s.servedModel != "gpt-6-astra" || s.servedEffort != "low" {
		t.Errorf("after a silent turn: %q/%q, want the earlier report kept", s.servedModel, s.servedEffort)
	}
	s.recordTurn(harness.TurnResult{Model: "gpt-5.5"})
	if s.servedModel != "gpt-5.5" || s.servedEffort != "" {
		t.Errorf("after a turn that named only a model: %q/%q, want that turn's pair, effort unknown",
			s.servedModel, s.servedEffort)
	}
}

func TestSessionWireModelAndEffortReachTheApp(t *testing.T) {
	argvFile := levelApp(t)
	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) { s.Level = "reasoning"; s.Model = "sonnet"; s.Effort = "medium" })
	if end.GetError() != "" {
		t.Fatal(end.GetError())
	}
	argv := readArgv(t, argvFile)
	if argvValue(argv, "--model") != "sonnet" || argvValue(argv, "--effort") != "medium" {
		t.Fatalf("wire override lost: %v", argv)
	}
	if !strings.Contains(stderrChunks(h), "session's explicit override") {
		t.Fatal("transcript misattributes override to defaults")
	}
}

func TestSessionExplicitModelWithoutLevelIsReported(t *testing.T) {
	argvFile := levelApp(t)
	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) { s.Model = "sonnet" })
	if end.GetError() != "" {
		t.Fatal(end.GetError())
	}
	if argvValue(readArgv(t, argvFile), "--model") != "sonnet" || !strings.Contains(stderrChunks(h), "session's explicit override") {
		t.Fatal("model-only override was lost or not recorded")
	}
}
