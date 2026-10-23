package harness

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// claudeactions_test.go drives the Claude Code client against RECORDED
// tool traffic and asserts the recording it produces.
//
// Every fixture was captured from claude 2.1.270 on 2026-09-13 by running
// the real binary in a scratch directory holding notes.txt, with one stdio
// MCP server named `echo` whose one tool echoes its text back. The lines
// are trimmed to the fields a reader here could care about: the workspace
// path is shortened to /w, and the thinking, hook, rate-limit and
// thinking_tokens lines are dropped. Everything else -- including the
// ToolSearch call Claude Code makes to load the deferred MCP tool before
// it can call it -- is as printed.

// claudeRecordedTurn is `claude -p --output-format stream-json --verbose
// --model haiku --mcp-config <echo> --allowedTools "Read Bash Write Edit
// mcp__echo__echo" -- "<read notes.txt; run ls -1; run a command that
// exits 3; write out.txt; edit it; call the echo tool with ping>"`.
const claudeRecordedTurn = `{"type":"system","subtype":"init","cwd":"/w","session_id":"sess-rec","claude_code_version":"2.1.270","model":"claude-haiku-4-5-20251001","permissionMode":"default"}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":"I'll execute these steps in order. Starting with step 1:"}]},"parent_tool_use_id":null,"session_id":"sess-rec"}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01UJqot242Qo8TW9Y6KgRamm","name":"Read","input":{"file_path":"/w/notes.txt"}}]},"parent_tool_use_id":null,"session_id":"sess-rec"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01UJqot242Qo8TW9Y6KgRamm","type":"tool_result","content":"1\talpha\n2\tbeta\n3\t"}]},"parent_tool_use_id":null,"session_id":"sess-rec","tool_use_result":{"type":"text","file":{"filePath":"/w/notes.txt","content":"alpha\nbeta\n","numLines":3,"startLine":1,"totalLines":3}}}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01T2zDSWmuhhNFhEmJgWnV8J","name":"Bash","input":{"command":"ls -1","description":"List files in current directory"}}]},"parent_tool_use_id":null,"session_id":"sess-rec"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01T2zDSWmuhhNFhEmJgWnV8J","type":"tool_result","content":"notes.txt","is_error":false}]},"parent_tool_use_id":null,"session_id":"sess-rec","tool_use_result":{"stdout":"notes.txt","stderr":"","interrupted":false,"isImage":false,"noOutputExpected":false}}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_017mvXJGvxMPCGy7EwdyX68g","name":"Bash","input":{"command":"sh -c 'echo oops >&2; exit 3'","description":"Run shell command with error output and exit code 3"}}]},"parent_tool_use_id":null,"session_id":"sess-rec"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"Exit code 3\noops","is_error":true,"tool_use_id":"toolu_017mvXJGvxMPCGy7EwdyX68g"}]},"parent_tool_use_id":null,"session_id":"sess-rec","tool_use_result":"Error: Exit code 3\noops"}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01ULvDD2AmfTHJ8V4GN3ESkH","name":"Write","input":{"file_path":"/w/out.txt","content":"hello"}}]},"parent_tool_use_id":null,"session_id":"sess-rec"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01ULvDD2AmfTHJ8V4GN3ESkH","type":"tool_result","content":"File created successfully at: /w/out.txt (file state is current in your context \u2014 no need to Read it back)"}]},"parent_tool_use_id":null,"session_id":"sess-rec","tool_use_result":{"type":"create","filePath":"/w/out.txt","content":"hello","structuredPatch":[],"originalFile":null,"userModified":false}}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_018Ru4q14KCxHfuz6PU77qPD","name":"Edit","input":{"replace_all":false,"file_path":"/w/out.txt","old_string":"hello","new_string":"goodbye"}}]},"parent_tool_use_id":null,"session_id":"sess-rec"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_018Ru4q14KCxHfuz6PU77qPD","type":"tool_result","content":"The file /w/out.txt has been updated successfully. (file state is current in your context \u2014 no need to Read it back)"}]},"parent_tool_use_id":null,"session_id":"sess-rec","tool_use_result":{"filePath":"/w/out.txt","oldString":"hello","newString":"goodbye","originalFile":"hello","structuredPatch":[{"oldStart":1,"oldLines":1,"newStart":1,"newLines":1,"lines":["-hello","\\ No newline at end of file","+goodbye","\\ No newline at end of file"]}],"userModified":false,"replaceAll":false}}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01GbW6bALaYNmPfoiRmcHaNL","name":"ToolSearch","input":{"query":"select:mcp__echo__echo","max_results":1}}]},"parent_tool_use_id":null,"session_id":"sess-rec"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01GbW6bALaYNmPfoiRmcHaNL","content":[{"type":"tool_reference","tool_name":"mcp__echo__echo"}]}]},"parent_tool_use_id":null,"session_id":"sess-rec","tool_use_result":{"matches":["mcp__echo__echo"],"query":"select:mcp__echo__echo","total_deferred_tools":117}}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01U1dXc3Da8DmxEaxmw4wQyf","name":"mcp__echo__echo","input":{"text":"ping"}}]},"parent_tool_use_id":null,"session_id":"sess-rec"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01U1dXc3Da8DmxEaxmw4wQyf","type":"tool_result","content":[{"type":"text","text":"echo: ping"}]}]},"parent_tool_use_id":null,"session_id":"sess-rec","tool_use_result":[{"type":"text","text":"echo: ping"}]}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":"done"}]},"parent_tool_use_id":null,"session_id":"sess-rec"}
{"type":"result","subtype":"success","is_error":false,"num_turns":8,"session_id":"sess-rec","total_cost_usd":0.0542359,"usage":{"input_tokens":68,"output_tokens":1657},"result":"done"}`

// claudeRefusalsTurn is the same binary and day with --allowedTools
// "Bash" only, asked to run `grep zzz notes.txt`, to run `sleep 3` in the
// background, to WebFetch a page and to Write a file. The grep found
// nothing and exited 1, which Claude Code reports as a SUCCESS; the sleep
// has not exited at all; the fetch and the write were refused for want of
// a permission this run did not grant.
const claudeRefusalsTurn = `{"type":"system","subtype":"init","cwd":"/w","session_id":"sess-refusals"}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_013VJPmcoJCjDKYZxPCcD58Y","name":"Bash","input":{"command":"grep zzz notes.txt","description":"Search for zzz in notes.txt"}}]},"parent_tool_use_id":null,"session_id":"sess-refusals"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_013VJPmcoJCjDKYZxPCcD58Y","type":"tool_result","content":"(Bash completed with no output)","is_error":false}]},"parent_tool_use_id":null,"session_id":"sess-refusals","tool_use_result":{"stdout":"","stderr":"","interrupted":false,"isImage":false,"returnCodeInterpretation":"No matches found","noOutputExpected":false}}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01PD6yUd3gwmAfzvjj1bUmYV","name":"Bash","input":{"command":"sleep 3","description":"Sleep for 3 seconds","run_in_background":true}}]},"parent_tool_use_id":null,"session_id":"sess-refusals"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01PD6yUd3gwmAfzvjj1bUmYV","type":"tool_result","content":"Command running in background with ID: brccvjji8. Output is being written to: /tmp/tasks/brccvjji8.output. You will be notified when it completes. To check interim output, use Read on that file path.","is_error":false}]},"parent_tool_use_id":null,"session_id":"sess-refusals","tool_use_result":{"stdout":"","stderr":"","interrupted":false,"isImage":false,"noOutputExpected":false,"backgroundTaskId":"brccvjji8"}}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01SJBiTETMsStTt1hepwqiCH","name":"WebFetch","input":{"url":"https://example.com","prompt":"Fetch this page"}}]},"parent_tool_use_id":null,"session_id":"sess-refusals"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"Claude requested permissions to use WebFetch, but you haven't granted it yet.","is_error":true,"tool_use_id":"toolu_01SJBiTETMsStTt1hepwqiCH"}]},"parent_tool_use_id":null,"session_id":"sess-refusals","tool_use_result":"Error: Claude requested permissions to use WebFetch, but you haven't granted it yet."}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01KggUaE4U4D7rLtoagMrraN","name":"Write","input":{"file_path":"/w/denied.txt","content":"x"}}]},"parent_tool_use_id":null,"session_id":"sess-refusals"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"Claude requested permissions to write to /w/denied.txt, but you haven't granted it yet.","is_error":true,"tool_use_id":"toolu_01KggUaE4U4D7rLtoagMrraN"}]},"parent_tool_use_id":null,"session_id":"sess-refusals","tool_use_result":"Error: Claude requested permissions to write to /w/denied.txt, but you haven't granted it yet."}
{"type":"result","subtype":"success","is_error":false,"num_turns":9,"session_id":"sess-refusals","total_cost_usd":0.02,"usage":{"input_tokens":40,"output_tokens":900},"result":"done"}`

// runClaudeFixture runs one turn of events through a fresh client and
// returns what the sink recorded.
func runClaudeFixture(t *testing.T, events string) (*recorder, TurnResult, error) {
	t.Helper()
	bin, _ := fakeClaude(t, prints(events))
	h := startClaude(t, claudeSpec(t, bin))
	rec := &recorder{}
	res, err := h.Turn(context.Background(), "do the steps", rec)
	return rec, res, err
}

// exitOf and errOf spell a pointer fact so an absent one reads as
// "absent" in a failure message rather than as a zero.
func exitOf(a Action) string {
	if a.ExitCode == nil {
		return "absent"
	}
	return strconv.Itoa(*a.ExitCode)
}

func errOf(a Action) string {
	if a.IsError == nil {
		return "absent"
	}
	if *a.IsError {
		return "true"
	}
	return "false"
}

// TestClaudeRecordsOneActionPerCompletedCall is #441's first acceptance
// criterion: the recorded transcript yields exactly one event per
// completed tool call, numbered densely in the order the calls finished.
func TestClaudeRecordsOneActionPerCompletedCall(t *testing.T) {
	rec, _, err := runClaudeFixture(t, claudeRecordedTurn)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	got := rec.recorded()
	want := []struct {
		id, tool, appTool, exit, isError, resultType string
	}{
		{"toolu_01UJqot242Qo8TW9Y6KgRamm", ActionFSRead, "Read", "absent", "false", "string"},
		{"toolu_01T2zDSWmuhhNFhEmJgWnV8J", ActionExec, "Bash", "0", "false", "string"},
		{"toolu_017mvXJGvxMPCGy7EwdyX68g", ActionExec, "Bash", "3", "true", "string"},
		{"toolu_01ULvDD2AmfTHJ8V4GN3ESkH", ActionFSWrite, "Write", "absent", "false", "string"},
		{"toolu_018Ru4q14KCxHfuz6PU77qPD", ActionFSWrite, "Edit", "absent", "false", "string"},
		{"toolu_01GbW6bALaYNmPfoiRmcHaNL", ActionAgent, "ToolSearch", "absent", "false", "array"},
		{"toolu_01U1dXc3Da8DmxEaxmw4wQyf", ActionMCP, "mcp__echo__echo", "absent", "false", "string"},
	}
	if len(got) != len(want) {
		t.Fatalf("recorded %d actions, want one per completed call (%d): %+v", len(got), len(want), got)
	}
	for i, w := range want {
		a := got[i]
		if a.Seq != uint64(i+1) || a.Turn != 1 {
			t.Errorf("action %d: seq %d turn %d, want seq %d turn 1", i, a.Seq, a.Turn, i+1)
		}
		if a.Type != ActionEventType || a.V != RecordVersion {
			t.Errorf("action %d is not stamped for the wire: %s v%d", i, a.Type, a.V)
		}
		if a.ID != w.id || a.Tool != w.tool || a.AppTool != w.appTool {
			t.Errorf("action %d = %s %s %s, want %s %s %s", i, a.ID, a.Tool, a.AppTool, w.id, w.tool, w.appTool)
		}
		if exitOf(a) != w.exit || errOf(a) != w.isError || a.ResultType != w.resultType {
			t.Errorf("action %d (%s): exit %s isError %s type %q, want %s %s %q",
				i, a.AppTool, exitOf(a), errOf(a), a.ResultType, w.exit, w.isError, w.resultType)
		}
		if a.Cwd != "/w" {
			t.Errorf("action %d cwd = %q, want the init event's /w", i, a.Cwd)
		}
		if !strings.HasPrefix(a.ResultDigest, DigestPrefix) {
			t.Errorf("action %d digest = %q", i, a.ResultDigest)
		}
	}

	read, ls, write, edit, mcp := got[0], got[1], got[3], got[4], got[6]
	// The Read carries what its record says the app got -- the file's
	// text without the line numbers the model is shown -- for the session
	// to compare with the file.
	if !reflect.DeepEqual(read.Contents, []Content{{Op: ContentRead, Path: "/w/notes.txt", Seen: []byte("alpha\nbeta\n")}}) {
		t.Errorf("Read contents = %+v", read.Contents)
	}
	if read.ResultDigest != Digest([]byte("1\talpha\n2\tbeta\n3\t")) {
		t.Errorf("Read digests %s, want the result the app saw", read.ResultDigest)
	}
	if ls.Command != "ls -1" || ls.ResultDigest != Digest([]byte("notes.txt")) {
		t.Errorf("ls = %q %s", ls.Command, ls.ResultDigest)
	}
	for _, w := range []Action{write, edit} {
		if !reflect.DeepEqual(w.Contents, []Content{{Op: ContentWrite, Path: "/w/out.txt"}}) {
			t.Errorf("%s contents = %+v", w.AppTool, w.Contents)
		}
	}
	if mcp.MCP == nil || *mcp.MCP != (MCPTarget{Server: "echo", Tool: "echo"}) {
		t.Errorf("mcp target = %+v", mcp.MCP)
	}
	if mcp.ResultDigest != Digest([]byte("echo: ping")) {
		t.Errorf("mcp digest %s, want the single text block's text", mcp.ResultDigest)
	}
	var args map[string]any
	if err := json.Unmarshal(mcp.Args, &args); err != nil || args["text"] != "ping" {
		t.Errorf("mcp args = %s, want the input whole", mcp.Args)
	}
}

// TestClaudeActionFollowsTheAppsOwnReport: the vendor chunk still goes out
// exactly as before, and the recording rides beside it -- it replaces
// nothing a reader of the transcript already had.
func TestClaudeActionFollowsTheAppsOwnReport(t *testing.T) {
	rec, _, err := runClaudeFixture(t, claudeRecordedTurn)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if tool := rec.on(StreamTool); len(tool) != 14 {
		t.Errorf("tool chunks = %d, want the 7 calls and 7 results forwarded verbatim", len(tool))
	}
	if got := rec.joined(StreamText); !strings.Contains(got, "done") {
		t.Errorf("the prose stayed where it was? StreamText = %q", got)
	}
	for _, c := range rec.on(StreamEvent) {
		if strings.Contains(c, ActionEventType) {
			t.Errorf("an action went out as a plain chunk rather than through Record: %q", c)
		}
	}
}

// TestClaudeToolResultWithIsErrorSetsIsError is #441's second acceptance
// criterion, on the refusals Claude Code actually printed -- and the
// exit-status rule on the successes it reported that did NOT exit 0.
func TestClaudeToolResultWithIsErrorSetsIsError(t *testing.T) {
	rec, _, err := runClaudeFixture(t, claudeRefusalsTurn)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	got := rec.recorded()
	if len(got) != 4 {
		t.Fatalf("recorded %d actions, want 4: %+v", len(got), got)
	}
	grep, bg, fetch, write := got[0], got[1], got[2], got[3]
	if errOf(grep) != "false" || exitOf(grep) != "absent" {
		t.Errorf("grep that found nothing: isError %s exit %s, want false and ABSENT -- it exited 1",
			errOf(grep), exitOf(grep))
	}
	if errOf(bg) != "false" || exitOf(bg) != "absent" {
		t.Errorf("background sleep: isError %s exit %s, want false and ABSENT -- it has not exited",
			errOf(bg), exitOf(bg))
	}
	if fetch.Tool != ActionFetch || fetch.URL != "https://example.com" || errOf(fetch) != "true" {
		t.Errorf("refused fetch = %+v", fetch)
	}
	if write.Tool != ActionFSWrite || errOf(write) != "true" || write.Contents != nil {
		t.Errorf("refused write = %+v, want isError and no file read: nothing was written", write)
	}
}

func TestClaudeExitCodeIsReadOnlyWhereItWasReported(t *testing.T) {
	fg := json.RawMessage(`{"command":"ls"}`)
	cases := []struct {
		name    string
		args    json.RawMessage
		result  any
		isError bool
		record  string
		want    string
	}{
		{"failure opening Exit code", fg, "Exit code 3\noops", true, `"Error: Exit code 3\noops"`, "3"},
		{"negative status", fg, "Exit code -1", true, ``, "-1"},
		{"refusal", fg, "Claude requested permissions to use Bash, but you haven't granted it yet.", true, ``, "absent"},
		{"timeout", fg, "Command timed out after 2m 0.0s", true, ``, "absent"},
		{"clean success", fg, "notes.txt", false, `{"stdout":"notes.txt","stderr":"","interrupted":false}`, "0"},
		{"reinterpreted status", fg, "(Bash completed with no output)", false,
			`{"interrupted":false,"returnCodeInterpretation":"No matches found"}`, "absent"},
		{"backgrounded by the record", fg, "Command running in background", false,
			`{"interrupted":false,"backgroundTaskId":"b1"}`, "absent"},
		{"backgrounded by the input", json.RawMessage(`{"command":"sleep 3","run_in_background":true}`),
			"Command running in background", false, `{"interrupted":false}`, "absent"},
		{"interrupted", fg, "", false, `{"interrupted":true}`, "absent"},
		{"no record", fg, "notes.txt", false, ``, "absent"},
		{"record without the flag", fg, "notes.txt", false, `{"stdout":"notes.txt"}`, "absent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := Action{ExitCode: claudeExitCode(tc.args, tc.result, tc.isError, json.RawMessage(tc.record))}
			if got := exitOf(a); got != tc.want {
				t.Errorf("exit = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestClaudeCallThatNeverAnsweredIsRecordedIncomplete: the process died
// with a call in flight. The call is recorded -- a gap that reads as a
// gap -- and nothing about its result is claimed.
func TestClaudeCallThatNeverAnsweredIsRecordedIncomplete(t *testing.T) {
	events := `{"type":"system","subtype":"init","cwd":"/w","session_id":"sess-cut"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_cut","name":"Write","input":{"file_path":"/w/half.txt","content":"x"}}]},"parent_tool_use_id":null,"session_id":"sess-cut"}`
	bin, _ := fakeClaude(t, prints(events)+"exit 9\n")
	h := startClaude(t, claudeSpec(t, bin))
	rec := &recorder{}
	if _, err := h.Turn(context.Background(), "write", rec); err == nil {
		t.Fatal("a turn whose process exited 9 reported success")
	}
	got := rec.recorded()
	if len(got) != 1 {
		t.Fatalf("recorded %d actions, want the one call that never finished", len(got))
	}
	a := got[0]
	if !a.Incomplete || a.ID != "toolu_cut" || a.Tool != ActionFSWrite || a.Seq != 1 {
		t.Errorf("= %+v, want toolu_cut, incomplete, seq 1", a)
	}
	if a.IsError != nil || a.ResultType != "" || a.Contents != nil {
		t.Errorf("an unfinished call claimed a result: %+v", a)
	}
	if !strings.Contains(string(a.Args), "half.txt") {
		t.Errorf("args = %s, want what the app asked for", a.Args)
	}
}

// TestClaudeOversizeResultLeavesItsCallIncomplete: a result line past the
// parse bound is streamed out, never parsed -- so its call can only be
// recorded as unfinished, never as a success nobody saw.
func TestClaudeOversizeResultLeavesItsCallIncomplete(t *testing.T) {
	script := "printf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"id\":\"toolu_big\",\"name\":\"Read\",\"input\":{\"file_path\":\"/w/big.log\"}}]},\"session_id\":\"sess-big\"}'\n" +
		"s=xxxxxxxxxxxxxxxx\n" +
		"while [ ${#s} -lt 1400000 ]; do s=$s$s; done\n" +
		"printf '{\"type\":\"user\",\"message\":{\"content\":[{\"tool_use_id\":\"toolu_big\",\"type\":\"tool_result\",\"content\":\"%s\"}]},\"session_id\":\"sess-big\"}\\n' \"$s\"\n" +
		prints(`{"type":"result","subtype":"success","is_error":false,"session_id":"sess-big","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2},"result":"ok"}`)
	bin, _ := fakeClaude(t, script)
	h := startClaude(t, claudeSpec(t, bin))
	rec := &recorder{}
	if _, err := h.Turn(context.Background(), "read a big file", rec); err != nil {
		t.Fatalf("turn: %v", err)
	}
	got := rec.recorded()
	if len(got) != 1 || !got[0].Incomplete || got[0].ID != "toolu_big" {
		t.Fatalf("recorded %+v, want toolu_big incomplete", got)
	}
}

// TestClaudeSubAgentCallsCarryTheirParent: a sub-agent's calls are the
// sub-agent's, and the call that spawned it is the only thing that says so.
func TestClaudeSubAgentCallsCarryTheirParent(t *testing.T) {
	events := `{"type":"system","subtype":"init","cwd":"/w","session_id":"sess-sub"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_task","name":"Task","input":{"description":"look","prompt":"list the files"}}]},"parent_tool_use_id":null,"session_id":"sess-sub"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_inner","name":"Bash","input":{"command":"ls"}}]},"parent_tool_use_id":"toolu_task","session_id":"sess-sub"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_inner","type":"tool_result","content":"a.txt","is_error":false}]},"parent_tool_use_id":"toolu_task","session_id":"sess-sub","tool_use_result":{"stdout":"a.txt","stderr":"","interrupted":false}}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_task","type":"tool_result","content":[{"type":"text","text":"one file: a.txt"}]}]},"parent_tool_use_id":null,"session_id":"sess-sub"}
{"type":"result","subtype":"success","is_error":false,"session_id":"sess-sub","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2},"result":"done"}`
	rec, _, err := runClaudeFixture(t, events)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	got := rec.recorded()
	if len(got) != 2 {
		t.Fatalf("recorded %d, want 2", len(got))
	}
	inner, task := got[0], got[1]
	if inner.ID != "toolu_inner" || inner.ParentID != "toolu_task" || inner.Tool != ActionExec || exitOf(inner) != "0" {
		t.Errorf("inner = %+v", inner)
	}
	if task.ID != "toolu_task" || task.ParentID != "" || task.Tool != ActionAgent {
		t.Errorf("task = %+v", task)
	}
}

// TestClaudeRecordingRunsAcrossTurns: the second turn is a new process,
// and its calls continue the session's numbering rather than restarting it.
func TestClaudeRecordingRunsAcrossTurns(t *testing.T) {
	turn := func(id string) string {
		return `{"type":"system","subtype":"init","cwd":"/w","session_id":"sess-two"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":"true"}}]},"parent_tool_use_id":null,"session_id":"sess-two"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"` + id + `","type":"tool_result","content":"","is_error":false}]},"parent_tool_use_id":null,"session_id":"sess-two","tool_use_result":{"stdout":"","stderr":"","interrupted":false}}
{"type":"result","subtype":"success","is_error":false,"session_id":"sess-two","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2},"result":"ok"}`
	}
	bin, _ := fakeClaude(t, printsPerTurn(turn("toolu_first"), turn("toolu_second")))
	h := startClaude(t, claudeSpec(t, bin))
	rec := &recorder{}
	if _, err := h.Turn(context.Background(), "one", rec); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if _, err := h.Turn(context.Background(), "two", rec); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	got := rec.recorded()
	if len(got) != 2 || got[0].Seq != 1 || got[0].Turn != 1 || got[1].Seq != 2 || got[1].Turn != 2 {
		t.Fatalf("recorded %+v, want seq 1 in turn 1 then seq 2 in turn 2", got)
	}
}

func TestClaudeRepeatedResultIsRecordedOnce(t *testing.T) {
	result := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_r","type":"tool_result","content":"ok","is_error":false}]},"parent_tool_use_id":null,"session_id":"sess-r"}`
	events := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_r","name":"Bash","input":{"command":"true"}}]},"parent_tool_use_id":null,"session_id":"sess-r"}
` + result + "\n" + result + `
{"type":"result","subtype":"success","is_error":false,"session_id":"sess-r","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2},"result":"ok"}`
	rec, _, err := runClaudeFixture(t, events)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if got := rec.recorded(); len(got) != 1 {
		t.Errorf("recorded %d actions for one call", len(got))
	}
}

func TestClaudeKindsAndMCPTargets(t *testing.T) {
	for name, want := range map[string]string{
		"Bash": ActionExec, "Read": ActionFSRead, "Grep": ActionFSRead, "Write": ActionFSWrite,
		"NotebookEdit": ActionFSWrite, "WebFetch": ActionFetch, "ToolSearch": ActionAgent,
		"StructuredOutput": ActionAgent, "Task": ActionAgent, "mcp__memql__query": ActionMCP,
		"PushNotification": ActionOther, "CronCreate": ActionOther, "SomethingNew": ActionOther,
	} {
		if got := claudeKind(name); got != want {
			t.Errorf("claudeKind(%q) = %q, want %q", name, got, want)
		}
	}
	for name, want := range map[string]*MCPTarget{
		"mcp__echo__echo":                 {Server: "echo", Tool: "echo"},
		"mcp__claude_ai_Gmail__get_draft": {Server: "claude_ai_Gmail", Tool: "get_draft"},
		"mcp__memql__query":               {Server: "memql", Tool: "query"},
		"mcp__noseparator":                nil,
		"mcp____tool":                     nil,
		"Bash":                            nil,
	} {
		got := claudeMCPTarget(name)
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Errorf("claudeMCPTarget(%q) = %+v, want %+v", name, got, want)
		}
	}
}
