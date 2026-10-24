package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// codexactions_test.go drives both Codex clients against recorded tool
// traffic and asserts the recording, then holds the two apps to one shape.
//
// codexTurnRecorded was CAPTURED from codex-cli 0.153.4 on 2026-09-13:
// `codex app-server -c 'mcp_servers.echo...'` driven through initialize,
// thread/start {cwd, approvalPolicy: never} and one turn/start asking for
// the same six steps the Claude Code fixture took. Codex read notes.txt
// with `cat`, wrote out.txt with `printf` and edited it with `sed -i` --
// shell commands, all of them, which is exactly why its reads are found
// through its own command parse.
//
// The item lines are EXACTLY as printed, and that includes what they
// lack: the app-server omits the "jsonrpc":"2.0" header on every frame
// (codex-rs/app-server/README.md at rust-v0.153.4: "JSON-RPC 2.0 messages
// (with the "jsonrpc":"2.0" header omitted on the wire)"). Only the
// workspace path is shortened to /w and the thread and turn ids replaced;
// the account, rate-limit, MCP-startup and token-usage notifications are
// dropped. The fake answers through a quoted heredoc, so no shell ever
// rewrites a byte of them.
const codexTurnRecorded = `
printf '{"id":%s,"result":{"turn":{"id":"turn_rec","items":[],"itemsView":"notLoaded","status":"inProgress","error":null,"startedAt":null,"completedAt":null,"durationMs":null}}}\n' "$id"
cat <<'CODEX_JSON'
{"method":"item/started","params":{"item":{"type":"commandExecution","id":"exec-b77282f9-db64-4899-96a4-18794035bba6","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc 'cat notes.txt'","cwd":"/w","processId":"97104","source":"unifiedExecStartup","status":"inProgress","commandActions":[{"type":"read","command":"cat notes.txt","name":"notes.txt","path":"/w/notes.txt"}],"aggregatedOutput":null,"exitCode":null,"durationMs":null},"threadId":"THREAD","turnId":"turn_rec","startedAtMs":1789365994259},"emittedAtMs":1789365994262}
{"method":"item/completed","params":{"item":{"type":"commandExecution","id":"exec-b77282f9-db64-4899-96a4-18794035bba6","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc 'cat notes.txt'","cwd":"/w","processId":"97104","source":"unifiedExecStartup","status":"completed","commandActions":[{"type":"read","command":"cat notes.txt","name":"notes.txt","path":"/w/notes.txt"}],"aggregatedOutput":"alpha\nbeta\n","exitCode":0,"durationMs":0},"threadId":"THREAD","turnId":"turn_rec","completedAtMs":1789365994259},"emittedAtMs":1789365994263}
{"method":"item/started","params":{"item":{"type":"commandExecution","id":"exec-02337035-bc20-401a-a4e4-630a60d68442","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc 'ls -1'","cwd":"/w","processId":"97074","source":"unifiedExecStartup","status":"inProgress","commandActions":[{"type":"listFiles","command":"ls -1","path":null}],"aggregatedOutput":null,"exitCode":null,"durationMs":null},"threadId":"THREAD","turnId":"turn_rec","startedAtMs":1789365996388},"emittedAtMs":1789365996388}
{"method":"item/completed","params":{"item":{"type":"commandExecution","id":"exec-02337035-bc20-401a-a4e4-630a60d68442","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc 'ls -1'","cwd":"/w","processId":"97074","source":"unifiedExecStartup","status":"completed","commandActions":[{"type":"listFiles","command":"ls -1","path":null}],"aggregatedOutput":"notes.txt\n","exitCode":0,"durationMs":0},"threadId":"THREAD","turnId":"turn_rec","completedAtMs":1789365996388},"emittedAtMs":1789365996394}
{"method":"item/started","params":{"item":{"type":"commandExecution","id":"exec-d8855f30-ddea-4313-a829-8f981ca16799","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc \"sh -c 'echo oops >&2; exit 3'\"","cwd":"/w","processId":"39845","source":"unifiedExecStartup","status":"inProgress","commandActions":[{"type":"unknown","command":"sh -c 'echo oops >&2; exit 3'"}],"aggregatedOutput":null,"exitCode":null,"durationMs":null},"threadId":"THREAD","turnId":"turn_rec","startedAtMs":1789365998809},"emittedAtMs":1789365998809}
{"method":"item/completed","params":{"item":{"type":"commandExecution","id":"exec-d8855f30-ddea-4313-a829-8f981ca16799","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc \"sh -c 'echo oops >&2; exit 3'\"","cwd":"/w","processId":"39845","source":"unifiedExecStartup","status":"failed","commandActions":[{"type":"unknown","command":"sh -c 'echo oops >&2; exit 3'"}],"aggregatedOutput":"oops\n","exitCode":3,"durationMs":0},"threadId":"THREAD","turnId":"turn_rec","completedAtMs":1789365998809},"emittedAtMs":1789365998811}
{"method":"item/started","params":{"item":{"type":"commandExecution","id":"exec-a9bda5da-698f-4906-ae4c-a5eba582397d","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc \"printf 'hello\\\\n' > out.txt\"","cwd":"/w","processId":"68520","source":"unifiedExecStartup","status":"inProgress","commandActions":[{"type":"unknown","command":"printf 'hello\\n' > out.txt"}],"aggregatedOutput":null,"exitCode":null,"durationMs":null},"threadId":"THREAD","turnId":"turn_rec","startedAtMs":1789366001388},"emittedAtMs":1789366001388}
{"method":"item/completed","params":{"item":{"type":"commandExecution","id":"exec-a9bda5da-698f-4906-ae4c-a5eba582397d","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc \"printf 'hello\\\\n' > out.txt\"","cwd":"/w","processId":"68520","source":"unifiedExecStartup","status":"completed","commandActions":[{"type":"unknown","command":"printf 'hello\\n' > out.txt"}],"aggregatedOutput":null,"exitCode":0,"durationMs":0},"threadId":"THREAD","turnId":"turn_rec","completedAtMs":1789366001388},"emittedAtMs":1789366001390}
{"method":"item/started","params":{"item":{"type":"commandExecution","id":"exec-c4568bae-9956-48d6-9e08-8445424f7bfd","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc \"sed -i 's/hello/goodbye/' out.txt\"","cwd":"/w","processId":"67768","source":"unifiedExecStartup","status":"inProgress","commandActions":[{"type":"unknown","command":"sed -i 's/hello/goodbye/' out.txt"}],"aggregatedOutput":null,"exitCode":null,"durationMs":null},"threadId":"THREAD","turnId":"turn_rec","startedAtMs":1789366004490},"emittedAtMs":1789366004491}
{"method":"item/completed","params":{"item":{"type":"commandExecution","id":"exec-c4568bae-9956-48d6-9e08-8445424f7bfd","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc \"sed -i 's/hello/goodbye/' out.txt\"","cwd":"/w","processId":"67768","source":"unifiedExecStartup","status":"completed","commandActions":[{"type":"unknown","command":"sed -i 's/hello/goodbye/' out.txt"}],"aggregatedOutput":null,"exitCode":0,"durationMs":0},"threadId":"THREAD","turnId":"turn_rec","completedAtMs":1789366004491},"emittedAtMs":1789366004494}
{"method":"item/started","params":{"item":{"type":"mcpToolCall","id":"exec-6df80e5a-51b0-4d75-aa52-79e22177519a","server":"echo","tool":"echo","status":"inProgress","arguments":{"text":"ping"},"appContext":null,"pluginId":null,"readOnlyHint":null,"result":null,"error":null,"durationMs":null},"threadId":"THREAD","turnId":"turn_rec","startedAtMs":1789366009184},"emittedAtMs":1789366009186}
{"method":"item/completed","params":{"item":{"type":"mcpToolCall","id":"exec-6df80e5a-51b0-4d75-aa52-79e22177519a","server":"echo","tool":"echo","status":"completed","arguments":{"text":"ping"},"appContext":null,"pluginId":null,"readOnlyHint":null,"result":{"content":[{"type":"text","text":"echo: ping"}],"structuredContent":null,"_meta":null},"error":null,"durationMs":2},"threadId":"THREAD","turnId":"turn_rec","completedAtMs":1789366009187},"emittedAtMs":1789366009191}
{"method":"item/completed","params":{"item":{"type":"agentMessage","id":"msg_0bf177e203eb66a2016aa78efb1d3087d0a97f0552752a34a9","text":"done","phase":"final_answer","memoryCitation":null,"delivery":null,"questions":null},"threadId":"THREAD","turnId":"turn_rec","completedAtMs":1789366011232},"emittedAtMs":1789366011238}
{"method":"turn/completed","params":{"threadId":"THREAD","turn":{"id":"turn_rec","items":[{"type":"agentMessage","id":"msg_0bf177e203eb66a2016aa78efb1d3087d0a97f0552752a34a9","text":"done","phase":"final_answer","memoryCitation":null,"delivery":null,"questions":null}],"itemsView":"summary","status":"completed","error":null,"startedAt":1789365989,"completedAt":1789366011,"durationMs":21513}},"emittedAtMs":1789366011264}
CODEX_JSON
`

// codexTurnFileChange is a fileChange item in the v2 ThreadItem shape
// (codex-cli 0.153.4 generate-ts: {type, id, changes: [{path, kind, diff}],
// status}). The recorded turn has none -- that Codex wrote with the shell
// -- so this one is BUILT from the type rather than captured, and says so.
const codexTurnFileChange = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn_fc","items":[],"itemsView":"notLoaded","status":"inProgress","error":null,"startedAt":null,"completedAt":null,"durationMs":null}}}\n' "$id"
cat <<'CODEX_JSON'
{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"THREAD","turnId":"turn_fc","startedAtMs":1,"item":{"type":"fileChange","id":"call_patch","changes":[{"path":"/w/out.txt","kind":{"type":"add"},"diff":"hello\n"}],"status":"inProgress"}}}
{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_fc","completedAtMs":2,"item":{"type":"fileChange","id":"call_patch","changes":[{"path":"/w/out.txt","kind":{"type":"add"},"diff":"hello\n"},{"path":"/w/old.txt","kind":{"type":"delete"},"diff":""},{"path":"/w/a.txt","kind":{"type":"update","move_path":"/w/b.txt"},"diff":"@@"},{"path":"rel.txt","kind":{"type":"update","move_path":null},"diff":"@@"}],"status":"completed"}}}
{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_fc","completedAtMs":3,"item":{"type":"fileChange","id":"call_refused","changes":[{"path":"/w/nope.txt","kind":{"type":"add"},"diff":"x"}],"status":"declined"}}}
{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_fc","completedAtMs":4,"item":{"type":"agentMessage","id":"msg_fc","text":"done","phase":"final_answer","memoryCitation":null,"delivery":null,"questions":null}}}
{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"THREAD","turn":{"id":"turn_fc","items":[],"itemsView":"notLoaded","status":"completed","error":null,"startedAt":1,"completedAt":2,"durationMs":1}}}
CODEX_JSON
`

// codexTurnProgressCalls completes two calls this build routes as
// progress rather than as tool activity: an image view, and an item of a
// type it has never seen.
const codexTurnProgressCalls = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn_pc","items":[],"itemsView":"notLoaded","status":"inProgress","error":null,"startedAt":null,"completedAt":null,"durationMs":null}}}\n' "$id"
cat <<'CODEX_JSON'
{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_pc","completedAtMs":1,"item":{"type":"imageView","id":"img_1","path":"/w/shot.png"}}}
{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_pc","completedAtMs":2,"item":{"type":"calendarInvite","id":"cal_1","status":"completed","attendees":["a@b.c"]}}}
{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"THREAD","turn":{"id":"turn_pc","items":[],"itemsView":"notLoaded","status":"completed","error":null,"startedAt":1,"completedAt":2,"durationMs":1}}}
CODEX_JSON
`

// codexTurnTwoOpen starts two commands, completes the first, and never
// ends the turn; codexMCPTwoOpen is the same on the fallback.
const codexTurnTwoOpen = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn_2","items":[],"itemsView":"notLoaded","status":"inProgress","error":null,"startedAt":null,"completedAt":null,"durationMs":null}}}\n' "$id"
cat <<'CODEX_JSON'
{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"THREAD","turnId":"turn_2","startedAtMs":1,"item":{"type":"commandExecution","id":"exec-a","command":"true","cwd":"/w","processId":null,"source":"agent","status":"inProgress","commandActions":[],"aggregatedOutput":null,"exitCode":null,"durationMs":null}}}
{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"THREAD","turnId":"turn_2","startedAtMs":2,"item":{"type":"commandExecution","id":"exec-b","command":"sleep 60","cwd":"/w","processId":null,"source":"agent","status":"inProgress","commandActions":[],"aggregatedOutput":null,"exitCode":null,"durationMs":null}}}
{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_2","completedAtMs":3,"item":{"type":"commandExecution","id":"exec-a","command":"true","cwd":"/w","processId":null,"source":"agent","status":"completed","commandActions":[],"aggregatedOutput":"","exitCode":0,"durationMs":1}}}
CODEX_JSON
`

const codexMCPTwoOpen = `
cat <<'CODEX_JSON'
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"exec_command_begin","call_id":"exec-a","turn_id":"turn_1","command":["true"],"cwd":"file:///w","parsed_cmd":[],"source":"agent"},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"exec_command_begin","call_id":"exec-b","turn_id":"turn_1","command":["sleep","60"],"cwd":"file:///w","parsed_cmd":[],"source":"agent"},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"exec_command_end","call_id":"exec-a","turn_id":"turn_1","command":["true"],"cwd":"file:///w","parsed_cmd":[],"source":"agent","stdout":"","stderr":"","aggregated_output":"","exit_code":0,"duration":{"secs":0,"nanos":1000000},"formatted_output":"","status":"completed"},"_meta":{"requestId":1}}}
CODEX_JSON
`

// codexTurnLeavesACallOpen starts a command and ends the turn interrupted
// before the command completes.
const codexTurnLeavesACallOpen = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn_open","items":[],"itemsView":"notLoaded","status":"inProgress","error":null,"startedAt":null,"completedAt":null,"durationMs":null}}}\n' "$id"
cat <<'CODEX_JSON'
{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"THREAD","turnId":"turn_open","startedAtMs":1,"item":{"type":"commandExecution","id":"exec-open","command":"/bin/bash -lc 'sleep 60'","cwd":"/w","processId":null,"source":"agent","status":"inProgress","commandActions":[{"type":"unknown","command":"sleep 60"}],"aggregatedOutput":null,"exitCode":null,"durationMs":null}}}
{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"THREAD","turn":{"id":"turn_open","items":[],"itemsView":"notLoaded","status":"interrupted","error":null,"startedAt":1,"completedAt":2,"durationMs":1}}}
CODEX_JSON
`

// codexMCPToolRecorded is the fallback's core events for the same calls,
// in the field spellings of codex-rs/protocol/src/protocol.rs at
// rust-v0.153.4 (ExecCommandBeginEvent / EndEvent, PatchApplyBeginEvent /
// EndEvent, McpToolCallBeginEvent / EndEvent, ViewImageToolCallEvent),
// with the cwd and image path as the file:// URLs PathUri serialises to.
// The patch_apply_end carries no `changes` -- the field is
// `#[serde(default)]`, and an older Codex does not send it -- and the
// last command never ends.
const codexMCPToolRecorded = `
cat <<'CODEX_JSON'
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"exec_command_begin","call_id":"call_exec","turn_id":"turn_1","command":["bash","-lc","cat notes.txt"],"cwd":"file:///w","parsed_cmd":[{"type":"read","cmd":"cat notes.txt","name":"notes.txt","path":"/w/notes.txt"}],"source":"agent"},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"exec_command_end","call_id":"call_exec","turn_id":"turn_1","command":["bash","-lc","cat notes.txt"],"cwd":"file:///w","parsed_cmd":[{"type":"read","cmd":"cat notes.txt","name":"notes.txt","path":"/w/notes.txt"}],"source":"agent","stdout":"alpha\nbeta\n","stderr":"","aggregated_output":"alpha\nbeta\n","exit_code":0,"duration":{"secs":0,"nanos":1000000},"formatted_output":"alpha\nbeta\n","status":"completed"},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"exec_command_begin","call_id":"call_fail","turn_id":"turn_1","command":["bash","-lc","sh -c 'echo oops >&2; exit 3'"],"cwd":"file:///w","parsed_cmd":[{"type":"unknown","cmd":"sh -c 'echo oops >&2; exit 3'"}],"source":"agent"},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"exec_command_end","call_id":"call_fail","turn_id":"turn_1","command":["bash","-lc","sh -c 'echo oops >&2; exit 3'"],"cwd":"file:///w","parsed_cmd":[{"type":"unknown","cmd":"sh -c 'echo oops >&2; exit 3'"}],"source":"agent","stdout":"","stderr":"oops\n","aggregated_output":"oops\n","exit_code":3,"duration":{"secs":0,"nanos":1000000},"formatted_output":"oops\n","status":"failed"},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"patch_apply_begin","call_id":"call_patch","turn_id":"turn_1","auto_approved":true,"changes":{"/w/out.txt":{"type":"add","content":"hello\n"}}},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"patch_apply_end","call_id":"call_patch","turn_id":"turn_1","stdout":"Success. Updated the following files:\nA /w/out.txt\n","stderr":"","success":true,"status":"completed"},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"mcp_tool_call_begin","call_id":"call_mcp","invocation":{"server":"echo","tool":"echo","arguments":{"text":"ping"}}},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"mcp_tool_call_end","call_id":"call_mcp","invocation":{"server":"echo","tool":"echo","arguments":{"text":"ping"}},"duration":{"secs":0,"nanos":2000000},"result":{"Ok":{"content":[{"type":"text","text":"echo: ping"}],"isError":false}}},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"view_image_tool_call","call_id":"call_img","path":"file:///w/shot.png"},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"exec_command_begin","call_id":"call_never","turn_id":"turn_1","command":["sleep","60"],"cwd":"file:///w","parsed_cmd":[],"source":"agent"},"_meta":{"requestId":1}}}
CODEX_JSON
printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"done"}],"structuredContent":{"threadId":"THREAD","content":"done"}}}\n' "$id"
`

func runCodexAppServer(t *testing.T, body string) ([]Action, Spec, error) {
	t.Helper()
	bin, _ := fakeCodexAppServer(t, body)
	spec := codexSpec(t, bin)
	h := startCodexAppServer(t, spec)
	rec := &recorder{}
	_, err := h.Turn(context.Background(), "do the steps", rec)
	return rec.recorded(), spec, err
}

// TestCodexAppServerRecordsOneActionPerCompletedItem: one action per
// completed tool item, in order, with Codex's own exit codes.
func TestCodexAppServerRecordsOneActionPerCompletedItem(t *testing.T) {
	got, spec, err := runCodexAppServer(t, codexTurnRecorded)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	want := []struct {
		tool, command, exit, isError string
	}{
		{ActionExec, "/bin/bash -lc 'cat notes.txt'", "0", "false"},
		{ActionExec, "/bin/bash -lc 'ls -1'", "0", "false"},
		{ActionExec, `/bin/bash -lc "sh -c 'echo oops >&2; exit 3'"`, "3", "true"},
		{ActionExec, `/bin/bash -lc "printf 'hello\\n' > out.txt"`, "0", "false"},
		{ActionExec, `/bin/bash -lc "sed -i 's/hello/goodbye/' out.txt"`, "0", "false"},
		{ActionMCP, "", "absent", "false"},
	}
	if len(got) != len(want) {
		t.Fatalf("recorded %d actions, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		a := got[i]
		if a.Seq != uint64(i+1) || a.Turn != 1 || a.Tool != w.tool || a.Command != w.command {
			t.Errorf("action %d = seq %d turn %d %s %q, want seq %d turn 1 %s %q",
				i, a.Seq, a.Turn, a.Tool, a.Command, i+1, w.tool, w.command)
		}
		if exitOf(a) != w.exit || errOf(a) != w.isError {
			t.Errorf("action %d: exit %s isError %s, want %s %s", i, exitOf(a), errOf(a), w.exit, w.isError)
		}
	}
	cat, printf, mcp := got[0], got[3], got[5]
	if !reflect.DeepEqual(cat.Contents, []Content{{Op: ContentRead, Path: "/w/notes.txt", Seen: []byte("alpha\nbeta\n")}}) {
		t.Errorf("cat contents = %+v, want Codex's own parse of the read, with what it printed", cat.Contents)
	}
	if cat.ResultDigest != Digest([]byte("alpha\nbeta\n")) || cat.Cwd != "/w" || cat.AppTool != "commandExecution" {
		t.Errorf("cat = %+v", cat)
	}
	if printf.ResultType != "string" || printf.ResultDigest != Digest(nil) || printf.Contents != nil {
		t.Errorf("printf = %+v, want the empty text it printed and no read", printf)
	}
	if mcp.MCP == nil || *mcp.MCP != (MCPTarget{Server: "echo", Tool: "echo"}) || mcp.Cwd != spec.Workspace {
		t.Errorf("mcp = %+v", mcp)
	}
	if mcp.ResultDigest != Digest([]byte("echo: ping")) || mcp.ResultType != "string" {
		t.Errorf("mcp result = %s %s", mcp.ResultType, mcp.ResultDigest)
	}
}

func TestCodexAppServerFileChangeIsAWrite(t *testing.T) {
	got, spec, err := runCodexAppServer(t, codexTurnFileChange)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("recorded %d, want the change and the refusal: %+v", len(got), got)
	}
	change, refused := got[0], got[1]
	// A file change answers nothing, and nothing is recorded for it -- not
	// the digest of null, which every change would share.
	if change.Tool != ActionFSWrite || change.AppTool != "fileChange" || errOf(change) != "false" ||
		change.ResultType != "" || change.ResultDigest != "" {
		t.Errorf("change = %+v", change)
	}
	want := []Content{
		{Op: ContentWrite, Path: "/w/out.txt"},
		{Op: ContentDelete, Path: "/w/old.txt"},
		{Op: ContentDelete, Path: "/w/a.txt"},
		{Op: ContentWrite, Path: "/w/b.txt"},
		{Op: ContentWrite, Path: filepath.Join(spec.Workspace, "rel.txt")},
	}
	if !reflect.DeepEqual(change.Contents, want) {
		t.Errorf("contents = %+v, want %+v: the COMPLETED item's changes, a move as a delete and a write", change.Contents, want)
	}
	if !strings.Contains(string(change.Args), `"changes"`) {
		t.Errorf("args = %s", change.Args)
	}
	if refused.ID != "call_refused" || errOf(refused) != "true" || refused.Contents != nil {
		t.Errorf("a declined change = %+v, want isError and no file read", refused)
	}
}

// TestCodexAppServerProgressCallsAreRecordedAfterTheirEventLine: a call
// routed as progress is still a call, recorded -- and, like a tool call,
// only after the app's own line for it has gone out.
func TestCodexAppServerProgressCallsAreRecordedAfterTheirEventLine(t *testing.T) {
	bin, _ := fakeCodexAppServer(t, codexTurnProgressCalls)
	h := startCodexAppServer(t, codexSpec(t, bin))
	rec := &recorder{}
	if _, err := h.Turn(context.Background(), "look", rec); err != nil {
		t.Fatalf("turn: %v", err)
	}
	got := rec.recorded()
	if len(got) != 2 {
		t.Fatalf("recorded %+v, want the image view and the unknown call", got)
	}
	img, cal := got[0], got[1]
	if img.Tool != ActionFSRead || img.IsError != nil || len(img.Contents) != 1 || img.Contents[0].Path != "/w/shot.png" {
		t.Errorf("image view = %+v, want a read of the image with no verdict", img)
	}
	if cal.Tool != ActionOther || cal.AppTool != "calendarInvite" || !strings.Contains(string(cal.Args), "attendees") {
		t.Errorf("unknown call = %+v, want other with the item as its arguments", cal)
	}
	for _, id := range []string{"img_1", "cal_1"} {
		if prev := rec.before(id); !strings.HasPrefix(prev, StreamEvent+" ") || !strings.Contains(prev, `"id":"`+id+`"`) {
			t.Errorf("before action %s came %q, want the app's own event line for it", id, prev)
		}
	}
}

// gateSink holds the action with one id inside Record until released,
// and keeps the order actions reached it in.
type gateSink struct {
	hold             string
	entered, release chan struct{}
	once             sync.Once
	mu               sync.Mutex
	order            []string
}

func (g *gateSink) Chunk(string, []byte) {}

func (g *gateSink) Record(a Action) {
	if a.ID == g.hold {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}
	g.mu.Lock()
	g.order = append(g.order, fmt.Sprintf("%s:%d", a.ID, a.Seq))
	g.mu.Unlock()
}

// TestCodexCancelledTurnFlushesAfterTheCompletionInFlight: the reader is
// still sending a completion (seq 1) when the turn is cancelled, and the
// turn's own flush numbers the call left open (seq 2) on another
// goroutine. The flush must wait for the send in flight -- the engine
// drops an action that arrives behind a higher seq, so a flush that went
// first would lose seq 1. Both Codex clients complete on the reader and
// flush on the turn, so both are held to it.
func TestCodexCancelledTurnFlushesAfterTheCompletionInFlight(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(t *testing.T) Harness
	}{
		{"app-server", func(t *testing.T) Harness {
			bin, _ := fakeCodexAppServer(t, codexTurnTwoOpen)
			return startCodexAppServer(t, codexSpec(t, bin))
		}},
		{"mcp-server", func(t *testing.T) Harness {
			bin, _ := fakeCodexMCP(t, codexMCPTwoOpen)
			return startCodexMCP(t, codexSpec(t, bin))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.start(t)
			g := &gateSink{hold: "exec-a", entered: make(chan struct{}), release: make(chan struct{})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = h.Turn(ctx, "go", g)
			}()
			select {
			case <-g.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("the completion never reached the sink")
			}
			cancel()
			// Room for a flush that did not wait to get ahead.
			time.Sleep(200 * time.Millisecond)
			close(g.release)
			<-done
			g.mu.Lock()
			defer g.mu.Unlock()
			if !reflect.DeepEqual(g.order, []string{"exec-a:1", "exec-b:2"}) {
				t.Fatalf("the sink saw %v, want exec-a:1 then exec-b:2", g.order)
			}
		})
	}
}

func TestCodexAppServerTurnThatEndsWithOpenItemsRecordsThemIncomplete(t *testing.T) {
	got, _, err := runCodexAppServer(t, codexTurnLeavesACallOpen)
	if err == nil {
		t.Fatal("an interrupted turn reported success")
	}
	if len(got) != 1 || !got[0].Incomplete || got[0].ID != "exec-open" || got[0].IsError != nil {
		t.Fatalf("recorded %+v, want exec-open incomplete with no verdict", got)
	}
	if got[0].Command != "/bin/bash -lc 'sleep 60'" {
		t.Errorf("command = %q, want what the started item said", got[0].Command)
	}
}

func TestCodexMCPFallbackRecordsCoreEvents(t *testing.T) {
	bin, _ := fakeCodexMCP(t, codexMCPToolRecorded)
	spec := codexSpec(t, bin)
	h := startCodexMCP(t, spec)
	rec := &recorder{}
	if _, err := h.Turn(context.Background(), "do the steps", rec); err != nil {
		t.Fatalf("turn: %v", err)
	}
	got := rec.recorded()
	if len(got) != 6 {
		t.Fatalf("recorded %d actions, want 6: %+v", len(got), got)
	}
	exec, fail, patch, mcp, img, never := got[0], got[1], got[2], got[3], got[4], got[5]
	if exec.Command != "bash -lc 'cat notes.txt'" || exec.Cwd != "/w" || exitOf(exec) != "0" || errOf(exec) != "false" {
		t.Errorf("exec = %+v", exec)
	}
	if !reflect.DeepEqual(exec.Contents, []Content{{Op: ContentRead, Path: "/w/notes.txt", Seen: []byte("alpha\nbeta\n")}}) ||
		exec.ResultDigest != Digest([]byte("alpha\nbeta\n")) {
		t.Errorf("exec read = %+v %s", exec.Contents, exec.ResultDigest)
	}
	if exitOf(fail) != "3" || errOf(fail) != "true" || fail.Command != `bash -lc 'sh -c '\''echo oops >&2; exit 3'\'''` {
		t.Errorf("failed exec = %+v", fail)
	}
	if patch.Tool != ActionFSWrite || errOf(patch) != "false" ||
		!reflect.DeepEqual(patch.Contents, []Content{{Op: ContentWrite, Path: "/w/out.txt"}}) {
		t.Errorf("patch = %+v, want the begin's changes kept when the end carries none", patch)
	}
	if mcp.MCP == nil || mcp.MCP.Server != "echo" || mcp.ResultDigest != Digest([]byte("echo: ping")) || errOf(mcp) != "false" {
		t.Errorf("mcp = %+v", mcp)
	}
	if img.Tool != ActionFSRead || !reflect.DeepEqual(img.Contents, []Content{{Op: ContentRead, Path: "/w/shot.png"}}) {
		t.Errorf("image view = %+v", img)
	}
	if never.ID != "call_never" || !never.Incomplete || never.Command != "sleep 60" {
		t.Errorf("never-ended exec = %+v", never)
	}
}

// TestCodexMCPEndThatCarriesNoOutputRecordsNoResult: the older end event
// in codexMCPToolOK carries an exit code and nothing it printed. The
// result is unknown, and unknown is absent -- not the digest of nothing.
func TestCodexMCPEndThatCarriesNoOutputRecordsNoResult(t *testing.T) {
	bin, _ := fakeCodexMCP(t, codexMCPToolOK)
	h := startCodexMCP(t, codexSpec(t, bin))
	rec := &recorder{}
	if _, err := h.Turn(context.Background(), "list", rec); err != nil {
		t.Fatalf("turn: %v", err)
	}
	got := rec.recorded()
	if len(got) != 1 {
		t.Fatalf("recorded %+v", got)
	}
	a := got[0]
	if a.Command != "ls -a" || exitOf(a) != "0" || errOf(a) != "false" || a.Cwd != "/w" {
		t.Errorf("= %+v, want the begin's command and cwd with the end's status", a)
	}
	if a.ResultType != "" || a.ResultDigest != "" {
		t.Errorf("result = %q %q, want absent", a.ResultType, a.ResultDigest)
	}
}

// TestRecordingIsTheSameShapeFromBothApps is #442's acceptance criterion:
// the same actions, from Claude Code's fixture and from the Codex
// app-server's, record in the same shape -- the same keys, the same kind,
// the same verdict, and where the apps report the same bytes, the same
// digest. What legitimately differs is named: the app's own id and tool
// word, the arguments as each app expressed them, and a result each app
// words its own way (Claude Code's Bash drops the trailing newline Codex
// keeps; its Write answers in prose where a Codex file change answers
// nothing, so only Claude Code's write carries a result at all).
func TestRecordingIsTheSameShapeFromBothApps(t *testing.T) {
	cRec, _, err := runClaudeFixture(t, claudeRecordedTurn)
	if err != nil {
		t.Fatalf("claude turn: %v", err)
	}
	claude := cRec.recorded()
	codex, _, err := runCodexAppServer(t, codexTurnRecorded)
	if err != nil {
		t.Fatalf("codex turn: %v", err)
	}
	change, _, err := runCodexAppServer(t, codexTurnFileChange)
	if err != nil {
		t.Fatalf("codex file-change turn: %v", err)
	}
	pairs := []struct {
		name          string
		claude, codex Action
		sameResult    bool
		// codexSilent: Codex reports no result for this call at all.
		codexSilent bool
	}{
		{"a command that succeeded", claude[1], codex[1], false, false},
		{"a command that exited 3", claude[2], codex[2], false, false},
		{"a file written", claude[3], change[0], false, true},
		{"an MCP call", claude[6], codex[5], true, false},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			a, b := jsonKeys(t, p.claude), jsonKeys(t, p.codex)
			if p.codexSilent {
				if p.codex.ResultType != "" || p.codex.ResultDigest != "" {
					t.Errorf("codex reported no result, and recorded %s %s", p.codex.ResultType, p.codex.ResultDigest)
				}
				a = slices.DeleteFunc(a, func(k string) bool { return k == "resultType" || k == "resultDigest" })
			}
			if !reflect.DeepEqual(a, b) {
				t.Errorf("keys differ:\n claude %v\n codex  %v", a, b)
			}
			if p.claude.Tool != p.codex.Tool || exitOf(p.claude) != exitOf(p.codex) || errOf(p.claude) != errOf(p.codex) {
				t.Errorf("claude %s exit %s isError %s, codex %s exit %s isError %s",
					p.claude.Tool, exitOf(p.claude), errOf(p.claude), p.codex.Tool, exitOf(p.codex), errOf(p.codex))
			}
			if p.claude.Tool == ActionExec && p.claude.ResultType != p.codex.ResultType {
				t.Errorf("result types %q and %q", p.claude.ResultType, p.codex.ResultType)
			}
			if p.sameResult && (p.claude.ResultDigest != p.codex.ResultDigest || p.claude.ResultType != p.codex.ResultType) {
				t.Errorf("one server answer digests two ways: %s %s vs %s %s",
					p.claude.ResultType, p.claude.ResultDigest, p.codex.ResultType, p.codex.ResultDigest)
			}
			if p.claude.Tool == ActionFSWrite &&
				(len(p.codex.Contents) == 0 || !reflect.DeepEqual(p.claude.Contents[0], p.codex.Contents[0])) {
				t.Errorf("the one file both wrote: claude %+v, codex %+v", p.claude.Contents, p.codex.Contents)
			}
		})
	}
	// The MCP call's arguments are the one argument set both apps pass
	// through untouched, so they must be the same value.
	var a, b any
	_ = json.Unmarshal(claude[6].Args, &a)
	_ = json.Unmarshal(codex[5].Args, &b)
	if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(claude[6].MCP, codex[5].MCP) {
		t.Errorf("mcp args %s vs %s, target %+v vs %+v", claude[6].Args, codex[5].Args, claude[6].MCP, codex[5].MCP)
	}
	// And the same file read -- Claude Code's Read tool, Codex's `cat` --
	// names the same file for the session to digest, though the calls are
	// different kinds.
	if !reflect.DeepEqual(claude[0].Contents, codex[0].Contents) {
		t.Errorf("the one read of notes.txt: claude %+v, codex %+v", claude[0].Contents, codex[0].Contents)
	}
}

func TestCodexItemsThatAreNotCallsAreNotRecorded(t *testing.T) {
	for _, raw := range []string{
		`{"type":"agentMessage","id":"m","text":"hi","phase":"final_answer"}`,
		`{"type":"reasoning","id":"r","summary":[],"content":[]}`,
		`{"type":"userMessage","id":"u","content":[]}`,
		`{"type":"hookPrompt","id":"h"}`,
		`{"type":"plan","id":"p","text":"1. read"}`,
		`{"type":"subAgentActivity","id":"s"}`,
		`{"type":"enteredReviewMode","id":"e","review":"x"}`,
		`{"type":"exitedReviewMode","id":"x","review":"x"}`,
		`{"type":"contextCompaction","id":"c"}`,
		`{"id":"no-type"}`,
		`not json`,
	} {
		if _, _, ok := codexItemCall(json.RawMessage(raw), "/w"); ok {
			t.Errorf("%s was recorded as a call", raw)
		}
	}
	a, _, ok := codexItemCall(json.RawMessage(`{"type":"webSearch","id":"w","query":"go","action":{"type":"openPage","url":"https://go.dev"}}`), "/w")
	if !ok || a.Tool != ActionFetch || a.Query != "go" || a.URL != "https://go.dev" {
		t.Errorf("web search = %+v", a)
	}
}

// TestCodexItemOfATypeThisBuildHasNotSeenIsRecordedAsOther: a newer
// Codex's new tool is a call whose effects nobody here knows, and it is
// recorded as exactly that -- `other`, the item whole as its arguments --
// rather than dropped, which would leave a replay skipping it.
func TestCodexItemOfATypeThisBuildHasNotSeenIsRecordedAsOther(t *testing.T) {
	raw := `{"type":"calendarInvite","id":"cal-1","status":"completed","attendees":["a@b.c"]}`
	a, it, ok := codexItemCall(json.RawMessage(raw), "/w")
	if !ok || a.Tool != ActionOther || a.AppTool != "calendarInvite" || a.ID != "cal-1" || string(a.Args) != raw {
		t.Fatalf("= %+v %v, want other with the item as its arguments", a, ok)
	}
	codexItemFinish(&a, it)
	if a.IsError != nil || a.ResultType != "" || a.ResultDigest != "" {
		t.Errorf("finished = %+v, want no verdict and no result: nothing here reads this type's outcome", a)
	}
}

// TestCodexItemFinishRecordsOnlyWhatTheItemReported: a verdict only where
// the item gives one, a result only where it carries one.
func TestCodexItemFinishRecordsOnlyWhatTheItemReported(t *testing.T) {
	for _, tc := range []struct {
		name, raw            string
		isError, exit, rtype string
		contents             int
	}{
		{"a declined command never ran",
			`{"type":"commandExecution","id":"c","command":"rm -rf x","status":"declined","commandActions":[{"type":"read","path":"a.txt"}],"aggregatedOutput":null,"exitCode":null}`,
			"true", "absent", "", 0},
		{"a declined command that says more still never ran",
			`{"type":"commandExecution","id":"c","command":"rm -rf x","status":"declined","commandActions":[],"aggregatedOutput":"declined by the user","exitCode":-1}`,
			"true", "absent", "", 0},
		{"a command that printed nothing printed the empty text",
			`{"type":"commandExecution","id":"c","command":"true","status":"completed","aggregatedOutput":null,"exitCode":0}`,
			"false", "0", "string", 0},
		{"a command still running reports no exit and no output",
			`{"type":"commandExecution","id":"c","command":"sleep 1","status":"inProgress","aggregatedOutput":null,"exitCode":null}`,
			"true", "absent", "", 0},
		{"a web search has no verdict and here no results",
			`{"type":"webSearch","id":"w","query":"go","action":null}`,
			"absent", "absent", "", 0},
		{"a web search that carried results",
			`{"type":"webSearch","id":"w","query":"go","results":[{"url":"https://go.dev"}]}`,
			"absent", "absent", "array", 0},
		{"an image view has no verdict and keeps its file",
			`{"type":"imageView","id":"i","path":"/w/shot.png"}`,
			"absent", "absent", "", 1},
		{"a generated image that completed",
			`{"type":"imageGeneration","id":"g","status":"completed","revisedPrompt":"a cat","result":"iVBORw0KGgo="}`,
			"false", "absent", "string", 0},
		{"a generated image that failed",
			`{"type":"imageGeneration","id":"g","status":"failed","failure":{"message":"blocked"},"result":""}`,
			"true", "absent", "string", 0},
		{"a generated image with no status says nothing",
			`{"type":"imageGeneration","id":"g","revisedPrompt":"a cat","result":null}`,
			"absent", "absent", "", 0},
		{"an MCP call that returned null",
			`{"type":"mcpToolCall","id":"m","server":"s","tool":"t","status":"completed","arguments":{},"result":null,"error":null}`,
			"false", "absent", "", 0},
		{"a sleep",
			`{"type":"sleep","id":"z","durationMs":100}`,
			"absent", "absent", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, it, ok := codexItemCall(json.RawMessage(tc.raw), "/w")
			if !ok {
				t.Fatalf("not recorded")
			}
			codexItemFinish(&a, it)
			if errOf(a) != tc.isError || exitOf(a) != tc.exit || a.ResultType != tc.rtype || len(a.Contents) != tc.contents {
				t.Errorf("isError %s exit %s result %q contents %d, want %s %s %q %d",
					errOf(a), exitOf(a), a.ResultType, len(a.Contents), tc.isError, tc.exit, tc.rtype, tc.contents)
			}
			if tc.rtype == "" && a.ResultDigest != "" {
				t.Errorf("digest %s for a result nobody reported", a.ResultDigest)
			}
		})
	}
}

// TestCodexCoreFinishRecordsOnlyWhatTheEventReported is the same rule on
// the mcp-server fallback's end events.
func TestCodexCoreFinishRecordsOnlyWhatTheEventReported(t *testing.T) {
	str := func(s string) *string { return &s }
	code := func(n int) *int { return &n }
	ok, notOK := true, false
	for _, tc := range []struct {
		name                 string
		ev                   codexCoreEvent
		isError, exit, rtype string
		seen                 string
		contents             int
	}{
		{"a declined exec never ran",
			codexCoreEvent{Type: "exec_command_end", CallID: "c", Status: "declined", ExitCode: code(-1), Stdout: str(""), Stderr: str("declined"),
				ParsedCmd: []codexCommandAction{{Type: "read", Path: "a.txt"}}},
			"true", "absent", "", "", 0},
		{"a read's stdout is what it saw",
			codexCoreEvent{Type: "exec_command_end", CallID: "c", Status: "completed", ExitCode: code(0), Stdout: str("alpha\n"), Stderr: str("warn\n"),
				AggregatedOutput: str("alpha\nwarn\n"), ParsedCmd: []codexCommandAction{{Type: "read", Path: "a.txt"}}},
			"false", "0", "string", "alpha\n", 1},
		{"a patch's own report is its result",
			codexCoreEvent{Type: "patch_apply_end", CallID: "p", Success: &ok, Stdout: str("Success. Updated the following files:\nA out.txt\n"), Stderr: str("")},
			"false", "absent", "string", "", 0},
		{"a patch that failed",
			codexCoreEvent{Type: "patch_apply_end", CallID: "p", Success: &notOK, Stderr: str("patch rejected")},
			"true", "absent", "string", "", 0},
		{"a web search has no verdict",
			codexCoreEvent{Type: "web_search_end", CallID: "w", Query: "go"},
			"absent", "absent", "", "", 0},
		{"an image view has no verdict and keeps its file",
			codexCoreEvent{Type: "view_image_tool_call", CallID: "i", Path: "/w/shot.png"},
			"absent", "absent", "", "", 1},
		{"an MCP result of null is no result",
			codexCoreEvent{Type: "mcp_tool_call_end", CallID: "m", Result: json.RawMessage(`{"Ok":null}`)},
			"false", "absent", "", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, recorded := codexCoreCall(tc.ev, "/w")
			if !recorded {
				t.Fatalf("not a call")
			}
			if a.Cwd == "" {
				a.Cwd = "/w"
			}
			codexCoreFinish(&a, tc.ev)
			if errOf(a) != tc.isError || exitOf(a) != tc.exit || a.ResultType != tc.rtype || len(a.Contents) != tc.contents {
				t.Errorf("isError %s exit %s result %q contents %d, want %s %s %q %d",
					errOf(a), exitOf(a), a.ResultType, len(a.Contents), tc.isError, tc.exit, tc.rtype, tc.contents)
			}
			if tc.seen != "" && (len(a.Contents) != 1 || string(a.Contents[0].Seen) != tc.seen) {
				t.Errorf("contents %+v, want the read to carry %q", a.Contents, tc.seen)
			}
		})
	}
}

func TestShellJoin(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"ls", "-a"}, "ls -a"},
		{[]string{"bash", "-lc", "ls -1"}, "bash -lc 'ls -1'"},
		{[]string{"echo", "it's"}, `echo 'it'\''s'`},
		{[]string{"printf", ""}, "printf ''"},
		{[]string{"=cmd", "a=b"}, "'=cmd' a=b"},
		{[]string{"go", "test", "./..."}, "go test ./..."},
		// The first word is the command: an assignment or a keyword there
		// would run something else, so either is quoted; later on, both
		// are only words.
		{[]string{"FOO=bar", "BAZ=qux"}, "'FOO=bar' BAZ=qux"},
		{[]string{"time", "make", "if"}, "'time' make if"},
	} {
		if got := shellJoin(tc.argv); got != tc.want {
			t.Errorf("shellJoin(%q) = %s, want %s", tc.argv, got, tc.want)
		}
	}
}

func TestCodexPath(t *testing.T) {
	for in, want := range map[string]string{
		"file:///w/notes.txt":        "/w/notes.txt",
		"file:///w/with%20space.txt": "/w/with space.txt",
		"file://localhost/w/a":       "/w/a",
		"file://elsewhere/share/a":   "",
		"/plain/path":                "/plain/path",
		"":                           "",
		"  file:///w/trimmed.txt  ":  "/w/trimmed.txt",
	} {
		if got := codexPath(in); got != want {
			t.Errorf("codexPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeCodexAppServerAsPrinted is a fake app-server that answers the way
// codex-cli 0.153.4 does -- with no "jsonrpc" header on any frame it
// prints, the handshake included. The client's own requests still carry
// the header, and the server accepts them either way.
func fakeCodexAppServerAsPrinted(t *testing.T, turnBody string) string {
	t.Helper()
	body := strings.ReplaceAll(turnBody, "THREAD", codexFakeThread)
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"app-server\" ]; then echo \"unexpected argv: $*\" >&2; exit 64; fi\n" +
		"while IFS= read -r line; do\n" +
		"  id=$(printf '%s' \"$line\" | sed -n 's/^{\"jsonrpc\":\"2.0\",\"id\":\\([0-9]*\\),.*/\\1/p')\n" +
		"  case \"$line\" in\n" +
		"    *'\"method\":\"initialize\"'*)\n" +
		"      printf '{\"id\":%s,\"result\":{\"userAgent\":\"memql-cockpit/0.153.4\",\"codexHome\":\"/tmp/codex-home\",\"platformFamily\":\"unix\",\"platformOs\":\"linux\"}}\\n' \"$id\"\n" +
		"      printf '{\"method\":\"remoteControl/status/changed\",\"params\":{\"status\":\"disabled\"}}\\n'\n" +
		"      ;;\n" +
		"    *'\"method\":\"thread/start\"'*)\n" +
		"      printf '{\"id\":%s,\"result\":{\"thread\":{\"id\":\"" + codexFakeThread + "\"},\"model\":\"gpt-5.1-codex\"}}\\n' \"$id\"\n" +
		"      ;;\n" +
		"    *'\"method\":\"turn/start\"'*)\n" +
		body +
		"      ;;\n" +
		"  esac\n" +
		"done\n"
	return fakeBinary(t, "codex", script)
}

// TestCodexAppServerSpeaksTheWireAsPrinted: the app-server leaves the
// JSON-RPC header off every frame. A client that required it took the
// answer to `initialize` for stray output and waited for it forever --
// every Codex session on a machine with the app-server hung at start,
// saying nothing, until its wall-clock ceiling. Found on 2026-09-13 by
// driving codex-cli 0.153.4 through this runner; the fakes above had
// always added a header the real server never sends.
func TestCodexAppServerSpeaksTheWireAsPrinted(t *testing.T) {
	spec := codexSpec(t, fakeCodexAppServerAsPrinted(t, codexTurnRecorded))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	h := &codexAppServer{}
	if err := h.Start(ctx, spec); err != nil {
		t.Fatalf("Start against the header-less wire: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	rec := &recorder{}
	res, err := h.Turn(ctx, "do the steps", rec)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.AppSessionRef != codexFakeThread || res.Text != "done" {
		t.Errorf("result = ref %q text %q, want the thread and the final answer", res.AppSessionRef, res.Text)
	}
	if got := rec.recorded(); len(got) != 6 {
		t.Errorf("recorded %d actions, want 6", len(got))
	}
}
