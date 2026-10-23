package harness

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// claudelevels_test.go pins what a LEVEL does to a Claude Code turn: which
// knobs reach the argv (memql-cockpit#437) and what comes back as the model
// that served it (#438).
//
// The argv is asserted from the fake's own log for the reason the rest of
// this package does: a flag the client believes it passed and did not is
// invisible from this side of the fork, and the whole failure mode of a
// level is SILENT -- a turn run at the wrong model answers perfectly well.

// claudeLevelTurn is `claude -p --output-format stream-json --verbose
// --no-session-persistence --model haiku --effort low -- 'Reply with exactly
// the word: ok'`, run on 2026-09-13 against claude 2.1.270 and trimmed to
// the fields this client reads (the hook, thinking_tokens and rate_limit
// lines it also printed are dropped, and modelUsage keeps four of its keys).
//
// Two recorded facts drive the report. `modelUsage` is keyed by the model
// that SPENT the tokens, which is what makes it a report rather than a
// setting; and nothing anywhere in the stream states an effort, at the
// effort that was passed or any other.
const claudeLevelTurn = `{"type":"system","subtype":"init","cwd":"/w","session_id":"sess-270","model":"claude-haiku-4-5-20251001","permissionMode":"default","claude_code_version":"2.1.270"}
{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]},"parent_tool_use_id":null,"session_id":"sess-270"}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"session_id":"sess-270","total_cost_usd":0.0245737,"modelUsage":{"claude-haiku-4-5-20251001":{"inputTokens":909,"outputTokens":284,"costUSD":0.0245737,"canonicalModel":"claude-haiku-4-5"}},"result":"ok","usage":{"input_tokens":10,"output_tokens":273}}`

// claudeTwoModelTurn is the 2.1.270 result shape for a turn that spent on
// TWO models -- the session's own and the small one Claude Code uses for its
// housekeeping -- with the housekeeping model having produced MORE output.
// The session's own model is the answer anyway, because it is the one the
// init event says the session runs on.
const claudeTwoModelTurn = `{"type":"system","subtype":"init","session_id":"sess-two","model":"claude-sonnet-5"}
{"type":"result","subtype":"success","is_error":false,"num_turns":2,"session_id":"sess-two","total_cost_usd":0.2,"modelUsage":{"claude-haiku-4-5-20251001":{"outputTokens":900},"claude-sonnet-5":{"outputTokens":120}},"result":"done","usage":{"input_tokens":20,"output_tokens":1020}}`

// claudeNoInitTwoModelTurn is the same spend with no init line, where the
// model that produced the most output is the best statement there is.
const claudeNoInitTwoModelTurn = `{"type":"result","subtype":"success","is_error":false,"num_turns":2,"session_id":"sess-two","total_cost_usd":0.2,"modelUsage":{"claude-haiku-4-5-20251001":{"outputTokens":900},"claude-sonnet-5":{"outputTokens":120}},"result":"done","usage":{"input_tokens":20,"output_tokens":1020}}`

// claudeFailedAfterInit is the recorded failed resume (claudeFailedResult)
// with the init line the real run printed before it. The init names a model;
// the result spent nothing (`"modelUsage":{}`), so no model served.
const claudeFailedAfterInit = `{"type":"system","subtype":"init","session_id":"sess-gone","model":"claude-opus-5"}
` + claudeFailedResult

func TestClaudeHeadlessEveryLevelReachesTheArgv(t *testing.T) {
	for level, want := range BuiltinLevels(HarnessClaudeHeadless) {
		t.Run(level, func(t *testing.T) {
			bin, log := fakeClaude(t, prints(claudeLevelTurn))
			spec := claudeSpec(t, bin)
			spec.Level = level
			h := startClaude(t, spec)

			if _, err := h.Turn(context.Background(), "do the thing", &recorder{}); err != nil {
				t.Fatalf("turn: %v", err)
			}
			argv := recordedArgv(t, log)[0]
			if got := argValue(t, argv, "--model"); got != want.Model {
				t.Errorf("--model = %q, want %q", got, want.Model)
			}
			if want.Effort == "" {
				if hasArg(argv, "--effort") {
					t.Errorf("level %s passed --effort; Haiku takes none, and Claude Code drops it "+
						"without a word, so it would be a knob that is never turned: %v", level, argv)
				}
			} else if got := argValue(t, argv, "--effort"); got != want.Effort {
				t.Errorf("--effort = %q, want %q", got, want.Effort)
			}
			assertKnobsBeforeSeparator(t, argv)
		})
	}
}

func TestClaudeHeadlessNoLevelPassesNoKnobs(t *testing.T) {
	bin, log := fakeClaude(t, prints(claudeLevelTurn))
	h := startClaude(t, claudeSpec(t, bin))

	if _, err := h.Turn(context.Background(), "do the thing", &recorder{}); err != nil {
		t.Fatalf("turn: %v", err)
	}
	argv := recordedArgv(t, log)[0]
	if hasArg(argv, "--model") || hasArg(argv, "--effort") {
		t.Errorf("a session with no level must run the app at its own defaults, "+
			"which is what every session did before the field existed: %v", argv)
	}
}

func TestClaudeHeadlessOwnerTableWins(t *testing.T) {
	bin, log := fakeClaude(t, prints(claudeLevelTurn))
	spec := claudeSpec(t, bin)
	spec.Level = "reasoning"
	spec.Levels = MergeLevels(BuiltinLevels(HarnessClaudeHeadless),
		Table{"reasoning": {Model: "claude-fable-5-1", Effort: "max"}})
	h := startClaude(t, spec)

	if _, err := h.Turn(context.Background(), "do the thing", &recorder{}); err != nil {
		t.Fatalf("turn: %v", err)
	}
	argv := recordedArgv(t, log)[0]
	if got := argValue(t, argv, "--model"); got != "claude-fable-5-1" {
		t.Errorf("--model = %q, want the owner's entry", got)
	}
	if got := argValue(t, argv, "--effort"); got != "max" {
		t.Errorf("--effort = %q, want the owner's entry", got)
	}
}

// Every turn is a process, so every turn carries the knobs -- including the
// ones that --resume, which is how a follow-up stays at the session's level.
func TestClaudeHeadlessResumedTurnKeepsTheLevel(t *testing.T) {
	bin, log := fakeClaude(t, printsPerTurn(claudeLevelTurn, claudeResumedTurn))
	spec := claudeSpec(t, bin)
	spec.Level = "strong"
	h := startClaude(t, spec)

	for _, prompt := range []string{"first", "second"} {
		if _, err := h.Turn(context.Background(), prompt, &recorder{}); err != nil {
			t.Fatalf("turn %q: %v", prompt, err)
		}
	}
	turns := recordedArgv(t, log)
	if len(turns) != 2 || !hasArg(turns[1], "--resume") {
		t.Fatalf("want a second turn that resumes, got %v", turns)
	}
	if got := argValue(t, turns[1], "--model"); got != "sonnet" {
		t.Errorf("the resumed turn's --model = %q, want sonnet", got)
	}
}

func TestClaudeHeadlessRefusesAnUnknownLevelBeforeForking(t *testing.T) {
	bin, log := fakeClaude(t, prints(claudeLevelTurn))
	spec := claudeSpec(t, bin)
	spec.Level = "turbo"

	h := &claudeHeadless{}
	err := h.Start(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "fast, strong, reasoning, embeddings") {
		t.Fatalf("Start = %v, want a refusal naming the four levels", err)
	}
	_ = h.Close()
	if _, statErr := os.Stat(log); !os.IsNotExist(statErr) {
		t.Errorf("the app ran for a level nobody can translate (%v)", statErr)
	}
}

func TestClaudeHeadlessRefusesEmbeddings(t *testing.T) {
	bin, _ := fakeClaude(t, prints(claudeLevelTurn))
	spec := claudeSpec(t, bin)
	spec.Level = "embeddings"

	h := &claudeHeadless{}
	if err := h.Start(context.Background(), spec); !errors.Is(err, ErrEmbeddingsLevel) {
		t.Fatalf("Start = %v, want ErrEmbeddingsLevel", err)
	}
	_ = h.Close()
}

func TestClaudeHeadlessRefusesAnEffortItWouldIgnore(t *testing.T) {
	bin, _ := fakeClaude(t, prints(claudeLevelTurn))
	spec := claudeSpec(t, bin)
	spec.Level = "strong"
	spec.Levels = Table{"strong": {Model: "sonnet", Effort: "extreme"}}

	h := &claudeHeadless{}
	err := h.Start(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "low, medium, high, xhigh, max") {
		t.Fatalf("Start = %v, want a refusal naming the efforts Claude Code takes", err)
	}
	_ = h.Close()
}

// THE REPORT, NOT THE REQUEST. The turn below asked for Opus at xhigh; the
// app says Haiku spent the tokens, and Haiku is what goes back. The effort
// stays empty because Claude Code states none -- reporting xhigh would be
// the request copied into the record as a measurement.
func TestClaudeHeadlessReportsTheModelFromModelUsage(t *testing.T) {
	bin, _ := fakeClaude(t, prints(claudeLevelTurn))
	spec := claudeSpec(t, bin)
	spec.Level = "reasoning"
	h := startClaude(t, spec)

	res, err := h.Turn(context.Background(), "do the thing", &recorder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("Model = %q, want the model the app said spent the tokens", res.Model)
	}
	if res.Effort != "" {
		t.Errorf("Effort = %q, want empty: Claude Code states no effort, so any value here is a guess", res.Effort)
	}
}

func TestClaudeHeadlessPrefersTheSessionsOwnModel(t *testing.T) {
	bin, _ := fakeClaude(t, prints(claudeTwoModelTurn))
	h := startClaude(t, claudeSpec(t, bin))

	res, err := h.Turn(context.Background(), "do the thing", &recorder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Model != "claude-sonnet-5" {
		t.Errorf("Model = %q, want the session's own model (the init event's), not the "+
			"housekeeping model that happened to print more", res.Model)
	}
}

func TestClaudeHeadlessMostOutputWinsWithoutAnInitModel(t *testing.T) {
	bin, _ := fakeClaude(t, prints(claudeNoInitTwoModelTurn))
	h := startClaude(t, claudeSpec(t, bin))

	res, err := h.Turn(context.Background(), "do the thing", &recorder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("Model = %q, want the model that produced the most output", res.Model)
	}
}

// A result that spent nothing names no model, whatever the init event said
// the session was configured to run.
func TestClaudeHeadlessReportsNoModelWhenNothingSpent(t *testing.T) {
	bin, _ := fakeClaude(t, prints(claudeFailedAfterInit))
	h := startClaude(t, claudeSpec(t, bin))

	res, err := h.Turn(context.Background(), "resume something gone", &recorder{})
	if err == nil {
		t.Fatal("the recorded failure reported success")
	}
	if res.Model != "" {
		t.Errorf("Model = %q, want empty: `modelUsage` is {} because no model ran", res.Model)
	}
}

// The 2.1.263 recordings were trimmed of `modelUsage` (their client did not
// read it). A result without one is no report, and the init line alone is a
// setting rather than a statement that anything ran on it.
func TestClaudeHeadlessResultWithoutModelUsageReportsNone(t *testing.T) {
	bin, _ := fakeClaude(t, prints(claudePongTurn))
	h := startClaude(t, claudeSpec(t, bin))

	res, err := h.Turn(context.Background(), "say pong", &recorder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Model != "" {
		t.Errorf("Model = %q, want empty with no modelUsage to read it from", res.Model)
	}
}

// A model named with a context suffix (`sonnet[1m]`, which CheckKnobs
// accepts) may come back from the init event with the suffix while
// modelUsage keys the bare id. The session's own model must still win over
// a housekeeping model that printed more.
func TestClaudeHeadlessMatchesTheSessionModelAcrossAContextSuffix(t *testing.T) {
	turn := `{"type":"system","subtype":"init","session_id":"sess-1m","model":"claude-sonnet-5[1m]"}
{"type":"result","subtype":"success","is_error":false,"session_id":"sess-1m","total_cost_usd":0.2,"modelUsage":{"claude-haiku-4-5-20251001":{"outputTokens":900},"claude-sonnet-5":{"outputTokens":120}},"result":"done","usage":{"input_tokens":20,"output_tokens":1020}}`
	bin, _ := fakeClaude(t, prints(turn))
	h := startClaude(t, claudeSpec(t, bin))

	res, err := h.Turn(context.Background(), "do the thing", &recorder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Model != "claude-sonnet-5" {
		t.Errorf("Model = %q, want the session's own model as modelUsage names it", res.Model)
	}
}

// THE ANSWER RIDES A FAILURE TOO, which is the proto's own rule for
// result_json: Claude Code can produce the structured answer and still exit
// non-zero, and dropping the answer because the run failed would lose the
// one part of it that can be read.
func TestClaudeHeadlessStructuredAnswerSurvivesAFailedExit(t *testing.T) {
	bin, _ := fakeClaude(t, prints(claudeSchemaTurn)+"exit 2\n")
	spec := claudeSpec(t, bin)
	spec.ResponseSchema = `{"type":"object","properties":{"city":{"type":"string"}}}`
	h := startClaude(t, spec)

	res, err := h.Turn(context.Background(), "capital of France", &recorder{})
	if err == nil {
		t.Fatal("a turn whose process exited 2 reported success")
	}
	if res.ExitCode != 2 {
		t.Errorf("ExitCode = %d, want the real 2", res.ExitCode)
	}
	if string(res.ResultJSON) != `{"city":"Paris","millions":2.2}` {
		t.Errorf("ResultJSON = %s, want the structured answer beside the failure", res.ResultJSON)
	}
}

// The envelope stays as tolerant as it was: a line whose top-level `model`
// is not a string (a shape a later Claude Code could emit) is still an
// event, not unrecognised narration, and its session id is still kept.
func TestClaudeHeadlessAnUnexpectedModelShapeStaysAnEvent(t *testing.T) {
	events := `{"type":"stream_event","model":{"id":"claude-sonnet-5"},"session_id":"sess-odd-model"}
{"type":"result","subtype":"success","is_error":false,"total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2},"result":"fine"}`
	bin, _ := fakeClaude(t, prints(events))
	h := startClaude(t, claudeSpec(t, bin))

	rec := &recorder{}
	res, err := h.Turn(context.Background(), "hello", rec)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if got := rec.joined(StreamStdout); got != "" {
		t.Errorf("StreamStdout = %q, want nothing: that line was stream-json", got)
	}
	if res.AppSessionRef != "sess-odd-model" {
		t.Errorf("AppSessionRef = %q, want the id that line carried", res.AppSessionRef)
	}
}

// assertKnobsBeforeSeparator pins the argv rule the knobs inherit from
// claudeArgv: every flag goes before `--`, which is what stops the variadic
// --mcp-config from swallowing whatever follows it.
func assertKnobsBeforeSeparator(t *testing.T, argv []string) {
	t.Helper()
	sep := -1
	for i, a := range argv {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		t.Fatalf("no -- separator in %v", argv)
	}
	for _, a := range argv[sep+1:] {
		if a == "--model" || a == "--effort" {
			t.Fatalf("a knob landed after the separator, where it is a prompt: %v", argv)
		}
	}
}
