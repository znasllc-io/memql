package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// codexappserver.go drives Codex through `codex app-server`: JSON-RPC
// 2.0 over the child's stdio, one process per SESSION and one request
// per turn.
//
// WHY THIS AND NOT `codex exec`. The old runner ran `codex exec <prompt>`
// with stdin closed and read the terminal. That gave no usage at all, no
// final answer distinguishable from the transcript, and no way to send a
// second prompt into the same conversation -- Codex prints for a person,
// and a person is not what was reading. The app-server exists for
// exactly this caller: it names the thread, streams typed items, reports
// what it spent, and takes the next turn on a thread it already holds.
//
// THE PROTOCOL IS CONFIRMED, NOT ASSUMED. Every method name, field
// spelling and payload shape below was read on 2026-09-07 from
// github.com/openai/codex at `main` (commit 5ecb3afd); the constants
// carry their source file. The prose documentation
// (developers.openai.com/codex/app-server, now
// learn.chatgpt.com/docs/app-server) names the methods but not the
// payloads, so the generated types under
// codex-rs/app-server-protocol/schema/typescript/v2/ are the citation
// that matters -- they are generated FROM the Rust structs that serialize
// the wire, rather than a description of them.
//
// EVERY DECODER HERE IS TOLERANT OF FIELDS IT DOES NOT KNOW, and that is
// a requirement rather than laziness. The app-server's payloads grow --
// `Thread` alone gained a dozen fields across the tags that were read --
// and the machines this runs on hold whatever Codex their owner
// installed. A decoder that insisted on an exact shape would turn every
// upstream release into a fleet-wide outage, reported as "the app went
// quiet".

// The JSON-RPC methods this client sends.
//
// Source: codex-rs/app-server-protocol/src/protocol/common.rs, the
// `client_request_definitions!` block, read 2026-09-07.
const (
	codexMethodInitialize   = "initialize"
	codexMethodInitialized  = "initialized"
	codexMethodThreadStart  = "thread/start"
	codexMethodThreadResume = "thread/resume"
	codexMethodTurnStart    = "turn/start"
)

// The notifications this client reads.
//
// Source: the same file's `server_notification_definitions!` block. The
// set is deliberately small: everything not named here is still
// forwarded to the sink as an event, because the engine renders progress
// from whatever arrives and a notification this build has not heard of
// is still something the app is doing.
const (
	codexNotifyAgentMessageDelta = "item/agentMessage/delta"
	codexNotifyItemStarted       = "item/started"
	codexNotifyItemCompleted     = "item/completed"
	codexNotifyTokenUsage        = "thread/tokenUsage/updated"
	codexNotifyTurnCompleted     = "turn/completed"
	codexNotifyError             = "error"
	// codexNotifyModelRerouted is the app moving a turn to another model
	// (ModelReroutedNotification, 0.153.4: fromModel, toModel, reason) --
	// the one execution-time statement of a model the app-server makes.
	codexNotifyModelRerouted = "model/rerouted"
	// codexNotifyThreadSettings is the app restating the thread's settings
	// after they change (ThreadSettingsUpdatedNotification, 0.153.4:
	// threadSettings.model and .effort).
	codexNotifyThreadSettings = "thread/settings/updated"
)

// Item types worth telling apart.
//
// Source: codex-rs/app-server-protocol/schema/typescript/v2/ThreadItem.ts,
// whose variants are tagged by `type`. Only two questions are asked of
// an item -- is it the assistant talking, and is it a tool -- so only
// those names are spelled out; every other variant rides the event
// stream with its own type word intact.
const (
	codexItemAgentMessage = "agentMessage"
)

// codexToolItems are the item types the engine should see as tool
// activity rather than as progress. A type missing from this set is
// reported as an event, which is the safe direction: an event that
// should have been a tool call is a cosmetic loss, whereas prose
// misfiled as a tool call is not shown to the person who asked.
var codexToolItems = map[string]bool{
	"commandExecution":   true,
	"mcpToolCall":        true,
	"dynamicToolCall":    true,
	"functionCallOutput": true,
	"fileChange":         true,
	"webSearch":          true,
	"imageGeneration":    true,
}

// Turn statuses.
//
// Source: codex-rs/app-server-protocol/schema/typescript/v2/TurnStatus.ts.
const (
	codexTurnCompleted   = "completed"
	codexTurnFailed      = "failed"
	codexTurnInterrupted = "interrupted"
)

// codexPhaseFinal marks the assistant message that is the ANSWER rather
// than commentary along the way.
//
// Source: codex-rs/app-server-protocol/schema/typescript/MessagePhase.ts,
// which is `"commentary" | "final_answer"` -- snake_case, unlike its
// neighbours. Its own doc comment warns that "providers do not emit this
// consistently, so callers must treat `None` as phase unknown", which is
// why codexTurnState.finalText below has a second rule rather than only
// this one.
const (
	codexPhaseFinal    = "final_answer"
	codexPhaseFinalAlt = "finalAnswer"
)

// codexStderrKeepBytes is how much of stderr is held back for the error
// message. A Codex that cannot start says why on stderr and then exits,
// and an error that says only "exit status 1" sends an operator to read
// a log the worker never wrote.
const codexStderrKeepBytes = 4 << 10

// codexStderrDrainGrace bounds the wait for stderr to reach EOF after
// the process has stopped. See drainStderr for why the wait exists and
// why it cannot be unbounded.
const codexStderrDrainGrace = 2 * time.Second

// --- the JSON-RPC transport -----------------------------------------

// rpcMessage is every JSON-RPC 2.0 frame in one struct: request,
// response, notification and error.
//
// FIELD ORDER IS LOAD-BEARING for the tests and only for the tests --
// the fake servers are shell scripts that pull the id out of the line
// with sed, which needs it in a fixed place. Nothing on the wire cares.
type rpcMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s (code %d)", e.Message, e.Code)
}

// isFrame reports whether a decoded line is a JSON-RPC message at all.
//
// THE `jsonrpc` HEADER IS NOT THE TEST, because codex app-server does not
// send it. Its README says so ("with the "jsonrpc":"2.0" header omitted on
// the wire"), and the real codex-cli 0.153.4 omits it on every frame
// (recorded 2026-09-13: `{"id":1,"result":{"userAgent":...}}`,
// `{"method":"remoteControl/status/changed","params":{...}}`), while the
// mcp-server fallback does send it. Requiring the header filed the
// initialize answer as stdout, so every app-server session hung at Start
// until its deadline -- and the fakes all sent the header, so no test saw
// it (the #440 session found it by driving the real binary).
//
// So a frame is what JSON-RPC 2.0 makes one: the header, a method (a
// request or a notification), or an id carrying a result or an error (a
// response). A JSON line that is none of those -- a structured log line
// the process printed, say -- is still narration and stays on stdout.
func (m rpcMessage) isFrame() bool {
	return m.JSONRPC != "" || m.Method != "" || (m.ID != nil && (len(m.Result) > 0 || m.Error != nil))
}

// rpcMethodNotFound is what this client answers a server request with.
//
// Source: JSON-RPC 2.0, section 5.1.
const rpcMethodNotFound = -32601

// jsonrpcConn is one JSON-RPC 2.0 conversation with a supervised child
// over its stdio, framed as one JSON value per line.
//
// Source for the framing: codex-rs/app-server/README.md, "stdio
// (default): newline-delimited JSON". The `codex mcp-server` fallback
// uses the same framing because stdio MCP does, which is why this type
// is shared rather than written twice.
//
// A LINE THAT IS NOT A PROTOCOL MESSAGE IS KEPT, not dropped. That is
// the StreamStdout contract, and it is the only reason an app that dies
// while printing a stack trace leaves the stack trace behind. A
// json.Decoder over the whole stream would have been shorter and would
// have swallowed the rest of the session on the first such line.
type jsonrpcConn struct {
	proc Process

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcMessage
	sink    Sink
	closed  bool

	// notify is called for every server notification, on the reader
	// goroutine. It must not block for long.
	notify func(method string, params json.RawMessage, raw []byte)

	// dead closes when stdout reaches EOF, which is the only honest
	// signal that no answer is coming.
	dead     chan struct{}
	deadOnce sync.Once

	// stderrDone closes when the stderr reader has drained its pipe.
	// It is separate from dead because the two streams end
	// independently, and the last thing a dying app says is usually on
	// this one.
	stderrDone chan struct{}

	stderrMu   sync.Mutex
	stderrTail []byte
}

// newJSONRPCConn starts reading proc. The caller owns proc's lifetime.
func newJSONRPCConn(proc Process, notify func(method string, params json.RawMessage, raw []byte)) *jsonrpcConn {
	c := &jsonrpcConn{
		proc:       proc,
		pending:    make(map[int64]chan rpcMessage),
		notify:     notify,
		dead:       make(chan struct{}),
		stderrDone: make(chan struct{}),
	}
	go c.readStdout()
	go c.readStderr()
	return c
}

// setSink points the connection's chunks at one turn's sink.
//
// Chunks that arrive with no turn in flight are DROPPED rather than
// buffered: a chunk belongs to the turn that caused it, and attaching a
// stray one to the next turn would put output in a transcript that did
// not produce it. Stderr is the exception the tail buffer covers, since
// a start failure has no turn to belong to.
func (c *jsonrpcConn) setSink(s Sink) {
	c.mu.Lock()
	c.sink = s
	c.mu.Unlock()
}

func (c *jsonrpcConn) emit(stream string, data []byte) {
	c.mu.Lock()
	s := c.sink
	c.mu.Unlock()
	if s != nil && len(data) > 0 {
		s.Chunk(stream, data)
	}
}

// record hands one finished call to the turn's sink. Like a chunk, an
// action that finishes with no turn in flight has nowhere to go -- and
// its seq is already spent, so the gap it leaves is visible downstream.
func (c *jsonrpcConn) record(a Action) {
	c.mu.Lock()
	s := c.sink
	c.mu.Unlock()
	if s != nil {
		s.Record(a)
	}
}

func (c *jsonrpcConn) readStdout() {
	defer c.deadOnce.Do(func() { close(c.dead) })
	r := bufio.NewReaderSize(c.proc.Stdout(), 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			c.route(line)
		}
		if err != nil {
			return
		}
	}
}

func (c *jsonrpcConn) readStderr() {
	defer close(c.stderrDone)
	src := c.proc.Stderr()
	if src == nil {
		return
	}
	r := bufio.NewReaderSize(src, 32<<10)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			c.keepStderr(line)
			c.emit(StreamStderr, line)
		}
		if err != nil {
			return
		}
	}
}

func (c *jsonrpcConn) keepStderr(line []byte) {
	c.stderrMu.Lock()
	defer c.stderrMu.Unlock()
	c.stderrTail = append(c.stderrTail, line...)
	if len(c.stderrTail) > codexStderrKeepBytes {
		c.stderrTail = c.stderrTail[len(c.stderrTail)-codexStderrKeepBytes:]
	}
}

func (c *jsonrpcConn) stderrSnapshot() string {
	c.stderrMu.Lock()
	defer c.stderrMu.Unlock()
	return strings.TrimSpace(string(c.stderrTail))
}

// route dispatches one line of the child's stdout.
func (c *jsonrpcConn) route(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}
	var msg rpcMessage
	// A protocol frame is decided by its shape (isFrame), not by the
	// `jsonrpc` header the app-server never sends.
	if json.Unmarshal(trimmed, &msg) != nil || !msg.isFrame() {
		c.emit(StreamStdout, line)
		return
	}
	switch {
	case msg.ID != nil && msg.Method != "":
		c.refuseRequest(*msg.ID, msg.Method)
	case msg.ID != nil:
		c.deliver(msg)
	case msg.Method != "":
		if c.notify != nil {
			c.notify(msg.Method, msg.Params, trimmed)
		}
	}
}

// refuseRequest answers a request from the server with an error.
//
// SILENCE IS NOT AN OPTION. The app-server asks the client for command
// approvals, file-change approvals, elicitations and dynamic tool calls
// (common.rs, `server_request_definitions!`), and a request nobody
// answers is a turn that hangs until the envelope's deadline and then
// reports nothing -- which reads downstream as an app that went quiet
// rather than as a client that never replied. Refusing is also the
// honest answer: there is no person on this end of the pipe, which is
// exactly why the thread is opened with the approval policy set to
// never.
func (c *jsonrpcConn) refuseRequest(id json.RawMessage, method string) {
	_ = c.write(rpcMessage{
		JSONRPC: "2.0",
		ID:      &id,
		Error: &rpcError{
			Code:    rpcMethodNotFound,
			Message: "memql cockpit: this session runs unattended and cannot answer " + method,
		},
	})
}

func (c *jsonrpcConn) deliver(msg rpcMessage) {
	var id int64
	if json.Unmarshal(*msg.ID, &id) != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	c.mu.Unlock()
	if ch != nil {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (c *jsonrpcConn) write(msg rpcMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	w := c.proc.Stdin()
	if w == nil {
		return errors.New("the process was started without a stdin to write the protocol to")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = w.Write(append(body, '\n'))
	return err
}

// notifyServer sends a JSON-RPC notification, which has no reply and so
// no way to fail loudly.
func (c *jsonrpcConn) notifyServer(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.write(rpcMessage{JSONRPC: "2.0", Method: method, Params: raw})
}

// call sends one request and waits for its response.
//
// what names the operation in every error this can return, because the
// caller's own wrapping is gone by the time an operator reads it.
func (c *jsonrpcConn) call(ctx context.Context, what, method string, params any) (json.RawMessage, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return nil, fmt.Errorf("%s: encoding the request failed: %w", what, err)
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: the connection is closed", what)
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	idRaw := json.RawMessage(strconv.FormatInt(id, 10))
	writeErr := c.write(rpcMessage{JSONRPC: "2.0", ID: &idRaw, Method: method, Params: raw})
	if writeErr != nil {
		// A request that could not even be written means this
		// connection is over. Terminating makes the reader hit EOF, so
		// the wait below ends with the process's REAL status instead of
		// blocking on a stream nobody will ever write to again -- the
		// race that would otherwise leave Start hanging on a context
		// with no deadline.
		c.proc.Terminate()
	}

	select {
	case msg := <-ch:
		if msg.Error != nil {
			return nil, fmt.Errorf("%s: %s refused: %w", what, method, msg.Error)
		}
		return msg.Result, nil
	case <-c.dead:
		return nil, c.exitError(what)
	case <-ctx.Done():
		if writeErr != nil {
			return nil, fmt.Errorf("%s: writing %s failed: %w", what, method, writeErr)
		}
		return nil, fmt.Errorf("%s: %w", what, ctx.Err())
	}
}

// drainStderr waits for the stderr reader to reach the end of its pipe.
//
// TWO THINGS DEPEND ON THIS AND BOTH FAIL SILENTLY WITHOUT IT. The
// chunks an app managed to print before it died have to reach the
// transcript while the turn's sink is still attached -- the sink is
// detached the moment Turn returns, so a line still in flight is lost
// for good and the transcript then shows a process that stopped for no
// stated reason. And the error built just below quotes the same bytes,
// so returning before the drain quotes a tail that is missing its last
// and most useful line.
//
// The wait is BOUNDED because a pipe can outlive the process that owned
// it: a grandchild that inherited the descriptor keeps it open, and a
// harness that blocked forever on somebody else's file descriptor would
// hang a session rather than report the death it already knows about.
func (c *jsonrpcConn) drainStderr() {
	select {
	case <-c.stderrDone:
	case <-time.After(codexStderrDrainGrace):
	}
}

// exitError describes a process that stopped, in the words the process
// itself used.
//
// THE DRAIN COMES BEFORE THE WAIT, and the order is not a preference.
// Both Process implementations get their streams from os/exec's
// StdoutPipe and StderrPipe, and Wait CLOSES those pipes as soon as it
// sees the child exit -- os/exec says so, and warns that "it is
// incorrect to call Wait before all reads from the pipe have completed".
// Waiting first therefore races the stderr reader and wins often enough
// to eat the one line that says why the app died, which is the only line
// anybody wanted.
func (c *jsonrpcConn) exitError(what string) error {
	c.drainStderr()
	_ = c.proc.Wait()
	code := c.proc.ExitCode()
	if tail := c.stderrSnapshot(); tail != "" {
		return fmt.Errorf("%s: the process exited with status %d: %s", what, code, tail)
	}
	return fmt.Errorf("%s: the process exited with status %d", what, code)
}

// exitCode is the process's real status once it has stopped, and zero
// while it is still running.
//
// Unnormalised on purpose: the engine reads non-zero as a FAILED run, so
// flattening a 7 to a 1 misfiles the outcome.
func (c *jsonrpcConn) exitCode() int {
	select {
	case <-c.dead:
		_ = c.proc.Wait()
		return c.proc.ExitCode()
	default:
		return 0
	}
}

// close releases the child.
//
// It returns nil once the process is released, and that is deliberate:
// the exit status of a process this method just signalled is not a Close
// failure, and reporting it would make every clean shutdown of every
// session look like an error in the log. The escalation from a polite
// signal to a fatal one belongs to Terminate, which is the app-session
// supervisor in production -- this package does not get its own grace
// window, because two of them would drift.
func (c *jsonrpcConn) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.sink = nil
	c.mu.Unlock()

	if w := c.proc.Stdin(); w != nil {
		_ = w.Close()
	}
	c.proc.Terminate()
	_ = c.proc.Wait()
	c.deadOnce.Do(func() { close(c.dead) })
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	return json.Marshal(params)
}

// --- the app-server client ------------------------------------------

// codexAppServer runs one Codex session over `codex app-server`.
//
// ONE PROCESS FOR THE WHOLE SESSION, one request per turn. That is the
// opposite of the Claude Code harness next door, and it is the app's
// choice rather than ours: the app-server owns a connection and takes
// turns on threads it holds, so a process per turn would throw the
// thread away and re-create it every time.
//
// The consequence worth stating is the one about credentials: because
// the process outlives the turns, a renewed MCP credential does NOT
// reach a running Codex. Codex reads its MCP servers out of
// CODEX_HOME/config.toml at startup, and CODEX_HOME arrives here in
// Spec.Env. That is also why Spec.MCPConfigPath is not passed on any
// argv below -- there is no flag for it, and inventing one would fail
// silently as an unknown argument.
type codexAppServer struct {
	spec Spec
	conn *jsonrpcConn
	// knobs are the session's level in Codex's words, settled in Start
	// before the process exists and put on the thread when it opens.
	knobs Knobs

	mu sync.Mutex
	// started is guarded because Close is the cancel path's job and can
	// land while a Turn is still in flight; an unguarded flag makes
	// that ordinary sequence a data race rather than a clean refusal.
	started  bool
	threadID string
	turn     *codexTurnState
	// servedModel and servedEffort are what the APP STATED the thread
	// runs at -- its thread/start or thread/resume answer, and a
	// model/rerouted since. A turn reports them only once it has shown the
	// model ran; see result.
	servedModel  string
	servedEffort string
	// rec is the session's recording. It is set before the process starts
	// because the reader goroutine may deliver an item the moment it does.
	rec *recording
}

// Name implements Harness.
func (h *codexAppServer) Name() string { return HarnessCodexAppServer }

// Start launches the app-server and completes the handshake.
//
// THE HANDSHAKE HAPPENS HERE AND THE THREAD DOES NOT. A Codex that is
// not installed, not signed in, or too old for `app-server` fails on
// `initialize`, which is the point: a session that reports itself
// running and then fails its first turn sends an operator to read the
// prompt instead of the install. Opening a thread, by contrast, costs a
// round trip that an attach may not want, so it waits for the first
// Turn.
func (h *codexAppServer) Start(ctx context.Context, spec Spec) error {
	if err := checkCodexSpec("codex app-server", spec); err != nil {
		return err
	}
	// The level is settled BEFORE the process exists: a level Codex cannot
	// run at is a sentence, and must not also be an app-server started and
	// torn down on somebody's machine for a session that could never run.
	knobs, err := spec.knobs(HarnessCodexAppServer)
	if err != nil {
		return fmt.Errorf("codex app-server: %w", err)
	}
	// Launch the resolved executable: Codex re-execs itself for sandboxed
	// filesystem helpers. A symlink outside the read grant is not traversable,
	// even when the binary it points to is granted explicitly.
	if path, lookupErr := exec.LookPath(spec.Binary); lookupErr == nil {
		if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
			spec.Binary = resolved
		}
	}
	h.spec = spec
	h.knobs = knobs
	h.rec = newRecording()

	proc, err := spec.Launch(ctx, spec.Workspace, []string{spec.Binary, "app-server"}, spec.Env, true)
	if err != nil {
		return fmt.Errorf("codex app-server: starting %s failed: %w", spec.Binary, err)
	}
	h.conn = newJSONRPCConn(proc, h.handleNotification)

	if _, err := h.conn.call(ctx, "codex app-server", codexMethodInitialize, codexInitializeParams()); err != nil {
		return err
	}
	// The server rejects every other method until this lands, and it is
	// a notification, so nothing acknowledges it.
	if err := h.conn.notifyServer(codexMethodInitialized, nil); err != nil {
		return fmt.Errorf("codex app-server: completing the handshake failed: %w", err)
	}

	h.mu.Lock()
	h.started = true
	h.mu.Unlock()
	return nil
}

// codexInitializeParams is the handshake.
//
// clientInfo is required (v1::InitializeParams). Naming ourselves honestly matters
// more than it looks: it is what an operator sees in Codex's own logs when they go
// looking for what drove a session they did not start.
func codexInitializeParams() map[string]any {
	return map[string]any{
		"clientInfo": map[string]any{
			"name":    "memql-cockpit",
			"title":   "MemQL Cockpit",
			"version": "1",
		},
		"capabilities": map[string]any{},
	}
}

// Close implements Harness and is idempotent.
func (h *codexAppServer) Close() error {
	h.mu.Lock()
	h.started = false
	h.mu.Unlock()
	if h.conn != nil {
		h.conn.close()
	}
	return nil
}

func (h *codexAppServer) running() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.started
}

// Turn opens or continues the thread and runs one prompt to completion.
func (h *codexAppServer) Turn(ctx context.Context, prompt string, sink Sink) (TurnResult, error) {
	if h.conn == nil || !h.running() {
		return TurnResult{}, errors.New("codex app-server: Turn was called outside the Start/Close lifecycle; the session has no app-server to talk to")
	}

	h.conn.setSink(sink)
	defer h.conn.setSink(nil)

	if err := h.ensureThread(ctx); err != nil {
		return h.result(nil), err
	}

	state := newCodexTurnState()
	h.mu.Lock()
	h.turn = state
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.turn = nil
		h.mu.Unlock()
	}()
	h.rec.startTurn()
	// The calls this turn started and never finished are recorded when it
	// ends, whichever way it ends -- and before the sink is detached, which
	// is the deferred call registered first and so run last.
	defer h.flushRecording()

	params := map[string]any{
		"threadId": h.currentThread(),
		// UserInput::Text. `text_elements` is snake_case even here
		// (turn.rs tags the enum's VARIANTS camelCase, not its fields)
		// and carries `#[serde(default)]`, so it is left off rather
		// than sent empty.
		"input": []map[string]any{{"type": "text", "text": prompt}},
	}
	if schema := strings.TrimSpace(h.spec.ResponseSchema); schema != "" {
		// Sent as the caller wrote it. The app-server forwards it
		// upstream as `text.format` with `"type":"json_schema"` and
		// `"strict":true`, so this is the one place a Codex answer is
		// actually CONSTRAINED rather than merely requested
		// (app-server/tests/suite/v2/output_schema.rs).
		params["outputSchema"] = json.RawMessage(schema)
	}

	res, err := h.conn.call(ctx, "codex app-server: the turn", codexMethodTurnStart, params)
	if err != nil {
		return h.result(state), err
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	// A response this build cannot read leaves the id empty, which the
	// turn state treats as "match anything" rather than as "match
	// nothing" -- see codexTurnState.match. Failing the turn here
	// instead would throw away an answer the app is already producing.
	_ = json.Unmarshal(res, &started)
	state.setTurnID(started.Turn.ID)

	select {
	case <-state.done:
	case <-h.conn.dead:
		// The error is built FIRST because building it drains stderr,
		// and the result below should be gathered from a connection
		// that has finished talking.
		err := h.conn.exitError("codex app-server: the turn")
		return h.result(state), err
	case <-ctx.Done():
		return h.result(state), fmt.Errorf("codex app-server: the turn: %w", ctx.Err())
	}

	out := h.result(state)
	status, reason := state.outcome()
	out.Readiness = codexTurnReadiness(status, reason, state.failureInfo())
	if reason == "" {
		reason = "the app gave no reason"
	}
	// The schema'd answer is read before the status decides the outcome,
	// because it rides a failure too: AppSessionEnd keeps result_json apart
	// from the error so a turn that produced its final answer and was then
	// marked failed still hands the answer back. The outputSchema makes the
	// final message the answer or nothing (strict json_schema upstream), so
	// text that parses is the answer rather than a fragment of one.
	schema := strings.TrimSpace(h.spec.ResponseSchema) != ""
	structured, parsed := codexJSONValue(out.Text)
	if schema && parsed {
		out.ResultJSON = structured
	}
	switch status {
	case codexTurnCompleted:
		// The only status that reaches the structured check below.
	case codexTurnInterrupted:
		return out, fmt.Errorf("codex app-server: the turn was interrupted: %s", reason)
	case codexTurnFailed:
		return out, fmt.Errorf("codex app-server: the turn failed: %s", reason)
	default:
		// A status this build has not heard of is a FAILURE rather than
		// a success, because the alternative is reporting an outcome
		// nobody here understood as a finished answer.
		return out, fmt.Errorf("codex app-server: the turn ended as %q: %s", status, reason)
	}
	if schema && !parsed {
		return out, ErrNoStructuredResult
	}
	return out, nil
}

// ensureThread opens the thread the session's turns run on, once.
func (h *codexAppServer) ensureThread(ctx context.Context) error {
	if h.currentThread() != "" {
		return nil
	}

	method := codexMethodThreadStart
	params := map[string]any{
		"cwd": h.spec.Workspace,
		// NEVER is not a convenience. There is no person on this end of
		// the pipe, so an approval request can only be refused (see
		// refuseRequest), and a thread that asks is a thread that
		// wastes a round trip to be told no. The session's CODEX_HOME
		// config grants its authenticated MCP tool list and confines
		// native commands to the approved workspace. Leave the sandbox
		// unset here so that permission profile remains authoritative.
		"approvalPolicy": "never",
	}
	if ref := strings.TrimSpace(h.spec.ResumeRef); ref != "" {
		method = codexMethodThreadResume
		params = map[string]any{"threadId": ref, "cwd": h.spec.Workspace}
	}
	if h.spec.InferenceOnly {
		// The supplied prompt owns the instructions for this single invocation.
		// Do not inherit the coding-agent personality or workspace discovery.
		params["baseInstructions"] = "Follow the supplied prompt and return its requested answer."
		params["developerInstructions"] = ""
		params["config"] = map[string]any{"features.shell_tool": false, "features.apply_patch_freeform": false, "features.multi_agent": false, "web_search": "disabled"}
	}
	h.applyKnobs(params)

	res, err := h.conn.call(ctx, "codex app-server: opening the thread", method, params)
	if err != nil {
		return err
	}
	// WHAT THE APP STATES, NOT WHAT WAS SENT. Both methods answer the
	// thread's settings beside the thread -- `model` and `reasoningEffort`
	// (ThreadStartResponse / ThreadResumeResponse, 0.153.4) -- and those
	// are what a turn reports. They are the app's statement of its
	// configuration rather than per-turn telemetry (Codex's own Thread
	// docs say so, and it echoes even a name no model answers to), which
	// is why result reports them only for a turn that showed the model
	// ran. reasoningEffort is null when the thread has none set, and null
	// stays empty.
	var out struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		Model           string  `json:"model"`
		ReasoningEffort *string `json:"reasoningEffort"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return fmt.Errorf("codex app-server: %s answered with something this build cannot read: %w", method, err)
	}
	if out.Thread.ID == "" {
		// Without an id there is no continuation and no attach: every
		// later turn would open a fresh conversation and the session
		// would silently become a series of strangers.
		return fmt.Errorf("codex app-server: %s returned no thread id, so this session cannot be continued", method)
	}
	h.mu.Lock()
	h.threadID = out.Thread.ID
	h.servedModel = strings.TrimSpace(out.Model)
	h.servedEffort = ""
	if out.ReasoningEffort != nil {
		h.servedEffort = strings.TrimSpace(*out.ReasoningEffort)
	}
	h.mu.Unlock()
	return nil
}

// applyKnobs puts the session's level on a thread/start or thread/resume.
//
// The model is the typed `model` both methods take. The effort rides
// `config` as model_reasoning_effort -- the configuration key the design
// record names, spelled as Codex 0.153.4 reads it from config.toml -- and
// NOT as turn/start's `effort`, because the THREAD is where the app states
// what it runs at: its answer carries `reasoningEffort`, and an effort sent
// per turn would leave that statement describing a setting the turns no
// longer use. A knob left empty is left out, and Codex decides it exactly
// as it did before levels existed -- from its OWN defaults, not the
// operator's ~/.codex/config.toml: the session runs under a per-session
// CODEX_HOME that holds only MemQL's MCP server and a link to auth.json
// (appsession/mcpconfig.go, layoutCodex), so "unset" here means the
// account's default model at that model's default effort.
func (h *codexAppServer) applyKnobs(params map[string]any) {
	if h.knobs.Model != "" {
		params["model"] = h.knobs.Model
	}
	if h.knobs.Effort != "" {
		config, _ := params["config"].(map[string]any)
		if config == nil {
			config = map[string]any{}
		}
		config["model_reasoning_effort"] = h.knobs.Effort
		params["config"] = config
	}
}

// served is the thread's model and effort as the app last stated them.
func (h *codexAppServer) served() (model, effort string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.servedModel, h.servedEffort
}

func (h *codexAppServer) currentThread() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.threadID
}

// result gathers whatever the turn produced, including when it failed.
//
// A turn that ran and failed still reports its text, its usage and its
// thread, because a caller that gets nothing back cannot tell a refusal
// from a crash.
func (h *codexAppServer) result(state *codexTurnState) TurnResult {
	out := TurnResult{AppSessionRef: h.currentThread(), ExitCode: h.conn.exitCode()}
	if state != nil {
		out.Text = state.finalText()
		out.Usage = state.spend()
		// The thread's stated settings are what SERVED only once the turn
		// shows the model ran. Before that they are what the thread was
		// configured with -- and a configured model that failed the turn
		// before answering (a name no model answers to, say) served nothing.
		// A reroute this turn announced replaces the model for this turn
		// alone.
		if state.ranTheModel() {
			out.Model, out.Effort = h.served()
			if to := state.reroutedTo(); to != "" {
				out.Model = to
			}
		}
	}
	return out
}

// handleNotification maps one server notification onto the sink's
// streams and onto the turn in flight.
//
// It runs on the reader goroutine, so it does no work that can block.
func (h *codexAppServer) handleNotification(method string, params json.RawMessage, raw []byte) {
	h.mu.Lock()
	state := h.turn
	h.mu.Unlock()

	switch method {
	case codexNotifyAgentMessageDelta:
		var n struct {
			TurnID string `json:"turnId"`
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		if json.Unmarshal(params, &n) != nil || n.Delta == "" {
			break
		}
		if state != nil {
			state.noteDelta(n.ItemID)
			if state.match(n.TurnID) && state.answerDelta(n.ItemID) {
				h.conn.emit(StreamAnswer, []byte(n.Delta))
			}
		}
		h.conn.emit(StreamText, []byte(n.Delta))
		return

	case codexNotifyItemStarted, codexNotifyItemCompleted:
		var n struct {
			TurnID string          `json:"turnId"`
			Item   json.RawMessage `json:"item"`
		}
		if json.Unmarshal(params, &n) != nil || len(n.Item) == 0 {
			break
		}
		kind, id, text, phase := codexReadItem(n.Item)
		if kind == codexItemAgentMessage && state != nil && state.match(n.TurnID) {
			state.mu.Lock()
			state.phases[id] = phase
			sent := state.answerDeltas[id]
			state.mu.Unlock()
			if method == codexNotifyItemCompleted && (phase == codexPhaseFinal || phase == codexPhaseFinalAlt) && !sent {
				h.conn.emit(StreamAnswer, []byte(text))
			}
		}
		if codexToolItems[kind] {
			h.conn.emit(StreamTool, n.Item)
			h.recordItem(method, n.Item)
			return
		}
		// A call this build routes as progress rather than as tool
		// activity -- an image view, a sub-agent, a sleep, a type it has
		// never seen -- is still a call, and still recorded; anything else
		// is ignored there. Deferred, so the action follows the app's own
		// event line as it follows a tool chunk above.
		defer h.recordItem(method, n.Item)
		if kind == codexItemAgentMessage && method == codexNotifyItemCompleted {
			if state != nil && state.match(n.TurnID) {
				state.addMessage(text, phase)
			}
			// The prose reaches the person exactly once. Deltas are
			// the live path, so the completed item only carries the
			// text when no delta did -- a server that streams and a
			// server that does not both produce one readable answer,
			// and neither produces it twice.
			if state == nil || !state.sawDelta(id) {
				h.conn.emit(StreamText, []byte(text))
				return
			}
		}

	case codexNotifyTokenUsage:
		var n struct {
			TurnID     string `json:"turnId"`
			TokenUsage struct {
				Total codexUsageBreakdown `json:"total"`
				Last  codexUsageBreakdown `json:"last"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(params, &n) == nil && state != nil && state.match(n.TurnID) {
			state.addUsage(n.TokenUsage.Total, n.TokenUsage.Last)
		}

	case codexNotifyTurnCompleted:
		var n struct {
			Turn struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Error  *struct {
					Message string          `json:"message"`
					Info    json.RawMessage `json:"codexErrorInfo"`
				} `json:"error"`
				Items []json.RawMessage `json:"items"`
			} `json:"turn"`
		}
		// THE CHUNK GOES OUT BEFORE THE TURN IS RELEASED. This
		// notification is the only thing that lets Turn return, and
		// Turn detaches the sink the instant it does -- so recording
		// the completion first would race this emit against that
		// detach, and the chunk that says the turn finished is exactly
		// the one a transcript cannot do without.
		h.conn.emit(StreamEvent, raw)
		if json.Unmarshal(params, &n) == nil && state != nil {
			var reason string
			if n.Turn.Error != nil {
				reason = n.Turn.Error.Message
				state.noteFailureInfo(n.Turn.Error.Info)
			}
			state.complete(n.Turn.ID, n.Turn.Status, reason, n.Turn.Items)
		}
		return

	case codexNotifyError:
		var n struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			WillRetry bool `json:"willRetry"`
		}
		// A retryable error is the app coping, not the app failing; only
		// a terminal one is worth keeping as the reason a turn ended.
		if json.Unmarshal(params, &n) == nil && !n.WillRetry && state != nil {
			state.noteError(n.Error.Message)
		}

	case codexNotifyModelRerouted:
		var n struct {
			TurnID  string `json:"turnId"`
			ToModel string `json:"toModel"`
		}
		// The app moved THIS turn to another model. The notification names
		// the turn and its one reason on 0.153.4 (highRiskCyberActivity) is
		// about a request, so the reroute is kept on the turn rather than
		// the thread: a later turn that was not rerouted ran on the thread's
		// own model again. The thread's effort is not restated. The
		// notification still reaches the transcript below -- a person
		// reading a session that moved models should see that it moved.
		if json.Unmarshal(params, &n) == nil && state != nil && state.match(n.TurnID) {
			state.noteReroute(n.ToModel)
		}

	case codexNotifyThreadSettings:
		var n struct {
			ThreadSettings struct {
				Model  string  `json:"model"`
				Effort *string `json:"effort"`
			} `json:"threadSettings"`
		}
		// The app restating the thread's settings after they changed
		// (ThreadSettingsUpdatedNotification, 0.153.4) -- a newer statement
		// of the same thing thread/start answered, so it replaces it.
		if json.Unmarshal(params, &n) == nil {
			if m := strings.TrimSpace(n.ThreadSettings.Model); m != "" {
				effort := ""
				if n.ThreadSettings.Effort != nil {
					effort = strings.TrimSpace(*n.ThreadSettings.Effort)
				}
				h.mu.Lock()
				h.servedModel, h.servedEffort = m, effort
				h.mu.Unlock()
			}
		}
	}

	h.conn.emit(StreamEvent, raw)
}

// recordItem feeds one item to the session's recording: a started item
// opens a call, a completed one closes it.
func (h *codexAppServer) recordItem(method string, raw json.RawMessage) {
	call, item, ok := codexItemCall(raw, h.spec.Workspace)
	if !ok {
		return
	}
	switch method {
	case codexNotifyItemStarted:
		h.rec.begin(call)
	case codexNotifyItemCompleted:
		// Sent under the recording's lock (completeAndEmit): this runs on
		// the reader while the turn's flush runs on its own goroutine, and
		// the order on the wire has to be the order of the seq.
		h.rec.completeAndEmit(call.ID, func(a *Action) {
			*a = mergeCall(*a, call)
			codexItemFinish(a, item)
		}, h.conn.record)
	}
}

// flushRecording records the calls the turn started and never finished.
func (h *codexAppServer) flushRecording() {
	h.rec.flushAndEmit(h.conn.record)
}

// codexReadItem pulls the four things anything asks of a ThreadItem.
func codexReadItem(raw json.RawMessage) (kind, id, text, phase string) {
	var item struct {
		Type  string `json:"type"`
		ID    string `json:"id"`
		Text  string `json:"text"`
		Phase string `json:"phase"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return "", "", "", ""
	}
	return item.Type, item.ID, item.Text, item.Phase
}

// --- one turn's accumulated facts -----------------------------------

// codexUsageBreakdown is TokenUsageBreakdown.
//
// Source:
// codex-rs/app-server-protocol/schema/typescript/v2/TokenUsageBreakdown.ts.
type codexUsageBreakdown struct {
	TotalTokens  int64 `json:"totalTokens"`
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
}

// codexMessage is one assistant message and how Codex classified it.
type codexMessage struct {
	text  string
	phase string
}

// codexTurnState is everything one turn reported, gathered off the
// notification stream.
//
// THE TURN ID ARRIVES AFTER THE NOTIFICATIONS CAN. `turn/start`'s
// response names the turn, but nothing orders that response against the
// notifications the turn is already producing, so this state accepts
// facts before it knows which turn it is and reconciles when the id
// lands.
type codexTurnState struct {
	mu     sync.Mutex
	turnID string
	// idKnown separates "not told yet" from "told, and it was empty".
	idKnown bool

	messages     []codexMessage
	deltas       map[string]bool
	phases       map[string]string
	answerDeltas map[string]bool

	usage     Usage
	lastTotal int64
	haveTotal bool

	status    string
	reason    string
	items     []json.RawMessage
	completed bool
	// rerouted is the model a model/rerouted moved this turn to.
	rerouted string
	// info is the failed turn's codexErrorInfo, kept as printed: a word such as
	// usageLimitExceeded, or an object holding an HTTP status.
	info json.RawMessage
	// pendingID holds a completion that arrived before the turn had a
	// name, so it can be matched once it does.
	pendingID string

	done     chan struct{}
	doneOnce sync.Once
}

func newCodexTurnState() *codexTurnState {
	return &codexTurnState{deltas: make(map[string]bool), phases: make(map[string]string), answerDeltas: make(map[string]bool), done: make(chan struct{})}
}

// match reports whether a notification belongs to this turn.
//
// An unknown id on either side matches, because the alternative is
// worse: an older app-server that omits the id, or a notification that
// beat the response, would otherwise produce a turn that finished with
// no text, no usage and no explanation. Only one turn runs on this
// connection at a time, so a false match is not reachable in practice
// while a false MISS silently empties the result.
func (t *codexTurnState) match(turnID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.matchLocked(turnID)
}

func (t *codexTurnState) matchLocked(turnID string) bool {
	return t.turnID == "" || turnID == "" || turnID == t.turnID
}

func (t *codexTurnState) setTurnID(id string) {
	t.mu.Lock()
	t.turnID = id
	t.idKnown = true
	t.mu.Unlock()
	t.settle()
}

func (t *codexTurnState) noteDelta(itemID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if itemID != "" {
		t.deltas[itemID] = true
	}
}

func (t *codexTurnState) answerDelta(itemID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	phase := t.phases[itemID]
	if phase != codexPhaseFinal && phase != codexPhaseFinalAlt {
		return false
	}
	t.answerDeltas[itemID] = true
	return true
}

func (t *codexTurnState) sawDelta(itemID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.deltas[itemID]
}

func (t *codexTurnState) addMessage(text, phase string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.messages = append(t.messages, codexMessage{text: text, phase: phase})
}

// addUsage folds one usage update into this turn's spend.
//
// THE TURN'S SPEND IS THE SUM OF THE `last` DELTAS, and neither of the
// two numbers on the wire is it. `total` is CUMULATIVE FOR THE THREAD
// (TokenUsageInfo::append_last_usage does `total += last`), so billing
// it charges the second turn for the first; `last` is only the most
// recent upstream completion, so billing it under-reports every turn
// that took more than one model request -- which is every turn that used
// a tool. Adding the deltas reproduces exactly what `total` gained while
// this turn ran.
//
// The `total` guard is for a repeat. The app-server replays a stored
// usage record to a client that attaches to a thread
// (app-server/src/request_processors/token_usage_replay.rs), and a
// repeated `last` added twice is a turn billed twice. A total that did
// not move means nothing new was appended, so there is nothing to add.
func (t *codexTurnState) addUsage(total, last codexUsageBreakdown) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.haveTotal && total.TotalTokens == t.lastTotal {
		return
	}
	t.lastTotal = total.TotalTokens
	t.haveTotal = true
	t.usage.InputTokens += last.InputTokens
	t.usage.OutputTokens += last.OutputTokens
	// Known is set only here. A turn the app said nothing about keeps
	// it false, and the engine files that as billing "unknown" rather
	// than as a free call.
	t.usage.Known = true
}

func (t *codexTurnState) noteError(message string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if message != "" {
		t.reason = message
	}
}

func (t *codexTurnState) noteFailureInfo(info json.RawMessage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.info = info
}

func (t *codexTurnState) failureInfo() json.RawMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.info
}

func (t *codexTurnState) complete(turnID, status, reason string, items []json.RawMessage) {
	t.mu.Lock()
	if !t.matchLocked(turnID) {
		t.mu.Unlock()
		return
	}
	t.status = status
	if reason != "" {
		t.reason = reason
	}
	t.items = items
	t.completed = true
	t.pendingID = turnID
	t.mu.Unlock()
	t.settle()
}

// settle releases Turn once the completion and the turn's identity have
// both arrived, in whichever order they did.
func (t *codexTurnState) settle() {
	t.mu.Lock()
	ready := t.completed && (t.idKnown || t.turnID != "")
	t.mu.Unlock()
	if ready {
		t.doneOnce.Do(func() { close(t.done) })
	}
}

func (t *codexTurnState) outcome() (status, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status, t.reason
}

func (t *codexTurnState) spend() Usage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.usage
}

// ranTheModel reports whether this turn shows the model actually ran: it
// completed, or it reported spend. Tokens spent are tokens a model produced,
// so an interrupted or failed turn that reported usage still ran on the
// thread's model; one that did neither may have failed before any model
// answered, and names none.
func (t *codexTurnState) ranTheModel() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return (t.completed && t.status == codexTurnCompleted) || t.usage.Known
}

// noteReroute records the model the app moved this turn to.
func (t *codexTurnState) noteReroute(model string) {
	if m := strings.TrimSpace(model); m != "" {
		t.mu.Lock()
		t.rerouted = m
		t.mu.Unlock()
	}
}

// reroutedTo is the model this turn was moved to, or "" when it was not.
func (t *codexTurnState) reroutedTo() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rerouted
}

// finalText picks the assistant message that is the ANSWER.
//
// The rule is Codex's own, from its Python SDK
// (sdk/python/src/openai_codex/_run.py,
// `_final_assistant_response_from_items`): the LAST message marked as
// the final answer wins, and when no message carries a phase at all --
// which MessagePhase's own documentation says happens, because
// "providers do not emit this consistently" -- the last unclassified
// message stands in. Reimplementing it differently would make MemQL
// disagree with Codex about what Codex said.
//
// Messages accumulated from `item/completed` are preferred over the ones
// on `turn/completed`, because that payload's `itemsView` may say
// `notLoaded` or `summary` and a summary is not the answer.
func (t *codexTurnState) finalText() string {
	t.mu.Lock()
	messages := t.messages
	items := t.items
	t.mu.Unlock()

	if len(messages) == 0 {
		for _, raw := range items {
			if kind, _, text, phase := codexReadItem(raw); kind == codexItemAgentMessage {
				messages = append(messages, codexMessage{text: text, phase: phase})
			}
		}
	}

	var fallback string
	var haveFallback bool
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.phase == codexPhaseFinal || m.phase == codexPhaseFinalAlt {
			return m.text
		}
		if m.phase == "" && !haveFallback {
			fallback, haveFallback = m.text, true
		}
	}
	return fallback
}

// --- shared helpers -------------------------------------------------

// checkCodexSpec refuses a Spec that cannot produce a running app.
//
// A nil Launch in particular is a programming error rather than a reason
// to reach for os/exec: falling back would fork a child outside the
// app-session supervisor's process group, and a cancel that reaps only
// the direct child leaves an agent running on somebody's machine with
// nothing watching it.
func checkCodexSpec(who string, spec Spec) error {
	switch {
	case spec.Launch == nil:
		return errors.New(who + ": the spec carries no launcher, so this harness has no supervised way to start a process")
	case strings.TrimSpace(spec.Binary) == "":
		return errors.New(who + ": the spec names no binary to run")
	case strings.TrimSpace(spec.Workspace) == "":
		return errors.New(who + ": the spec names no workspace, and a coding app with no working directory would run somewhere nobody chose")
	}
	return nil
}

// codexJSONValue returns text as JSON when it is JSON.
//
// Nothing is synthesised here. A structured result the app did not
// produce would read downstream as "the app answered nothing", which is
// a different event from "the app answered in prose" and retries
// differently.
func codexJSONValue(text string) ([]byte, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return nil, false
	}
	return []byte(trimmed), true
}
