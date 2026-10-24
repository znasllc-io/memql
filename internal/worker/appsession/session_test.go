//go:build linux || darwin

package appsession

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
)

// --- the test rig ----------------------------------------------------

type recordedChunk struct {
	stream string
	data   string
	seq    uint64
}

// fakeSender stands in for the worker's stream.
type fakeSender struct {
	mu       sync.Mutex
	chunks   []recordedChunk
	end      *memqlv1.AppSessionEnd
	done     chan struct{}
	failNext int
	attempts int
}

func newFakeSender() *fakeSender {
	return &fakeSender{done: make(chan struct{})}
}

func (f *fakeSender) SendAppSessionChunk(_, stream string, data []byte, seq uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.failNext > 0 {
		f.failNext--
		return fmt.Errorf("simulated transient send failure")
	}
	f.chunks = append(f.chunks, recordedChunk{stream: stream, data: string(data), seq: seq})
	return nil
}

func (f *fakeSender) SendAppSessionEnd(end *memqlv1.AppSessionEnd) error {
	f.mu.Lock()
	if f.end == nil {
		f.end = end
		close(f.done)
	}
	f.mu.Unlock()
	return nil
}

func (f *fakeSender) wait(t *testing.T) *memqlv1.AppSessionEnd {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(30 * time.Second):
		t.Fatal("session never ended")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.end
}

func (f *fakeSender) recorded() []recordedChunk {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedChunk(nil), f.chunks...)
}

func (f *fakeSender) transcript() string {
	var b strings.Builder
	for _, c := range f.recorded() {
		b.WriteString(c.data)
	}
	return b.String()
}

// readArgv reads the argv a fake app recorded, one argument per line.
//
// The argv is worth asserting rather than the app's behaviour because
// every way it goes wrong is SILENT: an app started without
// `--mcp-config` runs perfectly and reaches no MemQL tool, and one
// started without `--resume` holds a fresh conversation that looks
// exactly like a continued one.
func readArgv(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the app recorded no argv at %s: %v", path, err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// argvValue returns the argument following flag, or "".
func argvValue(argv []string, flag string) string {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// fakeApp installs a shell script on PATH under the app's binary name.
func fakeApp(t *testing.T, binary, body string) string {
	t.Helper()
	if binary == "claude" {
		body = `memql_test_config=""
memql_test_previous=""
for memql_test_arg in "$@"; do
  if [ "$memql_test_previous" = "--mcp-config" ]; then memql_test_config="$memql_test_arg"; fi
  memql_test_previous="$memql_test_arg"
done
` + body
	}
	dir := t.TempDir()
	path := filepath.Join(dir, binary)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", binary, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

// fakeLibrary serves the two byte-bearing Library routes.
type fakeLibrary struct {
	server   *httptest.Server
	mu       sync.Mutex
	uploads  map[string][]byte
	inputs   map[string][]byte
	pullCode map[string]int
	bearers  []string
	nextID   int
}

func newFakeLibrary(t *testing.T) *fakeLibrary {
	t.Helper()
	l := &fakeLibrary{
		uploads:  map[string][]byte{},
		inputs:   map[string][]byte{},
		pullCode: map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var message struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&message)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "test-preflight")
		result := map[string]any{}
		if message.Method == "tools/list" {
			result["tools"] = []map[string]any{{"name": "next_task"}, {"name": "submit"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
	})
	mux.HandleFunc("/artifacts/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/artifacts/"), "/content")
		l.mu.Lock()
		l.bearers = append(l.bearers, r.Header.Get("Authorization"))
		body, ok := l.inputs[id]
		code := l.pullCode[id]
		l.mu.Unlock()
		if code != 0 {
			http.Error(w, "refused", code)
			return
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+id+`.txt"`)
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/artifacts", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.bearers = append(l.bearers, r.Header.Get("Authorization"))
		l.mu.Unlock()
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		l.mu.Lock()
		l.nextID++
		id := fmt.Sprintf("artifact-%d:%s", l.nextID, header.Filename)
		l.uploads[id] = data
		l.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(PushResult{ArtifactId: id, FileId: "file-" + id})
	})
	l.server = httptest.NewServer(mux)
	t.Cleanup(l.server.Close)
	return l
}

func (l *fakeLibrary) uploaded() map[string][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string][]byte{}
	for k, v := range l.uploads {
		out[k] = v
	}
	return out
}

func (l *fakeLibrary) seenBearers() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.bearers...)
}

type rig struct {
	manager   *Manager
	sender    *fakeSender
	library   *fakeLibrary
	workspace string
	state     string
}

func newRig(t *testing.T, allow ...string) *rig {
	t.Helper()
	lib := newFakeLibrary(t)
	allowed := map[string]bool{}
	for _, a := range allow {
		allowed[a] = true
	}
	if len(allow) == 0 {
		allowed[apps.IDClaudeCode] = true
	}
	h := &rig{
		sender:    newFakeSender(),
		library:   lib,
		workspace: t.TempDir(),
		state:     t.TempDir(),
	}
	h.manager = NewManager(Options{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		StateDir:    h.state,
		LibraryBase: lib.server.URL,
		HTTPClient:  preflightFixtureClient(lib.server),
		Allowed:     func(id string) bool { return allowed[id] },
		// The fingerprint asks the app for its version and the machine for
		// its tools. Neither is asked for real here: several fake apps fork
		// a grandchild on EVERY invocation, and a session test must not
		// run every compiler on the machine running it.
		Detector: &apps.Detector{RunVersion: func(context.Context, string, []string) (string, error) {
			return rigAppVersion, nil
		}},
		ToolVersions: func(context.Context) []harness.ToolVersion { return rigTools },
	})
	return h
}

// The rig's fixed answers for the fingerprint's app version and tools.
const rigAppVersion = "9.9.9 (rig)"

var rigTools = []harness.ToolVersion{{Name: "git", Version: "git version 2.43.0"}}

func (h *rig) start(t *testing.T, mutate func(*memqlv1.AppSessionStart)) *memqlv1.AppSessionEnd {
	t.Helper()
	start := &memqlv1.AppSessionStart{
		SessionId:   "sess-test",
		App:         apps.IDClaudeCode,
		Kind:        KindRun,
		Prompt:      "do the thing",
		Workspace:   h.workspace,
		Credential:  testBearer,
		McpEndpoint: "https://mcp.example.com/mcp",
	}
	if mutate != nil {
		mutate(start)
	}
	h.manager.Start(context.Background(), h.sender, start)
	return h.sender.wait(t)
}

// --- tests -----------------------------------------------------------

// TestSession_RunStreamsAndEnds is the happy path for kind=run, and it
// pins the four things a reader of the transcript depends on: ordering,
// classification, the app's own usage, and the app's own session id.
func TestSession_RunStreamsAndEnds(t *testing.T) {
	fakeApp(t, "claude", `
echo '{"type":"system","subtype":"init","session_id":"app-run-42"}'
echo 'plain narration from the agent'
echo '{"type":"result","total_cost_usd":0.25,"usage":{"input_tokens":10,"output_tokens":20},"session_id":"app-run-42"}'
exit 0
`)
	h := newRig(t)
	end := h.start(t, nil)

	if end.GetExitCode() != 0 {
		t.Errorf("exit_code = %d, want 0", end.GetExitCode())
	}
	if end.GetError() != "" {
		t.Errorf("error = %q, want empty on a clean run", end.GetError())
	}

	chunks := h.sender.recorded()
	if len(chunks) < 3 {
		t.Fatalf("chunks = %d, want at least 3: %+v", len(chunks), chunks)
	}

	// seq is monotonic per session, starting at 1. The engine DROPS
	// out-of-order and duplicate chunks rather than appending them, so a
	// gap or a repeat is not a cosmetic problem -- it is a transcript
	// that no longer matches what the app printed, in a way no later
	// reader can detect.
	for i, c := range chunks {
		if c.seq != uint64(i+1) {
			t.Fatalf("chunk %d has seq %d, want %d (monotonic from 1): %+v", i, c.seq, i+1, chunks)
		}
	}

	// The app's own structured JSON goes out as `event`; prose goes out
	// as stdout narration. Nothing synthesises an event out of parsed
	// prose -- a live view that is confidently wrong about what the
	// agent did is worse than a plain one.
	var events, stdout int
	for _, c := range chunks {
		switch c.stream {
		case StreamEvent:
			events++
			if !strings.HasPrefix(strings.TrimSpace(c.data), "{") {
				t.Errorf("event chunk is not JSON: %q", c.data)
			}
		case StreamStdout:
			stdout++
			if strings.HasPrefix(strings.TrimSpace(c.data), "{") {
				t.Errorf("JSON was classified as narration: %q", c.data)
			}
		}
	}
	if events < 2 {
		t.Errorf("event chunks = %d, want the app's two JSON lines", events)
	}
	if stdout < 1 {
		t.Errorf("stdout chunks = %d, want the plain line", stdout)
	}

	usage := end.GetUsage()
	if !usage.GetKnown() {
		t.Fatal("the app reported usage; known must be true")
	}
	if usage.GetInputTokens() != 10 || usage.GetOutputTokens() != 20 || usage.GetCostUsd() != 0.25 {
		t.Errorf("usage = %+v, want what the app reported", usage)
	}
	// The app's own session id comes back so a later kind=attach can
	// resume this run.
	if end.GetAppSessionRef() != "app-run-42" {
		t.Errorf("app_session_ref = %q, want the app's own session id", end.GetAppSessionRef())
	}
}

// TestSession_UsageUnknownWhenTheAppSaysNothing. The engine records
// known=false as billing "unknown", which is the honest answer. An
// estimate would be recorded as measured, in a ledger somebody bills
// from.
func TestSession_UsageUnknownWhenTheAppSaysNothing(t *testing.T) {
	fakeApp(t, "claude", "echo 'did some work'\nexit 0\n")
	h := newRig(t)
	end := h.start(t, nil)

	usage := end.GetUsage()
	if usage == nil {
		t.Fatal("usage must be present with known=false, not omitted")
	}
	if usage.GetKnown() {
		t.Error("the app reported nothing; known must be false")
	}
	if usage.GetInputTokens() != 0 || usage.GetOutputTokens() != 0 || usage.GetCostUsd() != 0 {
		t.Errorf("usage = %+v, want zeroes alongside known=false", usage)
	}
}

// TestSession_RealExitCodeIsPassedThrough. The engine reads a non-zero
// exit as a FAILED run rather than an ended one, so normalising the code
// misfiles the outcome in a record people read back later.
func TestSession_RealExitCodeIsPassedThrough(t *testing.T) {
	for _, code := range []int{1, 2, 42} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			fakeApp(t, "claude", fmt.Sprintf("echo working\nexit %d\n", code))
			h := newRig(t)
			end := h.start(t, nil)
			if int(end.GetExitCode()) != code {
				t.Errorf("exit_code = %d, want %d verbatim", end.GetExitCode(), code)
			}
		})
	}
}

// TestSession_NoConfigSurvivesAnyExitPath is #348's acceptance criterion
// driven through the real session runner rather than the writer alone.
func TestSession_NoConfigSurvivesAnyExitPath(t *testing.T) {
	cases := map[string]string{
		"clean exit":    "echo done\nexit 0\n",
		"non-zero exit": "echo failing\nexit 7\n",
		"crash":         "kill -9 $$\n",
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			fakeApp(t, "claude", script)
			h := newRig(t)
			h.start(t, nil)

			if found := grepTree(t, h.workspace, testBearer); len(found) > 0 {
				t.Errorf("the bearer survived in: %v", found)
			}
			if _, err := os.Stat(filepath.Join(h.workspace, ".mcp.json")); !os.IsNotExist(err) {
				t.Errorf(".mcp.json survived (%v)", err)
			}
		})
	}
}

// TestSession_CancelKillsTheProcessGroup. A `claude` that survives its
// cancel is an agent running on somebody's machine with nothing watching
// it. The engine sends cancel when the user asks, when the calling plan's
// context dies, and when the kill switch flips -- all three mean STOP.
func TestSession_CancelKillsTheProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "grandchild-alive")
	// The app forks a grandchild that outlives it and keeps touching a
	// file. Killing only the direct child would leave it running.
	fakeApp(t, "claude", fmt.Sprintf(`
( while true; do touch %q; sleep 0.1; done ) &
echo started
sleep 60
`, marker))

	h := newRig(t)
	sender := h.sender
	start := &memqlv1.AppSessionStart{
		SessionId:   "sess-cancel",
		App:         apps.IDClaudeCode,
		Kind:        KindRun,
		Prompt:      "long job",
		Workspace:   h.workspace,
		Credential:  testBearer,
		McpEndpoint: "https://mcp.example.com/mcp",
	}
	h.manager.Start(context.Background(), sender, start)

	// Wait for the app to be up.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(sender.recorded()) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	h.manager.Control(&memqlv1.AppSessionControl{
		SessionId: "sess-cancel",
		Action:    ActionCancel,
		Reason:    "the user cancelled the plan",
	})
	end := sender.wait(t)

	if !strings.Contains(end.GetError(), "cancelled") {
		t.Errorf("error = %q, want it to name the cancel", end.GetError())
	}
	if !strings.Contains(end.GetError(), "the user cancelled the plan") {
		t.Errorf("error = %q, want the server's reason carried through", end.GetError())
	}

	// The grandchild must be gone. Give the group kill a moment, then
	// confirm the marker stops advancing.
	time.Sleep(1500 * time.Millisecond)
	before, err := os.Stat(marker)
	if err != nil {
		// Never touched at all is also a pass.
		return
	}
	time.Sleep(1500 * time.Millisecond)
	after, err := os.Stat(marker)
	if err != nil {
		return
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a grandchild survived the cancel: killing the process group is the point")
	}
}

// TestSession_MaxDurationEndsTheRun.
func TestSession_MaxDurationEndsTheRun(t *testing.T) {
	fakeApp(t, "claude", "echo started\nsleep 60\n")
	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-duration"
		s.Limits = &memqlv1.AppSessionLimits{MaxDurationSeconds: 1}
	})
	if !strings.Contains(end.GetError(), "max_duration_seconds") {
		t.Errorf("error = %q, want it to name the limit that bit", end.GetError())
	}
}

// TestSession_TranscriptCapBoundsTheStreamNotTheArtifact. The limit
// bounds what the ENGINE keeps on the session row; the complete
// transcript is expected to be an artifact. Stopping silently would leave
// a reader believing the run went quiet, so the cap announces itself.
func TestSession_TranscriptCapBoundsTheStreamNotTheArtifact(t *testing.T) {
	fakeApp(t, "claude", `
i=0
while [ $i -lt 200 ]; do
  echo "line $i xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
  i=$((i+1))
done
exit 0
`)
	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-cap"
		s.Limits = &memqlv1.AppSessionLimits{MaxTranscriptBytes: 500}
	})
	if end.GetExitCode() != 0 {
		t.Fatalf("exit_code = %d (%s)", end.GetExitCode(), end.GetError())
	}

	streamed := h.sender.transcript()
	if !strings.Contains(streamed, "live transcript truncated") {
		t.Error("the cap must announce itself; going silent reads as the run going quiet")
	}

	// The artifact carries the WHOLE thing.
	var transcript []byte
	for name, body := range h.library.uploaded() {
		if strings.Contains(name, "transcript") {
			transcript = body
		}
	}
	if transcript == nil {
		t.Fatal("no transcript artifact was pushed")
	}
	if !strings.Contains(string(transcript), "line 199") {
		t.Error("the transcript artifact was truncated too; the engine's row cap is not the artifact's cap")
	}
	if int64(len(transcript)) <= 500 {
		t.Errorf("transcript artifact is %d bytes, which is inside the row cap", len(transcript))
	}
}

// TestSession_InputsLandBeforeTheAppStarts. An agent that begins work and
// finds its inputs half-arrived produces confidently wrong output rather
// than an error, and nothing downstream can tell the difference.
func TestSession_InputsLandBeforeTheAppStarts(t *testing.T) {
	fakeApp(t, "claude", "cat spec-1.txt\nexit 0\n")
	h := newRig(t)
	h.library.mu.Lock()
	h.library.inputs["spec-1"] = []byte("the specification body")
	h.library.mu.Unlock()

	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-inputs"
		s.Inputs = []string{"spec-1"}
	})
	if end.GetExitCode() != 0 {
		t.Fatalf("exit_code = %d (%s)", end.GetExitCode(), end.GetError())
	}
	if !strings.Contains(h.sender.transcript(), "the specification body") {
		t.Error("the app did not see its input: it was not on disk before the run started")
	}
}

// TestSession_FailedInputEndsTheSessionNamingTheId, and the app never
// runs.
func TestSession_FailedInputEndsTheSessionNamingTheId(t *testing.T) {
	ran := filepath.Join(t.TempDir(), "the-app-ran")
	fakeApp(t, "claude", fmt.Sprintf("touch %q\nexit 0\n", ran))
	h := newRig(t)

	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-bad-input"
		s.Inputs = []string{"missing-artifact"}
	})
	if !strings.Contains(end.GetError(), "missing-artifact") {
		t.Errorf("error = %q, want it to name the id that failed", end.GetError())
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("the app ran despite an input that never arrived")
	}
	if end.GetExitCode() == 0 {
		t.Error("a session that never started must not report a clean exit")
	}
}

// TestSession_PullErrorNamesTheRightParty. The credential acts as the
// OWNING USER, so a refusal is a statement about that user's access, not
// about this machine. Getting it wrong sends an operator to check a
// worker token that is working fine.
func TestSession_PullErrorNamesTheRightParty(t *testing.T) {
	fakeApp(t, "claude", "exit 0\n")

	t.Run("403 is the user's access", func(t *testing.T) {
		h := newRig(t)
		h.library.mu.Lock()
		h.library.pullCode["restricted"] = http.StatusForbidden
		h.library.mu.Unlock()
		end := h.start(t, func(s *memqlv1.AppSessionStart) {
			s.SessionId = "sess-403"
			s.Inputs = []string{"restricted"}
		})
		if !strings.Contains(end.GetError(), "owning user") {
			t.Errorf("error = %q, want it to name the user's access", end.GetError())
		}
	})

	// A 401 used to be reported as "the cockpit's side: the bearer is
	// expired or malformed", and this test asserted that wording. Both were
	// wrong: before memql#4863 the bearer was fine and the Library simply
	// did not admit its class, and since memql#4863 the class IS admitted,
	// so what is left is an expiry the engine can fix in place. The
	// assertion moved with the sentence -- see memql-cockpit#371.
	t.Run("401 points at renewal, not at a broken cockpit", func(t *testing.T) {
		h := newRig(t)
		h.library.mu.Lock()
		h.library.pullCode["stale"] = http.StatusUnauthorized
		h.library.mu.Unlock()
		end := h.start(t, func(s *memqlv1.AppSessionStart) {
			s.SessionId = "sess-401"
			s.Inputs = []string{"stale"}
		})
		if !strings.Contains(end.GetError(), "renew_credential") {
			t.Errorf("error = %q, want it to name the renewal the engine can send", end.GetError())
		}
		if strings.Contains(end.GetError(), "malformed") {
			t.Errorf("error = %q, must not resurrect the malformed-bearer diagnosis", end.GetError())
		}
	})
}

// TestSession_ProducedFilesAndTranscriptArePushed.
func TestSession_ProducedFilesAndTranscriptArePushed(t *testing.T) {
	fakeApp(t, "claude", "echo 'result content' > output.txt\nmkdir -p sub && echo nested > sub/deep.txt\nexit 0\n")
	h := newRig(t)
	// A file that predates the run must NOT be reported as produced.
	if err := os.WriteFile(filepath.Join(h.workspace, "preexisting.txt"), []byte("old"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	end := h.start(t, nil)

	if end.GetExitCode() != 0 {
		t.Fatalf("exit_code = %d (%s)", end.GetExitCode(), end.GetError())
	}
	names := map[string]bool{}
	for name := range h.library.uploaded() {
		names[name] = true
	}
	var sawOutput, sawNested, sawTranscript, sawPreexisting bool
	for name := range names {
		switch {
		case strings.HasSuffix(name, "output.txt"):
			sawOutput = true
		case strings.HasSuffix(name, "sub__deep.txt"):
			sawNested = true
		case strings.Contains(name, "transcript"):
			sawTranscript = true
		case strings.Contains(name, "preexisting"):
			sawPreexisting = true
		}
	}
	if !sawOutput || !sawNested {
		t.Errorf("produced files not pushed: %v", names)
	}
	if !sawTranscript {
		t.Errorf("the full transcript must be pushed as an artifact: %v", names)
	}
	if sawPreexisting {
		t.Error("a file that predates the run was reported as produced")
	}
	// Every id comes back on the End so the portal can point at them.
	if len(end.GetProducedArtifactIds()) != len(names) {
		t.Errorf("produced_artifact_ids = %d, want %d", len(end.GetProducedArtifactIds()), len(names))
	}
	var manifest outputManifest
	for name, body := range h.library.uploaded() {
		if strings.HasSuffix(name, "-outputs.json") {
			if err := json.Unmarshal(body, &manifest); err != nil {
				t.Fatal(err)
			}
		}
	}
	if manifest.Version != 2 || manifest.TranscriptID == "" || len(manifest.Files) != 2 || len(manifest.Failures) != 0 {
		t.Fatalf("incomplete durable retention receipt: %+v", manifest)
	}
}

func TestSession_OutputOmissionIsDurableAndPreventsCleanup(t *testing.T) {
	fakeApp(t, "claude", "echo evidence > proof.txt\ndd if=/dev/zero of=too-large.bin bs=1 count=1 seek=67108864 2>/dev/null\n"+quietClaude)
	h := newRig(t)
	end := h.start(t, nil)
	if !strings.Contains(end.GetError(), "not retained") {
		t.Fatalf("incomplete retention reported success: %+v", end)
	}
	var manifest outputManifest
	for name, body := range h.library.uploaded() {
		if strings.HasSuffix(name, "-outputs.json") {
			if err := json.Unmarshal(body, &manifest); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(manifest.Files) != 1 || len(manifest.Omitted) != 1 || len(manifest.Failures) == 0 {
		t.Fatalf("omission disappeared from durable receipt: %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(h.workspace, "too-large.bin")); err != nil {
		t.Fatalf("unretained evidence removed: %v", err)
	}
}

// TestSession_AFileHoldingTheBearerIsNotPushed: an app that copies its MCP
// configuration into the workspace has produced a file holding the
// session's bearer. The bearer cannot be revoked, so the file is not
// pushed to the Library -- and the transcript says so, as it does for any
// file left behind.
func TestSession_AFileHoldingTheBearerIsNotPushed(t *testing.T) {
	fakeApp(t, "claude", "cp \"$memql_test_config\" leaked.json\necho fine > ok.txt\n"+quietClaude)
	h := newRig(t)
	end := h.start(t, nil)
	if end.GetExitCode() != 0 || end.GetError() != "" {
		t.Fatalf("end = %d %q", end.GetExitCode(), end.GetError())
	}
	sawOK := false
	for name, body := range h.library.uploaded() {
		if strings.Contains(string(body), testBearer) {
			t.Errorf("%s carried the bearer to the Library", name)
		}
		if strings.Contains(name, "leaked") {
			t.Errorf("%s was pushed", name)
		}
		sawOK = sawOK || strings.HasSuffix(name, "ok.txt")
	}
	if !sawOK {
		t.Error("the file without the bearer was not pushed")
	}
	if !strings.Contains(h.sender.transcript(), "not pushed to the Library: leaked.json (it holds this session's credential)") {
		t.Error("the transcript does not say the file was held back")
	}
}

// TestSession_BearerNeverReachesAChunk. The transcript is persisted on
// the engine side and rendered in the portal, so a chunk carrying the
// credential publishes it everywhere that record reaches.
func TestSession_BearerNeverReachesAChunk(t *testing.T) {
	fakeApp(t, "claude", "cat \"$memql_test_config\"\ncat \"$memql_test_config\" 1>&2\nexit 0\n")
	h := newRig(t)
	end := h.start(t, nil)

	if strings.Contains(h.sender.transcript(), testBearer) {
		t.Error("the session bearer was streamed to the engine in a chunk")
	}
	if !strings.Contains(h.sender.transcript(), string(redactedMarker)) {
		t.Error("the echoed config should show a visible redaction, not silently vanish")
	}
	if strings.Contains(end.GetError(), testBearer) {
		t.Error("the session bearer reached the End's error text")
	}
}

// TestSession_RetryDoesNotRenumber. "If you retry a send, do not
// renumber" -- the engine drops duplicates and out-of-order chunks, so a
// renumbered retry opens a gap rather than closing one.
func TestSession_RetryDoesNotRenumber(t *testing.T) {
	fakeApp(t, "claude", "echo one\necho two\necho three\nexit 0\n")
	h := newRig(t)
	h.sender.mu.Lock()
	h.sender.failNext = 1 // the first chunk send fails once
	h.sender.mu.Unlock()

	h.start(t, nil)

	chunks := h.sender.recorded()
	if len(chunks) < 3 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	seen := map[uint64]bool{}
	for i, c := range chunks {
		if seen[c.seq] {
			t.Fatalf("duplicate seq %d", c.seq)
		}
		seen[c.seq] = true
		if c.seq != uint64(i+1) {
			t.Fatalf("chunk %d has seq %d, want %d -- a retry renumbered", i, c.seq, i+1)
		}
	}
}

// TestSession_RefusesAnAppNotInPolicy. The engine derives its routing
// label from the allowed flag this machine reported, so it should never
// route here -- enforcing it again is the point. policy.yaml is the
// machine owner's word, checked where it is enforced rather than trusted
// from a round trip.
func TestSession_RefusesAnAppNotInPolicy(t *testing.T) {
	fakeApp(t, "codex", "exit 0\n")
	h := newRig(t, apps.IDClaudeCode)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-denied"
		s.App = apps.IDCodex
	})
	if !strings.Contains(end.GetError(), "apps.homes") {
		t.Errorf("error = %q, want it to name policy.yaml apps.homes", end.GetError())
	}
}

// TestSession_RefusesAnUnknownApp.
func TestSession_RefusesAnUnknownApp(t *testing.T) {
	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-unknown"
		s.App = "some-future-app"
	})
	if !strings.Contains(end.GetError(), "no runner") {
		t.Errorf("error = %q", end.GetError())
	}
}

// TestSession_AttachRequiresARef: kind=attach with nothing to resume must
// say so rather than quietly starting a fresh run, which would look like
// a resume and be a new session.
func TestSession_AttachRequiresARef(t *testing.T) {
	fakeApp(t, "claude", "exit 0\n")
	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-attach-noref"
		s.Kind = KindAttach
	})
	if !strings.Contains(end.GetError(), "app_session_ref") {
		t.Errorf("error = %q, want it to name the missing ref", end.GetError())
	}
}

// TestSession_AttachResumesByRef, and the argv the harness builds for it.
//
// The three assertions are the three ways this goes silently wrong.
// Without `--resume <ref>` the "resume" is a brand new conversation that
// looks identical from the outside. Without `--mcp-config <path>` the
// app starts perfectly and cannot reach a single MemQL tool. And the
// prompt has to come after `--`: `--mcp-config` is VARIADIC, so a prompt
// following it directly is swallowed as a second config path.
func TestSession_AttachResumesByRef(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	fakeApp(t, "claude", fmt.Sprintf(`
printf '%%s\n' "$@" > %q
echo '{"type":"result","total_cost_usd":0.1,"session_id":"app-earlier-run"}'
exit 0
`, argvFile))
	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-attach"
		s.Kind = KindAttach
		s.AppSessionRef = "app-earlier-run"
	})
	if end.GetExitCode() != 0 {
		t.Fatalf("exit_code = %d (%s); transcript: %s", end.GetExitCode(), end.GetError(), h.sender.transcript())
	}
	if end.GetAppSessionRef() != "app-earlier-run" {
		t.Errorf("app_session_ref = %q", end.GetAppSessionRef())
	}

	argv := readArgv(t, argvFile)
	if got := argvValue(argv, "--resume"); got != "app-earlier-run" {
		t.Errorf("--resume = %q, want the ref the engine named; argv: %v", got, argv)
	}
	if got := argvValue(argv, "--mcp-config"); !strings.HasPrefix(got, filepath.Join(h.workspace, sessionScaffoldDir)+string(os.PathSeparator)) {
		t.Errorf("--mcp-config = %q, want the config this session wrote; argv: %v", got, argv)
	}
	if got := argvValue(argv, "--"); got != "do the thing" {
		t.Errorf("the prompt after -- = %q, want it a positional the option parser cannot eat; argv: %v", got, argv)
	}
}

// TestSession_OpenFailsFastWhenTheAppIsMissing. An `open` that cannot
// launch must end immediately with a reason -- not return success and
// wait for a window that will never appear, and not fall back to a
// headless run, because the user asked to drive it themselves.
func TestSession_OpenFailsFastWhenTheAppIsMissing(t *testing.T) {
	// Nothing named `claude` on PATH.
	t.Setenv("PATH", t.TempDir())
	h := newRig(t)

	done := make(chan *memqlv1.AppSessionEnd, 1)
	go func() {
		done <- h.start(t, func(s *memqlv1.AppSessionStart) {
			s.SessionId = "sess-open-missing"
			s.Kind = KindOpen
		})
	}()

	select {
	case end := <-done:
		if end.GetError() == "" {
			t.Fatal("an open that could not launch must carry a non-empty error")
		}
		if !strings.Contains(end.GetError(), "PATH") {
			t.Errorf("error = %q, want it to name why it could not launch", end.GetError())
		}
		if end.GetExitCode() == 0 {
			t.Error("a failed open must not report a clean exit")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("open did not fail fast; it waited for a window that will never appear")
	}
}

// TestSession_DuplicateStartIsIgnored: a retried envelope must not give
// one session id two processes and two transcripts.
func TestSession_DuplicateStartIsIgnored(t *testing.T) {
	fakeApp(t, "claude", "echo once\nsleep 0.4\nexit 0\n")
	h := newRig(t)
	start := &memqlv1.AppSessionStart{
		SessionId:   "sess-dup",
		App:         apps.IDClaudeCode,
		Kind:        KindRun,
		Workspace:   h.workspace,
		Credential:  testBearer,
		McpEndpoint: "https://mcp.example.com/mcp",
	}
	h.manager.Start(context.Background(), h.sender, start)
	h.manager.Start(context.Background(), h.sender, start)
	h.sender.wait(t)

	var onces int
	for _, c := range h.sender.recorded() {
		if strings.Contains(c.data, "once") {
			onces++
		}
	}
	if onces != 1 {
		t.Errorf("the app ran %d times for one session id", onces)
	}
}

// TestSession_StopAllEndsLiveSessions. A worker disconnect ends every
// live session with a named error rather than leaving callers parked
// until their own deadlines expire -- and rather than leaving an agent
// running with nothing watching it.
func TestSession_StopAllEndsLiveSessions(t *testing.T) {
	fakeApp(t, "claude", "echo started\nsleep 60\n")
	h := newRig(t)
	h.manager.Start(context.Background(), h.sender, &memqlv1.AppSessionStart{
		SessionId:   "sess-stopall",
		App:         apps.IDClaudeCode,
		Kind:        KindRun,
		Workspace:   h.workspace,
		Credential:  testBearer,
		McpEndpoint: "https://mcp.example.com/mcp",
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && h.manager.Live() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	h.manager.StopAll("the worker's stream to the cluster was lost")

	end := h.sender.wait(t)
	if !strings.Contains(end.GetError(), "stream") {
		t.Errorf("error = %q, want the reason carried through", end.GetError())
	}
	// The End is sent before the session deregisters, so this is
	// inherently a moment later.
	settled := time.Now().Add(10 * time.Second)
	for time.Now().Before(settled) && h.manager.Live() != 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if h.manager.Live() != 0 {
		t.Errorf("live sessions = %d after StopAll", h.manager.Live())
	}
}

// TestSession_RenewRewritesTheConfigAndTheLibraryBearer.
func TestSession_RenewRewritesTheConfigAndTheLibraryBearer(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	fakeApp(t, "claude", fmt.Sprintf("touch %q\nsleep 30\n", ready))
	h := newRig(t)
	h.manager.Start(context.Background(), h.sender, &memqlv1.AppSessionStart{
		SessionId:   "sess-renew",
		App:         apps.IDClaudeCode,
		Kind:        KindRun,
		Workspace:   h.workspace,
		Credential:  testBearer,
		McpEndpoint: "https://mcp.example.com/mcp",
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	const next = "eyJhbGciOiJSUzI1NiJ9.renewed-mid-run.sig"
	h.manager.Control(&memqlv1.AppSessionControl{
		SessionId:  "sess-renew",
		Action:     ActionRenewCredential,
		Credential: next,
	})

	paths, globErr := filepath.Glob(filepath.Join(h.workspace, sessionScaffoldDir, "mcp-*.json"))
	if globErr != nil || len(paths) != 1 {
		t.Fatalf("session config paths: %v %v", paths, globErr)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), next) {
		t.Error("renew_credential did not rewrite the config in place")
	}
	if strings.Contains(string(data), testBearer) {
		t.Error("the superseded bearer survived the renewal")
	}

	h.manager.Control(&memqlv1.AppSessionControl{SessionId: "sess-renew", Action: ActionCancel})
	h.sender.wait(t)

	// The push at the end used the CURRENT bearer, not the one the
	// session opened with.
	var sawRenewed bool
	for _, b := range h.library.seenBearers() {
		if strings.Contains(b, next) {
			sawRenewed = true
		}
	}
	if !sawRenewed {
		t.Errorf("the Library calls did not pick up the renewed bearer: %v", h.library.seenBearers())
	}
}

// --- turns -----------------------------------------------------------

// waitForChunk blocks until the session has streamed something, which is
// how a test knows the app is actually up.
func (h *rig) waitForChunk(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.sender.recorded()) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the app never streamed anything; it did not start")
}

// twoTurnApp writes a fake `claude` that behaves differently on each
// turn, records each turn's argv, and holds the FIRST turn open until
// the test releases a gate file.
//
// The gate is what makes the follow-up arrive MID-TURN, which is the
// case that matters: a control cannot be answered, so the engine has no
// way to know a turn is in flight, and a cockpit that refused a
// follow-up for that reason would drop a prompt a person typed.
func twoTurnApp(t *testing.T, dir string) {
	t.Helper()
	fakeApp(t, "claude", fmt.Sprintf(`
d=%q
n=$(cat "$d/count" 2>/dev/null || echo 0)
n=$((n+1))
printf '%%s' "$n" > "$d/count"
printf '%%s\n' "$@" > "$d/argv.$n"
cat "$memql_test_config" > "$d/config.$n" 2>/dev/null
if [ "$n" = "1" ]; then
  echo '{"type":"system","subtype":"init","session_id":"app-turn-1"}'
  i=0
  while [ ! -f "$d/gate" ] && [ $i -lt 400 ]; do i=$((i+1)); sleep 0.05; done
  echo '{"type":"result","total_cost_usd":0.25,"usage":{"input_tokens":10,"output_tokens":20},"session_id":"app-turn-1"}'
  exit 0
fi
echo '{"type":"assistant","session_id":"app-turn-1","message":{"content":[{"type":"text","text":"the follow-up answer"},{"type":"tool_use","id":"t1","name":"Read"}]}}'
echo '{"type":"result","total_cost_usd":0.5,"usage":{"input_tokens":1,"output_tokens":2},"session_id":"app-turn-1"}'
exit 0
`, dir))
}

// TestSession_FollowUpRunsAsTheNextTurn is the two-turn session: a
// `message` control arriving while turn one is still running starts turn
// two in the SAME conversation, and the session ends when the queue
// drains rather than when the first process exits.
//
// It also pins the three things that only a second turn can show: the
// resume ref carried across processes, usage SUMMED over the session
// rather than taken from the last turn, and the typed chunks the harness
// produces (`text` for prose a person reads, `tool` for the envelope
// that carries the tool call).
func TestSession_FollowUpRunsAsTheNextTurn(t *testing.T) {
	dir := t.TempDir()
	twoTurnApp(t, dir)
	h := newRig(t)

	h.manager.Start(context.Background(), h.sender, &memqlv1.AppSessionStart{
		SessionId:   "sess-followup",
		App:         apps.IDClaudeCode,
		Kind:        KindRun,
		Prompt:      "the first question",
		Workspace:   h.workspace,
		Credential:  testBearer,
		McpEndpoint: "https://mcp.example.com/mcp",
	})
	h.waitForChunk(t)

	// Queued while turn one is in flight, then the gate is released so
	// turn one can finish. The order is the point: the follow-up is
	// accepted by a session that is busy, and it is delivered anyway.
	h.manager.Control(&memqlv1.AppSessionControl{
		SessionId: "sess-followup",
		Action:    ActionMessage,
		Prompt:    "and now the follow-up",
	})
	if err := os.WriteFile(filepath.Join(dir, "gate"), nil, 0o600); err != nil {
		t.Fatalf("release the gate: %v", err)
	}

	end := h.sender.wait(t)
	if end.GetExitCode() != 0 {
		t.Fatalf("exit_code = %d (%s); transcript: %s", end.GetExitCode(), end.GetError(), h.sender.transcript())
	}

	count, err := os.ReadFile(filepath.Join(dir, "count"))
	if err != nil || string(count) != "2" {
		t.Fatalf("the app ran %q times, want 2 -- the follow-up did not become a turn (%v)", count, err)
	}

	// Turn two resumes the conversation turn one opened. Without this
	// the follow-up is a stranger with no context, which looks identical
	// from the outside.
	second := readArgv(t, filepath.Join(dir, "argv.2"))
	if got := argvValue(second, "--resume"); got != "app-turn-1" {
		t.Errorf("second turn --resume = %q, want the app's own session id; argv: %v", got, second)
	}
	if got := argvValue(second, "--"); got != "and now the follow-up" {
		t.Errorf("second turn prompt = %q, want the follow-up's text; argv: %v", got, second)
	}
	first := readArgv(t, filepath.Join(dir, "argv.1"))
	if got := argvValue(first, "--resume"); got != "" {
		t.Errorf("the FIRST turn resumed %q; a run with no app_session_ref starts fresh", got)
	}

	// Usage is the SESSION's, summed across its turns. Taking only the
	// last would under-report every conversation in a ledger somebody
	// bills from.
	usage := end.GetUsage()
	if !usage.GetKnown() {
		t.Fatal("both turns reported usage; known must be true")
	}
	if usage.GetInputTokens() != 11 || usage.GetOutputTokens() != 22 || usage.GetCostUsd() != 0.75 {
		t.Errorf("usage = %+v, want both turns summed (11/22/0.75)", usage)
	}
	if end.GetAppSessionRef() != "app-turn-1" {
		t.Errorf("app_session_ref = %q, want the conversation both turns ran in", end.GetAppSessionRef())
	}

	// The typed chunks the harness produced: prose as `text`, the
	// envelope carrying the tool call as `tool`. The old runner had
	// neither -- everything that parsed as JSON was an `event` and
	// everything else was narration.
	var sawText, sawTool bool
	for _, c := range h.sender.recorded() {
		switch c.stream {
		case StreamText:
			if strings.Contains(c.data, "the follow-up answer") {
				sawText = true
			}
		case StreamTool:
			if strings.Contains(c.data, "tool_use") {
				sawTool = true
			}
		}
	}
	if !sawText {
		t.Errorf("no `text` chunk carried the assistant's prose: %+v", h.sender.recorded())
	}
	if !sawTool {
		t.Errorf("no `tool` chunk carried the tool call: %+v", h.sender.recorded())
	}
}

// TestSession_RenewLandsOnTheNextTurn. Claude Code reads its MCP
// configuration at STARTUP, so a renewal mid-run cannot reach the
// process already running -- that is the known limitation of
// mcpconfig.go. One process per turn is what makes it honest: the
// replacement bearer is in place before the next turn's process starts,
// and this proves the app actually reads it there.
func TestSession_RenewLandsOnTheNextTurn(t *testing.T) {
	dir := t.TempDir()
	twoTurnApp(t, dir)
	h := newRig(t)

	h.manager.Start(context.Background(), h.sender, &memqlv1.AppSessionStart{
		SessionId:   "sess-renew-turn",
		App:         apps.IDClaudeCode,
		Kind:        KindRun,
		Prompt:      "the first question",
		Workspace:   h.workspace,
		Credential:  testBearer,
		McpEndpoint: "https://mcp.example.com/mcp",
	})
	h.waitForChunk(t)

	const next = "eyJhbGciOiJSUzI1NiJ9.renewed-between-turns.sig"
	h.manager.Control(&memqlv1.AppSessionControl{
		SessionId:  "sess-renew-turn",
		Action:     ActionRenewCredential,
		Credential: next,
	})
	h.manager.Control(&memqlv1.AppSessionControl{
		SessionId: "sess-renew-turn",
		Action:    ActionMessage,
		Prompt:    "carry on",
	})
	if err := os.WriteFile(filepath.Join(dir, "gate"), nil, 0o600); err != nil {
		t.Fatalf("release the gate: %v", err)
	}
	h.sender.wait(t)

	// What the SECOND turn's process read off disk, captured by the fake
	// before it answered. Reading the file after the session would prove
	// nothing: it is deleted on the way out.
	second, err := os.ReadFile(filepath.Join(dir, "config.2"))
	if err != nil {
		t.Fatalf("the second turn read no config: %v", err)
	}
	if !strings.Contains(string(second), next) {
		t.Error("the second turn started with the superseded bearer; renewal is supposed to land on the next turn")
	}
	if strings.Contains(string(second), testBearer) {
		t.Error("the superseded bearer survived into the next turn's config")
	}
}

// TestSession_FollowUpQueueRefusesRatherThanSwallows.
//
// Both refusals exist because the alternative is silent. A follow-up
// queued into a session whose turn loop has already exited waits forever
// on a drain that will never come, and one appended to an unbounded
// queue is a memory hole fed by the network and drained by an app that
// takes minutes per turn. The queue is opened only by a running loop and
// latched shut by the same critical section that finds it empty.
func TestSession_FollowUpQueueRefusesRatherThanSwallows(t *testing.T) {
	s := &session{turnsClosed: true}

	if err := s.queueFollowUp("too late"); err == nil {
		t.Fatal("a follow-up for a session that is not taking turns must be refused, not queued")
	}

	s.openFollowUps()
	for i := range maxQueuedFollowUps {
		if err := s.queueFollowUp(fmt.Sprintf("prompt %d", i)); err != nil {
			t.Fatalf("follow-up %d was refused below the bound: %v", i, err)
		}
	}
	err := s.queueFollowUp("one too many")
	if err == nil {
		t.Fatal("the queue is unbounded; it is fed by the network")
	}
	if !strings.Contains(err.Error(), "not delivered") {
		t.Errorf("refusal = %q, want it to say the follow-up did not arrive", err)
	}

	for i := range maxQueuedFollowUps {
		got, ok := s.nextFollowUp()
		if !ok {
			t.Fatalf("the queue ran dry after %d of %d", i, maxQueuedFollowUps)
		}
		if want := fmt.Sprintf("prompt %d", i); got != want {
			t.Errorf("popped %q, want %q -- turns are a sequence, in order", got, want)
		}
	}
	if _, ok := s.nextFollowUp(); ok {
		t.Fatal("an empty queue must end the session, not invent a turn")
	}
	// Draining latched it shut, in the same critical section that found
	// it empty: anything less leaves a window where a follow-up is
	// accepted by a loop that has already decided to exit.
	if err := s.queueFollowUp("after the drain"); err == nil {
		t.Fatal("the queue accepted a follow-up after the turn loop had finished with it")
	}
}

// TestSession_StructuredResultRidesTheEnd pins AppSessionEnd.result_json
// (memql-cockpit#444), which the pin has carried since 2026-09-08 and this
// runner never set -- the answer left as a namespaced `event` chunk that
// nothing in the engine reads, so a structured answer never arrived.
//
// Two properties. The answer is on the End whatever the transcript cap has
// done, because the cap bounds NARRATION and an answer is not narration.
// And no chunk carries it any more: pre-release, a field that has landed
// replaces the stand-in rather than running beside it.
func TestSession_StructuredResultRidesTheEnd(t *testing.T) {
	newSession := func(sender *fakeSender, result []byte) *session {
		return &session{
			id:     "sess-result",
			start:  &memqlv1.AppSessionStart{SessionId: "sess-result"},
			sender: sender,
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			redact: newRedactor(testBearer),
			result: result,
			// The transcript cap has ALREADY bitten: every ordinary
			// chunk from here on is dropped.
			capped: true,
		}
	}

	t.Run("the answer is on the end", func(t *testing.T) {
		sender := newFakeSender()
		s := newSession(sender, []byte(`{"answer":42}`))
		s.sendEnd(0, "", nil)

		end := sender.wait(t)
		if end.GetResultJson() != `{"answer":42}` {
			t.Errorf("result_json = %q, want the app's own answer verbatim", end.GetResultJson())
		}
		for _, c := range sender.recorded() {
			if strings.Contains(c.data, "memql.app_session.result") {
				t.Errorf("the answer still left as a chunk as well: %q", c.data)
			}
		}
	})

	// IT DOES NOT IMPLY SUCCESS, which is the proto's own rule: a harness
	// can answer the schema and still exit non-zero, and dropping the
	// answer because the run failed would lose the one part we can read.
	t.Run("an answer rides a failed end too", func(t *testing.T) {
		sender := newFakeSender()
		s := newSession(sender, []byte(`{"answer":42}`))
		s.sendEnd(2, "the app exited 2", nil)

		end := sender.wait(t)
		if end.GetResultJson() != `{"answer":42}` || end.GetExitCode() != 2 || end.GetError() == "" {
			t.Errorf("end = %+v, want the answer beside the failure, neither folded into the other", end)
		}
	})

	t.Run("no result means an empty field, never an empty object", func(t *testing.T) {
		sender := newFakeSender()
		s := newSession(sender, nil)
		s.sendEnd(0, "", nil)

		if got := sender.wait(t).GetResultJson(); got != "" {
			t.Errorf("result_json = %q, want empty: an empty object reads as \"the app answered nothing\"", got)
		}
		if chunks := sender.recorded(); len(chunks) != 0 {
			t.Errorf("chunks = %+v, want none", chunks)
		}
	})
}

// TestSession_ResponseSchemaReachesTheHarness pins
// AppSessionStart.response_schema_json (memql-cockpit#444). The engine has
// been sending it since the pin carried it; this runner answered "the
// engine asked for nothing" to every session, so no app was ever asked for
// a structured answer.
func TestSession_ResponseSchemaReachesTheHarness(t *testing.T) {
	dir := t.TempDir()
	fakeApp(t, "claude", fmt.Sprintf(`
printf '%%s\n' "$@" > %q
echo '{"type":"system","subtype":"init","session_id":"app-schema"}'
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"app-schema","total_cost_usd":0.1,"usage":{"input_tokens":1,"output_tokens":2},"structured_output":{"city":"Paris"},"result":"{\"city\":\"Paris\"}"}'
`, filepath.Join(dir, "argv")))
	const schema = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`

	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.ResponseSchemaJson = schema
	})

	if end.GetError() != "" {
		t.Fatalf("error = %q; transcript: %s", end.GetError(), h.sender.transcript())
	}
	if got := argvValue(readArgv(t, filepath.Join(dir, "argv")), "--json-schema"); got != schema {
		t.Errorf("--json-schema = %q, want the engine's schema verbatim", got)
	}
	if end.GetResultJson() != `{"city":"Paris"}` {
		t.Errorf("result_json = %q, want the structured answer the app produced", end.GetResultJson())
	}
}

// TestSession_AReasonIsNeverAPrompt pins AppSessionControl.prompt
// (memql-cockpit#444). The engine sends a follow-up in `prompt`, and this
// runner read `reason` -- which the proto documents as transcript free-text
// on cancel -- so every follow-up arrived empty and was dropped. The fix
// reads `prompt` and ONLY `prompt`: one field meaning two things cannot be
// read without knowing which branch wrote it.
func TestSession_AReasonIsNeverAPrompt(t *testing.T) {
	dir := t.TempDir()
	twoTurnApp(t, dir)
	h := newRig(t)

	h.manager.Start(context.Background(), h.sender, &memqlv1.AppSessionStart{
		SessionId:   "sess-reason",
		App:         apps.IDClaudeCode,
		Kind:        KindRun,
		Prompt:      "the first question",
		Workspace:   h.workspace,
		Credential:  testBearer,
		McpEndpoint: "https://mcp.example.com/mcp",
	})
	h.waitForChunk(t)

	h.manager.Control(&memqlv1.AppSessionControl{
		SessionId: "sess-reason",
		Action:    ActionMessage,
		Reason:    "free text a cancel would carry",
	})
	if err := os.WriteFile(filepath.Join(dir, "gate"), nil, 0o600); err != nil {
		t.Fatalf("release the gate: %v", err)
	}
	h.sender.wait(t)

	if count, _ := os.ReadFile(filepath.Join(dir, "count")); string(count) != "1" {
		t.Errorf("the app ran %q times, want 1: a message control with no prompt is not a turn", count)
	}
}

// TestSession_CodexHarnessComesFromTheMachineNotTheId is the reason the
// runner resolves the app through the Detector rather than through the
// static spec table.
//
// Two harnesses answer to the id `codex`. apps.Specs() carries the FLOOR
// -- `codex mcp-server`, which every Codex has -- so a runner that
// trusted it would drive every Codex in the fleet through the fallback,
// losing usage numbers and structured answers on every machine whose
// binary has the app-server. Only a probe of THIS machine's binary can
// tell, and it is the same probe the registration advertised with.
func TestSession_CodexHarnessComesFromTheMachineNotTheId(t *testing.T) {
	cases := []struct {
		name       string
		probeExits int
		want       string
		notWant    string
	}{
		{name: "a codex with an app-server is driven through it", probeExits: 0, want: "app-server", notWant: "mcp-server"},
		{name: "a codex without one falls back to the mcp-server tools", probeExits: 9, want: "mcp-server", notWant: "app-server"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			argvFile := filepath.Join(dir, "argv")
			fakeApp(t, "codex", fmt.Sprintf(`
echo "$*" >> %q
if [ "$1" = "app-server" ] && [ "$2" = "--help" ]; then exit %d; fi
exit 9
`, argvFile, tc.probeExits))

			h := newRig(t, apps.IDCodex)
			end := h.start(t, func(s *memqlv1.AppSessionStart) {
				s.SessionId = "sess-codex-" + tc.want
				s.App = apps.IDCodex
			})
			// Every fake dies at once, so the handshake fails; what is
			// under test is WHICH subcommand was launched.
			if end.GetError() == "" {
				t.Fatal("a codex that dies at startup must end the session with a reason")
			}
			// Both Codex harnesses speak JSON-RPC over the child's
			// stdio. A supervisor that handed them the default closed
			// stdin would fail every Codex session on this machine with
			// this sentence, whatever the app did.
			if strings.Contains(end.GetError(), "without a stdin") {
				t.Errorf("the harness got no writable stdin from the supervisor: %s", end.GetError())
			}

			launched := ""
			for _, line := range readArgv(t, argvFile) {
				if line == "app-server" || line == "mcp-server" {
					launched = line
				}
			}
			if launched != tc.want {
				t.Errorf("launched %q, want %q; the recorded invocations were %v",
					launched, tc.want, readArgv(t, argvFile))
			}
			if launched == tc.notWant {
				t.Errorf("this machine's codex was driven through %q", launched)
			}
		})
	}
}

// TestSession_HarnessThatCannotStartStillDeletesTheConfig.
//
// A harness that forks in Start -- both Codex clients do -- is a NEW
// exit path out of the session, taken before any turn has run. The
// deletion of the MCP configuration is the security control rather than
// housekeeping (the bearer in it cannot be revoked), so every exit path
// has to pass through the teardown, including one added later by
// somebody who was thinking about something else.
func TestSession_HarnessThatCannotStartStillDeletesTheConfig(t *testing.T) {
	fakeApp(t, "codex", "exit 9\n")
	h := newRig(t, apps.IDCodex)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-codex-dead"
		s.App = apps.IDCodex
	})

	if end.GetError() == "" {
		t.Fatal("a session whose app could not be started must carry a reason")
	}
	if end.GetExitCode() == 0 {
		t.Error("a session that never ran a turn must not report a clean exit")
	}
	if found := grepTree(t, h.workspace, testBearer); len(found) > 0 {
		t.Errorf("the bearer survived a failed harness start in: %v", found)
	}
	if found := grepTree(t, h.state, testBearer); len(found) > 0 {
		t.Errorf("the bearer reached the ledger, which records paths only: %v", found)
	}
}

// TestSession_AttachWithoutAPromptIsRefused.
//
// Attaching is now RESUMING AND SPEAKING, because that is the only thing
// either app's protocol offers -- neither has a "watch the run somebody
// else started" primitive. A turn with nothing to say would spend the
// machine owner's subscription to ask the app nothing, so the refusal
// names what attach actually does instead.
func TestSession_AttachWithoutAPromptIsRefused(t *testing.T) {
	fakeApp(t, "claude", "exit 0\n")
	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-attach-noprompt"
		s.Kind = KindAttach
		s.AppSessionRef = "app-earlier-run"
		s.Prompt = ""
	})
	if !strings.Contains(end.GetError(), "prompt") {
		t.Errorf("error = %q, want it to name the missing prompt", end.GetError())
	}
	if !strings.Contains(end.GetError(), "app-earlier-run") {
		t.Errorf("error = %q, want it to name the session it would have resumed", end.GetError())
	}
}

// TestLauncher_IsTheSupervisorWithStdinAsADecision.
//
// The harness clients fork NOTHING: every process they need comes from
// this adapter, so that production keeps exactly one process supervisor.
// What that buys is asserted end to end by
// TestSession_CancelKillsTheProcessGroup, which now runs through this
// path -- a second supervisor would have quietly reaped only the direct
// child and left an agent running on somebody's machine.
//
// What is asserted here is the one thing the adapter had to ADD: stdin.
// The default stays closed so a prompt-on-stdin app fails fast instead
// of hanging on input that will never come; a JSON-RPC harness that owns
// both ends of the pipe opts in, because for `codex app-server` a closed
// stdin is not a safety property but a client with nothing to say.
func TestLauncher_IsTheSupervisorWithStdinAsADecision(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "reader.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nif read line; then echo \"got: $line\"; else echo 'stdin was closed'; fi\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	run := func(t *testing.T, stdin bool, write string) (*session, string) {
		t.Helper()
		s := &session{}
		proc, err := s.launcher()(context.Background(), dir, []string{script}, nil, stdin)
		if err != nil {
			t.Fatalf("launch: %v", err)
		}
		if w := proc.Stdin(); w != nil {
			if write != "" {
				if _, err := io.WriteString(w, write); err != nil {
					t.Fatalf("write to the child's stdin: %v", err)
				}
			}
			_ = w.Close()
		}
		out, err := io.ReadAll(proc.Stdout())
		if err != nil {
			t.Fatalf("read stdout: %v", err)
		}
		_ = proc.Wait()
		return s, strings.TrimSpace(string(out))
	}

	t.Run("a harness that asks for stdin can speak its protocol", func(t *testing.T) {
		s, out := run(t, true, "hello\n")
		if out != "got: hello" {
			t.Errorf("the child read %q, want the line written into its stdin", out)
		}
		// Registered on the session, which is what makes cancel and
		// teardown reach it. A process nothing has a handle on is an
		// agent running with nothing watching it.
		s.mu.Lock()
		registered := s.child != nil
		s.mu.Unlock()
		if !registered {
			t.Error("the launched process was not registered on the session; cancel cannot reach it")
		}
	})

	t.Run("the default is closed, and a reader sees EOF at once", func(t *testing.T) {
		_, out := run(t, false, "")
		if out != "stdin was closed" {
			t.Errorf("the child read %q, want an immediate EOF", out)
		}
	})

	t.Run("a session already over starts no further process", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "it-ran")
		s := &session{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := s.launcher()(ctx, dir, []string{"/bin/sh", "-c", "touch " + marker}, nil, false); err == nil {
			t.Fatal("a cancel that lands between two turns must not start the next one")
		}
		if _, err := os.Stat(marker); err == nil {
			t.Error("a process was forked for a session that was already over")
		}
	})
}

// --- the workspace a session runs in ---------------------------------------

// rigHome is the cluster home the workspace tests' manager serves.
const rigHome = "cluster.example"

// scratchApp is a fake claude that records the directory it ran in --
// OUTSIDE that directory, so the record outlives a workspace the session
// removes -- leaves a file behind, and answers.
func scratchApp(t *testing.T) (cwdFile string) {
	t.Helper()
	cwdFile = filepath.Join(t.TempDir(), "cwd")
	fakeApp(t, "claude", "pwd -P > '"+cwdFile+"'\necho 'made here' > notes.md\n"+quietClaude)
	return cwdFile
}

// ranIn is the directory the fake app recorded, or "" when it never ran.
func ranIn(t *testing.T, cwdFile string) string {
	t.Helper()
	data, err := os.ReadFile(cwdFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// realPath resolves a path the way `pwd -P` reports it (a macOS temp dir is
// behind /var -> /private/var).
func realPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return resolved
}

// isEmptyDir reports whether nothing was made under dir.
func isEmptyDir(t *testing.T, dir string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	return len(entries) == 0
}

// TestRunWithoutWorkspaceUsesScratchDir: an AppSessionStart that names no
// workspace is the engine saying "the machine chooses" -- it cannot know a
// path on somebody else's computer. The session runs in a directory of its
// own under this home, keyed by the session when no run owns it, pushes what
// it made, and takes the directory away with it.
func TestRunWithoutWorkspaceUsesScratchDir(t *testing.T) {
	cwdFile := scratchApp(t)
	h := newRig(t)
	scratch := t.TempDir()
	h.manager.opts.Home = rigHome
	h.manager.opts.ScratchRoot = scratch

	end := h.start(t, func(s *memqlv1.AppSessionStart) { s.Workspace = "" })

	if end.GetError() != "" || end.GetExitCode() != 0 {
		t.Fatalf("end = %d %q, want a session that ran", end.GetExitCode(), end.GetError())
	}
	want := filepath.Join(realPath(t, scratch), rigHome, "sess-test")
	if got := ranIn(t, cwdFile); got != want {
		t.Errorf("the app ran in %q, want the machine's own directory %q", got, want)
	}
	pushed := false
	for name, body := range h.library.uploaded() {
		pushed = pushed || (strings.HasSuffix(name, "notes.md") && string(body) == "made here\n")
	}
	if !pushed {
		t.Errorf("what the app made in the scratch directory was not pushed: %v", h.library.uploaded())
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Errorf("the session's own directory outlived it (stat: %v); nothing else will ever clean it", err)
	}
}

// TestScratchWorkspaceUnderPolicyRoot: a machine whose policy.yaml names
// fs.workspace_root keeps its sessions under it -- per home, then per run --
// and the path still goes through this machine's workspace check, as a path
// the engine named would.
func TestScratchWorkspaceUnderPolicyRoot(t *testing.T) {
	cwdFile := scratchApp(t)
	h := newRig(t)
	root := t.TempDir()
	unused := t.TempDir()
	var checked []string
	h.manager.opts.Home = rigHome
	h.manager.opts.ScratchRoot = unused
	h.manager.opts.WorkspaceRoot = func() string { return root }
	h.manager.opts.CheckWorkspace = func(path string) error {
		checked = append(checked, path)
		return nil
	}

	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.Workspace = ""
		s.RunId = "run-7"
	})

	if end.GetError() != "" {
		t.Fatalf("end error = %q", end.GetError())
	}
	want := filepath.Join(root, rigHome, "run-7")
	if got := ranIn(t, cwdFile); got != realPath(t, want) {
		t.Errorf("the app ran in %q, want %q under fs.workspace_root", got, want)
	}
	if len(checked) != 1 || checked[0] != want {
		t.Errorf("CheckWorkspace saw %v, want exactly the chosen %q", checked, want)
	}
	if !isEmptyDir(t, unused) {
		t.Error("the default scratch directory was used although policy.yaml names a workspace root")
	}
}

// TestScratchWorkspaceRefusedByPolicyNeverRuns: the machine's own choice is
// not exempt from its own policy. A refused path ends the session naming it,
// before anything runs.
func TestScratchWorkspaceRefusedByPolicyNeverRuns(t *testing.T) {
	cwdFile := scratchApp(t)
	h := newRig(t)
	h.manager.opts.Home = rigHome
	h.manager.opts.ScratchRoot = t.TempDir()
	h.manager.opts.CheckWorkspace = func(path string) error {
		return fmt.Errorf("fs deny list: %q", path)
	}

	end := h.start(t, func(s *memqlv1.AppSessionStart) { s.Workspace = "" })

	if !strings.Contains(end.GetError(), "workspace refused by this machine's policy") ||
		!strings.Contains(end.GetError(), filepath.Join(rigHome, "sess-test")) {
		t.Errorf("error = %q, want the refusal naming the directory this machine chose", end.GetError())
	}
	if got := ranIn(t, cwdFile); got != "" {
		t.Errorf("the app ran in %q in a workspace this machine refused", got)
	}
}

// TestRunKeyedScratchSurvivesSessionEnd: a directory keyed by a RUN belongs
// to the run -- a later session of the same run finds what an earlier one
// left there -- so the session's end removes only its own scaffolding, and
// the bearer with it.
func TestRunKeyedScratchSurvivesSessionEnd(t *testing.T) {
	cwdFile := scratchApp(t)
	h := newRig(t)
	scratch := t.TempDir()
	h.manager.opts.Home = rigHome
	h.manager.opts.ScratchRoot = scratch

	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.Workspace = ""
		s.RunId = "run-42"
	})

	if end.GetError() != "" {
		t.Fatalf("end error = %q", end.GetError())
	}
	dir := filepath.Join(scratch, rigHome, "run-42")
	if got := ranIn(t, cwdFile); got != realPath(t, dir) {
		t.Errorf("the app ran in %q, want the run's directory %q", got, dir)
	}
	if body, err := os.ReadFile(filepath.Join(dir, "notes.md")); err != nil || string(body) != "made here\n" {
		t.Errorf("the run's file did not survive the session's end: %q, %v", body, err)
	}
	for _, gone := range []string{".mcp.json", sessionScaffoldDir} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s outlived the session in the run's directory (stat: %v)", gone, err)
		}
	}
}

// TestExplicitWorkspaceUnchanged: a workspace the engine named is used as
// named, and never removed -- it can be a real project somebody works in.
func TestExplicitWorkspaceUnchanged(t *testing.T) {
	cwdFile := scratchApp(t)
	h := newRig(t)
	scratch := t.TempDir()
	h.manager.opts.Home = rigHome
	h.manager.opts.ScratchRoot = scratch

	end := h.start(t, func(s *memqlv1.AppSessionStart) { s.RunId = "run-9" })

	if end.GetError() != "" {
		t.Fatalf("end error = %q", end.GetError())
	}
	if got := ranIn(t, cwdFile); got != realPath(t, h.workspace) {
		t.Errorf("the app ran in %q, want the named workspace %q", got, h.workspace)
	}
	if _, err := os.Stat(filepath.Join(h.workspace, "notes.md")); err != nil {
		t.Errorf("the named workspace lost what the app made in it: %v", err)
	}
	if !isEmptyDir(t, scratch) {
		t.Error("a scratch directory was made although the engine named a workspace")
	}
}

// TestScratchRootIsOutsideTheWorkerDir: with no fs.workspace_root the
// sessions go in the platform's data directory, NEVER under ~/.memql -- that
// holds the worker's tokens and the consent file, and a session runs shell
// commands where it works.
func TestScratchRootIsOutsideTheWorkerDir(t *testing.T) {
	const home = "/home/ada"
	cases := []struct {
		name, goos, xdg, want string
	}{
		{"macOS", "darwin", "", "/home/ada/Library/Application Support/MemQL/workspaces"},
		{"macOS ignores XDG", "darwin", "/x/data", "/home/ada/Library/Application Support/MemQL/workspaces"},
		{"linux default", "linux", "", "/home/ada/.local/share/memql/workspaces"},
		{"linux XDG_DATA_HOME", "linux", "/x/data", "/x/data/memql/workspaces"},
		// The XDG spec says a relative value is to be ignored.
		{"linux relative XDG", "linux", "data", "/home/ada/.local/share/memql/workspaces"},
		{"no home", "linux", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := home
			if c.name == "no home" {
				h = ""
			}
			got := scratchRootFor(c.goos, h, c.xdg)
			if got != c.want {
				t.Errorf("scratchRootFor(%q, %q, %q) = %q, want %q", c.goos, h, c.xdg, got, c.want)
			}
			if got != "" && strings.HasPrefix(got+"/", filepath.Join(home, ".memql")+"/") {
				t.Errorf("%q is under ~/.memql", got)
			}
		})
	}
}

// TestScratchKeyIsOnePathSegment: the keys are server-minted ids, and a
// directory name is built from them, so nothing in one may climb out of the
// home's directory or hide in it.
func TestScratchKeyIsOnePathSegment(t *testing.T) {
	for in, want := range map[string]string{
		"v1:work:run:6de15921": "v1_work_run_6de15921",
		"../../etc":            "_._.._etc",
		"..":                   "unnamed",
		"":                     "unnamed",
		"api.memql.localhost":  "api.memql.localhost",
		".hidden":              "_hidden",
	} {
		if got := scratchSegment(in); got != want {
			t.Errorf("scratchSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

// logBuffer is a log sink a session goroutine can write while a test reads.
type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// settled waits until the manager has let go of every session. The End is
// logged AFTER it is sent, so the sender seeing it is not yet the log having
// it; the session is forgotten only once its goroutine has returned.
func (h *rig) settled(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for h.manager.Live() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the session never let go after its End")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSessionEndLogsWhyItFailed: a refused or failed session says WHY in the
// machine's own log, not just that it did. The End carries the reason to the
// engine, but the person debugging a machine reads its log -- and "error=true"
// sends them to a cluster they may not be able to query. The reason is
// redacted exactly as the End's is.
func TestSessionEndLogsWhyItFailed(t *testing.T) {
	t.Run("a refusal", func(t *testing.T) {
		// The app must be found, or the PATH refusal comes first and the
		// workspace refusal this asserts is never reached -- which is what a
		// runner without Claude Code installed does.
		fakeApp(t, "claude", "exit 0\n")
		h := newRig(t)
		logs := &logBuffer{}
		h.manager.logger = slog.New(slog.NewTextHandler(logs, nil))

		end := h.start(t, func(s *memqlv1.AppSessionStart) { s.Workspace = "not/absolute" })

		if !strings.Contains(end.GetError(), "is not absolute") {
			t.Fatalf("end error = %q, want the workspace refusal", end.GetError())
		}
		h.settled(t)
		out := logs.String()
		if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "is not absolute") {
			t.Errorf("the machine's log does not say why the session was refused:\n%s", out)
		}
	})

	t.Run("a failure quoting the bearer", func(t *testing.T) {
		fakeApp(t, "claude", "echo 'auth broke: "+testBearer+"' >&2\nexit 1\n")
		h := newRig(t)
		logs := &logBuffer{}
		h.manager.logger = slog.New(slog.NewTextHandler(logs, nil))

		end := h.start(t, nil)

		if end.GetError() == "" {
			t.Fatal("a session whose app exited 1 ended without an error")
		}
		h.settled(t)
		out := logs.String()
		if !strings.Contains(out, "auth broke") {
			t.Errorf("the machine's log does not carry the app's reason:\n%s", out)
		}
		if strings.Contains(out, testBearer) {
			t.Errorf("the session bearer reached the machine's log:\n%s", out)
		}
	})
}

// --- what a session may never touch ------------------------------------------

// protectedRig is a rig whose manager knows a home directory holding this
// worker's own directory (~/.memql: the worker tokens, and policy.yaml, whose
// apps.allow is the app consent gate) and one fs.deny entry (~/.ssh).
func protectedRig(t *testing.T) (h *rig, home, workerDir, sshDir string) {
	t.Helper()
	h = newRig(t)
	home = t.TempDir()
	workerDir = filepath.Join(home, ".memql")
	sshDir = filepath.Join(home, ".ssh")
	for _, dir := range []string{workerDir, sshDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	h.manager.opts.WorkerPaths = []string{workerDir}
	h.manager.opts.DenyPaths = func() []string { return []string{sshDir} }
	return h, home, workerDir, sshDir
}

// TestWorkspaceOverlappingAProtectedPathIsRefused: THE WORKSPACE IS THE WRITE
// GRANT -- the app may edit anything in it and run shell commands that write
// anywhere in it -- so a workspace that contains this worker's own directory
// or an fs.deny entry would hand the session the worker tokens and the
// consent file, however the engine came to name it (a delegation policy's
// workspace root of "~" names the home directory). One inside such a path is
// refused as well: that is the path itself. Refused before anything is
// written, the bearer's configuration included.
func TestWorkspaceOverlappingAProtectedPathIsRefused(t *testing.T) {
	cases := []struct {
		name      string
		workspace func(home, workerDir, sshDir string) string
		names     func(home, workerDir, sshDir string) string
	}{
		{"the home directory", func(home, _, _ string) string { return home },
			func(_, workerDir, _ string) string { return workerDir }},
		{"the worker's own directory", func(_, workerDir, _ string) string { return workerDir },
			func(_, workerDir, _ string) string { return workerDir }},
		{"inside the worker's own directory", func(_, workerDir, _ string) string { return filepath.Join(workerDir, "state", "x") },
			func(_, workerDir, _ string) string { return workerDir }},
		{"the root", func(_, _, _ string) string { return "/" },
			func(_, workerDir, _ string) string { return workerDir }},
		{"the fs.deny entry", func(_, _, sshDir string) string { return sshDir },
			func(_, _, sshDir string) string { return sshDir }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cwdFile := scratchApp(t)
			h, home, workerDir, sshDir := protectedRig(t)
			workspace := c.workspace(home, workerDir, sshDir)

			end := h.start(t, func(s *memqlv1.AppSessionStart) { s.Workspace = workspace })

			if !strings.Contains(end.GetError(), "overlaps") ||
				!strings.Contains(end.GetError(), c.names(home, workerDir, sshDir)) {
				t.Errorf("error = %q, want the refusal naming the protected path", end.GetError())
			}
			if got := ranIn(t, cwdFile); got != "" {
				t.Errorf("the app ran in %q", got)
			}
			if _, err := os.Stat(filepath.Join(workspace, ".mcp.json")); !os.IsNotExist(err) {
				t.Errorf("the bearer's configuration was written into a refused workspace (stat: %v)", err)
			}
		})
	}

	t.Run("a directory above an fs.deny entry", func(t *testing.T) {
		cwdFile := scratchApp(t)
		h, home, _, _ := protectedRig(t)
		projects := filepath.Join(home, "projects")
		secrets := filepath.Join(projects, "secrets")
		h.manager.opts.DenyPaths = func() []string { return []string{secrets} }

		end := h.start(t, func(s *memqlv1.AppSessionStart) { s.Workspace = projects })

		if !strings.Contains(end.GetError(), "overlaps") || !strings.Contains(end.GetError(), "fs.deny") ||
			!strings.Contains(end.GetError(), secrets) {
			t.Errorf("error = %q, want the refusal naming the fs.deny entry it contains", end.GetError())
		}
		if got := ranIn(t, cwdFile); got != "" {
			t.Errorf("the app ran in %q", got)
		}
	})

	t.Run("a directory beside them runs", func(t *testing.T) {
		cwdFile := scratchApp(t)
		h, home, _, _ := protectedRig(t)
		work := filepath.Join(home, "work")

		end := h.start(t, func(s *memqlv1.AppSessionStart) { s.Workspace = work })

		if end.GetError() != "" {
			t.Fatalf("end error = %q, want a session that ran", end.GetError())
		}
		if got := ranIn(t, cwdFile); got != realPath(t, work) {
			t.Errorf("the app ran in %q, want %q", got, work)
		}
	})
}

// TestSessionDeniesTheProtectedPathsToTheApp: the same paths reach the app as
// paths it may neither read nor write -- the worker's own directory from the
// manager's options, and fs.deny as policy.yaml says it NOW (a SIGHUP reaches
// the next session). Refusing the workspace keeps them out of what the app
// may write; only this keeps a sandboxed `cat` from reading them into a file
// that is then pushed to the Library.
func TestSessionDeniesTheProtectedPathsToTheApp(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	fakeApp(t, "claude", fmt.Sprintf("printf '%%s\\n' \"$@\" > %q\n", argvFile)+quietClaude)
	h, _, workerDir, sshDir := protectedRig(t)
	deny := sshDir
	h.manager.opts.DenyPaths = func() []string { return []string{deny, "relative/entry"} }

	end := h.start(t, nil)

	if end.GetError() != "" {
		t.Fatalf("end error = %q", end.GetError())
	}
	argv := readArgv(t, argvFile)
	rules := argvValue(argv, "--disallowedTools")
	for _, path := range []string{workerDir, sshDir} {
		for _, tool := range []string{"Read", "Edit"} {
			if want := tool + "(/" + path + ")"; !strings.Contains(rules, want) {
				t.Errorf("--disallowedTools = %q, want %s", rules, want)
			}
		}
	}
	var settings struct {
		Sandbox struct {
			Filesystem struct {
				DenyRead []string `json:"denyRead"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(argvValue(argv, "--settings")), &settings); err != nil {
		t.Fatalf("--settings: %v", err)
	}
	denied := strings.Join(settings.Sandbox.Filesystem.DenyRead, "\n")
	for _, path := range []string{workerDir, sshDir} {
		if !strings.Contains(denied, path) {
			t.Errorf("sandbox.filesystem.denyRead = %q, want %s in it", denied, path)
		}
	}
	// A relative entry names no place on this machine; CheckPath never
	// matches one either.
	if strings.Contains(denied, "relative/entry") || strings.Contains(rules, "relative/entry") {
		t.Errorf("a relative fs.deny entry reached the app: %q / %q", denied, rules)
	}
}

// failingLibrary is a Library that refuses every upload, as one that is
// unreachable would.
func failingLibrary(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "the library is down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSessionScratchKeptWhenThePushFails: a directory made for the session
// alone holds the only copy of what the app made until the push lands. When
// the push fails it is KEPT, and the machine's log says where -- deleting it
// would turn "the Library was unreachable" into "the work is gone".
func TestSessionScratchKeptWhenThePushFails(t *testing.T) {
	scratchApp(t)
	h := newRig(t)
	scratch := t.TempDir()
	logs := &logBuffer{}
	h.manager.logger = slog.New(slog.NewTextHandler(logs, nil))
	h.manager.opts.Home = rigHome
	h.manager.opts.ScratchRoot = scratch
	down := failingLibrary(t)
	h.manager.opts.LibraryBase = down.URL
	h.manager.opts.HTTPClient = preflightFixtureClient(h.library.server)

	end := h.start(t, func(s *memqlv1.AppSessionStart) { s.Workspace = "" })

	if !strings.Contains(end.GetError(), "pushing outputs to the Library failed") {
		t.Fatalf("end error = %q, want the failed push", end.GetError())
	}
	dir := filepath.Join(scratch, rigHome, "sess-test")
	if body, err := os.ReadFile(filepath.Join(dir, "notes.md")); err != nil || string(body) != "made here\n" {
		t.Errorf("what the app made is gone with the push failed: %q, %v", body, err)
	}
	h.settled(t)
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, dir) {
		t.Errorf("the machine's log does not say where the kept workspace is:\n%s", out)
	}
}

// TestOpenSessionKeepsItsScratchDir: an `open` session hands the app to a
// PERSON, whose terminal can outlive the session -- a cancel or the
// wall-clock ceiling stops the launcher, not the shell or the app in the
// window, and a forking Linux terminal returns at once. The directory they
// are working in is not removed from under them.
func TestOpenSessionKeepsItsScratchDir(t *testing.T) {
	fakeApp(t, "claude", "exit 0\n")
	h := newRig(t)
	scratch := t.TempDir()
	h.manager.opts.Home = rigHome
	h.manager.opts.ScratchRoot = scratch
	// The person's terminal: it writes into the workspace and stays up.
	h.manager.opts.OpenCommand = func(string) ([]string, string, error) {
		return []string{"/bin/sh", "-c", "echo 'still working' > person.md; exec sleep 60"}, "opened in the test terminal", nil
	}
	dir := filepath.Join(scratch, rigHome, "sess-open")

	h.manager.Start(context.Background(), h.sender, &memqlv1.AppSessionStart{
		SessionId:   "sess-open",
		App:         apps.IDClaudeCode,
		Kind:        KindOpen,
		Prompt:      "have a look",
		Credential:  testBearer,
		McpEndpoint: "https://mcp.example.com/mcp",
	})
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "person.md")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the open session never started its terminal")
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.manager.Control(&memqlv1.AppSessionControl{SessionId: "sess-open", Action: ActionCancel, Reason: "done watching"})
	end := h.sender.wait(t)

	if !strings.Contains(end.GetError(), "cancelled") {
		t.Errorf("end error = %q, want the cancel", end.GetError())
	}
	if body, err := os.ReadFile(filepath.Join(dir, "person.md")); err != nil || string(body) != "still working\n" {
		t.Errorf("the person's working directory was removed from under them: %q, %v", body, err)
	}
}

type fixtureMCPTransport struct {
	base   http.RoundTripper
	target string
}

func (t fixtureMCPTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == "mcp.example.com" {
		clone := r.Clone(r.Context())
		target, _ := http.NewRequest(r.Method, t.target+"/mcp", nil)
		clone.URL = target.URL
		r = clone
	}
	return t.base.RoundTrip(r)
}
func preflightFixtureClient(s *httptest.Server) *http.Client {
	return &http.Client{Transport: fixtureMCPTransport{base: s.Client().Transport, target: s.URL}}
}
