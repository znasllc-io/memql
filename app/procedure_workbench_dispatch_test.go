package app

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/planner"
	procedure "github.com/znasllc-io/memql/integrations/procedure"
)

// procedure_workbench_dispatch_test.go -- the workbench dispatcher against a
// fake dispatchHost that answers the way integrations/workbench does: the
// same envelope in ({action, args, runId, agentId, stepId}), the same node
// out ({ok, action, payload, errorCode, errorMessage}), over an in-memory
// workspace.

// fakeHostWorkspace answers dispatchHost the way a surface does.
type fakeHostWorkspace struct {
	// envelope is the key the action's reply is nested under: "payload" for
	// the workbench, "output" for the worker.
	envelope string
	files    map[string]string
	calls    []map[string]any
	// refuse answers an action with this error code and nothing run.
	refuse map[string]string
	// exec answers an exec that is not a sandbox write.
	exec  func(args map[string]any) map[string]any
	fetch func(args map[string]any) map[string]any
}

func newFakeWorkbench() *fakeHostWorkspace {
	return &fakeHostWorkspace{envelope: "payload", files: map[string]string{}, refuse: map[string]string{}}
}

func (f *fakeHostWorkspace) handler() procedureCapability {
	return func(_ context.Context, envelope map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		f.calls = append(f.calls, envelope)
		action, _ := envelope["action"].(string)
		args, _ := envelope["args"].(map[string]any)
		body := map[string]any{"ok": true, "action": action}
		if code := f.refuse[action]; code != "" {
			body = map[string]any{"ok": false, "action": action, "errorCode": code, "errorMessage": "refused by the fake"}
		} else {
			reply := f.answer(action, args)
			for k, v := range reply {
				body[k] = v
			}
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		return []memorynodes.MemoryNode{{Payload: raw}}, nil
	}
}

func (f *fakeHostWorkspace) answer(action string, args map[string]any) map[string]any {
	path, _ := args["path"].(string)
	switch action {
	case "exec":
		cmd, _ := args["cmd"].(string)
		if i := strings.Index(cmd, "cat > "); i >= 0 {
			f.files[strings.TrimSpace(cmd[i+len("cat > "):])], _ = args["stdin"].(string)
			return map[string]any{f.envelope: map[string]any{"exitCode": 0, "stdout": ""}}
		}
		if f.exec != nil {
			return map[string]any{f.envelope: f.exec(args)}
		}
		return map[string]any{f.envelope: map[string]any{"exitCode": 0, "stdout": ""}}
	case "fs_write":
		f.files[path], _ = args["content"].(string)
		return map[string]any{f.envelope: map[string]any{"path": path, "bytes": len(f.files[path])}}
	case "fs_read":
		content, ok := f.files[path]
		if !ok {
			return map[string]any{"ok": false, "errorCode": "fs_read_failed", "errorMessage": "open " + path + ": no such file or directory"}
		}
		return map[string]any{f.envelope: map[string]any{"path": path, "content": content, "bytes": len(content), "truncated": false}}
	case "fs_stat":
		_, ok := f.files[path]
		return map[string]any{f.envelope: map[string]any{"path": path, "exists": ok}}
	case "fs_list":
		var entries []any
		for name := range f.files {
			entries = append(entries, map[string]any{"name": name})
		}
		return map[string]any{f.envelope: map[string]any{"path": path, "entries": entries, "count": len(entries), "truncated": false}}
	case "http_fetch":
		if f.fetch != nil {
			return map[string]any{f.envelope: f.fetch(args)}
		}
		return map[string]any{f.envelope: map[string]any{"status": 200, "body": ""}}
	}
	return map[string]any{"ok": false, "errorCode": "unknown_action"}
}

// actions is the sequence of actions the fake was asked for.
func (f *fakeHostWorkspace) actions() []string {
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i], _ = c["action"].(string)
	}
	return out
}

func (f *fakeHostWorkspace) lastArgs() map[string]any {
	if len(f.calls) == 0 {
		return nil
	}
	args, _ := f.calls[len(f.calls)-1]["args"].(map[string]any)
	return args
}

// fakeProcedureTools is the MemQL tool registry.
type fakeProcedureTools struct {
	kinds   map[string]string
	result  string
	err     error
	calls   int
	gotName string
	gotArgs map[string]any
	gotCtx  context.Context
}

func (t *fakeProcedureTools) kindOf(name string) (string, bool) {
	kind, ok := t.kinds[name]
	return kind, ok
}

func (t *fakeProcedureTools) execute(ctx context.Context, name string, args map[string]any) (string, error) {
	t.calls++
	t.gotCtx, t.gotName, t.gotArgs = ctx, name, args
	return t.result, t.err
}

var testReasoningAgent = planner.ReasoningAgent{Id: "v1:agents:agent:assistant-owner", RoleSlug: "assistant"}

func testAgents(ctx context.Context, owner string) (planner.ReasoningAgent, error) {
	if owner == "" {
		return planner.ReasoningAgent{}, errors.New("no owner")
	}
	return testReasoningAgent, nil
}

func newTestWorkbenchDispatcher(wb *fakeHostWorkspace, tools procedureToolCatalog) *procedureWorkbenchDispatcher {
	return &procedureWorkbenchDispatcher{
		handler: wb.handler,
		tools:   tools,
		agents:  testAgents,
	}
}

func workbenchStep(tool string, args map[string]any) procedure.DispatchRequest {
	return procedure.DispatchRequest{
		Target:         work.TargetWorkbench,
		OwnerUserId:    "v1:identity:user:owner",
		RunId:          "v1:work:run:replay1",
		StepKey:        "step0",
		IdempotencyKey: "idem-0",
		Tool:           tool,
		Args:           args,
	}
}

func TestAWorkbenchExecTranslatesClaudesBashCommand(t *testing.T) {
	wb := newFakeWorkbench()
	wb.exec = func(args map[string]any) map[string]any {
		return map[string]any{"exitCode": 0, "stdout": "{\"passed\":3}\n"}
	}
	res, err := newTestWorkbenchDispatcher(wb, nil).Dispatch(context.Background(),
		workbenchStep("exec", map[string]any{"command": "cd ./app && npm test", "description": "run the tests", "timeout": float64(120000)}))
	if err != nil {
		t.Fatal(err)
	}
	call := wb.calls[0]
	if call["action"] != "exec" || call["runId"] != "v1:work:run:replay1" || call["stepId"] != "step0" {
		t.Fatalf("envelope = %+v, want exec on the replay run's workspace, naming the step", call)
	}
	args := wb.lastArgs()
	if args["cmd"] != "npm test" || args["cwd"] != "app" || args["timeoutSec"] != 120 {
		t.Fatalf("exec args = %+v, want the command in its directory with its timeout", args)
	}
	obs := res.Observation
	if obs.IsError == nil || *obs.IsError || obs.ExitCode == nil || *obs.ExitCode != 0 || obs.ResultType != "object" {
		t.Fatalf("observation = %+v", obs)
	}
	if res.Delivered {
		t.Fatal("a command in the run's own workspace delivered nothing outside it")
	}
}

func TestAWorkbenchExecUnwrapsCodexsShellVector(t *testing.T) {
	wb := newFakeWorkbench()
	if _, err := newTestWorkbenchDispatcher(wb, nil).Dispatch(context.Background(),
		workbenchStep("exec", map[string]any{"command": []any{"bash", "-lc", "cd . && ls -la"}})); err != nil {
		t.Fatal(err)
	}
	if args := wb.lastArgs(); args["cmd"] != "ls -la" || args["cwd"] != nil {
		t.Fatalf("exec args = %+v, want the script, in the workspace root", args)
	}
}

func TestAWorkbenchExecJoinsAVectorThatIsNotAShell(t *testing.T) {
	wb := newFakeWorkbench()
	if _, err := newTestWorkbenchDispatcher(wb, nil).Dispatch(context.Background(),
		workbenchStep("exec", map[string]any{"command": []any{"grep", "-r", "a b", "*.go"}})); err != nil {
		t.Fatal(err)
	}
	if args := wb.lastArgs(); args["cmd"] != "grep -r 'a b' '*.go'" {
		t.Fatalf("exec args = %+v", args)
	}
}

func TestANonZeroExitIsAnErrorCarryingItsCode(t *testing.T) {
	wb := newFakeWorkbench()
	wb.exec = func(map[string]any) map[string]any { return map[string]any{"exitCode": 1, "stdout": "", "stderr": "1 failing"} }
	res, err := newTestWorkbenchDispatcher(wb, nil).Dispatch(context.Background(), workbenchStep("exec", map[string]any{"command": "npm test"}))
	if err != nil {
		t.Fatal(err)
	}
	if o := res.Observation; o.IsError == nil || !*o.IsError || o.ExitCode == nil || *o.ExitCode != 1 {
		t.Fatalf("observation = %+v", o)
	}
}

// NOTHING RAN, so it is not an observation: the runner must not list a
// refused step among the ones the app is told not to repeat.
func TestAStepTheWorkbenchRefusesBeforeRunningIsAnError(t *testing.T) {
	for _, code := range []string{"command_not_allowed", "no_workbench_peer", "no_forwarded_authority", "workspace_owner_unresolved"} {
		wb := newFakeWorkbench()
		wb.refuse["exec"] = code
		_, err := newTestWorkbenchDispatcher(wb, nil).Dispatch(context.Background(), workbenchStep("exec", map[string]any{"command": "make"}))
		if err == nil || !strings.Contains(err.Error(), code) || !strings.Contains(err.Error(), "step0") {
			t.Errorf("%s -> %v, want an error naming the code and the step", code, err)
		}
	}
}

func TestAWriteReportsTheWorkspaceRelativePathAndTheDigestOfTheBytes(t *testing.T) {
	wb := newFakeWorkbench()
	content := "# Report\n\nThree passed.\n"
	res, err := newTestWorkbenchDispatcher(wb, nil).Dispatch(context.Background(),
		workbenchStep("fs_write", map[string]any{"file_path": "./out/report.md", "content": content}))
	if err != nil {
		t.Fatal(err)
	}
	if got := wb.actions(); len(got) != 1 || got[0] != "fs_write" {
		t.Fatalf("actions = %v, want one fs_write", got)
	}
	if args := wb.lastArgs(); args["path"] != "out/report.md" || args["content"] != content {
		t.Fatalf("fs_write args = %+v", args)
	}
	want := work.ContentDigest{Op: "write", Path: "out/report.md", Digest: procedureDigest(content)}
	if o := res.Observation; o.IsError == nil || *o.IsError || len(o.Contents) != 1 || o.Contents[0] != want {
		t.Fatalf("observation = %+v, want %+v", o, want)
	}
}

// A SHADOW WRITES NO ROW. The workbench promotes every fs_write into the
// owner's Library, so a shadow's write goes through exec -- the same bytes,
// no promotion.
func TestAShadowWriteGoesThroughExecSoNoLibraryRowIsWritten(t *testing.T) {
	wb := newFakeWorkbench()
	req := workbenchStep("fs_write", map[string]any{"file_path": "./out/report.md", "content": "hello\n"})
	req.Sandbox = true
	res, err := newTestWorkbenchDispatcher(wb, nil).Dispatch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range wb.actions() {
		if a == "fs_write" {
			t.Fatalf("a shadow called fs_write, which writes a Library row: %v", wb.actions())
		}
	}
	args := wb.lastArgs()
	if args["cmd"] != "mkdir -p out && cat > out/report.md" || args["stdin"] != "hello\n" {
		t.Fatalf("exec args = %+v", args)
	}
	if wb.files["out/report.md"] != "hello\n" {
		t.Fatalf("the bytes did not land: %+v", wb.files)
	}
	if o := res.Observation; len(o.Contents) != 1 || o.Contents[0].Digest != procedureDigest("hello\n") {
		t.Fatalf("observation = %+v", o)
	}
}

func TestAnEditReplacesTheExactStringAndReportsTheEditedFile(t *testing.T) {
	wb := newFakeWorkbench()
	wb.files["src/app.go"] = "package app\n\nconst v = 1\n"
	res, err := newTestWorkbenchDispatcher(wb, nil).Dispatch(context.Background(),
		workbenchStep("fs_write", map[string]any{"file_path": "./src/app.go", "old_string": "v = 1", "new_string": "v = 2"}))
	if err != nil {
		t.Fatal(err)
	}
	want := "package app\n\nconst v = 2\n"
	if wb.files["src/app.go"] != want {
		t.Fatalf("file = %q", wb.files["src/app.go"])
	}
	if o := res.Observation; o.IsError == nil || *o.IsError || o.Contents[0].Digest != procedureDigest(want) || o.Contents[0].Op != "write" {
		t.Fatalf("observation = %+v, want the write of the edited file", o)
	}
	if got := strings.Join(wb.actions(), ","); got != "fs_read,fs_write" {
		t.Fatalf("actions = %s", got)
	}
}

// Refused exactly where the app's Edit refuses -- as an OBSERVATION, since
// the step ran and the tool said no -- and nothing is written.
func TestAnEditTheAppWouldRefuseIsAnErrorAndWritesNothing(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"absent":    {"file_path": "a.txt", "old_string": "missing", "new_string": "x"},
		"ambiguous": {"file_path": "a.txt", "old_string": "a", "new_string": "b"},
		"multi, second refused": {"file_path": "a.txt", "edits": []any{
			map[string]any{"old_string": "a a", "new_string": "c"},
			map[string]any{"old_string": "zzz", "new_string": "y"},
		}},
	} {
		wb := newFakeWorkbench()
		wb.files["a.txt"] = "a a"
		res, err := newTestWorkbenchDispatcher(wb, nil).Dispatch(context.Background(), workbenchStep("fs_write", args))
		if err != nil {
			t.Fatalf("%s: %v -- an app-refused edit is an observation, not a dispatch failure", name, err)
		}
		if o := res.Observation; o.IsError == nil || !*o.IsError || len(o.Contents) != 0 {
			t.Errorf("%s: observation = %+v", name, o)
		}
		if out, _ := res.Output.(map[string]any); out["error"] == nil {
			t.Errorf("%s: the receipt does not say why: %+v", name, res.Output)
		}
		if wb.files["a.txt"] != "a a" {
			t.Errorf("%s: the file was written: %q", name, wb.files["a.txt"])
		}
	}
}

func TestAMultiEditAppliesInOrder(t *testing.T) {
	wb := newFakeWorkbench()
	wb.files["a.txt"] = "one two three"
	_, err := newTestWorkbenchDispatcher(wb, nil).Dispatch(context.Background(), workbenchStep("fs_write", map[string]any{
		"file_path": "./a.txt",
		"edits": []any{
			map[string]any{"old_string": "one", "new_string": "1"},
			map[string]any{"old_string": "1 two", "new_string": "12"},
			map[string]any{"old_string": "e", "new_string": "E", "replace_all": true},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if wb.files["a.txt"] != "12 thrEE" {
		t.Fatalf("file = %q", wb.files["a.txt"])
	}
}

func TestAnEditWithAnEmptyOldStringCreatesAMissingFile(t *testing.T) {
	wb := newFakeWorkbench()
	res, err := newTestWorkbenchDispatcher(wb, nil).Dispatch(context.Background(),
		workbenchStep("fs_write", map[string]any{"file_path": "new.txt", "old_string": "", "new_string": "fresh\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if wb.files["new.txt"] != "fresh\n" || res.Observation.IsError == nil || *res.Observation.IsError {
		t.Fatalf("file = %q, observation = %+v", wb.files["new.txt"], res.Observation)
	}
	if got := strings.Join(wb.actions(), ","); got != "fs_read,fs_stat,fs_write" {
		t.Fatalf("actions = %s -- a missing file is established by fs_stat, not assumed from a failed read", got)
	}
}

func TestAReadReportsTheDigestOfTheFile(t *testing.T) {
	wb := newFakeWorkbench()
	wb.files["data/in.csv"] = "a,b\n1,2\n"
	res, err := newTestWorkbenchDispatcher(wb, nil).Dispatch(context.Background(),
		workbenchStep("fs_read", map[string]any{"file_path": "./data/in.csv", "offset": float64(1), "limit": float64(10)}))
	if err != nil {
		t.Fatal(err)
	}
	want := work.ContentDigest{Op: "read", Path: "data/in.csv", Digest: procedureDigest("a,b\n1,2\n")}
	if o := res.Observation; o.IsError == nil || *o.IsError || len(o.Contents) != 1 || o.Contents[0] != want {
		t.Fatalf("observation = %+v, want %+v", o, want)
	}
	if res.Delivered {
		t.Fatal("a read delivers nothing")
	}
}

func TestAReadOfAMissingFileIsAnErrorWithNoContent(t *testing.T) {
	wb := newFakeWorkbench()
	res, err := newTestWorkbenchDispatcher(wb, nil).Dispatch(context.Background(), workbenchStep("fs_read", map[string]any{"file_path": "gone.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	if o := res.Observation; o.IsError == nil || !*o.IsError || len(o.Contents) != 0 {
		t.Fatalf("observation = %+v", o)
	}
}

func TestAFetchIsAGetTypedByItsBody(t *testing.T) {
	wb := newFakeWorkbench()
	wb.fetch = func(map[string]any) map[string]any { return map[string]any{"status": 200, "body": "{\"items\":[]}"} }
	res, err := newTestWorkbenchDispatcher(wb, nil).Dispatch(context.Background(),
		workbenchStep("fetch", map[string]any{"url": "https://example.com/items.json", "prompt": "list the items"}))
	if err != nil {
		t.Fatal(err)
	}
	if args := wb.lastArgs(); args["url"] != "https://example.com/items.json" || args["method"] != "GET" || args["prompt"] != nil {
		t.Fatalf("http_fetch args = %+v, want a GET of the url and nothing of the prompt", args)
	}
	if o := res.Observation; o.IsError == nil || *o.IsError || o.ResultType != "object" {
		t.Fatalf("observation = %+v", o)
	}
	wb.fetch = func(map[string]any) map[string]any { return map[string]any{"status": 503, "body": "busy"} }
	res, err = newTestWorkbenchDispatcher(wb, nil).Dispatch(context.Background(), workbenchStep("fetch", map[string]any{"url": "https://example.com/"}))
	if err != nil || res.Observation.IsError == nil || !*res.Observation.IsError {
		t.Fatalf("a 503 = %+v, %v", res.Observation, err)
	}
}

func TestAPathOutsideTheWorkspaceIsRefusedBeforeAnythingRuns(t *testing.T) {
	for _, p := range []string{"/Users/someone/notes.md", "../escape.txt", "~/notes.md"} {
		wb := newFakeWorkbench()
		_, err := newTestWorkbenchDispatcher(wb, nil).Dispatch(context.Background(), workbenchStep("fs_write", map[string]any{"file_path": p, "content": "x"}))
		if err == nil {
			t.Errorf("%q was dispatched", p)
		}
		if len(wb.calls) != 0 {
			t.Errorf("%q reached the workbench: %v", p, wb.actions())
		}
	}
}

func TestABackgroundCommandIsRefused(t *testing.T) {
	wb := newFakeWorkbench()
	_, err := newTestWorkbenchDispatcher(wb, nil).Dispatch(context.Background(),
		workbenchStep("exec", map[string]any{"command": "npm run dev", "run_in_background": true}))
	if err == nil || len(wb.calls) != 0 {
		t.Fatalf("a background command ran: %v, %v", err, wb.actions())
	}
}

func TestTheWorkbenchDispatcherRefusesWhatIsNotItsStep(t *testing.T) {
	wb := newFakeWorkbench()
	d := newTestWorkbenchDispatcher(wb, nil)
	for name, mutate := range map[string]func(*procedure.DispatchRequest){
		"machine target": func(r *procedure.DispatchRequest) { r.Target = work.TargetMachine },
		"no owner":       func(r *procedure.DispatchRequest) { r.OwnerUserId = "" },
		"no run":         func(r *procedure.DispatchRequest) { r.RunId = " " },
		"unknown tool":   func(r *procedure.DispatchRequest) { r.Tool = "automation:deploy" },
	} {
		req := workbenchStep("exec", map[string]any{"command": "ls"})
		mutate(&req)
		if _, err := d.Dispatch(context.Background(), req); err == nil {
			t.Errorf("%s was dispatched", name)
		}
	}
	if len(wb.calls) != 0 {
		t.Fatalf("a refused step reached the workbench: %v", wb.actions())
	}
	missing := &procedureWorkbenchDispatcher{handler: func() procedureCapability { return nil }}
	if _, err := missing.Dispatch(context.Background(), workbenchStep("exec", map[string]any{"command": "ls"})); err == nil || !strings.Contains(err.Error(), "no workbench dispatchHost") {
		t.Fatalf("a node with no workbench = %v", err)
	}
}

// EVERY DISPATCHER REPORTS isError (the plan's stream notes): an absent error
// flag is an absent measurement, which no comparison accepts.
func TestEveryToolReportsTheErrorFlag(t *testing.T) {
	wb := newFakeWorkbench()
	wb.files["a.txt"] = "x"
	tools := &fakeProcedureTools{kinds: map[string]string{"librarySearch": "query"},
		result: `{"content":[{"type":"text","text":"[]"}],"isError":false}`}
	d := newTestWorkbenchDispatcher(wb, tools)
	for tool, args := range map[string]map[string]any{
		"exec":     {"command": "ls"},
		"fs_write": {"file_path": "b.txt", "content": "y"},
		"fs_read":  {"file_path": "a.txt"},
		"fetch":    {"url": "https://example.com/"},
		"mcp":      {"tool": "librarySearch", "arguments": map[string]any{"q": "x"}},
	} {
		res, err := d.Dispatch(context.Background(), workbenchStep(tool, args))
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if res.Observation.IsError == nil {
			t.Errorf("%s reported no error flag", tool)
		}
	}
}

// An `mcp` step is a call back into MemQL, run as the app's call ran: the
// owner's actor, under the owner's reasoning agent, with the server's owner
// winning over any recorded one -- and typed by the MCP recorder's own rule.
func TestAnMCPQueryRunsAsTheOwnersAgentAndIsTypedAsItWasRecorded(t *testing.T) {
	tools := &fakeProcedureTools{kinds: map[string]string{"librarySearch": "query"},
		result: `{"content":[{"type":"text","text":"42"}],"isError":false}`}
	req := workbenchStep("mcp", map[string]any{"tool": "librarySearch", "arguments": map[string]any{"q": "budget", "ownerUserId": "v1:identity:user:somebody-else"}})
	req.Sandbox = true
	res, err := newTestWorkbenchDispatcher(newFakeWorkbench(), tools).Dispatch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if tools.gotName != "librarySearch" || tools.gotArgs["q"] != "budget" {
		t.Fatalf("called %q with %+v", tools.gotName, tools.gotArgs)
	}
	ctx := tools.gotCtx
	if access, ok := auth.AccessFromContext(ctx); !ok || access.UserId != "v1:identity:user:owner" {
		t.Fatalf("the call did not run as the owner: %+v", access)
	}
	if memql.ActingAgentRoleFromContext(ctx) != "assistant" || memql.ActingAgentIdFromContext(ctx) != testReasoningAgent.Id {
		t.Fatalf("acting agent = %q / %q", memql.ActingAgentRoleFromContext(ctx), memql.ActingAgentIdFromContext(ctx))
	}
	if defaults := common.ToolDefaultsFromContext(ctx); defaults["ownerUserId"] != "v1:identity:user:owner" {
		t.Fatalf("tool defaults = %+v, want the owner to win over a recorded one", defaults)
	}
	// "42" is text to the MCP recorder -- it is not component/work's number.
	if o := res.Observation; o.IsError == nil || *o.IsError || o.ResultType != "string" {
		t.Fatalf("observation = %+v", o)
	}
	if res.Delivered {
		t.Fatal("a query delivers nothing")
	}
}

func TestAShadowRefusesAnMCPToolThatCouldWrite(t *testing.T) {
	for _, kind := range []string{"mutation", "logic", "builtin", "automation", "webhook", "unknown"} {
		tools := &fakeProcedureTools{kinds: map[string]string{"saveNote": kind}}
		req := workbenchStep("mcp", map[string]any{"tool": "saveNote"})
		req.Sandbox = true
		if _, err := newTestWorkbenchDispatcher(newFakeWorkbench(), tools).Dispatch(context.Background(), req); err == nil {
			t.Errorf("a shadow ran a %s tool", kind)
		}
		if tools.calls != 0 {
			t.Errorf("a %s tool was executed in a shadow", kind)
		}
	}
}

func TestOutsideAShadowAnMCPToolThatWritesRunsAndDelivers(t *testing.T) {
	tools := &fakeProcedureTools{kinds: map[string]string{"saveNote": "mutation"},
		result: `{"content":[{"type":"text","text":"{\"id\":\"n1\"}"}],"isError":false}`}
	res, err := newTestWorkbenchDispatcher(newFakeWorkbench(), tools).Dispatch(context.Background(),
		workbenchStep("mcp", map[string]any{"tool": "saveNote", "arguments": map[string]any{"text": "x"}}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Delivered || res.Observation.ResultType != "object" {
		t.Fatalf("a mutation = %+v, delivered %v", res.Observation, res.Delivered)
	}
	tools.result = `{"content":[{"type":"text","text":"denied"}],"isError":true}`
	res, err = newTestWorkbenchDispatcher(newFakeWorkbench(), tools).Dispatch(context.Background(),
		workbenchStep("mcp", map[string]any{"tool": "saveNote"}))
	if err != nil || res.Delivered || res.Observation.IsError == nil || !*res.Observation.IsError {
		t.Fatalf("a failed mutation = %+v, delivered %v, %v", res.Observation, res.Delivered, err)
	}
}

func TestAnMCPStepThatNamesNoMemQLToolIsRefused(t *testing.T) {
	tools := &fakeProcedureTools{kinds: map[string]string{"librarySearch": "query"}}
	for name, args := range map[string]map[string]any{
		"no tool":        {"arguments": map[string]any{}},
		"another server": {"server": "github", "tool": "librarySearch"},
		"not registered": {"tool": "runQuery"},
		"bad arguments":  {"tool": "librarySearch", "arguments": "q=x"},
	} {
		if _, err := newTestWorkbenchDispatcher(newFakeWorkbench(), tools).Dispatch(context.Background(), workbenchStep("mcp", args)); err == nil {
			t.Errorf("%s was run", name)
		}
	}
	if tools.calls != 0 {
		t.Fatalf("a refused mcp step was executed")
	}
	tools.result = `{"content":[{"type":"text","text":"[]"}],"isError":false}`
	if _, err := newTestWorkbenchDispatcher(newFakeWorkbench(), tools).Dispatch(context.Background(),
		workbenchStep("mcp", map[string]any{"server": "memql", "tool": "librarySearch"})); err != nil {
		t.Fatalf("the memql server spelled out = %v", err)
	}
}

// A QUERY handler can call a mutation, so the handler TYPE says nothing; the
// call it makes does.
func TestAQueryHandlersKindIsTheConstructItCalls(t *testing.T) {
	for src, want := range map[string]string{
		"paginate(query searchUsers(active: args.active), args.limit)":         "query",
		"query upcomingEvents(windowStart: args.windowStart)":                  "query",
		"mutation updateCalendarEvent(eventId: args.eventId, payload: args.p)": "mutation",
		"builtin help(name: args.name)":                                        "builtin",
		"not a handler at all (":                                               "unknown",
	} {
		if got := procedureQueryHandlerKind(src); got != want {
			t.Errorf("%q -> %q, want %q", src, got, want)
		}
	}
}

// The registry the engine holds answers kindOf from the tree that actually
// ships, which is the negative control on the fake above: a real query tool
// reads, a real mutation tool does not.
func TestTheEngineToolRegistryClassifiesShippedTools(t *testing.T) {
	engine := procedureTestEngine(t)
	tools := engineProcedureTools{engine: engine}
	if kind, ok := tools.kindOf("searchUsers"); !ok || kind != "query" {
		t.Fatalf("searchUsers = %q, %v; want a registered query tool", kind, ok)
	}
	var mutating []string
	for _, tool := range engine.Tools().List() {
		if kind, ok := tools.kindOf(tool.Name); ok && kind != "query" {
			mutating = append(mutating, tool.Name+"="+kind)
		}
	}
	sort.Strings(mutating)
	if len(mutating) == 0 {
		t.Fatal("no shipped tool classifies as anything but a query, so the refusal a shadow applies is untested against the real registry")
	}
	if _, ok := tools.kindOf("runQuery"); ok {
		t.Fatal("runQuery is an MCP meta-tool, not a registered MemQL tool")
	}
}
