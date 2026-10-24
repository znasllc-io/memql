package harness

import (
	"encoding/json"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

// record_test.go pins the recording's wire shape and its bookkeeping.
//
// THE NAMES ARE THE CONTRACT. The engine half (memql#5396) decodes these
// events by key, and it has not been written yet -- so this repository is
// DEFINING the keys, the way TestWireContract says it defines the model
// labels' `params` and `quant`. A rename here compiles everywhere and
// breaks a reader nobody can see from this side; the tests below make it
// a decision.

// jsonKeys marshals v and returns its top-level keys, sorted.
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("%T did not marshal to an object: %v (%s)", v, err, body)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestActionWireContract(t *testing.T) {
	code, failed, size := 3, true, int64(5)
	data := "hello"
	full := Action{
		Type: ActionEventType, V: RecordVersion, Seq: 7, Turn: 2,
		ID: "toolu_1", ParentID: "toolu_0", Tool: ActionExec, AppTool: "Bash",
		Args: json.RawMessage(`{"command":"ls"}`), ArgsDigest: Digest([]byte(`{"command":"ls"}`)),
		ArgsOmitted: ArgsTooLarge, Cwd: "/w", Command: "ls",
		MCP: &MCPTarget{Server: "memql", Tool: "query"}, URL: "https://example.com", Query: "q",
		ExitCode: &code, IsError: &failed, ResultType: "string", ResultDigest: Digest([]byte("x")),
		Contents: []Content{{
			Op: ContentWrite, Path: "/w/a.txt", Digest: Digest([]byte("hello")), Bytes: &size,
			Encoding: EncodingUTF8, Data: &data, Omitted: OmittedOverBudget,
		}},
		Incomplete: true,
	}
	want := []string{
		"appTool", "args", "argsDigest", "argsOmitted", "command", "contents", "cwd", "exitCode", "id", "incomplete",
		"isError", "mcp", "parentId", "query", "resultDigest", "resultType", "seq", "tool",
		"turn", "type", "url", "v",
	}
	if got := jsonKeys(t, full); !reflect.DeepEqual(got, want) {
		t.Errorf("Action keys = %v, want %v", got, want)
	}
	if got := jsonKeys(t, full.Contents[0]); !reflect.DeepEqual(got,
		[]string{"bytes", "data", "digest", "encoding", "omitted", "op", "path"}) {
		t.Errorf("Content keys = %v", got)
	}
	if got := jsonKeys(t, *full.MCP); !reflect.DeepEqual(got, []string{"server", "tool"}) {
		t.Errorf("MCPTarget keys = %v", got)
	}

	// ABSENT MEANS ABSENT. A call that reported nothing optional carries
	// none of the optional keys -- not an exitCode of 0, not an isError of
	// false, not an empty contents list.
	bare := Action{Type: ActionEventType, V: RecordVersion, Seq: 1, Turn: 1, ID: "c",
		Tool: ActionOther, AppTool: "Mystery", Args: json.RawMessage("null"), Cwd: "/w"}
	if got := jsonKeys(t, bare); !reflect.DeepEqual(got,
		[]string{"appTool", "args", "cwd", "id", "seq", "tool", "turn", "type", "v"}) {
		t.Errorf("a bare Action carries %v; every optional key must be absent", got)
	}
	if got := jsonKeys(t, Content{Op: ContentRead, Path: "/w/x"}); !reflect.DeepEqual(got, []string{"op", "path"}) {
		t.Errorf("a path-only Content carries %v", got)
	}
	// What the app itself reported of a file is the session's to compare
	// and never travels.
	if got := jsonKeys(t, Content{Op: ContentRead, Path: "/w/x", Seen: []byte("secret")}); !reflect.DeepEqual(got, []string{"op", "path"}) {
		t.Errorf("Seen went on the wire: %v", got)
	}

	// Zero is a fact when it is one: an empty file has zero bytes and
	// empty data, and both must travel.
	zero, empty := int64(0), ""
	body, _ := json.Marshal(Content{Op: ContentRead, Path: "/w/e", Bytes: &zero, Encoding: EncodingUTF8, Data: &empty})
	if string(body) != `{"op":"read","path":"/w/e","bytes":0,"encoding":"utf8","data":""}` {
		t.Errorf("an empty file marshals as %s", body)
	}
}

func TestFingerprintWireContract(t *testing.T) {
	entries, size := 4, int64(10)
	fp := Fingerprint{
		Type: FingerprintEventType, V: RecordVersion, Seq: 0, TakenAt: "2026-09-13T00:00:00Z",
		App:      FingerprintApp{ID: "claude-code", Version: "2.1.270 (Claude Code)", Harness: HarnessClaudeHeadless},
		Platform: Platform{OS: "linux", Arch: "amd64"},
		Tools:    []ToolVersion{{Name: "git", Version: "git version 2.43.0"}},
		Cwd:      "/w", CwdDigest: Digest(nil), CwdEntries: &entries, CwdTruncated: true,
		Variables: []Variable{{Name: "PATH", Set: true, Digest: Digest([]byte("/bin"))}},
		Inputs:    []Input{{Artifact: "art-1", Path: "/w/in.txt", Digest: Digest(nil), Bytes: &size, Omitted: OmittedTooLarge}},
	}
	want := []string{"app", "cwd", "cwdDigest", "cwdEntries", "cwdTruncated", "inputs",
		"platform", "seq", "takenAt", "tools", "type", "v", "variables"}
	if got := jsonKeys(t, fp); !reflect.DeepEqual(got, want) {
		t.Errorf("Fingerprint keys = %v, want %v", got, want)
	}
	if got := jsonKeys(t, fp.App); !reflect.DeepEqual(got, []string{"harness", "id", "version"}) {
		t.Errorf("FingerprintApp keys = %v", got)
	}
	if got := jsonKeys(t, fp.Inputs[0]); !reflect.DeepEqual(got,
		[]string{"artifact", "bytes", "digest", "omitted", "path"}) {
		t.Errorf("Input keys = %v", got)
	}
	// An unset variable says so; it does not vanish, because "unset" and
	// "never asked about" are different facts.
	if got := jsonKeys(t, Variable{Name: "TZ"}); !reflect.DeepEqual(got, []string{"name", "set"}) {
		t.Errorf("an unset Variable carries %v", got)
	}
	// seq 0 is the fingerprint's own number and must be ON the wire.
	body, _ := json.Marshal(fp)
	var decoded map[string]any
	_ = json.Unmarshal(body, &decoded)
	if decoded["seq"] != float64(0) {
		t.Errorf("seq = %v, want 0 present", decoded["seq"])
	}
}

func TestFingerprintVariablesAreNamedByTheHarness(t *testing.T) {
	has := func(names []string, want string) bool {
		for _, n := range names {
			if n == want {
				return true
			}
		}
		return false
	}
	for _, word := range []string{HarnessClaudeHeadless, HarnessCodexAppServer, HarnessCodexMCP, ""} {
		names := FingerprintVariables(word)
		for _, common := range []string{"PATH", "SHELL", "LANG", "LC_ALL", "TZ"} {
			if !has(names, common) {
				t.Errorf("%q: %s is missing from %v", word, common, names)
			}
		}
		if has(names, "CODEX_HOME") {
			t.Errorf("%q names CODEX_HOME, which the cockpit makes fresh for every session", word)
		}
	}
	if !has(FingerprintVariables(HarnessClaudeHeadless), "CLAUDE_CONFIG_DIR") {
		t.Error("Claude Code's settings directory must be in its fingerprint")
	}
	// A copy every time: a caller that appends must not change the next
	// session's list.
	first := FingerprintVariables(HarnessCodexMCP)
	first[0] = "MUTATED"
	if FingerprintVariables(HarnessCodexMCP)[0] == "MUTATED" {
		t.Error("FingerprintVariables returned shared state")
	}
}

// TestSinkFuncCarriesAnActionAsAnEventChunk: a caller with no recording
// of its own to keep apart still receives every action, as the event
// chunk it would be on the wire.
func TestSinkFuncCarriesAnActionAsAnEventChunk(t *testing.T) {
	var streams, bodies []string
	sink := SinkFunc(func(stream string, data []byte) {
		streams = append(streams, stream)
		bodies = append(bodies, string(data))
	})
	sink.Record(Action{Type: ActionEventType, V: RecordVersion, Seq: 1, ID: "x", Tool: ActionOther,
		Args: json.RawMessage("null")})
	if len(streams) != 1 || streams[0] != StreamEvent {
		t.Fatalf("streams = %v, want one %q chunk", streams, StreamEvent)
	}
	var got Action
	if err := json.Unmarshal([]byte(bodies[0]), &got); err != nil || got.Type != ActionEventType || got.ID != "x" {
		t.Errorf("chunk = %q (%v), want the action as JSON", bodies[0], err)
	}
	if bodies[0][len(bodies[0])-1] != '\n' {
		t.Errorf("chunk %q is not a line", bodies[0])
	}
	var none SinkFunc
	none.Record(Action{}) // must not panic
}

func TestContentForMakesRelativePathsAbsolute(t *testing.T) {
	if got := contentFor(ContentRead, "sub/../notes.txt", "/w"); len(got) != 1 || got[0].Path != "/w/notes.txt" {
		t.Errorf("contentFor = %+v, want /w/notes.txt", got)
	}
	if got := contentFor(ContentWrite, "/abs/x", "/w"); got[0].Path != "/abs/x" || got[0].Op != ContentWrite {
		t.Errorf("contentFor = %+v", got)
	}
	if got := contentFor(ContentRead, "  ", "/w"); got != nil {
		t.Errorf("an empty path named %+v", got)
	}
}

// --- the bookkeeping --------------------------------------------------

func TestRecordingNumbersDenselyAcrossTurnsInCompletionOrder(t *testing.T) {
	r := newRecording()
	r.startTurn()
	r.begin(Action{ID: "a", Tool: ActionExec})
	r.begin(Action{ID: "b", Tool: ActionFSRead})
	b, okB := r.complete("b", nil)
	a, okA := r.complete("a", nil)
	r.startTurn()
	r.begin(Action{ID: "c", Tool: ActionMCP})
	c, okC := r.complete("c", nil)
	if !okA || !okB || !okC {
		t.Fatal("a first completion was refused")
	}
	for i, got := range []Action{b, a, c} {
		if got.Seq != uint64(i+1) {
			t.Errorf("action %s has seq %d, want %d: seq counts completions, densely", got.ID, got.Seq, i+1)
		}
		if got.Type != ActionEventType || got.V != RecordVersion {
			t.Errorf("action %s was not stamped for the wire: %+v", got.ID, got)
		}
		if string(got.Args) != "null" {
			t.Errorf("action %s args = %s, want null when none were reported", got.ID, got.Args)
		}
	}
	if b.Turn != 1 || a.Turn != 1 || c.Turn != 2 {
		t.Errorf("turns = %d %d %d, want 1 1 2", b.Turn, a.Turn, c.Turn)
	}
}

func TestRecordingEmitsOneCallOnce(t *testing.T) {
	r := newRecording()
	r.startTurn()
	r.begin(Action{ID: "x", Tool: ActionExec})
	if _, ok := r.complete("x", nil); !ok {
		t.Fatal("first completion refused")
	}
	if _, ok := r.complete("x", nil); ok {
		t.Error("a repeated result recorded one call twice")
	}
	// A begin replayed after the call was emitted must not reopen it.
	r.begin(Action{ID: "x", Tool: ActionExec})
	if got := r.flush(); len(got) != 0 {
		t.Errorf("a replayed begin reopened an emitted call: %+v", got)
	}
}

func TestRecordingCompletionWithoutABeginStandsAlone(t *testing.T) {
	r := newRecording()
	r.startTurn()
	got, ok := r.complete("orphan", func(a *Action) {
		failed := false
		a.IsError = &failed
	})
	if !ok {
		t.Fatal("a result whose call was never seen was dropped; it is still a completed call")
	}
	if got.Tool != ActionOther || string(got.Args) != "null" || got.IsError == nil {
		t.Errorf("orphan = %+v, want an `other` call with no arguments and the reported verdict", got)
	}
}

func TestRecordingFlushRecordsWhatNeverFinished(t *testing.T) {
	r := newRecording()
	r.startTurn()
	code, failed := 0, false
	r.begin(Action{ID: "first", Tool: ActionFSWrite, Contents: []Content{{Op: ContentWrite, Path: "/w/a"}}})
	r.begin(Action{ID: "second", Tool: ActionExec, ExitCode: &code, IsError: &failed, ResultType: "string"})
	r.begin(Action{ID: "done", Tool: ActionExec})
	if _, ok := r.complete("done", nil); !ok {
		t.Fatal("complete refused")
	}
	open := r.flush()
	if len(open) != 2 || open[0].ID != "first" || open[1].ID != "second" {
		t.Fatalf("flush = %+v, want first then second, in the order they began", open)
	}
	for _, a := range open {
		if !a.Incomplete {
			t.Errorf("%s is not marked incomplete", a.ID)
		}
		if a.IsError != nil || a.ExitCode != nil || a.ResultType != "" || a.ResultDigest != "" {
			t.Errorf("%s kept a result nobody reported: %+v", a.ID, a)
		}
		if a.Contents != nil {
			t.Errorf("%s would have a file read for a call nobody saw finish", a.ID)
		}
	}
	if open[0].Seq != 2 || open[1].Seq != 3 {
		t.Errorf("seqs = %d, %d, want 2, 3 after the one completion", open[0].Seq, open[1].Seq)
	}
	if _, ok := r.complete("first", nil); ok {
		t.Error("a result arriving after the flush recorded the call a second time")
	}
	if again := r.flush(); len(again) != 0 {
		t.Errorf("a second flush found %+v", again)
	}
}

// TestRecordingSendsInSeqOrderAcrossGoroutines: the Codex clients
// complete a call on the reader while the turn's flush runs on its own
// goroutine. A flush must not overtake a completion that holds a lower seq
// and is still being sent -- the engine drops an action that arrives
// behind a higher one, so the call would be lost.
func TestRecordingSendsInSeqOrderAcrossGoroutines(t *testing.T) {
	r := newRecording()
	r.startTurn()
	r.begin(Action{ID: "a", Tool: ActionExec})
	r.begin(Action{ID: "b", Tool: ActionExec})

	var mu sync.Mutex
	var sent []uint64
	send := func(a Action) {
		mu.Lock()
		sent = append(sent, a.Seq)
		mu.Unlock()
	}
	sending, release, completed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(completed)
		r.completeAndEmit("a", nil, func(a Action) {
			close(sending)
			<-release
			send(a)
		})
	}()
	<-sending
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		r.flushAndEmit(send)
	}()
	select {
	case <-flushed:
		t.Fatal("the flush sent while seq 1 was still being sent")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-completed
	<-flushed
	if !reflect.DeepEqual(sent, []uint64{1, 2}) {
		t.Fatalf("sent %v, want 1 then 2", sent)
	}
}

func TestRecordingIDlessCallsNeverPair(t *testing.T) {
	r := newRecording()
	r.startTurn()
	r.begin(Action{Tool: ActionExec})
	if got := r.flush(); len(got) != 0 {
		t.Errorf("a call with no id was held open: %+v", got)
	}
	if _, ok := r.complete("", nil); !ok {
		t.Error("an id-less completion was dropped")
	}
	if _, ok := r.complete("", nil); !ok {
		t.Error("a second id-less completion was dropped as a repeat; nothing says it is one")
	}
}

func TestRecordingIsANoOpWhenNil(t *testing.T) {
	var r *recording
	r.startTurn()
	r.begin(Action{ID: "x"})
	if _, ok := r.complete("x", nil); ok {
		t.Error("a nil recording emitted an action")
	}
	if got := r.flush(); got != nil {
		t.Errorf("a nil recording flushed %+v", got)
	}
}

func TestMergeCallKeepsWhatTheEndLeftOut(t *testing.T) {
	begun := Action{ID: "p", Tool: ActionFSWrite, AppTool: "patch_apply", Cwd: "/w",
		Args:     json.RawMessage(`{"changes":{"/w/a":{"type":"add","content":"x"}}}`),
		Contents: []Content{{Op: ContentWrite, Path: "/w/a"}}, Command: "c", Query: "q", URL: "u",
		MCP: &MCPTarget{Server: "s", Tool: "t"}, ParentID: "root"}
	ended := Action{ID: "p", Tool: ActionFSWrite, Args: json.RawMessage("null")}
	got := mergeCall(begun, ended)
	if string(got.Args) != string(begun.Args) || len(got.Contents) != 1 || got.Cwd != "/w" ||
		got.Command != "c" || got.Query != "q" || got.URL != "u" || got.MCP == nil ||
		got.ParentID != "root" || got.AppTool != "patch_apply" {
		t.Errorf("merge lost what only the begin said: %+v", got)
	}
	ended.Command = "ended"
	if mergeCall(begun, ended).Command != "ended" {
		t.Error("a field the end reported did not win")
	}
}
