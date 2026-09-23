package procedure

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// harness_test.go -- the RECORDING ENGINE every test here runs against, and
// the corpus fixtures it answers with.
//
// It records the CONTEXT of every call, not only the query: the two rules this
// package turns on -- internal origin on every @serverOnly construct, the
// owner's actor on every owned read -- both live in the context, and both
// fail SILENTLY at a seam that only watches strings. A fake parses nothing,
// either, which is why parse_test.go hands every statement a lift renders to
// the real front end.

type recordedCall struct {
	Query     string
	Internal  bool
	Actor     string
	Synthetic bool
}

// Name is the construct the call names: `mutation recordProcedure(...)` ->
// recordProcedure.
func (c recordedCall) Name() string {
	q := strings.TrimSpace(c.Query)
	for _, kind := range []string{"mutation ", "query ", "builtin "} {
		q = strings.TrimPrefix(q, kind)
	}
	if i := strings.IndexByte(q, '('); i > 0 {
		return q[:i]
	}
	return q
}

func (c recordedCall) IsWrite() bool {
	return strings.HasPrefix(strings.TrimSpace(c.Query), "mutation ")
}

type conditionalReply struct {
	name, contains string
	rows           []map[string]any
}

type fakeEngine struct {
	mu          sync.Mutex
	calls       []recordedCall
	replies     map[string][]map[string]any
	conditional []conditionalReply
	fail        map[string]error
	// ownedBy models an OWNED read: a construct listed here answers its rows
	// only to calls made as that actor, and nothing to anybody else -- the
	// composite tier's answer, zero rows and no error.
	ownedBy map[string]string
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{replies: map[string][]map[string]any{}, fail: map[string]error{}, ownedBy: map[string]string{}}
}

// ownedRead makes a construct answer only its owner.
func (e *fakeEngine) ownedRead(name, owner string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ownedBy[name] = owner
}

func (e *fakeEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	c := recordedCall{Query: query, Internal: auth.OriginFromContext(ctx).IsInternal()}
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		c.Actor, c.Synthetic = ac.UserId, ac.Synthetic
	}
	e.mu.Lock()
	e.calls = append(e.calls, c)
	name := c.Name()
	rows := e.replies[name]
	for _, r := range e.conditional {
		if r.name == name && strings.Contains(query, r.contains) {
			rows = r.rows
			break
		}
	}
	if owner, owned := e.ownedBy[name]; owned && c.Actor != owner {
		rows = nil
	}
	err := e.fail[name]
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// A shaped read answers `output` rows, the shape every read here has.
	payload := make([]any, 0, len(rows))
	for _, r := range rows {
		payload = append(payload, r)
	}
	return memql.NewResultWithOutput(payload), nil
}

func (e *fakeEngine) reply(name string, rows ...map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.replies[name] = rows
}

func (e *fakeEngine) replyWhen(name, contains string, rows ...map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.conditional = append(e.conditional, conditionalReply{name: name, contains: contains, rows: rows})
}

func (e *fakeEngine) recorded() []recordedCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]recordedCall(nil), e.calls...)
}

func (e *fakeEngine) callsTo(name string) []recordedCall {
	var out []recordedCall
	for _, c := range e.recorded() {
		if c.Name() == name {
			out = append(out, c)
		}
	}
	return out
}

func (e *fakeEngine) callTo(t *testing.T, name string) recordedCall {
	t.Helper()
	found := e.callsTo(name)
	if len(found) != 1 {
		t.Fatalf("expected exactly one call to %s, got %d (%s)", name, len(found), e.summary())
	}
	return found[0]
}

func (e *fakeEngine) writes() []recordedCall {
	var out []recordedCall
	for _, c := range e.recorded() {
		if c.IsWrite() {
			out = append(out, c)
		}
	}
	return out
}

func (e *fakeEngine) summary() string {
	var names []string
	for _, c := range e.recorded() {
		names = append(names, c.Name())
	}
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

// passingGate is a Gate 1 that compiles everything it is handed.
type passingGate struct{ compiled []memql.SandboxConstruct }

func (g *passingGate) CompileBundle(cs []memql.SandboxConstruct) memql.SandboxReport {
	g.compiled = append(g.compiled, cs...)
	rep := memql.SandboxReport{OK: true}
	for _, c := range cs {
		rep.Diagnostics = append(rep.Diagnostics, memql.SandboxDiagnostic{Name: c.Name, Kind: c.Kind, OK: true})
	}
	return rep
}

// failingGate refuses everything, naming why.
type failingGate struct{}

func (failingGate) CompileBundle(cs []memql.SandboxConstruct) memql.SandboxReport {
	rep := memql.SandboxReport{OK: false}
	for _, c := range cs {
		rep.Diagnostics = append(rep.Diagnostics, memql.SandboxDiagnostic{Name: c.Name, Kind: c.Kind, Error: "undefined: nothing"})
	}
	return rep
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var testNow = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func newTestIntegration(eng Engine) *Integration {
	i := New(eng, discardLogger())
	i.SetNow(func() time.Time { return testNow })
	return i
}

// --- the corpus -------------------------------------------------------------

const (
	testOwner     = "v1:identity:user:alice"
	testWorkspace = "/w/project"
	testStatement = "Write the greeting file"
)

// testSignature is a real goal signature: a hex digest, which is what a
// procedure's name is built from.
var testSignature = work.GoalSignature(testStatement, []string{"file"})

// recFixture is one recorded session: an exec that writes a file whose name
// is the goal's `file` input, then an fs_write of a fixed report. What varies
// between recordings is the file's name, which the template generalizes into
// a free hole the goal input supplies; the report is the same bytes at the
// same path every time, so every recording references ONE content-addressed
// Library row -- filed, and named, by the first session that wrote it.
type recFixture struct {
	runId       string
	createdAt   time.Time
	file        string // a.txt
	sessionId   string
	model       string
	effort      string
	triggeredBy string
	// stepFeedback are verdict rows naming the fs_write step: version ->
	// verdict, applied in order.
	stepFeedback []stepFeedback
	// runDisliked adds a dislike naming the whole run.
	runDisliked bool
	// noToolResult drops the exec step's observation.
	noToolResult bool
	// argsTruncated marks the exec step's arguments truncated.
	argsTruncated bool
	// noFingerprint leaves the session's fingerprint off.
	noFingerprint bool
	// execExit overrides the exec exit code.
	execExit int
}

type stepFeedback struct {
	version int
	verdict string
	at      time.Time
}

func recording1(file string, created time.Time) recFixture {
	return recFixture{
		runId:     "v1:work:run:rec-" + strings.TrimSuffix(file, ".txt"),
		createdAt: created,
		file:      file,
		sessionId: "v1:worker:appSession:sess-" + strings.TrimSuffix(file, ".txt"),
		model:     "claude-sonnet-4-6",
		effort:    "high",
	}
}

func (r recFixture) execKey() string {
	return "action-toolu_exec_" + strings.TrimSuffix(r.file, ".txt")
}
func (r recFixture) writeKey() string {
	return "action-toolu_write_" + strings.TrimSuffix(r.file, ".txt")
}

// reportFileId is the ONE Library row every recording's write references:
// identical bytes are one file (the recorder deduplicates by digest), filed
// under the name the FIRST session's action composed.
const reportFileId = "v1:library:file:f-report"

// reportPath is where every recording wrote the report.
const reportPath = testWorkspace + "/out/report.txt"

// helloDigest is sha256("hello\n"), the bytes every recording wrote.
const helloDigest = "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"

func (r recFixture) runRow() map[string]any {
	trig := r.triggeredBy
	if trig == "" {
		trig = "system"
	}
	return map[string]any{
		"id":             r.runId,
		"createdAt":      r.createdAt.Format(time.RFC3339Nano),
		"ownerUserId":    testOwner,
		"goalId":         "v1:work:goal:session-" + r.file,
		"goalSignature":  testSignature,
		"parentRunId":    "v1:work:run:parent-" + r.file,
		"status":         "succeeded",
		"triggeredBy":    trig,
		"automationName": "appSession",
		"variables":      map[string]any{"file": r.file},
		"input":          map[string]any{"app": "claude-code", "parentRunId": "v1:work:run:parent-" + r.file},
		"summary": map[string]any{
			"sessionId": r.sessionId, "model": r.model, "effort": r.effort,
			"recordedActions": float64(2), "droppedActions": float64(0),
		},
	}
}

func fingerprintFixture() map[string]any {
	return map[string]any{
		"type": "memql.app_session.fingerprint", "v": float64(1), "seq": float64(0),
		"platform": map[string]any{"os": "darwin", "arch": "arm64"},
		"tools": []any{
			map[string]any{"name": "mkdir", "version": "mkdir (GNU coreutils) 9.4"},
			map[string]any{"name": "echo", "version": "echo 9.4"},
			map[string]any{"name": "git", "version": "git version 2.43.0"},
		},
		"cwd":        testWorkspace,
		"cwdEntries": float64(0),
		"variables":  []any{map[string]any{"name": "PATH", "set": true, "digest": "sha256:path"}},
	}
}

func (r recFixture) stepRows() []map[string]any {
	exec := map[string]any{
		"id": "v1:work:step:" + r.execKey(), "runId": r.runId, "key": r.execKey(), "seq": float64(0),
		"stepType": "exec", "resultFingerprint": "sha256:cockpit-exec-" + r.file,
		"result": map[string]any{"digest": "sha256:cockpit-exec-" + r.file, "type": "string", "exitCode": float64(r.execExit)},
	}
	if !r.noFingerprint {
		exec["fingerprint"] = fingerprintFixture()
	}
	write := map[string]any{
		"id": "v1:work:step:" + r.writeKey(), "runId": r.runId, "key": r.writeKey(), "seq": float64(1),
		"stepType": "fs_write", "resultFingerprint": "sha256:cockpit-write-" + r.file,
		"result": map[string]any{"digest": "sha256:cockpit-write-" + r.file, "type": "string"},
	}
	answer := map[string]any{
		"id": "v1:work:step:app_answer-" + r.file, "runId": r.runId, "key": "app_answer", "seq": float64(2),
		"stepType": "app_answer", "result": map[string]any{"status": "ended"},
	}
	// Newest first, the way a caller that forgot to sort would see them.
	return []map[string]any{answer, write, exec}
}

func argsJSON(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (r recFixture) observationRows(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	if !r.noToolResult {
		data := map[string]any{
			"tool": "exec", "appActionId": "toolu_exec_" + strings.TrimSuffix(r.file, ".txt"),
			"sessionId": r.sessionId, "seq": float64(1), "isError": r.execExit != 0,
			"exitCode": float64(r.execExit), "resultType": "string", "cwd": testWorkspace,
			"args": argsJSON(t, map[string]any{"command": "mkdir -p out && echo hello > " + r.file}),
		}
		if r.argsTruncated {
			data["argsTruncated"] = true
		}
		out = append(out, map[string]any{"kind": "tool_result", "stepKey": r.execKey(), "data": data})
	}
	out = append(out, map[string]any{
		"kind": "tool_result", "stepKey": r.writeKey(),
		"data": map[string]any{
			"tool": "fs_write", "appActionId": "toolu_write_" + strings.TrimSuffix(r.file, ".txt"),
			"sessionId": r.sessionId, "seq": float64(2), "isError": false, "resultType": "string",
			"args":        argsJSON(t, map[string]any{"file_path": reportPath, "content": "hello\n"}),
			"contentRefs": []any{reportFileId},
		},
	})
	for _, f := range r.stepFeedback {
		out = append(out, map[string]any{
			"kind": "feedback", "createdAt": f.at.Format(time.RFC3339Nano),
			"data": map[string]any{"verdict": f.verdict, "target": map[string]any{"stepKey": r.writeKey(), "version": float64(f.version)}},
		})
	}
	if r.runDisliked {
		out = append(out, map[string]any{"kind": "feedback", "data": map[string]any{"verdict": "disliked"}})
	}
	return out
}

// seedCorpus answers every read a lift of these recordings makes.
func seedCorpus(t *testing.T, eng *fakeEngine, recs ...recFixture) {
	t.Helper()
	var runs []map[string]any
	for _, r := range recs {
		runs = append(runs, r.runRow())
		eng.replyWhen("workStepsForOwnerRun", `"`+r.runId+`"`, r.stepRows()...)
		eng.replyWhen("workObservationsForOwnerRun", `"`+r.runId+`"`, r.observationRows(t)...)
		eng.replyWhen("workRunForOwner", `"v1:work:run:parent-`+r.file+`"`, map[string]any{
			"id": "v1:work:run:parent-" + r.file, "goalId": "v1:work:goal:goal-" + r.file,
		})
		eng.replyWhen("workGoalForOwner", `"v1:work:goal:goal-`+r.file+`"`, map[string]any{
			"id": "v1:work:goal:goal-" + r.file, "statement": testStatement,
		})
	}
	// The report's row, filed by the first session that wrote it and named
	// by contentFileName's convention: the base, then the 12-rune tails of
	// that session's and that action's ids.
	eng.replyWhen("libraryFileById", `"`+reportFileId+`"`, map[string]any{
		"id": reportFileId, "name": "report.txt.sess-a.toolu_write_", "sha256": helloDigest,
	})
	// Newest first, as the query sorts.
	sort.SliceStable(runs, func(a, b int) bool {
		return str(runs[a], "createdAt") > str(runs[b], "createdAt")
	})
	eng.reply("workRunsForOwnerGoalSignature", runs...)
}

// twoRecordings is the smallest corpus that clears D14's floor.
func twoRecordings() []recFixture {
	return []recFixture{
		recording1("a.txt", testNow.Add(-2*time.Hour)),
		recording1("b.txt", testNow.Add(-1*time.Hour)),
	}
}

// corpusKeyFor is the key a lift of the fixtures runs under.
func corpusKeyFor() corpusKey {
	return corpusKey{OwnerUserId: testOwner, GoalSignature: testSignature, Level: LevelAction}
}

// argsOf decodes a recorded call's arguments through the REAL parser.
func argsOf(t *testing.T, c recordedCall) map[string]any {
	t.Helper()
	return parseCallArgs(t, c.Query)
}

func mustLift(t *testing.T, i *Integration) LearnResult {
	t.Helper()
	res, err := i.learnAndPersist(context.Background(), corpusKeyFor())
	if err != nil {
		t.Fatalf("learnAndPersist: %v", err)
	}
	if !res.Accepted {
		t.Fatalf("the fixture corpus lifted nothing: %s", res.Reason)
	}
	return res
}

// storedConstruct is the row procedureConstructByName would answer after the
// lift the engine recorded: the payload and the hash it wrote.
func storedConstruct(t *testing.T, eng *fakeEngine, rung string) map[string]any {
	t.Helper()
	rp := argsOf(t, eng.callTo(t, "recordProcedure"))
	return map[string]any{
		"id":                  "v1:authoring:construct:c1",
		"bundleId":            "v1:authoring:bundle:b1",
		"name":                procedureName(corpusKeyFor()),
		"ladder":              rung,
		"goalSignature":       testSignature,
		"procedureHash":       rp["procedureHash"],
		"procedure":           rp["procedure"],
		"preconditions":       rp["preconditions"],
		"source":              rp["source"],
		"shadowMatches":       float64(3),
		"promotionApprovalId": "",
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
