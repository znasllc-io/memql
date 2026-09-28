package conformance

// app_session_end_db_test.go -- the app-session row's terminal write, driven
// through the REAL Go writer (component/worker.EngineStore) against a real
// database.
//
// WHY HERE AND NOT BESIDE THE MUTATION. component/memql's
// appsession_persist_4360_db_test.go calls the mutations by hand, and every
// call it makes simply leaves `result` out -- so it passed for the whole life
// of the bug this file pins: the Go writer ALWAYS named `result`, rendered it
// as `null` when the session carried no structured answer, and the concept
// refused the whole terminal write. component/worker imports component/memql,
// so a test that drives the writer cannot live in that package; this suite
// already boots the whole tree over Postgres.
//
// Green-by-skip warning: without a database these skip. To verify for real:
//
//	MEMQL_DATABASE_DSN=... MEMQL_REQUIRE_DB=1 go test -count=1 \
//	  -run 'TestAppSession' ./test/conformance/

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// appSessionEnv is the rig, or a skip -- a failure under MEMQL_REQUIRE_DB=1.
func appSessionEnv(t *testing.T) *Env {
	t.Helper()
	env := newEnv(t)
	if !env.HasDB {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal("the app-session lifecycle needs Postgres, and MEMQL_REQUIRE_DB=1 makes its absence a failure")
		}
		t.Skip("the app-session lifecycle needs Postgres (MEMQL_DATABASE_DSN)")
	}
	return env
}

// openAppSession writes a fresh session row through the Go writer, as the
// runner does before AppSessionStart goes on the wire.
func openAppSession(t *testing.T, store *workerservice.EngineStore, owner, label string) string {
	t.Helper()
	sessionId := fmt.Sprintf("v1:worker:appSession:%s-%d", label, time.Now().UnixNano())
	if err := store.CreateAppSession(context.Background(), workerservice.AppSessionRow{
		ID:          sessionId,
		OwnerUserId: owner,
		WorkerId:    "v1:worker:registration:conformance-machine",
		App:         "claude-code",
		Kind:        workerservice.AppSessionKindRun,
		Status:      workerservice.AppSessionStatusStarting,
		Billing:     workerservice.BillingUnknown,
		StartedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("createAppSession through the Go writer: %v", err)
	}
	return sessionId
}

// appSessionRow reads the session back under its owner's actor, through the
// same query the Fleet page and the seq allocator read.
func appSessionRow(t *testing.T, env *Env, owner, sessionId string) map[string]any {
	t.Helper()
	res, err := env.Eng.Execute(auth.ContextWithUserActor(context.Background(), owner),
		"query appSessionById(sessionId: "+langparser.QuoteString(sessionId)+")")
	if err != nil {
		t.Fatalf("read %s back: %v", sessionId, err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 {
		t.Fatalf("read %s back: %d rows, want 1", sessionId, len(rows))
	}
	return rows[0]
}

func appSessionOwner(label string) string {
	return fmt.Sprintf("user-appsession-%s-%d", label, time.Now().UnixNano())
}

// TestAppSessionEndWithoutAResultReachesItsTerminalStatus is the regression:
// a session that ended with no structured answer -- a failed start, a
// disconnect, a free-text run -- must still leave `running`.
func TestAppSessionEndWithoutAResultReachesItsTerminalStatus(t *testing.T) {
	env := appSessionEnv(t)
	store := &workerservice.EngineStore{Engine: env.Eng}
	owner := appSessionOwner("end-failed")
	sessionId := openAppSession(t, store, owner, "end-failed")

	if err := store.EndAppSession(context.Background(), workerservice.AppSessionRow{
		ID:           sessionId,
		OwnerUserId:  owner,
		Status:       workerservice.AppSessionStatusFailed,
		Billing:      workerservice.BillingUnknown,
		ErrorMessage: "app session: no workspace in AppSessionStart",
		EndedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("the terminal write of a session with no structured answer was refused: %v", err)
	}

	row := appSessionRow(t, env, owner, sessionId)
	if row["status"] != workerservice.AppSessionStatusFailed {
		t.Fatalf("status = %v, want failed -- the row is left live forever otherwise", row["status"])
	}
	if row["errorMessage"] != "app session: no workspace in AppSessionStart" {
		t.Errorf("errorMessage = %v, want the machine's reason", row["errorMessage"])
	}
	if result, _ := row["result"].(map[string]any); len(result) != 0 {
		t.Errorf("result = %v, want the empty object the row opened with", row["result"])
	}
}

// TestAppSessionEndKeepsASubmittedResult is design D7's half of the same
// write: an app that submitted its answer over MCP mid-run, and a harness that
// had nothing to add at end. The end must not blank the answer.
func TestAppSessionEndKeepsASubmittedResult(t *testing.T) {
	env := appSessionEnv(t)
	store := &workerservice.EngineStore{Engine: env.Eng}
	owner := appSessionOwner("end-submitted")
	sessionId := openAppSession(t, store, owner, "end-submitted")

	// The `submit` MCP tool's write, as the app's own credential makes it:
	// caller-scoped, under the owning user's actor.
	submit := "mutation submitAppSessionResult(sessionId: " + langparser.QuoteString(sessionId) +
		`, result: {"answer": "forty-two"}, submittedAt: ` + langparser.QuoteString(time.Now().UTC().Format(time.RFC3339)) + ")"
	if _, err := env.Eng.Execute(auth.ContextWithUserActor(context.Background(), owner), submit); err != nil {
		t.Fatalf("submitAppSessionResult: %v", err)
	}

	if err := store.EndAppSession(context.Background(), workerservice.AppSessionRow{
		ID:          sessionId,
		OwnerUserId: owner,
		Status:      workerservice.AppSessionStatusEnded,
		Billing:     workerservice.BillingSubscription,
		EndedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("the terminal write was refused: %v", err)
	}

	row := appSessionRow(t, env, owner, sessionId)
	if row["status"] != workerservice.AppSessionStatusEnded {
		t.Fatalf("status = %v, want ended", row["status"])
	}
	if want := map[string]any{"answer": "forty-two"}; !reflect.DeepEqual(row["result"], want) {
		t.Fatalf("result = %#v, want the submitted %#v -- the end erased the app's answer", row["result"], want)
	}
}

// TestAStatuslessProgressWriteKeepsTheStatus: the MCP node's seq allocator
// (ClaimRecordingSlot) names no status, and it can land AFTER the session
// ended -- an app that calls `submit` and exits races its own end. A default
// of `running` on that write resurrected the terminal row.
func TestAStatuslessProgressWriteKeepsTheStatus(t *testing.T) {
	env := appSessionEnv(t)
	store := &workerservice.EngineStore{Engine: env.Eng}
	owner := appSessionOwner("progress")

	// A live row first: a status-less write must not promote `starting`
	// either -- `running` means the first chunk arrived, and a heartbeat is
	// not a chunk.
	// The progress write names only the session, so it runs under the actor
	// the caller already holds -- the runner's and the MCP node's contexts
	// both carry the owner's.
	asOwner := auth.ContextWithUserActor(context.Background(), owner)
	live := openAppSession(t, store, owner, "progress-live")
	if err := store.RecordAppSessionProgress(asOwner, live, -1, -1, "", time.Time{}); err != nil {
		t.Fatalf("status-less progress write: %v", err)
	}
	if got := appSessionRow(t, env, owner, live)["status"]; got != workerservice.AppSessionStatusStarting {
		t.Fatalf("a status-less progress write moved a starting row to %v", got)
	}

	ended := openAppSession(t, store, owner, "progress-ended")
	if err := store.EndAppSession(context.Background(), workerservice.AppSessionRow{
		ID: ended, OwnerUserId: owner, Status: workerservice.AppSessionStatusEnded,
		Billing: workerservice.BillingUnknown, EndedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("end: %v", err)
	}
	slot, err := store.ClaimRecordingSlot(asOwner, ended, owner)
	if err != nil {
		t.Fatalf("claim a recording slot after the end: %v", err)
	}
	row := appSessionRow(t, env, owner, ended)
	if row["status"] != workerservice.AppSessionStatusEnded {
		t.Fatalf("status = %v after a late MCP claim, want ended -- the claim resurrected a finished session", row["status"])
	}
	if got, _ := row["recordedSteps"].(float64); int(got) != slot.Seq+1 {
		t.Errorf("recordedSteps = %v, want %d: the claim must still advance the allocator", row["recordedSteps"], slot.Seq+1)
	}
}

// TestASweepEndThatLosesTheRaceKeepsTheHoldersAccount: workerAppSessionStale
// Sweep reads the open rows and then ends each one, and the holder can end the
// same row in between -- a replica that stalled past the grace and recovered
// in time to finish. The sweep's write names only what it knows (status,
// exitCode, errorMessage, endedAt), and it must write ONLY that: defaults for
// the rest overwrote the holder's usage, billing, transcript and produced
// artifacts with the empty values of a session nobody saw end, and the spend
// rollup reads those.
func TestASweepEndThatLosesTheRaceKeepsTheHoldersAccount(t *testing.T) {
	env := appSessionEnv(t)
	store := &workerservice.EngineStore{Engine: env.Eng}
	owner := appSessionOwner("sweep-race")
	sessionId := openAppSession(t, store, owner, "sweep-race")

	if err := store.EndAppSession(context.Background(), workerservice.AppSessionRow{
		ID:                  sessionId,
		OwnerUserId:         owner,
		Status:              workerservice.AppSessionStatusEnded,
		Usage:               workerservice.AppSessionUsage{InputTokens: 900, OutputTokens: 350, Known: true},
		Billing:             workerservice.BillingSubscription,
		TranscriptFileId:    "v1:library:file:transcript-sweep-race",
		ProducedArtifactIds: []string{"artifact-a"},
		AppSessionRef:       "cc-sweep-race",
		EndedAt:             time.Now().UTC(),
	}); err != nil {
		t.Fatalf("the holder's end: %v", err)
	}

	// The sweep's write, with exactly the arguments the automation names.
	sweep, err := langparser.RenderCall("endAppSession", map[string]any{
		"sessionId":    sessionId,
		"status":       workerservice.AppSessionStatusFailed,
		"exitCode":     -1,
		"errorMessage": "No agent replica is holding this session any more",
		"endedAt":      time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("render the sweep's end: %v", err)
	}
	if _, err := env.Eng.Execute(auth.ContextWithUserActor(auth.ContextWithInternalOrigin(context.Background()), owner),
		"mutation "+sweep); err != nil {
		t.Fatalf("the sweep's end: %v", err)
	}

	row := appSessionRow(t, env, owner, sessionId)
	if row["billing"] != workerservice.BillingSubscription {
		t.Errorf("billing = %v, want the holder's subscription -- the sweep's end defaulted it back to unknown", row["billing"])
	}
	usage, _ := row["usage"].(map[string]any)
	if usage["known"] != true || usage["inputTokens"] != float64(900) || usage["outputTokens"] != float64(350) {
		t.Errorf("usage = %v, want the holder's reported spend -- the sweep's end blanked it", row["usage"])
	}
	if row["transcriptFileId"] != "v1:library:file:transcript-sweep-race" {
		t.Errorf("transcriptFileId = %v, want the holder's transcript", row["transcriptFileId"])
	}
	if want := []any{"artifact-a"}; !reflect.DeepEqual(row["producedArtifactIds"], want) {
		t.Errorf("producedArtifactIds = %#v, want the holder's %#v", row["producedArtifactIds"], want)
	}
	if row["appSessionRef"] != "cc-sweep-race" {
		t.Errorf("appSessionRef = %v, want the holder's -- a later attach resumes by it", row["appSessionRef"])
	}
	// What the sweep DID name lands: the race costs the status, not the account.
	if row["status"] != workerservice.AppSessionStatusFailed {
		t.Errorf("status = %v, want the sweep's failed", row["status"])
	}
}
