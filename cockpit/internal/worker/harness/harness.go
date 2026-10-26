// Package harness drives one coding app through its OWN protocol, so the
// cockpit stops reading terminal output and starts reading results.
//
// WHY THIS PACKAGE EXISTS. Until now an app session was one argv with
// stdin closed, and every piece of structure the engine wanted was
// recovered by guessing at stdout: chunks were classified by "does this
// line parse as JSON", a Codex run reported no usage at all because
// `codex exec` prints none, and there was no final answer -- only a
// transcript and an exit code. That is a parser for a format nobody
// promised to keep. Both apps ship a real protocol for exactly this
// (Claude Code's stream-json with a resumable session id, Codex's
// app-server JSON-RPC), and speaking them turns three guesses into three
// facts: what the app said, what it spent, and which session to continue.
//
// A TURN IS THE UNIT, NOT A RUN. The engine's app door needs to send a
// follow-up into a session it already opened (design D7). Neither app
// offers a process you can keep feeding: Claude Code's headless mode is
// one prompt per process, resumed by id, and Codex's app-server takes one
// turn per request over a connection it owns. So `Turn` is the verb here
// and a session is a sequence of them -- which is also what makes a
// mid-session credential renewal honest, because the replacement lands
// when the next process starts rather than pretending to reach a running
// one.
//
// THE HARNESS NEVER FORKS. Every process comes from a Launcher the caller
// injects, because the cockpit already has exactly one process supervisor
// (internal/worker/appsession/process.go: own process group, partial-line
// flush, SIGTERM then SIGKILL) and a second one would diverge from it
// quietly. A cancel that reaps only the direct child leaves an agent
// running on somebody's machine with nothing watching it, and that bug is
// worth having in one place rather than two.
//
// EVERY UNKNOWN STAYS UNKNOWN. Usage the app did not report is reported
// as not known rather than estimated -- the engine writes it to a ledger
// somebody bills from, and an estimate recorded as a measurement is worse
// than a gap. The same rule governs the structured result: no schema
// asked for, or nothing that parses, means no result, never an empty
// object that reads downstream as "the app answered nothing".
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"io"
)

// Harness names, mirrored on the wire as AppDescriptor.harness so the
// engine never attempts a protocol this machine cannot speak.
//
// Two of them are Codex. That is deliberate and is the reason the engine
// must read the harness word rather than infer it from the app id: a
// Codex old enough to lack `app-server` is still perfectly drivable
// through `codex mcp-server`, with a thread id instead of usage, and the
// cockpit is the only side that can tell which one it has.
const (
	// HarnessClaudeHeadless is `claude -p` with --resume, one process
	// per turn.
	HarnessClaudeHeadless = "claude-headless"
	// HarnessCodexAppServer is `codex app-server`, JSON-RPC over stdio.
	HarnessCodexAppServer = "codex-app-server"
	// HarnessCodexMCP is `codex mcp-server`, the codex / codex-reply
	// tool pair over stdio MCP. The fallback.
	HarnessCodexMCP = "codex-mcp"
)

// MCPServerName is what MemQL calls itself in every app's MCP
// configuration.
//
// It lives here rather than beside the configuration writer
// (appsession/mcpconfig.go) because a harness now reads it too: Claude
// Code's permission grant names the server (`mcp__memql`) and its init
// event reports the server's status by that name. Two spellings of one
// name are two things a rename can pull apart in silence, and the
// symptom would be a session whose MemQL tools are all refused.
const MCPServerName = "memql"

// Chunk streams a harness emits. The first three are new here; stdout and
// stderr keep the meaning they had, so an operator reading a transcript
// from before this change reads the same words after it.
//
// The split matters to the engine, which maps `event` onto a progress
// event and shows `text` to a person. Handing it one undifferentiated
// stream is what forced the "does it parse as JSON" test that this
// package exists to delete.
const (
	// StreamEvent is a JSON object describing what the app is doing.
	StreamEvent = "event"
	// StreamText is assistant prose meant for a human to read.
	StreamText = "text"
	// StreamAnswer contains only final-answer prose, never commentary or reasoning.
	StreamAnswer = "answer"
	// StreamTool is a tool call or its result, as the app reported it.
	StreamTool = "tool"
	// StreamStdout is everything the process printed that the protocol
	// did not account for. It is kept rather than dropped: an app that
	// dies while printing a stack trace prints it here.
	StreamStdout = "stdout"
	// StreamStderr is the process's stderr, verbatim.
	StreamStderr = "stderr"
)

// ErrNoStructuredResult reports that a turn finished without the
// structured answer a schema asked for.
//
// It is a named error rather than an empty TurnResult.ResultJSON because
// the two outcomes bill and retry differently: an app that ran, spent
// tokens and answered in prose is not the same event as one that never
// started, and a caller that cannot tell them apart will retry the wrong
// one.
var ErrNoStructuredResult = errors.New("harness: the turn produced no structured result")

// Sink receives a turn's output as it happens.
//
// Chunk is called from the harness's own goroutines and must not block
// for long; the session's chunk sender is the intended implementation and
// it is already sequenced. An error is not returned because there is
// nothing a harness could do with one -- the stream to the server dying
// is the session's problem, and it is watching for it.
//
// Record receives the RECORDING: one Action for every tool call the app
// completed (record.go). It is a method of its own rather than one more
// stream word because the two are bounded differently -- the session caps
// narration at limits.max_transcript_bytes and must never cap the
// recording -- and a stream word is one typo away from being the other.
type Sink interface {
	Chunk(stream string, data []byte)
	Record(a Action)
}

// SinkFunc adapts a function to Sink.
//
// It has no recording of its own to keep apart, so an Action reaches the
// function as the event chunk it would be on the wire. A caller that
// wants the recording sent past the transcript cap implements Sink
// itself, as the app-session runner does.
type SinkFunc func(stream string, data []byte)

// Chunk implements Sink.
func (f SinkFunc) Chunk(stream string, data []byte) {
	if f != nil {
		f(stream, data)
	}
}

// Record implements Sink.
func (f SinkFunc) Record(a Action) {
	if f == nil {
		return
	}
	body, err := json.Marshal(a)
	if err != nil {
		return
	}
	f(StreamEvent, append(body, '\n'))
}

// Usage is what the app REPORTED about its own spend.
//
// Known is the whole point of the struct. Zeroes with Known false are
// recorded by the engine as billing "unknown"; zeroes with Known true are
// recorded as a free call. A harness that cannot read usage out of its
// protocol leaves Known false and says so, rather than picking whichever
// reading looks tidier.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	Known        bool
}

// TurnResult is what one turn produced.
type TurnResult struct {
	// Text is the app's final prose answer, when it gave one.
	Text string
	// ResultJSON is the structured final answer, present only when the
	// Spec asked for one with a schema AND the app produced something
	// that parsed against it. Never a synthesised empty object.
	ResultJSON []byte
	// AppSessionRef is the app's OWN session identifier -- Claude Code's
	// session_id, Codex's thread id. It is what makes the NEXT turn a
	// continuation rather than a fresh start, and what a later
	// kind=attach resumes, so a harness that loses it has silently
	// turned a conversation into a series of strangers.
	AppSessionRef string
	// Usage is the app's own accounting for this turn alone.
	Usage Usage
	// ExitCode is the process's real exit status, unnormalised, for the
	// harnesses that run a process per turn. The engine reads non-zero
	// as a FAILED run, so flattening a 2 to a 1 misfiles the outcome.
	ExitCode int
	// Model is the model the APP REPORTED this turn running on, and empty
	// when it said nothing. It is never the model this harness asked for:
	// a request is not a report, and the engine records this value as the
	// model that SERVED (design D9), so a copy of the request would be
	// recorded as a measurement nobody made.
	Model string
	// Effort is the reasoning effort the APP REPORTED, under the same
	// rule. Claude Code's headless output states no effort anywhere
	// (verified on 2.1.270), so a Claude Code turn leaves this empty even
	// when --effort was passed -- which is also the answer for an app that
	// ignored the effort it was given.
	Effort string
}

// Spec is everything a harness needs to run a session's turns.
type Spec struct {
	// InferenceOnly executes a prompt without coding tools or MCP.
	InferenceOnly bool
	// Binary is the app's name on PATH.
	Binary string
	// Workspace is the absolute directory every process runs in.
	Workspace string
	// Env is added to the worker's environment for every process --
	// CODEX_HOME for Codex, nothing for Claude Code today.
	Env []string
	// MCPConfigPath is the file the cockpit already wrote with the
	// per-run bearer in it. Harnesses pass it to the app; none of them
	// reads it, and none of them ever logs its contents.
	MCPConfigPath string
	// ResponseSchema is a JSON Schema, as text, for the structured final
	// answer. Empty means the engine asked for none, and a harness must
	// then not invent one -- a schema the caller did not ask for changes
	// what the app says.
	ResponseSchema string
	// ResumeRef starts the session already attached to the app's own
	// prior session, for the attach kind. Empty starts fresh.
	ResumeRef string
	// Level is the engine's level for this session -- one of core/airoute's
	// words, from AppSessionStart.level. Empty runs the app at its own
	// defaults. A harness turns it into the app's knobs through Levels in
	// Start and refuses a level it cannot translate (levels.go).
	Level string
	// Levels translates Level into this app's knobs. Nil means the
	// harness's built-in table (BuiltinLevels); the session passes the
	// built-in table with the machine owner's policy.yaml entries laid
	// over it.
	Levels Table
	// Model and Effort are explicit per-session overrides from the owner.
	// Empty fields retain the level's value. They never change the table.
	Model  string
	Effort string
	// DenyPaths are absolute paths the app must neither READ nor write:
	// this worker's own files (its tokens, and policy.yaml, whose
	// apps.allow is the app consent gate) and this machine's fs.deny list.
	// The session refuses a workspace that overlaps one, so none is ever
	// inside the directory the app works in.
	//
	// Claude Code's harness hands them to the app as deny rules and as its
	// sandbox's own filesystem lists (claudeGrantArgs). The Codex harnesses
	// do not yet: Codex runs under the sandbox its own configuration names.
	DenyPaths []string
	// Launch forks every process this harness needs. Required: a nil
	// Launch is a programming error rather than a reason to fall back
	// to os/exec, because falling back would silently lose the process
	// group that makes cancel mean cancel.
	Launch LaunchFunc
}

// LaunchFunc starts one supervised process.
//
// stdin selects whether the child gets a writable stdin. It defaults off
// and each harness opts in for itself: the engine's stream never reaches
// an app's stdin (an app that can be typed at by a remote caller is a
// different and much larger trust question), but a JSON-RPC client that
// owns both ends of a pipe it opened is not that -- it is the protocol. A
// print-mode harness may also send one complete prompt and close the pipe;
// follow-ups remain separately authorized turns, never writes to that pipe.
type LaunchFunc func(ctx context.Context, dir string, argv []string, env []string, stdin bool) (Process, error)

// Process is a running app process, as a harness needs it.
//
// The interface is the seam that keeps this package from forking: the
// production implementation is the app-session supervisor, and the tests'
// implementation is a plain os/exec wrapper over a fake binary. Nothing
// here reaps, signals or process-groups on its own.
type Process interface {
	// Stdin is the child's stdin, or nil when it was not requested.
	Stdin() io.WriteCloser
	// Stdout and Stderr are the child's streams, readable to EOF.
	Stdout() io.Reader
	Stderr() io.Reader
	// Terminate stops the whole process group.
	Terminate()
	// Wait reaps the process. Safe to call more than once and from more
	// than one goroutine; every caller gets the same answer.
	Wait() error
	// ExitCode is the real exit status once Wait has returned.
	ExitCode() int
}

// Harness runs one app's turns for one session.
//
// The lifecycle is Start, then Turn any number of times, then Close.
// Close is idempotent and is called on every exit path including a failed
// Start, because the thing it releases -- a live app-server process -- is
// the thing a leak leaves running on somebody's laptop.
type Harness interface {
	// Start prepares the harness. For a process-per-turn harness this
	// does no forking at all; for the app-server it starts the server
	// and completes the handshake, so a Codex that cannot start fails
	// HERE, before the session claims to be running.
	Start(ctx context.Context, spec Spec) error
	// Turn sends one prompt and runs it to completion, streaming to
	// sink. The returned error is the turn's failure; a turn that ran
	// and failed still returns whatever TurnResult it gathered, so a
	// caller can report the exit code and the chunks that did arrive.
	Turn(ctx context.Context, prompt string, sink Sink) (TurnResult, error)
	// Name is the harness word from the constants above.
	Name() string
	// Close releases everything. Idempotent.
	Close() error
}

// New builds the harness named by word.
//
// The set is closed and the failure is named, because the alternative --
// falling back to a "default" harness -- would drive an app through the
// wrong protocol and report the resulting silence as an app problem.
func New(word string) (Harness, error) {
	switch word {
	case HarnessClaudeHeadless:
		return &claudeHeadless{}, nil
	case HarnessCodexAppServer:
		return &codexAppServer{}, nil
	case HarnessCodexMCP:
		return &codexMCP{}, nil
	}
	return nil, errors.New("harness: no client for harness " + word)
}
