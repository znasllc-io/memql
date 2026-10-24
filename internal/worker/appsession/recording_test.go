//go:build linux || darwin

package appsession

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
)

// recording_test.go drives whole sessions and reads the recording off the
// wire the way the engine will: event chunks, decoded by their type word.

// recordedEvents splits the event chunks a session sent into its
// fingerprints and its actions, in the order they were sent.
func recordedEvents(t *testing.T, chunks []recordedChunk) ([]harness.Fingerprint, []harness.Action) {
	t.Helper()
	var fps []harness.Fingerprint
	var actions []harness.Action
	for _, c := range chunks {
		if c.stream != StreamEvent {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(c.data), &head) != nil {
			continue
		}
		switch head.Type {
		case harness.FingerprintEventType:
			var fp harness.Fingerprint
			if err := json.Unmarshal([]byte(c.data), &fp); err != nil {
				t.Fatalf("a fingerprint chunk does not decode: %v (%s)", err, c.data)
			}
			fps = append(fps, fp)
		case harness.ActionEventType:
			var a harness.Action
			if err := json.Unmarshal([]byte(c.data), &a); err != nil {
				t.Fatalf("an action chunk does not decode: %v (%s)", err, c.data)
			}
			actions = append(actions, a)
		}
	}
	return fps, actions
}

const quietClaude = `
echo '{"type":"system","subtype":"init","session_id":"app-quiet"}'
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"app-quiet","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2},"result":"ok"}'
`

// TestSession_FingerprintIsTheFirstEvent is #443's acceptance criterion:
// the first event of every session is the fingerprint, with the tool
// versions and the workspace digest -- for a run, for an attach, and for a
// session whose harness never gets as far as a turn.
func TestSession_FingerprintIsTheFirstEvent(t *testing.T) {
	cases := []struct {
		name    string
		app     string
		mutate  func(*memqlv1.AppSessionStart)
		harness string
	}{
		{"run", apps.IDClaudeCode, nil, harness.HarnessClaudeHeadless},
		{"attach", apps.IDClaudeCode, func(s *memqlv1.AppSessionStart) {
			s.Kind = KindAttach
			s.AppSessionRef = "app-quiet"
		}, harness.HarnessClaudeHeadless},
		{"a codex that dies at its handshake", apps.IDCodex, func(s *memqlv1.AppSessionStart) {
			s.App = apps.IDCodex
		}, harness.HarnessCodexMCP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.app == apps.IDCodex {
				fakeApp(t, "codex", "exit 9\n")
			} else {
				fakeApp(t, "claude", quietClaude)
			}
			h := newRig(t, tc.app)
			writeFile(t, h.workspace, "README.md", "hi")
			h.start(t, tc.mutate)

			chunks := h.sender.recorded()
			if len(chunks) == 0 {
				t.Fatal("the session sent nothing")
			}
			if chunks[0].stream != StreamEvent || chunks[0].seq != 1 {
				t.Fatalf("the first chunk is %q seq %d, want the fingerprint as event seq 1", chunks[0].stream, chunks[0].seq)
			}
			fps, _ := recordedEvents(t, chunks[:1])
			if len(fps) != 1 {
				t.Fatalf("the first chunk is not the fingerprint: %s", chunks[0].data)
			}
			fp := fps[0]
			if fp.V != harness.RecordVersion || fp.Seq != 0 || fp.TakenAt == "" {
				t.Errorf("fingerprint = %+v", fp)
			}
			if fp.App != (harness.FingerprintApp{ID: tc.app, Version: rigAppVersion, Harness: tc.harness}) {
				t.Errorf("app = %+v, want %s %s %s", fp.App, tc.app, rigAppVersion, tc.harness)
			}
			if !reflect.DeepEqual(fp.Tools, rigTools) {
				t.Errorf("tools = %+v, want the tool versions", fp.Tools)
			}
			if fp.Cwd != h.workspace || !strings.HasPrefix(fp.CwdDigest, harness.DigestPrefix) {
				t.Errorf("cwd = %q digest %q", fp.Cwd, fp.CwdDigest)
			}
			// README.md alone: the cockpit's .mcp.json and .memql-session
			// are the session's, not the workspace's.
			if fp.CwdEntries == nil || *fp.CwdEntries != 1 {
				t.Errorf("cwdEntries = %v, want 1", fp.CwdEntries)
			}
			if fp.Platform.OS != runtime.GOOS || fp.Platform.Arch != runtime.GOARCH {
				t.Errorf("platform = %+v", fp.Platform)
			}
			var path *harness.Variable
			for i := range fp.Variables {
				if fp.Variables[i].Name == "PATH" {
					path = &fp.Variables[i]
				}
			}
			if path == nil || !path.Set || path.Digest != harness.Digest([]byte(os.Getenv("PATH"))) {
				t.Errorf("PATH = %+v, want the digest of the PATH the app runs with", path)
			}
			if strings.Contains(chunks[0].data, os.Getenv("PATH")) {
				t.Error("the fingerprint carries PATH's value; it carries a digest")
			}
		})
	}
}

func TestSession_FingerprintDigestsTheInputs(t *testing.T) {
	fakeApp(t, "claude", quietClaude)
	h := newRig(t)
	h.library.mu.Lock()
	h.library.inputs["spec-1"] = []byte("the specification body")
	h.library.mu.Unlock()
	h.start(t, func(s *memqlv1.AppSessionStart) {
		s.Inputs = []string{"spec-1"}
	})
	fps, _ := recordedEvents(t, h.sender.recorded())
	if len(fps) != 1 || len(fps[0].Inputs) != 1 {
		t.Fatalf("fingerprints = %+v", fps)
	}
	in := fps[0].Inputs[0]
	if in.Artifact != "spec-1" || in.Path != filepath.Join(h.workspace, "spec-1.txt") ||
		in.Digest != harness.Digest([]byte("the specification body")) || in.Bytes == nil || *in.Bytes != 22 {
		t.Errorf("input = %+v", in)
	}
}

// TestSession_ActionsCarryTheirFilesPastTheCap: the recording goes out
// whole after the transcript cap has bitten, a file the app wrote travels
// with its bytes, and a file the app named inside the scaffolding or
// outside the workspace travels as a path and a reason -- never a byte of
// it, and never the bearer. A file the app read travels only as far as the
// app's own report of it went: whole when it read the whole file, as a
// digest when it read one line.
func TestSession_ActionsCarryTheirFilesPastTheCap(t *testing.T) {
	outside := writeFile(t, t.TempDir(), "elsewhere.txt", "not the session's")
	fakeApp(t, "claude", fmt.Sprintf(`
printf 'goodbye' > out.txt
printf 'alpha\nbeta\n' > notes.txt
printf 'USER=me\nTOKEN=hunter2\n' > .env
printf '%%s\n' '{"type":"system","subtype":"init","cwd":"'"$PWD"'","session_id":"app-rec"}'
i=0
while [ $i -lt 40 ]; do
  echo "narration line $i xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
  i=$((i+1))
done
printf '%%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_w","name":"Write","input":{"file_path":"'"$PWD"'/out.txt","content":"goodbye"}}]},"parent_tool_use_id":null,"session_id":"app-rec"}'
printf '%%s\n' '{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_w","type":"tool_result","content":"File created successfully"}]},"parent_tool_use_id":null,"session_id":"app-rec"}'
printf '%%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_cfg","name":"Read","input":{"file_path":"'"$memql_test_config"'"}}]},"parent_tool_use_id":null,"session_id":"app-rec"}'
printf '%%s\n' '{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_cfg","type":"tool_result","content":"(the configuration)"}]},"parent_tool_use_id":null,"session_id":"app-rec"}'
printf '%%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_out","name":"Read","input":{"file_path":"%s"}}]},"parent_tool_use_id":null,"session_id":"app-rec"}'
printf '%%s\n' '{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_out","type":"tool_result","content":"(elsewhere)"}]},"parent_tool_use_id":null,"session_id":"app-rec"}'
printf '%%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_r","name":"Read","input":{"file_path":"'"$PWD"'/notes.txt"}}]},"parent_tool_use_id":null,"session_id":"app-rec"}'
printf '%%s\n' '{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_r","type":"tool_result","content":"1\talpha\n2\tbeta\n3\t"}]},"parent_tool_use_id":null,"session_id":"app-rec","tool_use_result":{"type":"text","file":{"filePath":"'"$PWD"'/notes.txt","content":"alpha\nbeta\n"}}}'
printf '%%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_env","name":"Read","input":{"file_path":"'"$PWD"'/.env","limit":1}}]},"parent_tool_use_id":null,"session_id":"app-rec"}'
printf '%%s\n' '{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_env","type":"tool_result","content":"1\tUSER=me"}]},"parent_tool_use_id":null,"session_id":"app-rec","tool_use_result":{"type":"text","file":{"filePath":"'"$PWD"'/.env","content":"USER=me\n"}}}'
printf '%%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"app-rec","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2},"result":"done"}'
`, outside))
	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.Limits = &memqlv1.AppSessionLimits{MaxTranscriptBytes: 300}
	})
	if end.GetExitCode() != 0 || end.GetError() != "" {
		t.Fatalf("end = %d %q", end.GetExitCode(), end.GetError())
	}
	if !strings.Contains(h.sender.transcript(), "live transcript truncated") {
		t.Fatal("the cap never bit, so this test proves nothing about the recording outliving it")
	}
	_, actions := recordedEvents(t, h.sender.recorded())
	if len(actions) != 5 {
		t.Fatalf("actions = %d past the cap, want all 5: %+v", len(actions), actions)
	}
	for i, a := range actions {
		if a.Seq != uint64(i+1) || len(a.Contents) != 1 {
			t.Fatalf("action %d = %+v", i, a)
		}
	}
	wrote := actions[0].Contents[0]
	if wrote.Data == nil || *wrote.Data != "goodbye" || wrote.Digest != harness.Digest([]byte("goodbye")) ||
		wrote.Bytes == nil || *wrote.Bytes != 7 || wrote.Encoding != harness.EncodingUTF8 {
		t.Errorf("the written file = %+v, want its bytes as they stood when the call completed", wrote)
	}
	if got := actions[1].Contents[0]; got.Omitted != harness.OmittedScaffolding || got.Data != nil || got.Digest != "" {
		t.Errorf("the MCP configuration = %+v, want session_scaffolding and nothing read", got)
	}
	if got := actions[2].Contents[0]; got.Omitted != harness.OmittedOutsideWorkspace || got.Data != nil {
		t.Errorf("a file outside the workspace = %+v, want outside_workspace and nothing read", got)
	}
	if got := actions[3].Contents[0]; got.Data == nil || *got.Data != "alpha\nbeta\n" || got.Omitted != "" {
		t.Errorf("a file the app read whole = %+v, want its bytes", got)
	}
	if got := actions[4].Contents[0]; got.Data != nil || got.Omitted != harness.OmittedDigestOnly ||
		got.Digest != harness.Digest([]byte("USER=me\nTOKEN=hunter2\n")) {
		t.Errorf("a file the app read one line of = %+v, want digest_only with the whole file's digest", got)
	}
	for _, c := range h.sender.recorded() {
		if strings.Contains(c.data, "hunter2") {
			t.Fatalf("the part of .env the app never read reached a chunk: %s", c.data)
		}
		if strings.Contains(c.data, testBearer) {
			t.Fatalf("the bearer reached a chunk: %s", c.data)
		}
		if strings.Contains(c.data, "not the session's") {
			t.Fatalf("a file outside the workspace reached a chunk: %s", c.data)
		}
	}
}

// gateSender holds the send of chunk seq 1 until released.
type gateSender struct {
	*fakeSender
	entered, release chan struct{}
	once             sync.Once
}

func (g *gateSender) SendAppSessionChunk(id, stream string, data []byte, seq uint64) error {
	if seq == 1 {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}
	return g.fakeSender.SendAppSessionChunk(id, stream, data, seq)
}

// TestSession_ChunksLeaveInTheOrderOfTheirSeq: an action is sent from the
// harness's goroutine while narration goes out from another. A chunk
// numbered after one still being sent must not reach the stream before
// it -- the engine drops a chunk that arrives behind a higher seq, so the
// lower one would be lost.
func TestSession_ChunksLeaveInTheOrderOfTheirSeq(t *testing.T) {
	g := &gateSender{fakeSender: newFakeSender(), entered: make(chan struct{}), release: make(chan struct{})}
	s := &session{id: "s-order", sender: g, logger: slog.Default()}
	first := make(chan error, 1)
	go func() { first <- s.emitRecord([]byte("{\"type\":\"memql.app_session.action\"}\n")) }()
	<-g.entered
	second := make(chan error, 1)
	go func() { second <- s.emitChunk(StreamText, []byte("narration")) }()
	select {
	case <-second:
		t.Fatal("seq 2 was sent while seq 1 was still being sent")
	case <-time.After(50 * time.Millisecond):
	}
	close(g.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	got := g.recorded()
	if len(got) != 2 || got[0].seq != 1 || got[1].seq != 2 {
		t.Fatalf("sent %+v, want seq 1 then seq 2", got)
	}
}
