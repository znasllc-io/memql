package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// claudeheadless_test.go drives the Claude Code client against a fake
// `claude` that prints RECORDED stream-json.
//
// Every fixture in this file was CAPTURED from claude 2.1.263 on
// 2026-09-07 by running the real binary and then trimmed to the fields
// this client reads; each one names the invocation it came from. They
// are recordings rather than inventions because the whole point of this
// package is to stop guessing at an app's output: a fixture written from
// the design record would pass forever against a client that reads a
// field Claude Code does not emit, and the first real run would return
// an empty answer with nothing to say about why.
//
// The argv assertions matter as much as the event ones. Claude Code's
// own parser has two traps that turn a wrong argv into a wrong ANSWER
// rather than an error (see claudeArgv), so every test that runs a turn
// also reads back exactly what the fake was called with.

// --- fixtures -------------------------------------------------------

// claudePongTurn is `claude -p 'Reply with exactly the word: pong'
// --output-format stream-json --verbose --tools "" --model haiku`, run
// on 2026-09-07 against claude 2.1.263. The hook, thinking_tokens and
// rate_limit_event lines it also printed are dropped here; the shapes
// this client reads are the init, the assistant message and the result.
const claudePongTurn = `{"type":"system","subtype":"init","cwd":"/w","session_id":"sess-one","model":"claude-haiku-4-5-20251001","permissionMode":"default"}
{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001","id":"msg_011Ceo","type":"message","role":"assistant","content":[{"type":"text","text":"pong"}],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":4}},"parent_tool_use_id":null,"session_id":"sess-one","uuid":"febefc19"}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"duration_ms":2116,"session_id":"sess-one","total_cost_usd":0.10044700000000001,"usage":{"input_tokens":10,"cache_creation_input_tokens":49966,"cache_read_input_tokens":0,"output_tokens":101},"permission_denials":[],"result":"pong","uuid":"7d42a559"}`

// claudeResumedTurn is the SECOND process of that conversation:
// `claude -p '...' --resume sess-one ...`, same run. The session id is
// unchanged, which is the fact the resume test pins -- Claude Code
// reuses the id it was given unless --fork-session is passed, so a
// client that expected a new one on every turn would resume the first
// turn forever.
const claudeResumedTurn = `{"type":"system","subtype":"init","cwd":"/w","session_id":"sess-one","model":"claude-haiku-4-5-20251001"}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":"pong"}]},"parent_tool_use_id":null,"session_id":"sess-one"}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"session_id":"sess-one","total_cost_usd":0.0055596000000000005,"usage":{"input_tokens":10,"cache_creation_input_tokens":154,"cache_read_input_tokens":49966,"output_tokens":49},"result":"pong","uuid":"58d86f67"}`

// claudeSchemaTurn is the same binary and date with
// `--json-schema '{"type":"object","properties":{"city":{"type":"string"},
// "millions":{"type":"number"}},"required":["city","millions"]}'`.
//
// Two recorded facts drive the client. The structured answer arrives in
// `structured_output` as a parsed object, and `result` becomes that same
// value SERIALISED rather than the prose the model also produced -- so
// the turn's Text is the app's own final answer either way, and
// ResultJSON is not reconstructed from prose.
const claudeSchemaTurn = `{"type":"system","subtype":"init","session_id":"sess-schema"}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":"Paris is the capital of France."}]},"parent_tool_use_id":null,"session_id":"sess-schema"}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_015yq8","name":"StructuredOutput","input":{"city":"Paris","millions":2.2},"caller":{"type":"direct"}}]},"parent_tool_use_id":null,"session_id":"sess-schema"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_015yq8","type":"tool_result","content":"Structured output provided successfully"}]},"parent_tool_use_id":null,"session_id":"sess-schema"}
{"type":"result","subtype":"success","is_error":false,"num_turns":3,"stop_reason":"tool_use","session_id":"sess-schema","total_cost_usd":0.1082524,"usage":{"input_tokens":10,"output_tokens":220},"structured_output":{"city":"Paris","millions":2.2},"result":"{\"city\":\"Paris\",\"millions\":2.2}"}`

// claudeProseTurn is the recorded success shape with `structured_output`
// removed and `result` left as prose: the app ran, spent tokens and
// answered in words when a schema was asked for. Claude Code enforces
// its schema hard enough that this is rare, which is exactly why the
// client must not treat it as an empty object -- an empty object reads
// downstream as "the app answered nothing", and this app answered.
const claudeProseTurn = `{"type":"system","subtype":"init","session_id":"sess-prose"}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":"Paris, about 2.2 million."}]},"parent_tool_use_id":null,"session_id":"sess-prose"}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"session_id":"sess-prose","total_cost_usd":0.004,"usage":{"input_tokens":10,"output_tokens":20},"result":"Paris, about 2.2 million."}`

// claudeSilentUsageTurn is the recorded success shape with the `usage`
// object and `total_cost_usd` removed -- an app that said nothing about
// its own spend. The engine records that as billing "unknown"; a client
// that folded it to zeroes would record it as a free run.
const claudeSilentUsageTurn = `{"type":"system","subtype":"init","session_id":"sess-quiet"}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"session_id":"sess-quiet","result":"done"}`

// claudeBlockTurn carries one of every content block Claude Code emits
// in a `-p` stream, plus a line that is not stream-json at all. The
// thinking block's `signature` is the real base64 shape, shortened; it
// is here so the test can prove it never reaches a chunk.
const claudeBlockTurn = `{"type":"system","subtype":"init","session_id":"sess-blocks"}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":"the user wants the file read","signature":"EosFCrIBCBEYAipAxA2b"}]},"parent_tool_use_id":null,"session_id":"sess-blocks"}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":"Reading it now."}]},"parent_tool_use_id":null,"session_id":"sess-blocks"}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01ab","name":"Read","input":{"file_path":"/w/main.go"}}]},"parent_tool_use_id":null,"session_id":"sess-blocks"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01ab","type":"tool_result","content":"package main"}]},"parent_tool_use_id":null,"session_id":"sess-blocks"}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"},"session_id":"sess-blocks"}
npm warn config production Use --omit=dev instead.
{"type":"result","subtype":"success","is_error":false,"num_turns":2,"session_id":"sess-blocks","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2},"result":"Reading it now."}`

// claudeDiesMidStream is the recorded shape of a run that fails after
// it has already printed: `claude -p hi --resume <unknown id>` on
// 2026-09-07 exited 1, put the reason on STDERR in plain text, and
// still printed a result event with subtype error_during_execution and
// its own `errors` array. The fake exits 3 rather than 1 so the test
// can tell a real exit code from a normalised one.
const claudeDiesMidStream = `{"type":"system","subtype":"init","session_id":"sess-dies"}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":"working on it"}]},"parent_tool_use_id":null,"session_id":"sess-dies"}`

const claudeStderrLine = "No conversation found with session ID: sess-dies"

// claudeFailedResult is the result event that same failing run printed
// on stdout while stderr carried the sentence: subtype
// error_during_execution, is_error true, an `errors` array in the app's
// own words, and a usage object of zeros for a run that made no
// request.
//
// The fake that prints it exits 0, which the real binary does not do.
// That disagreement is the point: a result event declaring failure must
// not be reported as a success just because the process managed to
// exit cleanly.
const claudeFailedResult = `{"type":"result","subtype":"error_during_execution","duration_ms":0,"is_error":true,"num_turns":0,"session_id":"sess-gone","total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0},"modelUsage":{},"permission_denials":[],"errors":["No conversation found with session ID: sess-gone"],"uuid":"58d86f67"}`

// --- helpers --------------------------------------------------------

type recordedChunk struct {
	stream string
	data   string
}

// recorder is a Sink that keeps every chunk in arrival order, and every
// Action the harness recorded.
type recorder struct {
	mu      sync.Mutex
	chunks  []recordedChunk
	actions []Action
	// order is every call in the order it arrived: "<stream> <data>" for a
	// chunk, "action <id>" for an action.
	order []string
}

func (r *recorder) Chunk(stream string, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chunks = append(r.chunks, recordedChunk{stream: stream, data: string(data)})
	r.order = append(r.order, stream+" "+string(data))
}

func (r *recorder) Record(a Action) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.actions = append(r.actions, a)
	r.order = append(r.order, "action "+a.ID)
}

// before is what arrived immediately before the action with id.
func (r *recorder) before(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, entry := range r.order {
		if entry == "action "+id && i > 0 {
			return r.order[i-1]
		}
	}
	return ""
}

// recorded returns the actions, in the order the harness recorded them.
func (r *recorder) recorded() []Action {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Action(nil), r.actions...)
}

func (r *recorder) on(stream string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.chunks {
		if c.stream == stream {
			out = append(out, c.data)
		}
	}
	return out
}

func (r *recorder) joined(stream string) string {
	return strings.Join(r.on(stream), "")
}

// fakeClaude writes a fake `claude` that appends its own argv to a log,
// one record per invocation, and then runs body.
//
// The argv is asserted from the log rather than from a seam in the
// client because the argv is the whole contract with the app: a flag
// this client believes it passed and does not is invisible to any
// assertion made on this side of the fork.
func fakeClaude(t *testing.T, body string) (binary, argvLog string) {
	t.Helper()
	argvLog = filepath.Join(t.TempDir(), "argv.log")
	script := "#!/bin/sh\n" +
		"{ echo '--turn--'; printf '%s\\n' \"$@\"; } >> '" + argvLog + "'\n" +
		body
	return fakeBinary(t, "claude", script), argvLog
}

// prints wraps recorded events in a heredoc the fake shell prints
// verbatim, so the fixture in the test is byte-for-byte what the client
// parses.
func prints(events string) string {
	return "cat <<'CLAUDE_JSON'\n" + events + "\nCLAUDE_JSON\n"
}

// printsPerTurn picks the second fixture once --resume appears in argv,
// which is how the real binary distinguishes the two calls.
func printsPerTurn(first, second string) string {
	return "case \" $* \" in\n" +
		"  *' --resume '*)\n" + prints(second) +
		"    ;;\n" +
		"  *)\n" + prints(first) +
		"    ;;\n" +
		"esac\n"
}

// recordedArgv reads the fake's log back as one []string per invocation.
func recordedArgv(t *testing.T, path string) [][]string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the fake claude recorded no argv at all: %v", err)
	}
	var turns [][]string
	for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		if line == "--turn--" {
			turns = append(turns, []string{})
			continue
		}
		if len(turns) == 0 {
			t.Fatalf("argv log began without a turn marker: %q", body)
		}
		turns[len(turns)-1] = append(turns[len(turns)-1], line)
	}
	return turns
}

func hasArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

// argValue returns the argument after name, which is how every flag this
// client passes carries its value.
func argValue(t *testing.T, argv []string, name string) string {
	t.Helper()
	for i, a := range argv {
		if a != name {
			continue
		}
		if i+1 >= len(argv) {
			t.Fatalf("%s was the last argument, so it carried no value: %v", name, argv)
		}
		return argv[i+1]
	}
	t.Fatalf("%s is missing from the argv: %v", name, argv)
	return ""
}

func claudeSpec(t *testing.T, binary string) Spec {
	t.Helper()
	dir := t.TempDir()
	return Spec{
		Binary:        binary,
		Workspace:     dir,
		MCPConfigPath: filepath.Join(dir, ".mcp.json"),
		Launch:        testLaunch,
	}
}

// startClaude starts a client on spec and guarantees the Close that the
// interface says happens on every exit path.
func startClaude(t *testing.T, spec Spec) *claudeHeadless {
	t.Helper()
	h := &claudeHeadless{}
	if err := h.Start(context.Background(), spec); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// --- argv -----------------------------------------------------------

func TestClaudeHeadlessFirstTurnCarriesNoResume(t *testing.T) {
	bin, log := fakeClaude(t, prints(claudePongTurn))
	spec := claudeSpec(t, bin)
	h := startClaude(t, spec)

	rec := &recorder{}
	res, err := h.Turn(context.Background(), "say pong", rec)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}

	turns := recordedArgv(t, log)
	if len(turns) != 1 {
		t.Fatalf("want one invocation, got %d: %v", len(turns), turns)
	}
	argv := turns[0]
	if hasArg(argv, "--resume") {
		t.Errorf("the first turn passed --resume with nothing to resume: %v", argv)
	}
	if !hasArg(argv, "-p") {
		t.Errorf("the turn is not a headless print run: %v", argv)
	}
	if got := argValue(t, argv, "--output-format"); got != "stream-json" {
		t.Errorf("--output-format = %q, want stream-json", got)
	}
	if !hasArg(argv, "--verbose") {
		t.Errorf("stream-json under --print is REFUSED without --verbose: %v", argv)
	}
	if got := argValue(t, argv, "--mcp-config"); got != spec.MCPConfigPath {
		t.Errorf("--mcp-config = %q, want %q", got, spec.MCPConfigPath)
	}
	if hasArg(argv, "--json-schema") {
		t.Errorf("a schema nobody asked for changes what the app says: %v", argv)
	}
	if len(argv) < 2 || argv[len(argv)-2] != "--" || argv[len(argv)-1] != "say pong" {
		t.Errorf("the prompt must be the only thing after --, got %v", argv)
	}

	if res.Text != "pong" {
		t.Errorf("Text = %q, want pong", res.Text)
	}
	if res.AppSessionRef != "sess-one" {
		t.Errorf("AppSessionRef = %q, want sess-one", res.AppSessionRef)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// TestClaudeHeadlessPromptSurvivesTheVariadicMCPFlag pins the trap that
// makes claudeArgv's ordering load-bearing: --mcp-config is variadic and
// swallows a following positional, and a prompt that begins with a dash
// is read as an unknown option. Both fail as a wrong answer, not an
// error the operator can trace back here.
func TestClaudeHeadlessPromptSurvivesTheVariadicMCPFlag(t *testing.T) {
	bin, log := fakeClaude(t, prints(claudePongTurn))
	h := startClaude(t, claudeSpec(t, bin))

	prompt := "--resume this is a prompt, not a flag"
	if _, err := h.Turn(context.Background(), prompt, &recorder{}); err != nil {
		t.Fatalf("turn: %v", err)
	}

	argv := recordedArgv(t, log)[0]
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
	if got := argv[sep+1:]; len(got) != 1 || got[0] != prompt {
		t.Fatalf("after -- want exactly the prompt, got %v", got)
	}
	for _, a := range argv[:sep] {
		if a == prompt {
			t.Fatalf("the prompt appears before the separator, where --mcp-config can eat it: %v", argv)
		}
	}
}

func TestClaudeHeadlessAttachResumesFromTheSpec(t *testing.T) {
	bin, log := fakeClaude(t, printsPerTurn(claudePongTurn, claudeResumedTurn))
	spec := claudeSpec(t, bin)
	spec.ResumeRef = "sess-one"
	h := startClaude(t, spec)

	res, err := h.Turn(context.Background(), "carry on", &recorder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	argv := recordedArgv(t, log)[0]
	if got := argValue(t, argv, "--resume"); got != "sess-one" {
		t.Errorf("--resume = %q, want the spec's ResumeRef sess-one", got)
	}
	if res.AppSessionRef != "sess-one" {
		t.Errorf("AppSessionRef = %q, want sess-one", res.AppSessionRef)
	}
}

// --- turns ----------------------------------------------------------

func TestClaudeHeadlessSecondTurnResumesTheReportedSession(t *testing.T) {
	bin, log := fakeClaude(t, printsPerTurn(claudePongTurn, claudeResumedTurn))
	h := startClaude(t, claudeSpec(t, bin))

	first, err := h.Turn(context.Background(), "say pong", &recorder{})
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	second, err := h.Turn(context.Background(), "again", &recorder{})
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}

	turns := recordedArgv(t, log)
	if len(turns) != 2 {
		t.Fatalf("want two invocations, got %d: %v", len(turns), turns)
	}
	if got := argValue(t, turns[1], "--resume"); got != first.AppSessionRef {
		t.Errorf("--resume = %q, want the id the first turn reported (%q)", got, first.AppSessionRef)
	}
	if second.AppSessionRef != "sess-one" {
		t.Errorf("AppSessionRef = %q, want sess-one; a turn that loses the ref turns a "+
			"conversation into a series of strangers", second.AppSessionRef)
	}
}

func TestClaudeHeadlessUsageComesFromTheResultEvent(t *testing.T) {
	bin, _ := fakeClaude(t, prints(claudePongTurn))
	h := startClaude(t, claudeSpec(t, bin))

	res, err := h.Turn(context.Background(), "say pong", &recorder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	want := Usage{InputTokens: 10, OutputTokens: 101, CostUSD: 0.10044700000000001, Known: true}
	if res.Usage != want {
		t.Errorf("Usage = %+v, want %+v", res.Usage, want)
	}
}

func TestClaudeHeadlessSilentUsageStaysUnknown(t *testing.T) {
	bin, _ := fakeClaude(t, prints(claudeSilentUsageTurn))
	h := startClaude(t, claudeSpec(t, bin))

	res, err := h.Turn(context.Background(), "do a thing", &recorder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Usage.Known {
		t.Errorf("Usage = %+v, want Known false; zeroes with Known true are billed as a "+
			"free run, and nobody knows what this one cost", res.Usage)
	}
	if res.Usage.InputTokens != 0 || res.Usage.OutputTokens != 0 || res.Usage.CostUSD != 0 {
		t.Errorf("Usage = %+v, want no invented numbers alongside Known false", res.Usage)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q, want done", res.Text)
	}
}

// --- structured results ---------------------------------------------

func TestClaudeHeadlessSchemaProducesResultJSON(t *testing.T) {
	bin, log := fakeClaude(t, prints(claudeSchemaTurn))
	spec := claudeSpec(t, bin)
	spec.ResponseSchema = `{"type":"object","properties":{"city":{"type":"string"},"millions":{"type":"number"}},"required":["city","millions"]}`
	h := startClaude(t, spec)

	res, err := h.Turn(context.Background(), "capital of France", &recorder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if got := argValue(t, recordedArgv(t, log)[0], "--json-schema"); got != spec.ResponseSchema {
		t.Errorf("--json-schema = %q, want the spec's schema verbatim", got)
	}
	var got struct {
		City     string  `json:"city"`
		Millions float64 `json:"millions"`
	}
	if err := json.Unmarshal(res.ResultJSON, &got); err != nil {
		t.Fatalf("ResultJSON %q did not parse: %v", res.ResultJSON, err)
	}
	if got.City != "Paris" || got.Millions != 2.2 {
		t.Errorf("ResultJSON = %s, want Paris / 2.2", res.ResultJSON)
	}
	if res.AppSessionRef != "sess-schema" {
		t.Errorf("AppSessionRef = %q, want sess-schema", res.AppSessionRef)
	}
}

func TestClaudeHeadlessSchemaAnsweredInProseIsNamed(t *testing.T) {
	bin, _ := fakeClaude(t, prints(claudeProseTurn))
	spec := claudeSpec(t, bin)
	spec.ResponseSchema = `{"type":"object","properties":{"city":{"type":"string"}}}`
	h := startClaude(t, spec)

	res, err := h.Turn(context.Background(), "capital of France", &recorder{})
	if !errors.Is(err, ErrNoStructuredResult) {
		t.Fatalf("err = %v, want ErrNoStructuredResult", err)
	}
	if res.ResultJSON != nil {
		t.Errorf("ResultJSON = %s, want nothing; an empty object reads downstream as "+
			"\"the app answered nothing\"", res.ResultJSON)
	}
	if res.Text != "Paris, about 2.2 million." {
		t.Errorf("Text = %q, want the prose the app did produce", res.Text)
	}
	if !res.Usage.Known || res.Usage.CostUSD != 0.004 {
		t.Errorf("Usage = %+v, want the spend of a run that really happened", res.Usage)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0; the process succeeded, the schema did not", res.ExitCode)
	}
}

func TestClaudeHeadlessWithoutASchemaProducesNoResultJSON(t *testing.T) {
	bin, _ := fakeClaude(t, prints(claudeSchemaTurn))
	h := startClaude(t, claudeSpec(t, bin))

	res, err := h.Turn(context.Background(), "capital of France", &recorder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.ResultJSON != nil {
		t.Errorf("ResultJSON = %s, want nothing: the caller asked for no schema, so a "+
			"structured answer is one this harness invented a use for", res.ResultJSON)
	}
}

// --- streams --------------------------------------------------------

func TestClaudeHeadlessMapsEventsOntoTheSinkStreams(t *testing.T) {
	bin, _ := fakeClaude(t, prints(claudeBlockTurn)+"echo '"+claudeStderrLine+"' >&2\n")
	h := startClaude(t, claudeSpec(t, bin))

	rec := &recorder{}
	if _, err := h.Turn(context.Background(), "read main.go", rec); err != nil {
		t.Fatalf("turn: %v", err)
	}

	text := rec.joined(StreamText)
	for _, want := range []string{"the user wants the file read", "Reading it now."} {
		if !strings.Contains(text, want) {
			t.Errorf("StreamText %q is missing %q", text, want)
		}
	}
	if strings.Contains(text, "EosFCrIBCBEYAipAxA2b") {
		t.Errorf("a thinking block's signature reached a chunk: %q", text)
	}

	tool := rec.on(StreamTool)
	if len(tool) != 2 {
		t.Fatalf("want two tool chunks (the call and its result), got %d: %v", len(tool), tool)
	}
	if !strings.Contains(tool[0], `"name":"Read"`) || !strings.Contains(tool[0], "toolu_01ab") {
		t.Errorf("the tool call chunk lost the app's own report of it: %q", tool[0])
	}
	if !strings.Contains(tool[1], "tool_result") || !strings.Contains(tool[1], "package main") {
		t.Errorf("the tool result chunk lost the result: %q", tool[1])
	}

	event := rec.joined(StreamEvent)
	for _, want := range []string{`"subtype":"init"`, `"type":"rate_limit_event"`, `"type":"result"`} {
		if !strings.Contains(event, want) {
			t.Errorf("StreamEvent is missing %q", want)
		}
	}

	if got := rec.joined(StreamStdout); !strings.Contains(got, "npm warn config production") {
		t.Errorf("StreamStdout = %q, want the line that was not stream-json at all; an app "+
			"that dies while printing a stack trace prints it there", got)
	}
	if got := rec.joined(StreamStderr); !strings.Contains(got, claudeStderrLine) {
		t.Errorf("StreamStderr = %q, want %q", got, claudeStderrLine)
	}
}

// TestClaudeHeadlessUnfamiliarMessageShapeStaysAnEvent covers the shape
// this client refuses to guess at: a message whose `content` is a plain
// string rather than an array of blocks. It goes out whole as
// structure, not as unrecognised narration -- stdout is where an app's
// crash output lands, and an event misfiled there reads like one.
func TestClaudeHeadlessUnfamiliarMessageShapeStaysAnEvent(t *testing.T) {
	events := `{"type":"user","message":{"role":"user","content":"a plain string"},"session_id":"sess-odd"}
{"type":"result","subtype":"success","is_error":false,"session_id":"sess-odd","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2},"result":"fine"}`
	bin, _ := fakeClaude(t, prints(events))
	h := startClaude(t, claudeSpec(t, bin))

	rec := &recorder{}
	if _, err := h.Turn(context.Background(), "hello", rec); err != nil {
		t.Fatalf("turn: %v", err)
	}
	if got := rec.joined(StreamEvent); !strings.Contains(got, "a plain string") {
		t.Errorf("StreamEvent = %q, want the whole envelope forwarded", got)
	}
	if got := rec.joined(StreamStdout); got != "" {
		t.Errorf("StreamStdout = %q, want nothing: this line was stream-json", got)
	}
	if got := rec.joined(StreamText); got != "" {
		t.Errorf("StreamText = %q, want nothing guessed out of an unfamiliar shape", got)
	}
}

// TestClaudeHeadlessOversizeLineIsNotBuffered proves the client neither
// grows a line without bound nor loses the stream after one: a single
// enormous event goes out as stdout and the result event AFTER it is
// still parsed.
func TestClaudeHeadlessOversizeLineIsNotBuffered(t *testing.T) {
	script := "printf '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"sess-big\"}\\n'\n" +
		"s=xxxxxxxxxxxxxxxx\n" +
		"while [ ${#s} -lt 1400000 ]; do s=$s$s; done\n" +
		"printf '{\"type\":\"noise\",\"blob\":\"%s\"}\\n' \"$s\"\n" +
		prints(`{"type":"result","subtype":"success","is_error":false,"session_id":"sess-big","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2},"result":"survived"}`)
	bin, _ := fakeClaude(t, script)
	h := startClaude(t, claudeSpec(t, bin))

	rec := &recorder{}
	res, err := h.Turn(context.Background(), "print a lot", rec)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Text != "survived" {
		t.Errorf("Text = %q, want survived: the result event after the oversize line was lost", res.Text)
	}
	if n := len(rec.joined(StreamStdout)); n < 1_400_000 {
		t.Errorf("StreamStdout carried %d bytes of the oversize line, want all of it", n)
	}
	for _, c := range rec.on(StreamEvent) {
		if len(c) > claudeMaxEventBytes {
			t.Errorf("an event chunk of %d bytes was buffered whole", len(c))
		}
	}
}

// --- failures -------------------------------------------------------

func TestClaudeHeadlessProcessDiesMidStream(t *testing.T) {
	bin, _ := fakeClaude(t, prints(claudeDiesMidStream)+
		"echo '"+claudeStderrLine+"' >&2\n"+
		"exit 3\n")
	h := startClaude(t, claudeSpec(t, bin))

	rec := &recorder{}
	res, err := h.Turn(context.Background(), "resume something gone", rec)
	if err == nil {
		t.Fatal("a turn whose process exited 3 reported success")
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want the real 3; the engine reads non-zero as a FAILED "+
			"run and a flattened code misfiles the outcome", res.ExitCode)
	}
	if !strings.Contains(err.Error(), claudeStderrLine) {
		t.Errorf("err = %v, want the app's own reason in it", err)
	}
	if got := rec.joined(StreamText); !strings.Contains(got, "working on it") {
		t.Errorf("StreamText = %q, want the chunks that did arrive before the death", got)
	}
	if res.AppSessionRef != "sess-dies" {
		t.Errorf("AppSessionRef = %q, want sess-dies; a failed turn is still resumable", res.AppSessionRef)
	}
	if res.Usage.Known {
		t.Errorf("Usage = %+v, want Known false: there was no result event to read", res.Usage)
	}
}

func TestClaudeHeadlessSelfDeclaredFailureBeatsAZeroExit(t *testing.T) {
	bin, _ := fakeClaude(t, prints(claudeFailedResult)+
		"echo 'some earlier noise' >&2\n")
	h := startClaude(t, claudeSpec(t, bin))

	res, err := h.Turn(context.Background(), "resume something gone", &recorder{})
	if err == nil {
		t.Fatal("a turn whose result event said is_error reported success")
	}
	if !strings.Contains(err.Error(), "error_during_execution") {
		t.Errorf("err = %v, want the app's own subtype in it", err)
	}
	if !strings.Contains(err.Error(), "No conversation found with session ID: sess-gone") {
		t.Errorf("err = %v, want the app's own errors array preferred over stderr", err)
	}
	if strings.Contains(err.Error(), "some earlier noise") {
		t.Errorf("err = %v, want the app's own words rather than whatever stderr held", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want the real 0", res.ExitCode)
	}
	if !res.Usage.Known {
		t.Errorf("Usage = %+v, want Known true: the app reported zeros for a run that "+
			"made no request, which is what appsession/chunks.go records too", res.Usage)
	}
}

func TestClaudeHeadlessMissingResultEventIsNamed(t *testing.T) {
	bin, _ := fakeClaude(t, prints(claudeDiesMidStream))
	h := startClaude(t, claudeSpec(t, bin))

	res, err := h.Turn(context.Background(), "say something", &recorder{})
	if err == nil {
		t.Fatal("a turn that printed no result event reported success")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want the real 0", res.ExitCode)
	}
	if res.Usage.Known {
		t.Errorf("Usage = %+v, want Known false", res.Usage)
	}
}

// Recorded from a bounded Sonnet probe on 2026-10-09. Identifiers and reset
// date were removed; subtype=success with is_error=true is the real shape.
func TestClaudeHeadlessWeeklyLimitIsFailureWithItsOwnExplanation(t *testing.T) {
	const limited = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"seven_day","overageStatus":"rejected","isUsingOverage":false}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"You've hit your weekly limit"}]},"error":"rate_limit","is_api_error_message":true}
{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"terminal_reason":"api_error","result":"You've hit your weekly limit","total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0},"modelUsage":{}}`
	for _, exit := range []int{0, 1} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			bin, log := fakeClaude(t, prints(limited)+fmt.Sprintf("exit %d\n", exit))
			h := startClaude(t, claudeSpec(t, bin))
			res, err := h.Turn(context.Background(), "availability check", &recorder{})
			if err == nil || !strings.Contains(err.Error(), "weekly limit") {
				t.Fatalf("quota refusal became success or lost its explanation: %+v %v", res, err)
			}
			if !res.Usage.Known || res.Usage.InputTokens != 0 || res.Usage.OutputTokens != 0 || res.Model != "" {
				t.Fatalf("refused call invented model usage: %+v", res)
			}
			if calls := recordedArgv(t, log); len(calls) != 1 {
				t.Fatalf("quota refusal retried: %v", calls)
			}
		})
	}
}

func TestClaudeHeadlessMissingBinaryFailsAtTheTurn(t *testing.T) {
	spec := claudeSpec(t, filepath.Join(t.TempDir(), "no-such-claude"))
	h := startClaude(t, spec)

	res, err := h.Turn(context.Background(), "hello", &recorder{})
	if err == nil {
		t.Fatal("a turn against a binary that does not exist reported success")
	}
	if !strings.Contains(err.Error(), "no-such-claude") {
		t.Errorf("err = %v, want the binary named in it", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 for a process that never ran", res.ExitCode)
	}
}

func TestClaudeHeadlessCancelledTurnStops(t *testing.T) {
	bin, _ := fakeClaude(t, "exec sleep 60\n")
	h := startClaude(t, claudeSpec(t, bin))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.Turn(ctx, "take forever", &recorder{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled turn reported success")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a cancelled turn did not return; the process was never terminated")
	}
}

// --- lifecycle ------------------------------------------------------

func TestClaudeHeadlessStartRejectsAnUnusableSpec(t *testing.T) {
	bin, _ := fakeClaude(t, prints(claudePongTurn))
	for name, spec := range map[string]Spec{
		"no launcher": {Binary: bin, Workspace: t.TempDir()},
		"no binary":   {Workspace: t.TempDir(), Launch: testLaunch},
	} {
		t.Run(name, func(t *testing.T) {
			h := &claudeHeadless{}
			if err := h.Start(context.Background(), spec); err == nil {
				t.Fatalf("Start accepted a spec with %s", name)
			}
			if err := h.Close(); err != nil {
				t.Fatalf("Close after a failed Start: %v", err)
			}
		})
	}
}

func TestClaudeHeadlessTurnOutsideTheLifecycleRefuses(t *testing.T) {
	bin, log := fakeClaude(t, prints(claudePongTurn))

	unstarted := &claudeHeadless{}
	if _, err := unstarted.Turn(context.Background(), "hello", &recorder{}); err == nil {
		t.Error("a turn before Start was accepted")
	}

	h := startClaude(t, claudeSpec(t, bin))
	if _, err := h.Turn(context.Background(), "hello", &recorder{}); err != nil {
		t.Fatalf("turn: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close is documented idempotent, second call: %v", err)
	}
	if _, err := h.Turn(context.Background(), "late follow-up", &recorder{}); err == nil {
		t.Error("a turn after Close started a process nobody is watching")
	}
	if turns := recordedArgv(t, log); len(turns) != 1 {
		t.Errorf("want one invocation, got %d", len(turns))
	}
}

// TestClaudeHeadlessConcurrentTurnRefused pins the refusal rather than
// a queue: two processes resuming one Claude Code session id both
// append to the same transcript and neither sees the other's turn, so
// the second caller has to be told rather than made to wait for an
// answer that would be wrong.
func TestClaudeHeadlessConcurrentTurnRefused(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	bin, log := fakeClaude(t, "while [ ! -f '"+gate+"' ]; do sleep 0.02; done\n"+prints(claudePongTurn))
	h := startClaude(t, claudeSpec(t, bin))

	first := make(chan error, 1)
	go func() {
		_, err := h.Turn(context.Background(), "the turn in flight", &recorder{})
		first <- err
	}()

	// The fake records its argv the moment it starts, so the log is the
	// signal that the first turn really is in flight.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if body, err := os.ReadFile(log); err == nil && strings.Contains(string(body), "--turn--") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first turn never forked")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A bounded context so a broken guard fails the assertion rather
	// than hanging the suite behind the gate.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, second := h.Turn(ctx, "a follow-up too soon", &recorder{})

	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-first; err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if second == nil || !strings.Contains(second.Error(), "already running a turn") {
		t.Errorf("err = %v, want it to name the turn already in flight", second)
	}
	if turns := recordedArgv(t, log); len(turns) != 1 {
		t.Errorf("want one invocation, got %d: a refused turn still forked", len(turns))
	}
}

func TestClaudeHeadlessName(t *testing.T) {
	h := &claudeHeadless{}
	if h.Name() != HarnessClaudeHeadless {
		t.Errorf("Name = %q, want %q", h.Name(), HarnessClaudeHeadless)
	}
}
