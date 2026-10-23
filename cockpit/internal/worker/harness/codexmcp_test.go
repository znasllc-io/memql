package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// codexmcp_test.go drives the FALLBACK client against a fake
// `codex mcp-server`.
//
// WHY THERE IS A FALLBACK AT ALL, with dates. On 2026-09-07 the Codex
// CLI's own subcommand table was read at four tags of
// github.com/openai/codex:
//
//   - rust-v0.50.0 has `codex mcp-server` and NO `codex app-server`.
//   - rust-v0.75.0 and rust-v0.100.0 have both.
//   - rust-v0.153.4 has both, and `codex mcp-server` prints
//     "`codex mcp-server` is deprecated and will be removed in a future
//     release." on startup (codex-rs/cli/src/main.rs).
//   - `main` (commit 5ecb3afd) has NO `McpServer` variant at all: the
//     subcommand is gone and `codex mcp` now only manages the servers
//     Codex itself connects to.
//
// So this harness is for OLD machines, and it can never be improved into
// the app-server's equal: the two tools below are the entire surface.
//
// THE FIXTURES ARE RECORDED, from rust-v0.153.4 -- the last tag that
// still ships the crate:
//
//   - the tool names, their input schemas and the shared output schema:
//     codex-rs/mcp-server/src/codex_tool_config.rs, whose
//     `verify_codex_tool_json_schema` and
//     `verify_codex_tool_reply_json_schema` tests assert the exact JSON
//     reproduced here. Note that the `codex` tool's options are
//     HYPHENATED (`approval-policy`) while `codex-reply`'s are camelCase
//     (`threadId`), and that the `codex` tool declares
//     `"additionalProperties": false` -- an extra key is a refusal, not
//     a warning.
//   - the result shape: `create_call_tool_result_with_thread_id` in
//     codex-rs/mcp-server/src/codex_tool_runner.rs, which mirrors the
//     text into `structuredContent` because "some MCP clients ignore
//     `content` when `structuredContent` is present".
//   - the in-flight event notifications: `send_event_as_notification` in
//     codex-rs/mcp-server/src/outgoing_message.rs sends method
//     `codex/event` with the core `Event` (`{id, msg}`) as params;
//     `EventMsg` is `#[serde(tag = "type", rename_all = "snake_case")]`
//     in codex-rs/protocol/src/protocol.rs.

const codexMCPThread = "019bbb20-bff6-7130-83aa-bf45ab33250e"

// codexMCPToolOK is one successful tool call: some events, then the
// result. `agent_message_content_delta` and `agent_message` are real
// EventMsg variants (protocol.rs); `exec_command_begin` and
// `exec_command_end` are the tool pair.
const codexMCPToolOK = `
printf '{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"exec_command_begin","call_id":"call_1","command":["ls","-a"],"cwd":"/w"},"_meta":{"requestId":1}}}\n'
printf '{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"agent_message_content_delta","thread_id":"THREAD","turn_id":"turn_1","item_id":"item_1","delta":"the answer "},"_meta":{"requestId":1}}}\n'
printf '{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"agent_message_content_delta","thread_id":"THREAD","turn_id":"turn_1","item_id":"item_1","delta":"is 42"},"_meta":{"requestId":1}}}\n'
printf '{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"exec_command_end","call_id":"call_1","exit_code":0,"duration":{"secs":0,"nanos":200000000}},"_meta":{"requestId":1}}}\n'
printf '{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"agent_message","message":"the answer is 42","phase":"final_answer","memory_citation":null},"_meta":{"requestId":1}}}\n'
printf '{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1160,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":70,"reasoning_output_tokens":0,"total_tokens":1230},"last_token_usage":{"input_tokens":60,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":20,"reasoning_output_tokens":0,"total_tokens":80},"model_context_window":272000},"rate_limits":null},"_meta":{"requestId":1}}}\n'
printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"the answer is 42"}],"structuredContent":{"threadId":"THREAD","content":"the answer is 42"}}}\n' "$id"
`

// codexMCPToolJSON answers with a JSON object. The mcp-server has no
// output-schema parameter at all, so this is what the app happened to
// produce rather than something the protocol constrained.
const codexMCPToolJSON = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"{\\"answer\\":\\"42\\"}"}],"structuredContent":{"threadId":"THREAD","content":"{\\"answer\\":\\"42\\"}"}}}\n' "$id"
`

// codexMCPToolProse answers a schema'd turn in prose.
const codexMCPToolProse = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"I could not answer that."}],"structuredContent":{"threadId":"THREAD","content":"I could not answer that."}}}\n' "$id"
`

// codexMCPToolError is the shape a Codex runtime error takes: a
// SUCCESSFUL JSON-RPC response whose result is flagged `isError`. A
// client that only checked for a JSON-RPC error reads it as a good turn.
const codexMCPToolError = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"Codex runtime error: usage limit reached"}],"structuredContent":{"threadId":"THREAD","content":"Codex runtime error: usage limit reached"},"isError":true}}\n' "$id"
`

// codexMCPToolDies stops the process after some output has reached the
// sink. It exits 7 so the test can tell a real status from a normalised
// one.
const codexMCPToolDies = `
printf '{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"agent_message_content_delta","thread_id":"THREAD","turn_id":"turn_1","item_id":"item_1","delta":"halfway th"},"_meta":{"requestId":1}}}\n'
printf 'panic: the mcp-server fell over\n'
printf 'codex: out of memory\n' >&2
exit 7
`

// --- helpers --------------------------------------------------------

// fakeCodexMCP writes a fake `codex` whose `mcp-server` subcommand
// speaks stdio MCP, and logs every line the client sent.
func fakeCodexMCP(t *testing.T, toolBody string) (binary, log string) {
	t.Helper()
	log = codexWireLogPath(t)
	body := strings.ReplaceAll(toolBody, "THREAD", codexMCPThread)
	script := "#!/bin/sh\n" +
		"LOG='" + log + "'\n" +
		": > \"$LOG\"\n" +
		"if [ \"$1\" != \"mcp-server\" ]; then echo \"unexpected argv: $*\" >&2; exit 64; fi\n" +
		"while IFS= read -r line; do\n" +
		"  printf '%s\\n' \"$line\" >> \"$LOG\"\n" +
		"  id=$(printf '%s' \"$line\" | sed -n 's/^{\"jsonrpc\":\"2.0\",\"id\":\\([0-9]*\\),.*/\\1/p')\n" +
		"  case \"$line\" in\n" +
		"    *'\"method\":\"initialize\"'*)\n" +
		"      printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"protocolVersion\":\"2025-06-18\",\"capabilities\":{\"tools\":{}},\"serverInfo\":{\"name\":\"codex-mcp-server\",\"version\":\"0.153.4\"}}}\\n' \"$id\"\n" +
		"      ;;\n" +
		"    *'\"method\":\"tools/call\"'*)\n" +
		body +
		"      ;;\n" +
		"  esac\n" +
		"done\n"
	return fakeBinary(t, "codex", script), log
}

func startCodexMCP(t *testing.T, spec Spec) *codexMCP {
	t.Helper()
	h := &codexMCP{}
	if err := h.Start(context.Background(), spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// codexToolCalls returns the decoded params of every tools/call the
// client sent.
func codexToolCalls(t *testing.T, log string) []struct {
	Name      string
	Arguments map[string]any
} {
	t.Helper()
	var out []struct {
		Name      string
		Arguments map[string]any
	}
	for _, params := range codexCalls(t, log, "tools/call") {
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		out = append(out, struct {
			Name      string
			Arguments map[string]any
		}{Name: name, Arguments: args})
	}
	return out
}

// --- tests ----------------------------------------------------------

func TestCodexMCPName(t *testing.T) {
	h := &codexMCP{}
	if h.Name() != HarnessCodexMCP {
		t.Fatalf("Name() = %q, want %q", h.Name(), HarnessCodexMCP)
	}
}

func TestCodexMCPStartCompletesTheHandshake(t *testing.T) {
	binary, log := fakeCodexMCP(t, codexMCPToolOK)
	startCodexMCP(t, codexSpec(t, binary))

	wire := strings.Join(codexWaitFor(t, log, `"method":"notifications/initialized"`), "\n")
	if !strings.Contains(wire, `"method":"initialize"`) {
		t.Fatalf("no MCP initialize on the wire: %v", wire)
	}
	if !strings.Contains(wire, `"method":"notifications/initialized"`) {
		t.Fatalf("no notifications/initialized on the wire: %v", wire)
	}
	if strings.Contains(wire, `"method":"tools/call"`) {
		t.Fatal("Start called a tool; a tool call is a turn and costs the subscription")
	}
}

func TestCodexMCPStartFailsWhenTheProcessCannotStart(t *testing.T) {
	binary := fakeBinary(t, "codex", "#!/bin/sh\necho 'codex: not logged in' >&2\nexit 3\n")
	h := &codexMCP{}
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

func TestCodexMCPStartRejectsAnUnusableSpec(t *testing.T) {
	cases := map[string]Spec{
		"no launcher":  {Binary: "codex", Workspace: t.TempDir()},
		"no binary":    {Workspace: t.TempDir(), Launch: testLaunch},
		"no workspace": {Binary: "codex", Launch: testLaunch},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			h := &codexMCP{}
			if err := h.Start(context.Background(), spec); err == nil {
				t.Fatal("Start accepted a spec it cannot run")
			}
		})
	}
}

func TestCodexMCPTurnRunsTheCodexTool(t *testing.T) {
	binary, log := fakeCodexMCP(t, codexMCPToolOK)
	spec := codexSpec(t, binary)
	h := startCodexMCP(t, spec)

	var rec recorder
	res, err := h.Turn(context.Background(), "what is the answer", &rec)
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}

	if res.Text != "the answer is 42" {
		t.Fatalf("Text = %q, want the tool result's content", res.Text)
	}
	if res.AppSessionRef != codexMCPThread {
		t.Fatalf("AppSessionRef = %q, want the thread id %q", res.AppSessionRef, codexMCPThread)
	}

	calls := codexToolCalls(t, log)
	if len(calls) != 1 {
		t.Fatalf("tools/call sent %d times, want 1", len(calls))
	}
	if calls[0].Name != "codex" {
		t.Fatalf("first turn called %q, want the codex tool", calls[0].Name)
	}
	if calls[0].Arguments["prompt"] != "what is the answer" {
		t.Fatalf("prompt = %v, want the caller's own words", calls[0].Arguments["prompt"])
	}
	if calls[0].Arguments["cwd"] != spec.Workspace {
		t.Fatalf("cwd = %v, want the workspace %q", calls[0].Arguments["cwd"], spec.Workspace)
	}
	// `additionalProperties: false` on the codex tool means an unknown
	// key is a refused call rather than an ignored one, so every key
	// sent has to be one the recorded schema declares.
	allowed := map[string]bool{
		"prompt": true, "model": true, "cwd": true, "sandbox": true,
		"config": true, "approval-policy": true, "base-instructions": true,
		"developer-instructions": true, "compact-prompt": true,
	}
	for key := range calls[0].Arguments {
		if !allowed[key] {
			t.Fatalf("the codex tool was sent %q, which its schema does not declare", key)
		}
	}

	if got := rec.joined(StreamText); got != "the answer is 42" {
		t.Fatalf("text stream = %q, want the deltas in order", got)
	}
	tools := rec.on(StreamTool)
	if len(tools) != 2 {
		t.Fatalf("tool stream had %d chunks, want the command's begin and end: %v", len(tools), tools)
	}
	if !strings.Contains(rec.joined(StreamEvent), "token_count") {
		t.Fatalf("event stream lost the events it does not otherwise use: %v", rec.on(StreamEvent))
	}
}

func TestCodexMCPUsageStaysUnknown(t *testing.T) {
	// The two tools' shared output schema is {threadId, content} and
	// nothing else, so there is no measured spend to report. The
	// `token_count` events the fixture DOES carry are the wrong number
	// twice over: `total_token_usage` is cumulative for the thread, and
	// codex-rs/protocol/src/protocol.rs says a TokenCountEvent is
	// "accumulated, estimated, or replayed". An estimate written into a
	// ledger somebody bills from is worse than a gap.
	binary, _ := fakeCodexMCP(t, codexMCPToolOK)
	h := startCodexMCP(t, codexSpec(t, binary))

	res, err := h.Turn(context.Background(), "hello", &recorder{})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Usage.Known {
		t.Fatalf("Usage.Known is true although this protocol reports no usage: %+v", res.Usage)
	}
	if res.Usage.InputTokens != 0 || res.Usage.OutputTokens != 0 {
		t.Fatalf("Usage carries numbers it cannot have measured: %+v", res.Usage)
	}
}

func TestCodexMCPSecondTurnRepliesOnTheSameThread(t *testing.T) {
	binary, log := fakeCodexMCP(t, codexMCPToolOK)
	h := startCodexMCP(t, codexSpec(t, binary))

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

	calls := codexToolCalls(t, log)
	if len(calls) != 2 {
		t.Fatalf("tools/call sent %d times, want 2", len(calls))
	}
	if calls[1].Name != "codex-reply" {
		t.Fatalf("second turn called %q, want codex-reply", calls[1].Name)
	}
	if calls[1].Arguments["threadId"] != codexMCPThread {
		t.Fatalf("codex-reply threadId = %v, want %q", calls[1].Arguments["threadId"], codexMCPThread)
	}
	if calls[1].Arguments["prompt"] != "two" {
		t.Fatalf("codex-reply prompt = %v", calls[1].Arguments["prompt"])
	}
	if _, ok := calls[1].Arguments["cwd"]; ok {
		t.Fatal("codex-reply was sent a cwd its schema does not declare")
	}
}

func TestCodexMCPAttachRepliesToTheSpecsThread(t *testing.T) {
	binary, log := fakeCodexMCP(t, codexMCPToolOK)
	spec := codexSpec(t, binary)
	spec.ResumeRef = "019cc317-0000-7130-aaaa-000000000000"
	h := startCodexMCP(t, spec)

	if _, err := h.Turn(context.Background(), "carry on", &recorder{}); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	calls := codexToolCalls(t, log)
	if len(calls) != 1 {
		t.Fatalf("tools/call sent %d times, want 1", len(calls))
	}
	if calls[0].Name != "codex-reply" {
		t.Fatalf("an attach called %q, want codex-reply", calls[0].Name)
	}
	if calls[0].Arguments["threadId"] != spec.ResumeRef {
		t.Fatalf("codex-reply threadId = %v, want the spec's ref", calls[0].Arguments["threadId"])
	}
}

func TestCodexMCPSchemaIsBestEffortAndSaysSo(t *testing.T) {
	// The mcp-server has NO output-schema parameter, so a schema cannot
	// be enforced here at all. The turn still reports a structured
	// answer when the app happened to produce one, and names the gap
	// when it did not -- the same two outcomes the app-server has, with
	// the constraint missing.
	t.Run("parses", func(t *testing.T) {
		binary, log := fakeCodexMCP(t, codexMCPToolJSON)
		spec := codexSpec(t, binary)
		spec.ResponseSchema = `{"type":"object","properties":{"answer":{"type":"string"}}}`
		h := startCodexMCP(t, spec)

		res, err := h.Turn(context.Background(), "answer in JSON", &recorder{})
		if err != nil {
			t.Fatalf("Turn: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(res.ResultJSON, &got); err != nil {
			t.Fatalf("ResultJSON does not parse (%s): %v", res.ResultJSON, err)
		}
		if got["answer"] != "42" {
			t.Fatalf("ResultJSON = %s", res.ResultJSON)
		}
		// The schema must not be smuggled into the tool call: the
		// codex tool refuses an argument its schema does not declare.
		for _, c := range codexToolCalls(t, log) {
			for key := range c.Arguments {
				if strings.Contains(strings.ToLower(key), "schema") {
					t.Fatalf("the schema was sent as %q, which the tool does not accept", key)
				}
			}
		}
	})

	t.Run("prose", func(t *testing.T) {
		binary, _ := fakeCodexMCP(t, codexMCPToolProse)
		spec := codexSpec(t, binary)
		spec.ResponseSchema = `{"type":"object"}`
		h := startCodexMCP(t, spec)

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
	})
}

func TestCodexMCPToolErrorIsAFailure(t *testing.T) {
	binary, _ := fakeCodexMCP(t, codexMCPToolError)
	h := startCodexMCP(t, codexSpec(t, binary))

	res, err := h.Turn(context.Background(), "hello", &recorder{})
	if err == nil {
		t.Fatal("an isError result was read as a good turn")
	}
	if !strings.Contains(err.Error(), "usage limit reached") {
		t.Fatalf("the error does not name the app's own reason: %v", err)
	}
	if res.AppSessionRef != codexMCPThread {
		t.Fatalf("AppSessionRef = %q; a failed turn still has to name its thread", res.AppSessionRef)
	}
}

func TestCodexMCPProcessDiesMidTurn(t *testing.T) {
	binary, _ := fakeCodexMCP(t, codexMCPToolDies)
	h := startCodexMCP(t, codexSpec(t, binary))

	var rec recorder
	res, err := h.Turn(context.Background(), "hello", &rec)
	if err == nil {
		t.Fatal("a turn whose process died returned no error")
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want the real 7", res.ExitCode)
	}
	if !strings.Contains(err.Error(), "7") {
		t.Fatalf("the error does not name the exit status: %v", err)
	}
	if rec.joined(StreamText) != "halfway th" {
		t.Fatalf("the chunks that did arrive were dropped: %v", rec.on(StreamText))
	}
	if !strings.Contains(rec.joined(StreamStdout), "panic: the mcp-server fell over") {
		t.Fatalf("the unaccounted stdout was dropped: %v", rec.on(StreamStdout))
	}
	if !strings.Contains(rec.joined(StreamStderr), "codex: out of memory") {
		t.Fatalf("stderr was dropped: %v", rec.on(StreamStderr))
	}
}

func TestCodexMCPTurnOutsideTheLifecycleRefuses(t *testing.T) {
	h := &codexMCP{}
	if _, err := h.Turn(context.Background(), "hello", &recorder{}); err == nil {
		t.Fatal("Turn ran before Start")
	}
}

func TestCodexMCPCloseIsIdempotent(t *testing.T) {
	binary, _ := fakeCodexMCP(t, codexMCPToolOK)
	h := &codexMCP{}
	if err := h.Start(context.Background(), codexSpec(t, binary)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := (&codexMCP{}).Close(); err != nil {
		t.Fatalf("Close before Start: %v", err)
	}
}

func TestCodexMCPCancelledTurnStops(t *testing.T) {
	binary, _ := fakeCodexMCP(t, codexMCPToolOK)
	h := startCodexMCP(t, codexSpec(t, binary))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.Turn(ctx, "hello", &recorder{}); err == nil {
		t.Fatal("a cancelled turn returned no error")
	}
}
