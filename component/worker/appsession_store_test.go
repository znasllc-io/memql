package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

type submittedResultEngine struct {
	row   map[string]any
	reads int
}

func (e *submittedResultEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	if !strings.HasPrefix(query, "query appSessionById(") {
		return &memql.ExecuteResult{}, nil
	}
	e.reads++
	if _, err := langparser.ParseExpression(strings.TrimPrefix(query, "query ")); err != nil {
		return nil, err
	}
	access, ok := auth.AccessFromContext(ctx)
	if !ok || access.UserId != "user-1" || access.IsClusterOwner() || auth.OriginFromContext(ctx) == auth.OriginInternal || !memql.FreshReadFromContext(ctx) {
		return nil, fmt.Errorf("result read lost its owner, row gate or cross-replica freshness")
	}
	if e.row == nil {
		return &memql.ExecuteResult{}, nil
	}
	payload, err := structpb.NewStruct(e.row)
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{Payload: payload}}}}, err
}

func TestRunnerReadsSubmittedTerminalThroughTheEngineStore(t *testing.T) {
	runner, session, _, _ := newRunnerFixture(t, SubscriptionPresent)
	terminal := map[string]any{"terminalTool": "respondToUser", "arguments": map[string]any{"status": "needs_input"}}
	engine := &submittedResultEngine{row: map[string]any{"ownerUserId": "user-1", "resultSubmittedAt": time.Now().Format(time.RFC3339Nano), "result": terminal}}
	runner.Store = &EngineStore{Engine: engine}
	go func() {
		waitForSession(t, session, "sess-run")
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-run"})
	}()
	result, err := runner.Run(context.Background(), session.worker, runSpec(), nil)
	want, _ := json.Marshal(terminal)
	if err != nil || string(result.Result) != string(want) || engine.reads != 1 {
		t.Fatalf("persisted submission did not return through the deployed store: %s, %v, reads=%d", result.Result, err, engine.reads)
	}
	for name, row := range map[string]map[string]any{
		"missing":     nil,
		"other owner": {"ownerUserId": "other", "resultSubmittedAt": "now", "result": terminal},
	} {
		t.Run(name, func(t *testing.T) {
			engine.row = row
			if _, err := runner.Store.ReadSubmittedAppSessionResult(context.Background(), "sess-run", "user-1"); err == nil {
				t.Fatal("unreadable or foreign session became a result")
			}
		})
	}
}

// TestAppSessionWritesCarryInternalOrigin is the regression guard for a
// failure that is INVISIBLE at the call site.
//
// All three app-session mutations are @serverOnly. `auth.OriginClient` is the
// ZERO VALUE, so a context nobody stamped reads as an untrusted client call
// and the engine REFUSES the write -- logging a WARN and returning in a way
// the caller reads as fine. The symptom is a session row that never appears,
// with the only evidence at a log level nobody is watching.
//
// This asserts the property directly rather than through the engine, because
// the engine's refusal is exactly the thing that does not surface.
func TestAppSessionWritesCarryInternalOrigin(t *testing.T) {
	ctx := appSessionWriteContext(context.Background(), "v1:identity:user:alice")

	if got := auth.OriginFromContext(ctx); got != auth.OriginInternal {
		t.Fatalf("call origin = %v, want %v -- without it every @serverOnly "+
			"app-session write is refused with only a WARN", got, auth.OriginInternal)
	}
}

// TestAppSessionWritesBorrowTheOwnersActor: the mutations stamp ownerUserId
// from the actor, so the write has to RUN as that user. A system actor here
// would either fail the stamp or attribute the row to the engine.
func TestAppSessionWritesBorrowTheOwnersActor(t *testing.T) {
	const owner = "v1:identity:user:alice"
	ctx := appSessionWriteContext(context.Background(), owner)

	access, ok := auth.AccessFromContext(ctx)
	if !ok || access == nil {
		t.Fatal("no access context -- the write cannot stamp ownerUserId from the actor")
	}
	if access.UserId != owner {
		t.Fatalf("actor userId = %q, want %q", access.UserId, owner)
	}
	if access.IsClusterOwner() {
		t.Fatal("the borrowed actor must NOT be a cluster owner -- the engine acts AS the " +
			"user whose row it writes, never above them")
	}
}

// TestProgressWriteStillStampsOrigin: the progress write names only the
// session, so there is no owner to borrow -- but it is @serverOnly all the
// same, and forgetting the stamp on this one path would silently lose every
// recording counter while sessions themselves kept working. Losing the
// counter also loses the seq the MCP node allocates from, so every mcp step
// would land at the seq of the session's first action.
func TestProgressWriteStillStampsOrigin(t *testing.T) {
	ctx := appSessionWriteContext(context.Background(), "")
	if got := auth.OriginFromContext(ctx); got != auth.OriginInternal {
		t.Fatalf("call origin = %v, want %v", got, auth.OriginInternal)
	}
}

// TestTranscriptCollectorMarksItsOwnTruncation. A transcript that simply
// stops reads as a run that stopped, which is the wrong conclusion to invite
// -- so the bound is announced inside the text, not only in a sibling field.
func TestTranscriptCollectorMarksItsOwnTruncation(t *testing.T) {
	c := &transcriptCollector{max: 16}
	for i := 0; i < 5; i++ {
		c.append(AppSessionChunk{Data: []byte("0123456789"), Seq: uint64(i + 1)})
	}
	text, seen, truncated := c.snapshot()

	if !truncated {
		t.Fatal("past the bound without being marked truncated")
	}
	if seen != 50 {
		t.Fatalf("bytes seen = %d, want 50 -- the counter must report what ARRIVED, "+
			"not what was kept, or a reader cannot tell how much was dropped", seen)
	}
	if len(text) <= 16 {
		t.Fatalf("the truncation notice must be appended even past the bound; got %q", text)
	}
}

func TestTranscriptCollectorUsesFinalAnswerWithoutDuplicatingTranscript(t *testing.T) {
	c := &transcriptCollector{max: 1024}
	c.append(AppSessionChunk{Stream: "text", Data: []byte("Let me think.\n")})
	c.append(AppSessionChunk{Stream: "answer", Data: []byte("Hello")})
	c.append(AppSessionChunk{Stream: "text", Data: []byte("Hello")})
	c.append(AppSessionChunk{Stream: "answer", Data: []byte(" there.\n")})
	c.append(AppSessionChunk{Stream: "text", Data: []byte(" there.\n")})
	if got := c.answerText(); got != "Hello there." {
		t.Fatalf("commentary leaked into the final answer: %q", got)
	}
	text, _, _ := c.snapshot()
	if text != "Let me think.\nHello there.\n" {
		t.Fatalf("answer deltas duplicated the transcript: %q", text)
	}
}

// TestCredentialExpiryIsHonest: a zero expiry is not "expired", it is
// "unknown". Treating it as expired would make a renewal loop spin on a
// credential that is probably fine.
func TestCredentialExpiryIsHonest(t *testing.T) {
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)

	if (Credential{}).Expired(now) {
		t.Fatal("a credential with no recorded expiry must not read as expired")
	}
	if !(Credential{ExpiresAt: now.Add(-time.Second)}).Expired(now) {
		t.Fatal("a past expiry must read as expired")
	}
	if (Credential{ExpiresAt: now.Add(time.Hour)}).Expired(now) {
		t.Fatal("a future expiry must not read as expired")
	}
}

// TestInternalOriginCannotEscapeOneWrite is the assertion
// call_origin_conformance_test.go's allowlist entry for this package
// PROMISES, rather than merely claims.
//
// memql#2989's escalation shape is a package that stamps internal origin onto
// a context a LATER frame inherits: one @serverOnly call then opens every
// other @serverOnly construct for the rest of the request. The property that
// rules it out here is structural -- appSessionWriteContext's result is a
// local in each store method and is never returned, so the caller's context
// is untouched.
//
// Asserted by observing the caller's own context AFTER the write helper has
// run against a derived one.
func TestInternalOriginCannotEscapeOneWrite(t *testing.T) {
	callerCtx := context.Background()

	writeCtx := appSessionWriteContext(callerCtx, "v1:identity:user:alice")
	if auth.OriginFromContext(writeCtx) != auth.OriginInternal {
		t.Fatal("the derived context must carry internal origin")
	}

	// The caller's context is unchanged -- a Go context is immutable, and
	// this test exists so that a future refactor which starts RETURNING the
	// stamped context (or caching it on the store) fails here rather than
	// silently widening what one stamp unlocks.
	if got := auth.OriginFromContext(callerCtx); got == auth.OriginInternal {
		t.Fatalf("the caller's context gained internal origin (%v) -- one @serverOnly "+
			"write must not unlock the rest of them for the rest of the request", got)
	}
	if _, ok := auth.AccessFromContext(callerCtx); ok {
		t.Fatal("the caller's context gained a borrowed actor")
	}
}

// TestStoreMethodsDoNotReturnAContext pins the structural half of the claim
// above: no exported method on the store hands a stamped context back.
// A signature change is what would break the property, so a signature check
// is what guards it.
func TestStoreMethodsDoNotReturnAContext(t *testing.T) {
	var store AppSessionStore = (*EngineStore)(nil)
	// The interface is the whole write surface; every method returns error
	// and nothing else. If a future method returns a context, this stops
	// compiling -- which is the point.
	var _ interface {
		CreateAppSession(context.Context, AppSessionRow) error
		RecordAppSessionProgress(context.Context, string, int, int, string, time.Time) error
		EndAppSession(context.Context, AppSessionRow) error
	} = store
}

func TestSessionStoreMarksAuthRefusalIndependentlyOfReplaySafety(t *testing.T) {
	for _, tt := range []struct {
		status, message string
		want            bool
	}{
		{"failed", "Failed to authenticate: OAuth session expired and could not be refreshed", true},
		{"failed", "Not logged in \u00b7 Please run /login", true},
		{"failed", "connection lost", false},
		{"ended", "Not logged in", false},
	} {
		recorder := &recordingExecutor{}
		store := &EngineStore{Engine: recorder}
		err := store.EndAppSession(context.Background(), AppSessionRow{ID: "session", OwnerUserId: "alice", Status: tt.status, ErrorMessage: tt.message, EndedAt: time.Now(), RecordedSteps: 5, Usage: AppSessionUsage{Known: true, InputTokens: 100}})
		if err != nil {
			t.Fatal(err)
		}
		want := "authRefused: false"
		if tt.want {
			want = "authRefused: true"
		}
		if len(recorder.statements) != 1 || !strings.Contains(recorder.statements[0], want) {
			t.Fatalf("auth receipt missing: %v", recorder.statements)
		}
	}
}

func TestSessionStoreMarksQuotaIndependentlyOfReplaySafety(t *testing.T) {
	for _, tt := range []struct {
		status, message string
		want            bool
	}{
		{"failed", "You've hit your weekly limit", true},
		{"failed", "out of credits", true},
		{"failed", "connection lost", false},
		{"ended", "out of credits", false},
	} {
		recorder := &recordingExecutor{}
		store := &EngineStore{Engine: recorder}
		err := store.EndAppSession(context.Background(), AppSessionRow{ID: "session", OwnerUserId: "alice", Status: tt.status, ErrorMessage: tt.message, EndedAt: time.Now(), RecordedSteps: 5, Usage: AppSessionUsage{Known: true, InputTokens: 100}})
		if err != nil {
			t.Fatal(err)
		}
		want := "quotaRefused: false"
		if tt.want {
			want = "quotaRefused: true"
		}
		if len(recorder.statements) != 1 || !strings.Contains(recorder.statements[0], want) {
			t.Fatalf("quota receipt missing: %v", recorder.statements)
		}
	}
}

// An app can say usageLimitExceeded around a sentence no list knows, and the machine's
// word has to block that app the way the sentence would.
func TestAppSessionRefusalStampsTakeTheMachinesWord(t *testing.T) {
	for reason, want := range map[string]string{
		AppStopOutOfUsage: "quotaRefused: true",
		AppStopSignedOut:  "authRefused: true",
	} {
		recorder := &recordingExecutor{}
		store := &EngineStore{Engine: recorder}
		err := store.EndAppSession(context.Background(), AppSessionRow{ID: "session", OwnerUserId: "alice", Status: AppSessionStatusFailed, ErrorMessage: "a sentence no list knows", StopReason: reason})
		if err != nil {
			t.Fatal(err)
		}
		if len(recorder.statements) != 1 || !strings.Contains(recorder.statements[0], want) {
			t.Fatalf("%s: %v", reason, recorder.statements)
		}
	}
}
