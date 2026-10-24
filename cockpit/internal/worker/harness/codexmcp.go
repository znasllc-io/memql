package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// codexmcp.go drives Codex through `codex mcp-server`: stdio MCP, with
// the `codex` tool to run a session and `codex-reply` to continue one by
// thread id.
//
// THIS IS THE FALLBACK, AND IT IS FOR OLD MACHINES. The two Codex
// harnesses are not two opinions about the same binary; they are two
// eras of it. The Codex CLI's own subcommand table was read at four tags
// of github.com/openai/codex on 2026-09-07:
//
//   - rust-v0.50.0 has `codex mcp-server` and no `codex app-server`.
//   - rust-v0.75.0 and rust-v0.100.0 have both.
//   - rust-v0.153.4 has both, and `codex mcp-server` warns on startup
//     that it "is deprecated and will be removed in a future release".
//   - `main` (commit 5ecb3afd) has no `McpServer` subcommand at all.
//
// So this client exists for a Codex too old for the app-server, it will
// stop being reachable as those machines update, and it can never be
// improved into the app-server's equal -- the tool pair below is the
// entire protocol. That is why the app descriptor the cockpit reports on
// Register carries the harness word: the engine must never attempt a
// protocol the machine cannot speak, and it cannot work out which one
// this is from the app id.
//
// TWO THINGS THIS PROTOCOL CANNOT DO, and both are reported honestly
// rather than papered over:
//
//  1. IT REPORTS NO USAGE. The shared output schema of both tools is
//     `{threadId, content}` and nothing else. The `codex/event` stream
//     does carry `token_count`, and it is NOT a substitute: its
//     `total_token_usage` is cumulative for the thread, and
//     codex-rs/protocol/src/protocol.rs describes a TokenCountEvent as
//     "accumulated, estimated, or replayed". The engine writes usage to
//     a ledger somebody bills from, and an estimate recorded as a
//     measurement is worse than a gap. Usage.Known therefore stays
//     false, always.
//
//  2. IT CANNOT CONSTRAIN THE ANSWER. The `codex` tool takes no
//     output-schema argument, and its input schema declares
//     `"additionalProperties": false`, so inventing one would not be
//     ignored -- it would be REFUSED, and the session would fail on an
//     argument the caller never asked for. A schema'd turn therefore
//     runs unconstrained and reports what the app happened to produce:
//     the structured answer if the text parses, and
//     ErrNoStructuredResult if it does not.
//
// SOURCES, all read on 2026-09-07 at tag rust-v0.153.4, the last tag
// that still ships the crate:
//   - tool names, input schemas and the shared output schema:
//     codex-rs/mcp-server/src/codex_tool_config.rs, whose
//     `verify_codex_tool_json_schema` and
//     `verify_codex_tool_reply_json_schema` tests pin the exact JSON.
//   - the result shape: `create_call_tool_result_with_thread_id` in
//     codex-rs/mcp-server/src/codex_tool_runner.rs.
//   - the in-flight notifications: `send_event_as_notification` in
//     codex-rs/mcp-server/src/outgoing_message.rs, method `codex/event`.

// The MCP methods this client sends.
//
// Source: the Model Context Protocol's stdio transport. The server is
// built on rmcp and ECHOES the protocol version the client declares
// (`InitializeResult::new(..).with_protocol_version(params.protocol_version)`
// in mcp-server/src/message_processor.rs), so version negotiation here
// is a formality -- the field is required, and whatever it says comes
// back.
const (
	mcpMethodInitialize  = "initialize"
	mcpMethodInitialized = "notifications/initialized"
	mcpMethodToolsCall   = "tools/call"

	mcpProtocolVersion = "2025-06-18"
)

// The two tools that are the whole of this protocol.
//
// Their argument spellings DIFFER and the difference is not cosmetic:
// `codex` takes hyphenated options (`approval-policy`) and refuses
// anything its schema does not declare; `codex-reply` takes camelCase
// (`threadId`) and declares no such refusal. Sending one's vocabulary to
// the other is a failed call, not a warning.
const (
	mcpToolCodex      = "codex"
	mcpToolCodexReply = "codex-reply"
)

// codexEventMethod is the notification the server sends while a tool
// call is running, carrying the core `Event` (`{id, msg}`) as params.
const codexEventMethod = "codex/event"

// The EventMsg types that are the assistant talking to a person.
// `EventMsg` is tagged by `type` in snake_case
// (codex-rs/protocol/src/protocol.rs).
const (
	codexEventMessage      = "agent_message"
	codexEventMessageDelta = "agent_message_content_delta"
	// codexEventSessionConfigured is the app stating what the session runs
	// at: `model` and `reasoning_effort`, once, when the `codex` tool sets
	// the session up (verified on 0.153.4 -- the only line in the stream
	// that carries either word).
	codexEventSessionConfigured = "session_configured"
)

// codexMCPToolEvents are the EventMsg types that are tool activity.
// Anything absent rides the event stream instead, which is the safe
// direction: an event that should have been a tool call is a cosmetic
// loss, whereas prose misfiled as a tool call never reaches the person
// who asked for it.
var codexMCPToolEvents = map[string]bool{
	"exec_command_begin":        true,
	"exec_command_end":          true,
	"exec_command_output_delta": true,
	"mcp_tool_call_begin":       true,
	"mcp_tool_call_end":         true,
	"web_search_begin":          true,
	"web_search_end":            true,
	"patch_apply_begin":         true,
	"patch_apply_end":           true,
	"view_image_tool_call":      true,
}

// codexMCP runs one Codex session over `codex mcp-server`.
//
// One process for the whole session, one `tools/call` per turn, and the
// thread id carried between them. The server keeps the conversation
// alive across calls, which is the only reason a follow-up is possible
// at all here.
type codexMCP struct {
	spec Spec
	conn *jsonrpcConn
	// knobs are the session's level in Codex's words, settled in Start and
	// put on the `codex` tool call that opens the session.
	knobs Knobs

	mu sync.Mutex
	// started is guarded for the reason the app-server client's is:
	// Close is the cancel path's job and can land while a Turn is still
	// in flight.
	started  bool
	threadID string
	turn     *codexMCPTurn
	// servedModel and servedEffort are what session_configured stated. The
	// statement is made once per session and outlives the call that
	// carried it: a codex-reply continues the same thread at the same
	// settings and restates nothing.
	servedModel  string
	servedEffort string
	// rec is the session's recording, set before the server starts.
	rec *recording
}

// Name implements Harness.
func (h *codexMCP) Name() string { return HarnessCodexMCP }

// Start launches the server and completes the MCP handshake.
//
// The handshake is here for the same reason it is in the app-server
// client: a Codex that is not installed or not signed in must fail
// while the session is still starting, not on the first turn, where the
// failure reads as a bad prompt.
func (h *codexMCP) Start(ctx context.Context, spec Spec) error {
	if err := checkCodexSpec("codex mcp-server", spec); err != nil {
		return err
	}
	// Before the process, for the app-server client's reason: a level
	// Codex cannot run at must not start a server on somebody's machine.
	knobs, err := spec.knobs(HarnessCodexMCP)
	if err != nil {
		return fmt.Errorf("codex mcp-server: %w", err)
	}
	h.spec = spec
	h.knobs = knobs
	h.rec = newRecording()
	h.threadID = strings.TrimSpace(spec.ResumeRef)

	proc, err := spec.Launch(ctx, spec.Workspace, []string{spec.Binary, "mcp-server"}, spec.Env, true)
	if err != nil {
		return fmt.Errorf("codex mcp-server: starting %s failed: %w", spec.Binary, err)
	}
	h.conn = newJSONRPCConn(proc, h.handleNotification)

	init := map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "memql-cockpit",
			"version": "1",
		},
	}
	if _, err := h.conn.call(ctx, "codex mcp-server", mcpMethodInitialize, init); err != nil {
		return err
	}
	if err := h.conn.notifyServer(mcpMethodInitialized, map[string]any{}); err != nil {
		return fmt.Errorf("codex mcp-server: completing the handshake failed: %w", err)
	}

	h.mu.Lock()
	h.started = true
	h.mu.Unlock()
	return nil
}

// Close implements Harness and is idempotent.
func (h *codexMCP) Close() error {
	h.mu.Lock()
	h.started = false
	h.mu.Unlock()
	if h.conn != nil {
		h.conn.close()
	}
	return nil
}

func (h *codexMCP) running() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.started
}

// Turn runs one prompt as a tool call and reports what came back.
func (h *codexMCP) Turn(ctx context.Context, prompt string, sink Sink) (TurnResult, error) {
	if h.conn == nil || !h.running() {
		return TurnResult{}, errors.New("codex mcp-server: Turn was called outside the Start/Close lifecycle; the session has no server to talk to")
	}

	h.conn.setSink(sink)
	defer h.conn.setSink(nil)

	state := &codexMCPTurn{}
	h.mu.Lock()
	h.turn = state
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.turn = nil
		h.mu.Unlock()
	}()
	h.rec.startTurn()
	defer h.flushRecording()

	name, args := mcpToolCodex, map[string]any{
		"prompt": prompt,
		"cwd":    h.spec.Workspace,
		// There is nobody here to approve a command, so a thread that
		// asks is a thread that stalls. `never` is one of the two
		// values this tool's schema accepts (the other is
		// `on-request`); the SANDBOX is deliberately not set, because
		// what a Codex session may touch on this machine is already
		// the operator's decision in the CODEX_HOME config this
		// process was pointed at.
		"approval-policy": "never",
	}
	// The level's knobs ride the `codex` tool, which is the call that
	// configures the session. `model` is one of its declared options, and
	// the effort goes in `config` -- "individual config settings that will
	// override what is in CODEX_HOME/config.toml", in the tool's own schema
	// (0.153.4) -- as model_reasoning_effort. `config` is also the one
	// option the schema declares open, which matters here: every other key
	// the tool is sent must be one it declares, or the call is refused.
	if h.knobs.Model != "" {
		args["model"] = h.knobs.Model
	}
	if h.knobs.Effort != "" {
		args["config"] = map[string]any{"model_reasoning_effort": h.knobs.Effort}
	}
	if ref := h.currentThread(); ref != "" {
		// Continuing takes the OTHER tool and the OTHER vocabulary,
		// and takes no cwd: the thread already has one, and an
		// undeclared argument here is a refused call.
		//
		// It takes no knobs either, for the same reason: codex-reply
		// declares prompt and threadId and nothing else. A thread this
		// harness opened keeps the settings its `codex` call gave it; one
		// an attach resumes keeps the settings its own first call chose,
		// and the report stays empty until the app states them.
		name, args = mcpToolCodexReply, map[string]any{
			"threadId": ref,
			"prompt":   prompt,
		}
	}

	res, err := h.conn.call(ctx, "codex mcp-server: the turn", mcpMethodToolsCall, map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return h.result(state.text(), false), err
	}

	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent struct {
			ThreadID string `json:"threadId"`
			Content  string `json:"content"`
		} `json:"structuredContent"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return h.result(state.text(), false), fmt.Errorf("codex mcp-server: the tool result could not be read: %w", err)
	}

	// The thread id is the whole reason a second turn is possible, so a
	// call that came back without one has ended the conversation
	// whether or not it says so. Keeping the id already held is what
	// makes `codex-reply` survive a server that only fills
	// structuredContent on the first call.
	if id := strings.TrimSpace(out.StructuredContent.ThreadID); id != "" {
		h.mu.Lock()
		h.threadID = id
		h.mu.Unlock()
	}

	// `structuredContent.content` mirrors the text blocks; the mirror
	// exists because "some MCP clients ignore `content` when
	// `structuredContent` is present". Preferring it and falling back to
	// the blocks reads both server generations.
	text := out.StructuredContent.Content
	if text == "" {
		var parts []string
		for _, block := range out.Content {
			if block.Type == "text" && block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		text = strings.Join(parts, "")
	}

	result := h.result(text, false)

	// A Codex runtime failure comes back as a SUCCESSFUL JSON-RPC
	// response whose result is flagged, so a client that only checked
	// for a JSON-RPC error would read a refusal as a good turn and
	// report the refusal text as the answer.
	if out.IsError {
		reason := strings.TrimSpace(text)
		if reason == "" {
			reason = "the app gave no reason"
		}
		return result, fmt.Errorf("codex mcp-server: the turn failed: %s", reason)
	}

	// The tool answered, so the model ran -- and ran at what the session
	// was configured with, which is the one thing this protocol lets the
	// app state. It is set here, past the isError return, because a call
	// that failed has not shown any model answered it; and before the
	// schema check, because an answer in prose was still an answer a
	// model produced.
	result.Model, result.Effort = h.served()

	if strings.TrimSpace(h.spec.ResponseSchema) != "" {
		structured, ok := codexJSONValue(text)
		if !ok {
			return result, ErrNoStructuredResult
		}
		result.ResultJSON = structured
	}
	return result, nil
}

// served is the session's model and effort as session_configured stated
// them, empty until it has.
func (h *codexMCP) served() (model, effort string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.servedModel, h.servedEffort
}

// result assembles what this protocol can honestly report.
//
// usageKnown is always false and is a parameter only so that the one
// place it could ever change is visible. See the file comment: the tool
// contract carries no spend, and the estimate on the event stream is not
// one.
func (h *codexMCP) result(text string, usageKnown bool) TurnResult {
	return TurnResult{
		Text:          text,
		AppSessionRef: h.currentThread(),
		Usage:         Usage{Known: usageKnown},
		ExitCode:      h.conn.exitCode(),
	}
}

func (h *codexMCP) currentThread() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.threadID
}

// handleNotification maps one `codex/event` onto the sink's streams.
//
// It runs on the reader goroutine and does no work that can block.
func (h *codexMCP) handleNotification(method string, params json.RawMessage, raw []byte) {
	if method != codexEventMethod {
		h.conn.emit(StreamEvent, raw)
		return
	}

	var event struct {
		Msg struct {
			Type    string `json:"type"`
			Delta   string `json:"delta"`
			Message string `json:"message"`
			// Model and ReasoningEffort are session_configured's. The
			// effort is a pointer because the app sends null for a
			// session with none set, and null stays empty.
			Model           string  `json:"model"`
			ReasoningEffort *string `json:"reasoning_effort"`
		} `json:"msg"`
	}
	if json.Unmarshal(params, &event) != nil {
		h.conn.emit(StreamEvent, raw)
		return
	}

	h.mu.Lock()
	state := h.turn
	h.mu.Unlock()

	switch {
	case event.Msg.Type == codexEventMessageDelta && event.Msg.Delta != "":
		if state != nil {
			state.noteDelta()
		}
		h.conn.emit(StreamText, []byte(event.Msg.Delta))
		return

	case event.Msg.Type == codexEventMessage:
		if state != nil {
			state.addMessage(event.Msg.Message)
		}
		// The prose reaches the person exactly once. A server that
		// streamed deltas has already delivered it; one that did not
		// delivers it here. `agent_message` carries no item id, so the
		// question asked is per-TURN rather than per-message -- coarser
		// than the app-server's answer, which is the price of the older
		// protocol and not a bug to fix here.
		if state == nil || !state.sawDelta() {
			h.conn.emit(StreamText, []byte(event.Msg.Message))
			return
		}

	case codexMCPToolEvents[event.Msg.Type]:
		h.conn.emit(StreamTool, raw)
		h.recordEvent(params)
		return

	case event.Msg.Type == codexEventSessionConfigured:
		// Kept for the turn to report once it has shown the model ran
		// (see Turn), and still forwarded below: it is the line a person
		// reading the transcript looks for to see what the session ran at.
		effort := ""
		if event.Msg.ReasoningEffort != nil {
			effort = strings.TrimSpace(*event.Msg.ReasoningEffort)
		}
		h.mu.Lock()
		h.servedModel = strings.TrimSpace(event.Msg.Model)
		h.servedEffort = effort
		h.mu.Unlock()
	}

	h.conn.emit(StreamEvent, raw)
}

// recordEvent feeds one core event to the session's recording: a begin
// opens a call, an end closes it, and an event that is a whole call on
// its own does both.
func (h *codexMCP) recordEvent(params json.RawMessage) {
	var envelope struct {
		Msg codexCoreEvent `json:"msg"`
	}
	if !decodeTolerant(params, &envelope) {
		return
	}
	call, phase, ok := codexCoreCall(envelope.Msg, h.spec.Workspace)
	if !ok {
		return
	}
	if phase == corePhaseBegin {
		if call.Cwd == "" {
			call.Cwd = h.spec.Workspace
		}
		h.rec.begin(call)
		return
	}
	// Sent under the recording's lock, as on the app-server: completions
	// arrive on the reader and the flush on the turn's goroutine.
	h.rec.completeAndEmit(call.ID, func(a *Action) {
		*a = mergeCall(*a, call)
		if a.Cwd == "" {
			a.Cwd = h.spec.Workspace
		}
		codexCoreFinish(a, envelope.Msg)
	}, h.conn.record)
}

// flushRecording records the calls the turn started and never finished.
func (h *codexMCP) flushRecording() {
	h.rec.flushAndEmit(h.conn.record)
}

// codexMCPTurn is the little this protocol lets a turn accumulate before
// its one result arrives.
//
// It exists for the turn that never gets that result: a process that
// dies mid-call has still said things, and the last thing it said is
// worth more to whoever reads the transcript than an empty string.
type codexMCPTurn struct {
	mu       sync.Mutex
	messages []string
	deltas   bool
}

func (t *codexMCPTurn) noteDelta() {
	t.mu.Lock()
	t.deltas = true
	t.mu.Unlock()
}

func (t *codexMCPTurn) sawDelta() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.deltas
}

func (t *codexMCPTurn) addMessage(message string) {
	if message == "" {
		return
	}
	t.mu.Lock()
	t.messages = append(t.messages, message)
	t.mu.Unlock()
}

// text is the last thing the assistant said.
//
// There is no phase to sort by here -- the older event stream does carry
// one on `agent_message`, but the mcp-server's own answer is whatever
// the tool result returns, so this is only ever the consolation prize
// for a turn that did not get one.
func (t *codexMCPTurn) text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.messages) == 0 {
		return ""
	}
	return t.messages[len(t.messages)-1]
}
