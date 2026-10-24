package harness

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// codexlevels_test.go pins what a LEVEL does to a Codex session on both of
// its harnesses (memql-cockpit#437) and what comes back as the model and
// effort that served it (#438).
//
// THE SPELLINGS ARE VERIFIED, NOT ASSUMED. On 2026-09-13, against the
// installed codex-cli 0.153.4:
//
//   - `codex app-server generate-ts` gives ThreadStartParams and
//     ThreadResumeParams a typed `model` and an open `config` map, and
//     ThreadStartResponse / ThreadResumeResponse a `model` and a
//     `reasoningEffort` -- the app's statement of what the thread runs at.
//   - a live thread/start with `config:{"model_reasoning_effort":"low"}`
//     answered `"model":"gpt-6-astra","reasoningEffort":"low"`, and repeated
//     both on thread/started; nothing later in the turn restated either.
//   - the mcp-server's `codex` tool declares `model` and an open `config`
//     ("Individual config settings that will override what is in
//     CODEX_HOME/config.toml"); `codex-reply` declares only prompt and
//     threadId.
//   - a live `codex` call with `config:{"model_reasoning_effort":"low"}`
//     emitted `session_configured` with `"model":"gpt-6-astra"` and
//     `"reasoning_effort":"low"` -- the only place in that stream either
//     word appears.
//
// So model_reasoning_effort is the configuration key on both harnesses, and
// the report is the app's own statement on each.

// codexTurnRerouted is a successful turn during which the app moved to
// another model -- ModelReroutedNotification (0.153.4:
// `{threadId, turnId, fromModel, toModel, reason}`, reason
// highRiskCyberActivity). From that notification on, what serves is the
// model it moved to, whatever the thread was configured with.
const codexTurnRerouted = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"inProgress","error":null,"startedAt":1,"completedAt":null,"durationMs":null}}}\n' "$id" "$turn"
printf '{"jsonrpc":"2.0","method":"model/rerouted","params":{"threadId":"THREAD","turnId":"turn_%s","fromModel":"gpt-6-astra","toModel":"gpt-5.5","reason":"highRiskCyberActivity"}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_%s","completedAtMs":1500,"item":{"type":"agentMessage","id":"item_msg","text":"done","phase":"final_answer","memoryCitation":null,"delivery":null,"questions":null}}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"THREAD","turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"completed","error":null,"startedAt":1,"completedAt":2,"durationMs":1000}}}\n' "$turn"
`

// codexTurnFailsAfterSpending is a turn that reported spend and then
// failed. The spend is the proof the model ran, so the thread's model is
// what served it even though the turn did not complete.
const codexTurnFailsAfterSpending = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"inProgress","error":null,"startedAt":1,"completedAt":null,"durationMs":null}}}\n' "$id" "$turn"
printf '{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"THREAD","turnId":"turn_%s","tokenUsage":{"total":{"totalTokens":150,"inputTokens":100,"cachedInputTokens":0,"cacheWriteInputTokens":0,"outputTokens":50,"reasoningOutputTokens":0},"last":{"totalTokens":150,"inputTokens":100,"cachedInputTokens":0,"cacheWriteInputTokens":0,"outputTokens":50,"reasoningOutputTokens":0},"modelContextWindow":272000}}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"THREAD","turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"failed","error":{"message":"the stream was cut","codexErrorInfo":null,"additionalDetails":null,"misalignment":null},"startedAt":1,"completedAt":2,"durationMs":1000}}}\n' "$turn"
`

// codexStatedSettings is a thread answer that states a model AND an effort,
// the way 0.153.4 answered a thread/start configured at low.
const codexStatedSettings = `"model":"gpt-6-astra","modelProvider":"openai","serviceTier":null,"reasoningEffort":"low"`

// --- the app-server -------------------------------------------------

func TestCodexAppServerEveryLevelReachesThreadStart(t *testing.T) {
	for level, want := range BuiltinLevels(HarnessCodexAppServer) {
		t.Run(level, func(t *testing.T) {
			bin, log := fakeCodexAppServer(t, codexTurnOK)
			spec := codexSpec(t, bin)
			spec.Level = level
			h := startCodexAppServer(t, spec)

			if _, err := h.Turn(context.Background(), "do the thing", &recorder{}); err != nil {
				t.Fatalf("turn: %v", err)
			}
			starts := codexCalls(t, log, codexMethodThreadStart)
			if len(starts) != 1 {
				t.Fatalf("thread/start calls = %d, want 1", len(starts))
			}
			params := starts[0]
			if got := codexConfigEffort(params); got != want.Effort {
				t.Errorf("config.model_reasoning_effort = %q, want %q", got, want.Effort)
			}
			if _, ok := params["model"]; ok {
				t.Errorf("the built-in Codex table names no model, but thread/start carried one: %v", params)
			}
			// The level adds to the thread's settings; it must not displace
			// the two the harness already depends on.
			if params["approvalPolicy"] != "never" || params["cwd"] != spec.Workspace {
				t.Errorf("the level displaced approvalPolicy or cwd: %v", params)
			}
			// The effort rides the THREAD, where the app states it back,
			// never the turn -- see applyKnobs.
			for _, turn := range codexCalls(t, log, codexMethodTurnStart) {
				if _, ok := turn["effort"]; ok {
					t.Errorf("turn/start carried an effort: %v", turn)
				}
			}
		})
	}
}

func TestCodexAppServerOwnerModelReachesThreadStart(t *testing.T) {
	bin, log := fakeCodexAppServer(t, codexTurnOK)
	spec := codexSpec(t, bin)
	spec.Level = "strong"
	spec.Levels = Table{"strong": {Model: "gpt-5.5", Effort: "xhigh"}}
	h := startCodexAppServer(t, spec)

	if _, err := h.Turn(context.Background(), "do the thing", &recorder{}); err != nil {
		t.Fatalf("turn: %v", err)
	}
	params := codexCalls(t, log, codexMethodThreadStart)[0]
	if params["model"] != "gpt-5.5" {
		t.Errorf("model = %v, want the owner's gpt-5.5", params["model"])
	}
	if got := codexConfigEffort(params); got != "xhigh" {
		t.Errorf("config.model_reasoning_effort = %q, want xhigh", got)
	}
}

func TestCodexAppServerSessionOverrideReachesProtocol(t *testing.T) {
	for _, resume := range []string{"", codexFakeResumed} {
		t.Run("resume="+resume, func(t *testing.T) {
			bin, log := fakeCodexAppServer(t, codexTurnOK)
			spec := codexSpec(t, bin)
			spec.Level = "reasoning"
			spec.Model = "gpt-5.6-terra"
			spec.Effort = "medium"
			spec.ResumeRef = resume
			h := startCodexAppServer(t, spec)
			if _, err := h.Turn(context.Background(), "one bounded research answer", &recorder{}); err != nil {
				t.Fatal(err)
			}
			method := codexMethodThreadStart
			if resume != "" {
				method = codexMethodThreadResume
			}
			params := codexCalls(t, log, method)[0]
			if params["model"] != spec.Model || codexConfigEffort(params) != spec.Effort {
				t.Fatalf("explicit override lost at app protocol: %v", params)
			}
		})
	}
}

// An attach resumes a thread, and ThreadResumeParams takes the same
// overrides -- so a resumed session runs at its level too.
func TestCodexAppServerLevelReachesThreadResume(t *testing.T) {
	bin, log := fakeCodexAppServer(t, codexTurnOK)
	spec := codexSpec(t, bin)
	spec.ResumeRef = codexFakeResumed
	spec.Level = "reasoning"
	h := startCodexAppServer(t, spec)

	if _, err := h.Turn(context.Background(), "carry on", &recorder{}); err != nil {
		t.Fatalf("turn: %v", err)
	}
	resumes := codexCalls(t, log, codexMethodThreadResume)
	if len(resumes) != 1 {
		t.Fatalf("thread/resume calls = %d, want 1", len(resumes))
	}
	if got := codexConfigEffort(resumes[0]); got != "high" {
		t.Errorf("thread/resume config.model_reasoning_effort = %q, want high", got)
	}
	if resumes[0]["threadId"] != codexFakeResumed {
		t.Errorf("the level displaced the thread being resumed: %v", resumes[0])
	}
}

func TestCodexAppServerNoLevelSendsNoKnobs(t *testing.T) {
	bin, log := fakeCodexAppServer(t, codexTurnOK)
	h := startCodexAppServer(t, codexSpec(t, bin))

	if _, err := h.Turn(context.Background(), "do the thing", &recorder{}); err != nil {
		t.Fatalf("turn: %v", err)
	}
	params := codexCalls(t, log, codexMethodThreadStart)[0]
	for _, key := range []string{"model", "config"} {
		if _, ok := params[key]; ok {
			t.Errorf("a session with no level sent %q; the app's own settings must stand: %v", key, params)
		}
	}
}

func TestCodexAppServerRefusesAnUnknownLevelBeforeLaunching(t *testing.T) {
	for name, spec := range map[string]func(Spec) Spec{
		"an undefined word": func(s Spec) Spec { s.Level = "turbo"; return s },
		"embeddings":        func(s Spec) Spec { s.Level = "embeddings"; return s },
		"an effort that is not a word": func(s Spec) Spec {
			s.Level = "fast"
			s.Levels = Table{"fast": {Effort: "High!"}}
			return s
		},
	} {
		t.Run(name, func(t *testing.T) {
			bin, log := fakeCodexAppServer(t, codexTurnOK)
			h := &codexAppServer{}
			err := h.Start(context.Background(), spec(codexSpec(t, bin)))
			if err == nil {
				t.Fatal("Start accepted a level it cannot run at")
			}
			_ = h.Close()
			if _, statErr := os.Stat(log); !os.IsNotExist(statErr) {
				t.Errorf("an app-server was launched for a session that could never run (%v)", statErr)
			}
		})
	}
}

// THE REPORT IS THE APP'S STATEMENT. The fake answers thread/start with
// the settings the app says the thread runs at, and those -- not what this
// client sent -- are what a completed turn reports.
func TestCodexAppServerReportsWhatTheThreadStates(t *testing.T) {
	bin, _ := fakeCodexAppServerSettings(t, codexStatedSettings, codexTurnOK)
	spec := codexSpec(t, bin)
	spec.Level = "reasoning" // asks for high; the app states low
	h := startCodexAppServer(t, spec)

	res, err := h.Turn(context.Background(), "do the thing", &recorder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Model != "gpt-6-astra" || res.Effort != "low" {
		t.Errorf("Model/Effort = %q/%q, want the app's own statement gpt-6-astra/low, "+
			"never the request", res.Model, res.Effort)
	}
}

func TestCodexAppServerRerouteReplacesTheModel(t *testing.T) {
	bin, _ := fakeCodexAppServerSettings(t, codexStatedSettings, codexTurnRerouted)
	h := startCodexAppServer(t, codexSpec(t, bin))

	rec := &recorder{}
	res, err := h.Turn(context.Background(), "do the thing", rec)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Model != "gpt-5.5" {
		t.Errorf("Model = %q, want the model the app rerouted the turn to", res.Model)
	}
	if !strings.Contains(rec.joined(StreamEvent), `"model/rerouted"`) {
		t.Error("the reroute must still reach the transcript as an event")
	}
}

// A turn that failed before the model answered names no model: the thread's
// settings are what it was configured with, and a configured model that
// never ran -- an unknown name among them -- did not serve anything.
func TestCodexAppServerFailedTurnReportsNoModel(t *testing.T) {
	bin, _ := fakeCodexAppServerSettings(t, codexStatedSettings, codexTurnFails)
	h := startCodexAppServer(t, codexSpec(t, bin))

	res, err := h.Turn(context.Background(), "do the thing", &recorder{})
	if err == nil {
		t.Fatal("a failed turn reported success")
	}
	if res.Model != "" || res.Effort != "" {
		t.Errorf("Model/Effort = %q/%q, want nothing for a turn that never showed the model ran",
			res.Model, res.Effort)
	}
}

func TestCodexAppServerFailedTurnThatSpentReportsTheModel(t *testing.T) {
	bin, _ := fakeCodexAppServerSettings(t, codexStatedSettings, codexTurnFailsAfterSpending)
	h := startCodexAppServer(t, codexSpec(t, bin))

	res, err := h.Turn(context.Background(), "do the thing", &recorder{})
	if err == nil {
		t.Fatal("a failed turn reported success")
	}
	if res.Model != "gpt-6-astra" || res.Effort != "low" {
		t.Errorf("Model/Effort = %q/%q, want the thread's statement: the turn spent tokens, so the model ran",
			res.Model, res.Effort)
	}
}

func TestCodexAppServerNullEffortIsEmpty(t *testing.T) {
	bin, _ := fakeCodexAppServerSettings(t,
		`"model":"gpt-6-astra","modelProvider":"openai","serviceTier":null,"reasoningEffort":null`, codexTurnOK)
	h := startCodexAppServer(t, codexSpec(t, bin))

	res, err := h.Turn(context.Background(), "do the thing", &recorder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Model != "gpt-6-astra" || res.Effort != "" {
		t.Errorf("Model/Effort = %q/%q, want gpt-6-astra and no effort: null is the app saying nothing",
			res.Model, res.Effort)
	}
}

// --- the mcp-server fallback ----------------------------------------

// codexMCPSessionConfigured is the session_configured event codex-cli
// 0.153.4's mcp-server emitted on 2026-09-13 for a `codex` call configured
// at low, trimmed of the permission profile and the rollout path. It is the
// only line in that stream that states a model or an effort.
const codexMCPSessionConfigured = `
printf '{"jsonrpc":"2.0","method":"codex/event","params":{"_meta":{"requestId":1,"threadId":"THREAD"},"msg":{"type":"session_configured","session_id":"THREAD","thread_id":"THREAD","model":"gpt-6-astra","model_provider_id":"openai","service_tier":"default","approval_policy":"never","reasoning_effort":"low"},"id":""}}\n'
`

// codexMCPConfiguredOnFirstCall states the session's settings on the `codex`
// call only, the way a session is configured once: a codex-reply continues
// the thread and restates nothing.
const codexMCPConfiguredOnFirstCall = `
case "$line" in
  *'"name":"codex-reply"'*) ;;
  *)` + codexMCPSessionConfigured + `  ;;
esac
` + codexMCPToolOK

func TestCodexMCPEveryLevelReachesTheCodexTool(t *testing.T) {
	for level, want := range BuiltinLevels(HarnessCodexMCP) {
		t.Run(level, func(t *testing.T) {
			bin, log := fakeCodexMCP(t, codexMCPToolOK)
			spec := codexSpec(t, bin)
			spec.Level = level
			h := startCodexMCP(t, spec)

			if _, err := h.Turn(context.Background(), "do the thing", &recorder{}); err != nil {
				t.Fatalf("turn: %v", err)
			}
			calls := codexToolCalls(t, log)
			if len(calls) != 1 || calls[0].Name != mcpToolCodex {
				t.Fatalf("want one codex call, got %+v", calls)
			}
			if got := codexConfigEffort(calls[0].Arguments); got != want.Effort {
				t.Errorf("config.model_reasoning_effort = %q, want %q", got, want.Effort)
			}
			if _, ok := calls[0].Arguments["model"]; ok {
				t.Errorf("the built-in Codex table names no model, but the codex tool got one: %v", calls[0].Arguments)
			}
		})
	}
}

func TestCodexMCPOwnerModelReachesTheCodexTool(t *testing.T) {
	bin, log := fakeCodexMCP(t, codexMCPToolOK)
	spec := codexSpec(t, bin)
	spec.Level = "reasoning"
	spec.Levels = Table{"reasoning": {Model: "gpt-5.5", Effort: "xhigh"}}
	h := startCodexMCP(t, spec)

	if _, err := h.Turn(context.Background(), "do the thing", &recorder{}); err != nil {
		t.Fatalf("turn: %v", err)
	}
	args := codexToolCalls(t, log)[0].Arguments
	if args["model"] != "gpt-5.5" || codexConfigEffort(args) != "xhigh" {
		t.Errorf("codex tool arguments = %v, want the owner's model and effort", args)
	}
}

// codex-reply declares prompt and threadId and nothing else, so a
// continuation carries no knobs -- and needs none, since the thread keeps
// the settings its first call gave it.
func TestCodexMCPReplyCarriesNoKnobs(t *testing.T) {
	bin, log := fakeCodexMCP(t, codexMCPToolOK)
	spec := codexSpec(t, bin)
	spec.Level = "strong"
	h := startCodexMCP(t, spec)

	for _, prompt := range []string{"first", "second"} {
		if _, err := h.Turn(context.Background(), prompt, &recorder{}); err != nil {
			t.Fatalf("turn %q: %v", prompt, err)
		}
	}
	calls := codexToolCalls(t, log)
	if len(calls) != 2 || calls[1].Name != mcpToolCodexReply {
		t.Fatalf("want a codex call then a codex-reply, got %+v", calls)
	}
	for key := range calls[1].Arguments {
		if key != "threadId" && key != "prompt" {
			t.Errorf("codex-reply was sent %q, which it does not declare: %v", key, calls[1].Arguments)
		}
	}
}

func TestCodexMCPReportsSessionConfigured(t *testing.T) {
	bin, _ := fakeCodexMCP(t, codexMCPConfiguredOnFirstCall)
	spec := codexSpec(t, bin)
	spec.Level = "reasoning" // asks for high; the app states low
	h := startCodexMCP(t, spec)

	rec := &recorder{}
	res, err := h.Turn(context.Background(), "do the thing", rec)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Model != "gpt-6-astra" || res.Effort != "low" {
		t.Errorf("Model/Effort = %q/%q, want the app's session_configured statement", res.Model, res.Effort)
	}
	if !strings.Contains(rec.joined(StreamEvent), "session_configured") {
		t.Error("session_configured must still reach the transcript as an event")
	}

	// The statement outlives the call that carried it: codex-reply does
	// not restate it, and the thread still runs at it.
	again, err := h.Turn(context.Background(), "and again", &recorder{})
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if again.Model != "gpt-6-astra" || again.Effort != "low" {
		t.Errorf("second turn Model/Effort = %q/%q, want the thread's settings carried", again.Model, again.Effort)
	}
}

// A call that came back flagged isError did not show the model answered, so
// it names no model even though the session was configured with one.
func TestCodexMCPToolErrorReportsNoModel(t *testing.T) {
	bin, _ := fakeCodexMCP(t, codexMCPSessionConfigured+codexMCPToolError)
	h := startCodexMCP(t, codexSpec(t, bin))

	res, err := h.Turn(context.Background(), "do the thing", &recorder{})
	if err == nil {
		t.Fatal("an isError result reported success")
	}
	if res.Model != "" || res.Effort != "" {
		t.Errorf("Model/Effort = %q/%q, want nothing for a turn that failed", res.Model, res.Effort)
	}
}

func TestCodexMCPWithoutSessionConfiguredReportsNothing(t *testing.T) {
	bin, _ := fakeCodexMCP(t, codexMCPToolOK)
	h := startCodexMCP(t, codexSpec(t, bin))

	res, err := h.Turn(context.Background(), "do the thing", &recorder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Model != "" || res.Effort != "" {
		t.Errorf("Model/Effort = %q/%q, want nothing: the app stated neither", res.Model, res.Effort)
	}
}

// AN ATTACH CANNOT TAKE A LEVEL ON THIS PROTOCOL, so it is refused rather
// than run at settings nobody chose. The first call of an attach is a
// codex-reply -- the thread already exists -- and codex-reply declares no
// configuration, so the level's knobs would never be sent while the session
// told its transcript they had been. With no level, an attach is fine.
func TestCodexMCPRefusesALevelOnAnAttach(t *testing.T) {
	bin, log := fakeCodexMCP(t, codexMCPToolOK)
	spec := codexSpec(t, bin)
	spec.ResumeRef = codexMCPThread
	spec.Level = "strong"

	h := &codexMCP{}
	err := h.Start(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "codex-reply takes no configuration") {
		t.Fatalf("Start = %v, want a refusal that names why the level cannot reach the app", err)
	}
	_ = h.Close()
	if _, statErr := os.Stat(log); !os.IsNotExist(statErr) {
		t.Errorf("an mcp-server was launched for an attach that could never run at its level (%v)", statErr)
	}

	spec.Level = ""
	resumed := startCodexMCP(t, spec)
	if _, err := resumed.Turn(context.Background(), "carry on", &recorder{}); err != nil {
		t.Fatalf("an attach with no level must still run: %v", err)
	}
	if calls := codexToolCalls(t, log); len(calls) != 1 || calls[0].Name != mcpToolCodexReply {
		t.Errorf("want the attach to continue with codex-reply, got %+v", calls)
	}
}

// The other two harnesses DO apply a level to a session they resume --
// Claude Code passes the knobs to every process, --resume included, and the
// app-server's thread/resume takes the same overrides as thread/start -- so
// the refusal above is the fallback's alone.
func TestCheckResume(t *testing.T) {
	knobs := Knobs{Effort: "high"}
	if err := CheckResume(HarnessClaudeHeadless, knobs); err != nil {
		t.Errorf("claude-headless resume: %v", err)
	}
	if err := CheckResume(HarnessCodexAppServer, knobs); err != nil {
		t.Errorf("codex-app-server resume: %v", err)
	}
	if err := CheckResume(HarnessCodexMCP, knobs); err == nil {
		t.Error("codex-mcp resume with knobs must refuse")
	}
	if err := CheckResume(HarnessCodexMCP, Knobs{}); err != nil {
		t.Errorf("codex-mcp resume with no knobs must not refuse: %v", err)
	}
}

func TestCodexMCPRefusesAnUnknownLevelBeforeLaunching(t *testing.T) {
	bin, log := fakeCodexMCP(t, codexMCPToolOK)
	spec := codexSpec(t, bin)
	spec.Level = "turbo"

	h := &codexMCP{}
	if err := h.Start(context.Background(), spec); err == nil {
		t.Fatal("Start accepted a level it cannot run at")
	}
	_ = h.Close()
	if _, statErr := os.Stat(log); !os.IsNotExist(statErr) {
		t.Errorf("an mcp-server was launched for a session that could never run (%v)", statErr)
	}
}

// A REROUTE BELONGS TO THE TURN IT NAMES. ModelReroutedNotification carries
// a turnId and its one reason (highRiskCyberActivity) is about a request,
// so a later turn that was not rerouted ran on the thread's own model again
// and must say so.
func TestCodexAppServerRerouteStaysWithItsTurn(t *testing.T) {
	body := `
if [ "$turn" = "1" ]; then
printf '{"jsonrpc":"2.0","method":"model/rerouted","params":{"threadId":"THREAD","turnId":"turn_%s","fromModel":"gpt-6-astra","toModel":"gpt-5.5","reason":"highRiskCyberActivity"}}\n' "$turn"
fi
` + codexTurnOK
	bin, _ := fakeCodexAppServerSettings(t, codexStatedSettings, body)
	h := startCodexAppServer(t, codexSpec(t, bin))

	first, err := h.Turn(context.Background(), "first", &recorder{})
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	second, err := h.Turn(context.Background(), "second", &recorder{})
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if first.Model != "gpt-5.5" {
		t.Errorf("first turn Model = %q, want the model it was rerouted to", first.Model)
	}
	if second.Model != "gpt-6-astra" {
		t.Errorf("second turn Model = %q, want the thread's own model: it was not rerouted", second.Model)
	}
}

// thread/settings/updated is the app restating the thread's settings
// (ThreadSettings.model and .effort, 0.153.4) after they change, so a turn
// after it reports the new ones.
func TestCodexAppServerSettingsUpdateIsTheNewStatement(t *testing.T) {
	body := `
printf '{"jsonrpc":"2.0","method":"thread/settings/updated","params":{"threadId":"THREAD","threadSettings":{"cwd":"/w","model":"gpt-5.6-sol","modelProvider":"openai","serviceTier":null,"effort":"xhigh","summary":null}}}\n'
` + codexTurnOK
	bin, _ := fakeCodexAppServerSettings(t, codexStatedSettings, body)
	h := startCodexAppServer(t, codexSpec(t, bin))

	res, err := h.Turn(context.Background(), "do the thing", &recorder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Model != "gpt-5.6-sol" || res.Effort != "xhigh" {
		t.Errorf("Model/Effort = %q/%q, want the settings the app restated", res.Model, res.Effort)
	}
}

// The answer rides a failure too, as for Claude Code: a turn Codex marked
// failed after its final answer still hands that answer back beside the
// error.
func TestCodexAppServerStructuredAnswerSurvivesAFailedTurn(t *testing.T) {
	body := `
printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"inProgress","error":null,"startedAt":1,"completedAt":null,"durationMs":null}}}\n' "$id" "$turn"
printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_%s","completedAtMs":1500,"item":{"type":"agentMessage","id":"item_msg","text":"{\\"answer\\":\\"42\\"}","phase":"final_answer","memoryCitation":null,"delivery":null,"questions":null}}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"THREAD","turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"failed","error":{"message":"the stream was cut","codexErrorInfo":null,"additionalDetails":null,"misalignment":null},"startedAt":1,"completedAt":2,"durationMs":1000}}}\n' "$turn"
`
	bin, _ := fakeCodexAppServer(t, body)
	spec := codexSpec(t, bin)
	spec.ResponseSchema = `{"type":"object","properties":{"answer":{"type":"string"}}}`
	h := startCodexAppServer(t, spec)

	res, err := h.Turn(context.Background(), "the question", &recorder{})
	if err == nil {
		t.Fatal("a failed turn reported success")
	}
	if string(res.ResultJSON) != `{"answer":"42"}` {
		t.Errorf("ResultJSON = %s, want the final answer beside the failure", res.ResultJSON)
	}
}

// THE REAL APP-SERVER SENDS NO `jsonrpc` HEADER. codex-cli 0.153.4 omits
// "jsonrpc":"2.0" on every frame -- its README says so, and a live
// initialize answered `{"id":1,"result":{...}}` -- while the mcp-server
// sends it. A client that required the header dropped the initialize
// answer as stdout, and every app-server session hung at Start until its
// deadline; the fakes all sent the header, so nothing here noticed. This
// test runs the whole session -- handshake, thread, a turn at a level, the
// report -- against frames with the header stripped, under a deadline so a
// regression fails fast instead of hanging.
func TestCodexAppServerSpeaksToFramesWithoutTheHeader(t *testing.T) {
	bin, log := fakeCodexAppServerSettings(t, codexStatedSettings, codexTurnOK)
	stripJSONRPCHeader(t, bin)
	spec := codexSpec(t, bin)
	spec.Level = "strong"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := &codexAppServer{}
	if err := h.Start(ctx, spec); err != nil {
		t.Fatalf("Start against a header-less app-server: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	rec := &recorder{}
	res, err := h.Turn(ctx, "do the thing", rec)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Text != "the answer is 42" || res.Model != "gpt-6-astra" || res.Effort != "low" {
		t.Errorf("Text/Model/Effort = %q/%q/%q, want the turn's answer and the thread's statement",
			res.Text, res.Model, res.Effort)
	}
	if got := rec.joined(StreamStdout); got != "" {
		t.Errorf("StreamStdout = %q; header-less protocol frames were misfiled as narration", got)
	}
	if got := codexConfigEffort(codexCalls(t, log, codexMethodThreadStart)[0]); got != "medium" {
		t.Errorf("config.model_reasoning_effort = %q, want the strong level's medium", got)
	}
}

// A line that is JSON but no JSON-RPC message -- no header, no method, no
// id with a result or an error -- is still what the process printed rather
// than protocol, and stays on stdout.
func TestJSONRPCFrameTest(t *testing.T) {
	cases := map[string]bool{
		`{"jsonrpc":"2.0","method":"x"}`:                        true,
		`{"id":1,"result":{"ok":true}}`:                         true,
		`{"id":1,"result":null}`:                                true,
		`{"id":2,"error":{"code":-32601,"message":"nope"}}`:     true,
		`{"method":"remoteControl/status/changed","params":{}}`: true,
		`{"id":7}`:                            false,
		`{"level":"warn","msg":"a log line"}`: false,
	}
	for line, want := range cases {
		var msg rpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		if got := msg.isFrame(); got != want {
			t.Errorf("isFrame(%s) = %v, want %v", line, got, want)
		}
	}
}

// stripJSONRPCHeader rewrites a fake app-server so every frame it PRINTS
// omits "jsonrpc":"2.0", the way the real 0.153.4 prints them. Only the
// printf lines change: the sed that reads the client's request id matches
// the client's own frames, which still carry the header.
func stripJSONRPCHeader(t *testing.T, binary string) {
	t.Helper()
	body, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "printf '{") {
			lines[i] = strings.ReplaceAll(line, `"jsonrpc":"2.0",`, "")
		}
	}
	if err := os.WriteFile(binary, []byte(strings.Join(lines, "\n")), 0o755); err != nil {
		t.Fatal(err)
	}
}

// codexConfigEffort pulls config.model_reasoning_effort out of decoded
// params, "" when either level is absent.
func codexConfigEffort(params map[string]any) string {
	cfg, _ := params["config"].(map[string]any)
	effort, _ := cfg["model_reasoning_effort"].(string)
	return effort
}
