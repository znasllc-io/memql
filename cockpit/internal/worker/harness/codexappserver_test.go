package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// codexappserver_test.go drives the client against a fake `codex
// app-server` written as a shell script.
//
// THE FIXTURES BELOW ARE RECORDED, NOT INVENTED, and every one carries
// the file it came from. They were read on 2026-09-07 from
// github.com/openai/codex at `main` (commit 5ecb3afd), which is the only
// place the app-server protocol is written down completely -- the prose
// documentation names the methods but not the payloads:
//
//   - the method names and their payload types:
//     codex-rs/app-server-protocol/src/protocol/common.rs, the
//     `client_request_definitions!` and `server_notification_definitions!`
//     blocks (`initialize`, `thread/start`, `thread/resume`,
//     `turn/start`, `turn/completed`, `item/completed`,
//     `item/agentMessage/delta`, `thread/tokenUsage/updated`).
//   - the payload field names: the ts-rs generated types under
//     codex-rs/app-server-protocol/schema/typescript/v2/ -- these are
//     generated from the Rust structs, so they are the wire spelling
//     rather than a description of it.
//   - `outputSchema` on turn/start, and that it is per-turn:
//     codex-rs/app-server/tests/suite/v2/output_schema.rs.
//   - which assistant message is the ANSWER, and that a turn's usage is
//     the SUM of the `last` breakdowns:
//     sdk/python/src/openai_codex/_run.py plus
//     codex-rs/protocol/src/protocol.rs `TokenUsageInfo::append_last_usage`.
//
// The framing (one JSON value per line over the child's stdio) is from
// codex-rs/app-server/README.md, "stdio (default): newline-delimited
// JSON".

// The recorded thread ids. Two of them because a resumed session must
// prove it did NOT create a thread.
const (
	codexFakeThread   = "thr_019bbb20bff6713083aabf45ab33250e"
	codexFakeResumed  = "thr_019cc31700007130aaaa000000000000"
	codexApprovalCall = 9001
)

// codexTurnOK is one complete, successful turn.
//
// It emits a command execution (a tool item), the assistant answer as
// two deltas AND as a completed item, three usage updates, and the
// completion. The third usage update repeats the SECOND one's totals
// unchanged: that is the shape a replayed usage record has, and a client
// that adds every `last` it sees would bill it twice.
const codexTurnOK = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"inProgress","error":null,"startedAt":1,"completedAt":null,"durationMs":null}}}\n' "$id" "$turn"
printf '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"THREAD","turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"inProgress","error":null,"startedAt":1,"completedAt":null,"durationMs":null}}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"THREAD","turnId":"turn_%s","startedAtMs":1000,"item":{"type":"commandExecution","id":"item_cmd","pluginId":null,"scriptPath":null,"command":"ls -a","cwd":"/w","processId":null,"source":"agent","status":"inProgress","commandActions":[],"aggregatedOutput":null,"exitCode":null,"durationMs":null}}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_%s","completedAtMs":1200,"item":{"type":"commandExecution","id":"item_cmd","pluginId":null,"scriptPath":null,"command":"ls -a","cwd":"/w","processId":null,"source":"agent","status":"completed","commandActions":[],"aggregatedOutput":"README.md","exitCode":0,"durationMs":200}}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"THREAD","turnId":"turn_%s","itemId":"item_msg","delta":"the answer "}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"THREAD","turnId":"turn_%s","itemId":"item_msg","delta":"is 42"}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_%s","completedAtMs":1500,"item":{"type":"agentMessage","id":"item_msg","text":"the answer is 42","phase":"final_answer","memoryCitation":null,"delivery":null,"questions":null}}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"THREAD","turnId":"turn_%s","tokenUsage":{"total":{"totalTokens":1150,"inputTokens":1100,"cachedInputTokens":0,"cacheWriteInputTokens":0,"outputTokens":50,"reasoningOutputTokens":0},"last":{"totalTokens":150,"inputTokens":100,"cachedInputTokens":0,"cacheWriteInputTokens":0,"outputTokens":50,"reasoningOutputTokens":0},"modelContextWindow":272000}}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"THREAD","turnId":"turn_%s","tokenUsage":{"total":{"totalTokens":1230,"inputTokens":1160,"cachedInputTokens":0,"cacheWriteInputTokens":0,"outputTokens":70,"reasoningOutputTokens":0},"last":{"totalTokens":80,"inputTokens":60,"cachedInputTokens":0,"cacheWriteInputTokens":0,"outputTokens":20,"reasoningOutputTokens":0},"modelContextWindow":272000}}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"THREAD","turnId":"turn_%s","tokenUsage":{"total":{"totalTokens":1230,"inputTokens":1160,"cachedInputTokens":0,"cacheWriteInputTokens":0,"outputTokens":70,"reasoningOutputTokens":0},"last":{"totalTokens":80,"inputTokens":60,"cachedInputTokens":0,"cacheWriteInputTokens":0,"outputTokens":20,"reasoningOutputTokens":0},"modelContextWindow":272000}}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"THREAD","turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"completed","error":null,"startedAt":1,"completedAt":2,"durationMs":1000}}}\n' "$turn"
`

// codexTurnSchema answers with the JSON object `outputSchema` asked for.
// Codex constrains the FINAL ASSISTANT MESSAGE to the schema (the
// app-server sends it upstream as `text.format` with
// `"type":"json_schema","strict":true`), so the structured answer arrives
// as the agentMessage's own text and nowhere else.
const codexTurnSchema = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"inProgress","error":null,"startedAt":1,"completedAt":null,"durationMs":null}}}\n' "$id" "$turn"
printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_%s","completedAtMs":1500,"item":{"type":"agentMessage","id":"item_msg","text":"{\\"answer\\":\\"42\\",\\"confidence\\":0.9}","phase":"final_answer","memoryCitation":null,"delivery":null,"questions":null}}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"THREAD","turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"completed","error":null,"startedAt":1,"completedAt":2,"durationMs":1000}}}\n' "$turn"
`

// codexTurnProse answers a schema'd turn in prose, which is the shape
// ErrNoStructuredResult exists to name.
const codexTurnProse = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"inProgress","error":null,"startedAt":1,"completedAt":null,"durationMs":null}}}\n' "$id" "$turn"
printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_%s","completedAtMs":1500,"item":{"type":"agentMessage","id":"item_msg","text":"I could not answer that.","phase":"final_answer","memoryCitation":null,"delivery":null,"questions":null}}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"THREAD","turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"completed","error":null,"startedAt":1,"completedAt":2,"durationMs":1000}}}\n' "$turn"
`

// codexTurnFails is a turn the model could not finish. The status and
// the message both come off `turn/completed`; TurnError's shape is
// codex-rs/app-server-protocol/schema/typescript/v2/TurnError.ts.
const codexTurnFails = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"inProgress","error":null,"startedAt":1,"completedAt":null,"durationMs":null}}}\n' "$id" "$turn"
printf '{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"THREAD","turnId":"turn_%s","itemId":"item_msg","delta":"starting"}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"THREAD","turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"failed","error":{"message":"usage limit reached","codexErrorInfo":"usageLimitExceeded","additionalDetails":null,"misalignment":null},"startedAt":1,"completedAt":2,"durationMs":1000}}}\n' "$turn"
`

// codexTurnDies stops the process in the middle of a turn, after some
// output has already reached the sink. It exits 7 rather than 1 so the
// test can tell a real status from a normalised one.
const codexTurnDies = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"inProgress","error":null,"startedAt":1,"completedAt":null,"durationMs":null}}}\n' "$id" "$turn"
printf '{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"THREAD","turnId":"turn_%s","itemId":"item_msg","delta":"halfway th"}}\n' "$turn"
printf 'panic: the app-server fell over\n'
printf 'codex: out of memory\n' >&2
exit 7
`

// codexTurnAsksApproval makes the server send a REQUEST at the client,
// which is the shape that hangs a client that only knows how to read
// notifications. The approval methods are in common.rs's
// `server_request_definitions!` block.
//
// The fake BLOCKS on the answer, the way a server waiting for an
// approval does. That is what makes the test decisive rather than racy:
// a client that never answers does not merely fail an assertion about a
// log, it stops the turn dead.
const codexTurnAsksApproval = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"inProgress","error":null,"startedAt":1,"completedAt":null,"durationMs":null}}}\n' "$id" "$turn"
printf '{"jsonrpc":"2.0","id":9001,"method":"item/commandExecution/requestApproval","params":{"threadId":"THREAD","turnId":"turn_%s","itemId":"item_cmd"}}\n' "$turn"
IFS= read -r answer
printf '%s\n' "$answer" >> "$LOG"
printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_%s","completedAtMs":1500,"item":{"type":"agentMessage","id":"item_msg","text":"done","phase":"final_answer","memoryCitation":null,"delivery":null,"questions":null}}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"THREAD","turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"completed","error":null,"startedAt":1,"completedAt":2,"durationMs":1000}}}\n' "$turn"
`

// --- helpers --------------------------------------------------------

// fakeCodexAppServer writes a fake `codex` that answers the app-server's
// JSON-RPC methods with turnBody, and logs every line the client sent.
//
// The log is the assertion surface for everything the client PUT ON THE
// WIRE, for the reason fakeClaude's argv log exists: a field this client
// believes it sent and does not is invisible to any assertion made on
// this side of the pipe, and the whole point of the harness is that the
// protocol is a contract rather than a guess.
func fakeCodexAppServer(t *testing.T, turnBody string) (binary, log string) {
	t.Helper()
	return fakeCodexAppServerSettings(t, codexFakeSettings, turnBody)
}

// codexFakeSettings is what the fake states beside the thread on
// thread/start and thread/resume, in ThreadStartResponse's own shape: the
// model and provider, and no reasoningEffort -- the recorded 2026-09-07
// answer. The level tests pass their own, because what the app STATES here
// is what a turn reports as having served it.
const codexFakeSettings = `"model":"gpt-5.1-codex","modelProvider":"openai","serviceTier":null`

// fakeCodexAppServerSettings is fakeCodexAppServer with the thread's stated
// settings chosen by the test. settings is spliced into a shell printf
// format, so it must hold no `%` and no `'`.
func fakeCodexAppServerSettings(t *testing.T, settings, turnBody string) (binary, log string) {
	t.Helper()
	log = codexWireLogPath(t)
	body := strings.ReplaceAll(turnBody, "THREAD", codexFakeThread)
	script := "#!/bin/sh\n" +
		"LOG='" + log + "'\n" +
		": > \"$LOG\"\n" +
		"if [ \"$1\" != \"app-server\" ]; then echo \"unexpected argv: $*\" >&2; exit 64; fi\n" +
		"printf 'CODEX_HOME=%s\\n' \"$CODEX_HOME\" >> \"$LOG\"\n" +
		"printf 'PWD=%s\\n' \"$(pwd)\" >> \"$LOG\"\n" +
		"turn=0\n" +
		"while IFS= read -r line; do\n" +
		"  printf '%s\\n' \"$line\" >> \"$LOG\"\n" +
		"  id=$(printf '%s' \"$line\" | sed -n 's/^{\"jsonrpc\":\"2.0\",\"id\":\\([0-9]*\\),.*/\\1/p')\n" +
		"  case \"$line\" in\n" +
		"    *'\"method\":\"initialize\"'*)\n" +
		"      printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"userAgent\":\"codex-fake/1.0.0\",\"codexHome\":\"/tmp/codex-home\",\"platformFamily\":\"unix\",\"platformOs\":\"linux\"}}\\n' \"$id\"\n" +
		"      ;;\n" +
		"    *'\"method\":\"thread/start\"'*)\n" +
		"      printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"thread\":{\"id\":\"" + codexFakeThread + "\",\"preview\":\"\",\"modelProvider\":\"openai\",\"createdAt\":1730910000}," + settings + "}}\\n' \"$id\"\n" +
		"      printf '{\"jsonrpc\":\"2.0\",\"method\":\"thread/started\",\"params\":{\"thread\":{\"id\":\"" + codexFakeThread + "\",\"status\":{\"type\":\"active\"}}}}\\n'\n" +
		"      ;;\n" +
		"    *'\"method\":\"thread/resume\"'*)\n" +
		"      printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"thread\":{\"id\":\"" + codexFakeResumed + "\",\"preview\":\"\",\"modelProvider\":\"openai\",\"createdAt\":1730910000}," + settings + "}}\\n' \"$id\"\n" +
		"      ;;\n" +
		"    *'\"method\":\"turn/start\"'*)\n" +
		"      turn=$((turn + 1))\n" +
		body +
		"      ;;\n" +
		"  esac\n" +
		"done\n"
	return fakeBinary(t, "codex", script), log
}

// codexSpec is the Spec every app-server test starts from.
func codexSpec(t *testing.T, binary string) Spec {
	t.Helper()
	home := t.TempDir()
	return Spec{
		Binary:        binary,
		Workspace:     t.TempDir(),
		Env:           []string{"CODEX_HOME=" + home},
		MCPConfigPath: filepath.Join(home, "config.toml"),
		Launch:        testLaunch,
	}
}

// startCodexAppServer starts the client and closes it when the test ends,
// because a leaked app-server is a process still running on the machine
// after the test that forked it has gone.
func startCodexAppServer(t *testing.T, spec Spec) *codexAppServer {
	t.Helper()
	h := &codexAppServer{}
	if err := h.Start(context.Background(), spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// codexWire reads what the client sent, one line per record.
func codexWire(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read wire log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// codexWaitFor reads the wire log once want has reached it.
//
// A NOTIFICATION HAS NO REPLY TO WAIT ON. Start returns the moment the
// `initialized` bytes are written, so a test that read the log right
// then would be asking whether the fake had been scheduled yet -- which
// is a question about this machine's load, not about the client. Waiting
// for the last thing Start sent also makes every ABSENCE assertion after
// it sound, because the fake logs strictly in the order it reads.
func codexWaitFor(t *testing.T, log, want string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		lines := codexWire(t, log)
		if strings.Contains(strings.Join(lines, "\n"), want) {
			return lines
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q never reached the wire: %v", want, lines)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// codexWireLogPath is where a fake Codex records what the client sent.
func codexWireLogPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "wire.log")
}

// codexCalls returns the params of every request the client sent for
// method, decoded.
func codexCalls(t *testing.T, log, method string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range codexWire(t, log) {
		var msg struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal([]byte(line), &msg) != nil || msg.Method != method {
			continue
		}
		out = append(out, msg.Params)
	}
	return out
}

// --- tests ----------------------------------------------------------

func TestCodexAppServerName(t *testing.T) {
	h := &codexAppServer{}
	if h.Name() != HarnessCodexAppServer {
		t.Fatalf("Name() = %q, want %q", h.Name(), HarnessCodexAppServer)
	}
}

func TestCodexAppServerStartCompletesTheHandshake(t *testing.T) {
	binary, log := fakeCodexAppServer(t, codexTurnOK)
	spec := codexSpec(t, binary)
	startCodexAppServer(t, spec)

	// Waiting for the notification makes every assertion below sound:
	// the fake logs in the order it reads, so once `initialized` is
	// there, everything Start sent is there.
	wire := codexWaitFor(t, log, `"method":"initialized"`)
	var sawInitialize, sawInitialized bool
	for _, line := range wire {
		if strings.Contains(line, `"method":"initialize"`) {
			sawInitialize = true
			// The handshake must be answered before anything else is
			// sent; the server rejects a second initialize and refuses
			// every other method before the first.
			if sawInitialized {
				t.Fatal("initialize was sent after the initialized notification")
			}
		}
		if strings.Contains(line, `"method":"initialized"`) {
			sawInitialized = true
		}
	}
	if !sawInitialize {
		t.Fatalf("no initialize request on the wire: %v", wire)
	}
	if !sawInitialized {
		t.Fatalf("no initialized notification on the wire: %v", wire)
	}
	if strings.Contains(strings.Join(wire, "\n"), `"method":"thread/start"`) {
		t.Fatal("Start opened a thread; a thread costs a round trip and belongs to the first Turn")
	}
	if !strings.Contains(strings.Join(wire, "\n"), "CODEX_HOME="+spec.Env[0][len("CODEX_HOME="):]) {
		t.Fatalf("CODEX_HOME did not reach the process: %v", wire)
	}
	// macOS may report /private/var where t.TempDir returned /var. The
	// process must be in the same directory, including through such aliases.
	workspace, err := os.Stat(spec.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	var inWorkspace bool
	for _, line := range wire {
		if path, ok := strings.CutPrefix(line, "PWD="); ok {
			actual, err := os.Stat(path)
			inWorkspace = err == nil && os.SameFile(workspace, actual)
			break
		}
	}
	if !inWorkspace {
		t.Fatalf("the process did not run in the workspace: %v", wire)
	}
}

func TestCodexAppServerStartFailsWhenTheProcessCannotStart(t *testing.T) {
	// A Codex that cannot start says so on stderr and exits. The
	// failure has to land in Start: a session that reports itself
	// running and then fails every turn sends an operator looking at
	// the prompt instead of at the install.
	binary := fakeBinary(t, "codex", "#!/bin/sh\necho 'codex: not logged in' >&2\nexit 3\n")
	h := &codexAppServer{}
	err := h.Start(context.Background(), codexSpec(t, binary))
	if err == nil {
		t.Fatal("Start succeeded against a codex that exits immediately")
	}
	t.Cleanup(func() { _ = h.Close() })
	if !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("Start error does not carry the process's own words: %v", err)
	}
	if !strings.Contains(err.Error(), "3") {
		t.Fatalf("Start error does not carry the exit status: %v", err)
	}
}

func TestCodexAppServerStartRejectsAnUnusableSpec(t *testing.T) {
	cases := map[string]Spec{
		"no launcher":  {Binary: "codex", Workspace: t.TempDir()},
		"no binary":    {Workspace: t.TempDir(), Launch: testLaunch},
		"no workspace": {Binary: "codex", Launch: testLaunch},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			h := &codexAppServer{}
			if err := h.Start(context.Background(), spec); err == nil {
				t.Fatal("Start accepted a spec it cannot run")
			}
		})
	}
}

func TestCodexAppServerTurnStreamsAndReportsTheAnswer(t *testing.T) {
	binary, log := fakeCodexAppServer(t, codexTurnOK)
	h := startCodexAppServer(t, codexSpec(t, binary))

	var rec recorder
	res, err := h.Turn(context.Background(), "what is the answer", &rec)
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}

	if res.Text != "the answer is 42" {
		t.Fatalf("Text = %q, want the completed agent message", res.Text)
	}
	if res.AppSessionRef != codexFakeThread {
		t.Fatalf("AppSessionRef = %q, want the thread id %q", res.AppSessionRef, codexFakeThread)
	}
	if res.ResultJSON != nil {
		t.Fatalf("ResultJSON = %s, want none without a schema", res.ResultJSON)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 while the app-server is still running", res.ExitCode)
	}

	// The prompt reached the app in the recorded input-item shape.
	calls := codexCalls(t, log, "turn/start")
	if len(calls) != 1 {
		t.Fatalf("turn/start sent %d times, want 1", len(calls))
	}
	if calls[0]["threadId"] != codexFakeThread {
		t.Fatalf("turn/start threadId = %v, want %q", calls[0]["threadId"], codexFakeThread)
	}
	input, _ := calls[0]["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("turn/start input = %v, want one item", calls[0]["input"])
	}
	item, _ := input[0].(map[string]any)
	if item["type"] != "text" || item["text"] != "what is the answer" {
		t.Fatalf("turn/start input item = %v, want the recorded text shape", item)
	}
	if _, ok := calls[0]["outputSchema"]; ok {
		t.Fatal("turn/start carried an outputSchema the caller never asked for")
	}

	// The streams are SPLIT, which is the whole reason this package
	// exists: the engine renders `text` to a person and `event` as
	// progress, and one undifferentiated stream forced the old runner
	// to guess with a JSON-parse test.
	if got := rec.joined(StreamText); got != "the answer is 42" {
		t.Fatalf("text stream = %q, want the deltas in order", got)
	}
	tools := rec.on(StreamTool)
	if len(tools) != 2 {
		t.Fatalf("tool stream had %d chunks, want the command's start and end: %v", len(tools), tools)
	}
	if !strings.Contains(tools[1], `"exitCode":0`) {
		t.Fatalf("tool stream lost the command result: %v", tools)
	}
	if !strings.Contains(rec.joined(StreamEvent), `"method":"turn/completed"`) {
		t.Fatalf("event stream lost the completion: %v", rec.on(StreamEvent))
	}
	// The prose is delivered ONCE. The deltas carried it, so the
	// completed item that repeats it must not be shown to the person a
	// second time -- two chunks, not three.
	if got := len(rec.on(StreamText)); got != 2 {
		t.Fatalf("text stream had %d chunks, want the two deltas and no repeat from the completed item: %v", got, rec.on(StreamText))
	}
}

func TestCodexAppServerUsageIsTheSumOfTheTurnsDeltas(t *testing.T) {
	// `total` is CUMULATIVE FOR THE THREAD and `last` is only the most
	// recent model request, so neither is this turn's spend on its own:
	// billing `total` charges turn 2 for turn 1, and billing `last`
	// under-reports every turn that took more than one request. The
	// fixture's thread already carried 1000 input tokens before this
	// turn, and the turn itself spent 100+60 in and 50+20 out, with the
	// last update repeated the way a replay repeats it.
	binary, _ := fakeCodexAppServer(t, codexTurnOK)
	h := startCodexAppServer(t, codexSpec(t, binary))

	res, err := h.Turn(context.Background(), "hello", &recorder{})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if !res.Usage.Known {
		t.Fatal("Usage.Known is false although the app reported usage")
	}
	if res.Usage.InputTokens != 160 {
		t.Fatalf("InputTokens = %d, want 160 (100+60, not the thread total 1160 and not the last 60)", res.Usage.InputTokens)
	}
	if res.Usage.OutputTokens != 70 {
		t.Fatalf("OutputTokens = %d, want 70 (50+20, not the last 20)", res.Usage.OutputTokens)
	}
	if res.Usage.CostUSD != 0 {
		t.Fatalf("CostUSD = %v, want 0: Codex reports no money and an estimate must not be recorded as one", res.Usage.CostUSD)
	}
}

func TestCodexAppServerSilentUsageStaysUnknown(t *testing.T) {
	// No usage notification at all. Zeroes with Known true would be
	// filed by the engine as a FREE call rather than an unmeasured one.
	binary, _ := fakeCodexAppServer(t, codexTurnProse)
	h := startCodexAppServer(t, codexSpec(t, binary))

	res, err := h.Turn(context.Background(), "hello", &recorder{})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Usage.Known {
		t.Fatalf("Usage.Known is true although the app reported nothing: %+v", res.Usage)
	}
}

func TestCodexAppServerSecondTurnContinuesTheSameThread(t *testing.T) {
	binary, log := fakeCodexAppServer(t, codexTurnOK)
	h := startCodexAppServer(t, codexSpec(t, binary))

	first, err := h.Turn(context.Background(), "one", &recorder{})
	if err != nil {
		t.Fatalf("first Turn: %v", err)
	}
	second, err := h.Turn(context.Background(), "two", &recorder{})
	if err != nil {
		t.Fatalf("second Turn: %v", err)
	}

	if second.AppSessionRef != first.AppSessionRef {
		t.Fatalf("the ref changed between turns: %q then %q", first.AppSessionRef, second.AppSessionRef)
	}
	if starts := codexCalls(t, log, "thread/start"); len(starts) != 1 {
		t.Fatalf("thread/start sent %d times; the second turn started a stranger", len(starts))
	}
	calls := codexCalls(t, log, "turn/start")
	if len(calls) != 2 {
		t.Fatalf("turn/start sent %d times, want 2", len(calls))
	}
	for i, c := range calls {
		if c["threadId"] != codexFakeThread {
			t.Fatalf("turn %d carried threadId %v, want %q", i+1, c["threadId"], codexFakeThread)
		}
	}
}

func TestCodexAppServerAttachResumesTheSpecsThread(t *testing.T) {
	binary, log := fakeCodexAppServer(t, codexTurnOK)
	spec := codexSpec(t, binary)
	spec.ResumeRef = "thr_from_a_previous_session"
	h := startCodexAppServer(t, spec)

	res, err := h.Turn(context.Background(), "carry on", &recorder{})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if starts := codexCalls(t, log, "thread/start"); len(starts) != 0 {
		t.Fatalf("an attach opened a new thread: %v", starts)
	}
	resumes := codexCalls(t, log, "thread/resume")
	if len(resumes) != 1 {
		t.Fatalf("thread/resume sent %d times, want 1", len(resumes))
	}
	if resumes[0]["threadId"] != "thr_from_a_previous_session" {
		t.Fatalf("thread/resume threadId = %v, want the spec's ref", resumes[0]["threadId"])
	}
	// The ref reported back is the server's, not the one we asked with:
	// the server is the authority on what the thread is now called.
	if res.AppSessionRef != codexFakeResumed {
		t.Fatalf("AppSessionRef = %q, want the resumed thread id %q", res.AppSessionRef, codexFakeResumed)
	}
}

func TestCodexAppServerSchemaProducesResultJSON(t *testing.T) {
	binary, log := fakeCodexAppServer(t, codexTurnSchema)
	spec := codexSpec(t, binary)
	spec.ResponseSchema = `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`
	h := startCodexAppServer(t, spec)

	res, err := h.Turn(context.Background(), "answer in JSON", &recorder{})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}

	calls := codexCalls(t, log, "turn/start")
	if len(calls) != 1 {
		t.Fatalf("turn/start sent %d times, want 1", len(calls))
	}
	schema, ok := calls[0]["outputSchema"].(map[string]any)
	if !ok {
		t.Fatalf("turn/start carried no outputSchema: %v", calls[0])
	}
	if schema["type"] != "object" {
		t.Fatalf("outputSchema went across mangled: %v", schema)
	}

	var got map[string]any
	if err := json.Unmarshal(res.ResultJSON, &got); err != nil {
		t.Fatalf("ResultJSON does not parse (%s): %v", res.ResultJSON, err)
	}
	if got["answer"] != "42" {
		t.Fatalf("ResultJSON = %s, want the structured answer", res.ResultJSON)
	}
	if res.Text == "" {
		t.Fatal("Text is empty; the structured answer is also the final message and both are reported")
	}
}

func TestCodexAppServerSchemaAnsweredInProseIsNamed(t *testing.T) {
	// The turn RAN and SPENT the subscription, so this is not the same
	// event as a turn that never started. A caller that cannot tell
	// them apart retries the wrong one.
	binary, _ := fakeCodexAppServer(t, codexTurnProse)
	spec := codexSpec(t, binary)
	spec.ResponseSchema = `{"type":"object"}`
	h := startCodexAppServer(t, spec)

	res, err := h.Turn(context.Background(), "answer in JSON", &recorder{})
	if !errors.Is(err, ErrNoStructuredResult) {
		t.Fatalf("Turn error = %v, want ErrNoStructuredResult", err)
	}
	if res.ResultJSON != nil {
		t.Fatalf("ResultJSON = %s, want none rather than a synthesised object", res.ResultJSON)
	}
	if res.Text != "I could not answer that." {
		t.Fatalf("Text = %q, want the prose the app did produce", res.Text)
	}
	if res.AppSessionRef != codexFakeThread {
		t.Fatalf("AppSessionRef = %q; a failed turn still has to name its thread", res.AppSessionRef)
	}
}

func TestCodexAppServerFailedTurnCarriesTheReason(t *testing.T) {
	binary, _ := fakeCodexAppServer(t, codexTurnFails)
	h := startCodexAppServer(t, codexSpec(t, binary))

	var rec recorder
	res, err := h.Turn(context.Background(), "hello", &rec)
	if err == nil {
		t.Fatal("a failed turn returned no error")
	}
	if !strings.Contains(err.Error(), "usage limit reached") {
		t.Fatalf("the error does not name the app's own reason: %v", err)
	}
	if rec.joined(StreamText) != "starting" {
		t.Fatalf("the chunks that did arrive were dropped: %v", rec.on(StreamText))
	}
	if res.AppSessionRef != codexFakeThread {
		t.Fatalf("AppSessionRef = %q, want the thread even on a failure", res.AppSessionRef)
	}
}

func TestCodexAppServerProcessDiesMidTurn(t *testing.T) {
	binary, _ := fakeCodexAppServer(t, codexTurnDies)
	h := startCodexAppServer(t, codexSpec(t, binary))

	var rec recorder
	res, err := h.Turn(context.Background(), "hello", &rec)
	if err == nil {
		t.Fatal("a turn whose process died returned no error")
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want the real 7: the engine reads non-zero as a failed run and a flattened code misfiles it", res.ExitCode)
	}
	if !strings.Contains(err.Error(), "7") {
		t.Fatalf("the error does not name the exit status: %v", err)
	}
	if rec.joined(StreamText) != "halfway th" {
		t.Fatalf("the chunks that did arrive were dropped: %v", rec.on(StreamText))
	}
	// Whatever the protocol did not account for is KEPT, because an app
	// that dies while printing a stack trace prints it here.
	if !strings.Contains(rec.joined(StreamStdout), "panic: the app-server fell over") {
		t.Fatalf("the unaccounted stdout was dropped: %v", rec.on(StreamStdout))
	}
	if !strings.Contains(rec.joined(StreamStderr), "codex: out of memory") {
		t.Fatalf("stderr was dropped: %v", rec.on(StreamStderr))
	}
}

func TestCodexAppServerAnswersAServerRequest(t *testing.T) {
	// A request the client leaves unanswered is a turn that hangs until
	// the envelope's deadline and then reports nothing, which reads
	// downstream as an app that went quiet rather than a client that
	// did not reply.
	binary, log := fakeCodexAppServer(t, codexTurnAsksApproval)
	h := startCodexAppServer(t, codexSpec(t, binary))

	if _, err := h.Turn(context.Background(), "hello", &recorder{}); err != nil {
		t.Fatalf("Turn: %v", err)
	}

	var answered bool
	for _, line := range codexWire(t, log) {
		var msg struct {
			ID    *int `json:"id"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(line), &msg) != nil || msg.ID == nil || *msg.ID != codexApprovalCall {
			continue
		}
		if msg.Error == nil {
			t.Fatalf("the approval request was answered with a result: %s", line)
		}
		answered = true
	}
	if !answered {
		t.Fatalf("the server's approval request went unanswered: %v", codexWire(t, log))
	}
}

func TestCodexAppServerTurnOutsideTheLifecycleRefuses(t *testing.T) {
	h := &codexAppServer{}
	if _, err := h.Turn(context.Background(), "hello", &recorder{}); err == nil {
		t.Fatal("Turn ran before Start")
	}
}

func TestCodexAppServerCloseIsIdempotent(t *testing.T) {
	// Close is called on every exit path, including one that already
	// called it, and the thing it releases is a live app-server.
	binary, _ := fakeCodexAppServer(t, codexTurnOK)
	h := &codexAppServer{}
	if err := h.Start(context.Background(), codexSpec(t, binary)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// And on a harness that never started, because a failed Start is
	// followed by Close too.
	if err := (&codexAppServer{}).Close(); err != nil {
		t.Fatalf("Close before Start: %v", err)
	}
}

func TestCodexAppServerCancelledTurnStops(t *testing.T) {
	binary, _ := fakeCodexAppServer(t, codexTurnOK)
	h := startCodexAppServer(t, codexSpec(t, binary))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.Turn(ctx, "hello", &recorder{}); err == nil {
		t.Fatal("a cancelled turn returned no error")
	}
}

func TestCodexAppServerResolvesItsExecutableBeforeSandboxedHelpers(t *testing.T) {
	binary, _ := fakeCodexAppServer(t, codexTurnOK)
	link := filepath.Join(t.TempDir(), "codex-link")
	if err := os.Symlink(binary, link); err != nil {
		t.Fatal(err)
	}
	h := startCodexAppServer(t, codexSpec(t, link))
	want, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	if h.spec.Binary != want {
		t.Fatalf("helper executable %s, want %s", h.spec.Binary, want)
	}
}
