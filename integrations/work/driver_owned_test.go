package work

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/component/workjournal"
)

// driver_owned_test.go -- runs a Go DRIVER writes and runs itself, which the
// dispatcher must never adopt.
//
// Two drivers write such runs: component/workjournal (the Library's analysis
// pass, and the D7 delegate's child run) and SessionWriter.OpenRecording (an
// app session's recording). Both used to write triggeredBy "system" beside a
// goalId and an automationName that is a template WORD, not an automation --
// which is exactly the shape the dispatcher takes for compiled goal work. So
// every agent replica claimed each one about a millisecond after it opened,
// loadWorkTemplate could not find "appSession" or "libraryAnalyzeFile", and
// FailRun wrote automation_not_runnable over whatever the driver had already
// recorded. Measured on the cluster: 13 of 13 such runs.
//
// TO CONFIRM THESE ARE LOAD-BEARING: drop the journal prefix from
// isDriverOwnedRun, or write "system" again in either driver, and they fail.

func journalEvent(id, template, triggeredBy string) events.Event {
	ev := runEvent(id, runStatusRunning, template, "u1")
	ev.Payload["payload"].(map[string]any)["triggeredBy"] = triggeredBy
	return ev
}

// settledClaims waits for HandleRunEvent's goroutine and reports the claims.
func settledClaims(c *stubClaimer, want int) int {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		n := len(c.keys)
		c.mu.Unlock()
		if n >= want && want > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.keys)
}

func TestHandleRunEventSkipsJournalRuns(t *testing.T) {
	for _, template := range []string{"appSession", "libraryAnalyzeFile"} {
		t.Run(template, func(t *testing.T) {
			i, d, c := newDispatchProbe(t, true)
			i.HandleRunEvent(journalEvent("v1:work:run:journal-"+template, template, workjournal.TriggeredBy(template)))
			if got := settledClaims(c, 0); got != 0 || len(d.seen()) != 0 {
				t.Fatalf("claimed %d and dispatched %+v for a %s journal run; its driver owns it, and the "+
					"template executor would fail it automation_not_runnable", got, d.seen(), template)
			}
		})
	}

	// THE CONTROL is the shape these runs used to have. Without it, a
	// dispatcher that took nothing would pass.
	i, _, c := newDispatchProbe(t, true)
	i.HandleRunEvent(journalEvent("v1:work:run:old-shape", "appSession", "system"))
	if got := settledClaims(c, 1); got != 1 {
		t.Fatalf("the control: a goal-backed running run naming an automation was claimed %d time(s), want 1", got)
	}
}

func TestCanDispatchStoredRunRefusesJournal(t *testing.T) {
	now := time.Now()
	for _, recovery := range []bool{false, true} {
		req := DispatchRequest{RunId: "r", Status: runStatusRunning, Recovery: recovery, TriggeredBy: workjournal.TriggeredBy("appSession")}
		if req.CanDispatchStoredRun("v1:work:goal:g", runStatusRunning, nil, now) {
			t.Errorf("recovery=%v: a journal run was admitted behind the privileged read", recovery)
		}
	}
	control := DispatchRequest{RunId: "r", Status: runStatusRunning, TriggeredBy: "schedule"}
	if !control.CanDispatchStoredRun("v1:work:goal:g", runStatusRunning, nil, now) {
		t.Fatal("the control: a compiled goal run is no longer admitted")
	}
}

func TestRedispatchStaleRefusesJournal(t *testing.T) {
	i, d, c := newDispatchProbe(t, true)
	run := map[string]any{
		"id": "v1:work:run:silent-journal", "ownerUserId": "u1", "status": runStatusRunning,
		"goalId": "v1:work:goal:g", "automationName": "libraryAnalyzeFile",
		"triggeredBy": workjournal.TriggeredBy("libraryAnalyzeFile"),
	}
	if i.redispatchStale(context.Background(), run, "v1:work:run:silent-journal", "u1") {
		t.Fatal("the backstop handed a silent journal run to the dispatcher")
	}
	if len(c.keys) != 0 || len(d.seen()) != 0 {
		t.Fatalf("claimed %v and dispatched %+v; a journal run's claim is never taken", c.keys, d.seen())
	}
}

// TestSweepLeavesAJournalRunToItsDriver is the sweep's half: a journal that
// beats is left alone, a silent one is closed by its heartbeat -- the one
// judgment made of every run -- and never offered to the dispatcher.
func TestSweepLeavesAJournalRunToItsDriver(t *testing.T) {
	now := testNow
	stale := now.Add(-10 * time.Minute).Format(time.RFC3339)
	fresh := now.Add(-5 * time.Second).Format(time.RFC3339)
	journal := func(id, heartbeatAt string) map[string]any {
		return map[string]any{
			"id": id, "ownerUserId": "u-alice", "status": runStatusRunning, "goalId": "v1:work:goal:g",
			"automationName": "appSession", "triggeredBy": workjournal.TriggeredBy("appSession"),
			"heartbeatAt": heartbeatAt,
		}
	}

	i, eng := newTestIntegration(t)
	d, c := &capturingDispatcher{}, &stubClaimer{grant: true}
	i.SetDispatcher(d)
	i.SetRunClaimer(c)
	res := sweepRows(context.Background(), i, []map[string]any{
		journal("v1:work:run:journal-silent", stale),
		journal("v1:work:run:journal-beating", fresh),
		// THE CONTROL: an ordinary silent run is still handed back.
		{
			"id": "v1:work:run:ordinary", "ownerUserId": "u-bob", "status": runStatusRunning,
			"automationName": "invokeAgent", "triggeredBy": "schedule", "heartbeatAt": stale,
		},
	}, now, time.Minute)

	if got := d.seen(); len(got) != 1 || got[0].RunId != "v1:work:run:ordinary" {
		t.Fatalf("dispatched %+v, want only the ordinary run", got)
	}
	if res.Redispatched != 1 || res.Abandoned != 1 {
		t.Errorf("sweep = %+v, want one redispatched and one abandoned", res)
	}
	writes := map[string]map[string]any{}
	for _, call := range eng.callsTo("updateWorkRun") {
		args := call.Args(t)
		writes[args["runId"].(string)] = args
	}
	if w, written := writes["v1:work:run:journal-beating"]; written {
		t.Errorf("a beating journal run was written %v", w)
	}
	if w := writes["v1:work:run:journal-silent"]; w["status"] != runStatusAbandoned {
		t.Errorf("the silent journal run was written %v, want it closed abandoned by its heartbeat", w)
	}
}

// TestJournalRunsAreDriverOwnedAsWritten is the SPELLING PIN. The dispatcher
// recognises a journal by a prefix, and two drivers write it; a test that
// hand-wrote the prefix on both sides would stay green after either driver
// changed its spelling. So each driver writes its run against a capturing
// engine, and the row it wrote goes through the SAME event decoding the
// subscriber uses.
func TestJournalRunsAreDriverOwnedAsWritten(t *testing.T) {
	var journalCalls []string
	j := workjournal.New(workjournal.ExecutorFunc(func(_ context.Context, q string) (any, error) {
		journalCalls = append(journalCalls, q)
		return nil, nil
	}), nil, "node-1")
	if _, err := j.Begin(context.Background(), workjournal.Work{
		OwnerUserID: "u1", Template: "libraryAnalyzeFile", Statement: "Analyze notes.pdf", GoalKey: "v1:library:file:f1",
	}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	var journalRun recordedCall
	for _, q := range journalCalls {
		if strings.HasPrefix(q, "mutation createWorkRun(") {
			journalRun = recordedCall{Query: q}
		}
	}
	if journalRun.Query == "" {
		t.Fatalf("workjournal wrote no run: %v", journalCalls)
	}

	w, eng := newSessionWriter(t)
	if _, err := w.OpenRecording(context.Background(), workerservice.RecordingOpen{
		SessionId: testSessionId, OwnerUserId: testOwner, App: "claude-code", Prompt: "do the thing",
	}); err != nil {
		t.Fatalf("OpenRecording: %v", err)
	}
	recordingRun := eng.callTo(t, "createWorkRun")

	for name, call := range map[string]recordedCall{"workjournal.Begin": journalRun, "SessionWriter.OpenRecording": recordingRun} {
		t.Run(name, func(t *testing.T) {
			args := call.Args(t)
			payload := map[string]any{"ownerUserId": "u1"}
			for k, v := range args {
				payload[k] = v
			}
			req, ok := runEventFields(events.Event{Payload: map[string]any{"id": args["runId"], "payload": payload}})
			if !ok {
				t.Fatalf("the written run did not decode as a run event: %v", args)
			}
			if req.GoalId == "" || req.AutomationName == "" || req.Status != runStatusRunning {
				t.Fatalf("decoded %+v -- this is not the shape that was adopted, so the pin checks nothing", req)
			}
			i, d, c := newDispatchProbe(t, true)
			if i.dispatchRun(context.Background(), req) || len(c.keys) != 0 || len(d.seen()) != 0 {
				t.Fatalf("%s's run (triggeredBy %q) was claimed; the dispatcher does not recognise its spelling", name, req.TriggeredBy)
			}
		})
	}
}

// TestFailRunDoesNotOverwriteTerminal: the dispatcher reads the run, spends
// about a second loading a template, and fails the run -- while the run's own
// driver closed it in between. The write was unconditional, so the driver's
// true failure (app_session_failed, with the machine's reason) was replaced by
// "automation not found", and a success kept a failure code.
func TestFailRunDoesNotOverwriteTerminal(t *testing.T) {
	for _, status := range []string{runStatusSucceeded, runStatusFailed, runStatusCancelled, runStatusAbandoned} {
		t.Run(status, func(t *testing.T) {
			eng := newRecordingEngine()
			eng.reply("workRunForOwner", map[string]any{
				"id": "run-1", "status": status, "errorCode": "app_session_failed",
			})
			i := New(eng, nil)
			err := i.FailRun(context.Background(), "v1:identity:user:u1", "run-1", "automation_not_runnable", `automation "appSession" not found`)
			if !errors.Is(err, ErrRunAlreadyClosed) {
				t.Errorf("FailRun on a %s run returned %v, want ErrRunAlreadyClosed so the caller can say so", status, err)
			}
			if calls := eng.callsTo("updateWorkRun"); len(calls) != 0 {
				t.Fatalf("FailRun overwrote a %s run: %s", status, calls[0].Query)
			}
		})
	}

	// THE CONTROL: every status a run can still be failed from IS failed,
	// waiting included -- a waiting inference-retry run whose template cannot
	// load would otherwise stay parked and be redispatched every lease.
	for _, status := range []string{runStatusRunning, runStatusCompiling, runStatusWaiting} {
		t.Run(status, func(t *testing.T) {
			eng := newRecordingEngine()
			eng.reply("workRunForOwner", map[string]any{"id": "run-1", "status": status})
			i := New(eng, nil)
			if err := i.FailRun(context.Background(), "v1:identity:user:u1", "run-1", "automation_not_runnable", "x"); err != nil {
				t.Fatalf("FailRun on a %s run: %v", status, err)
			}
			if got := eng.callTo(t, "updateWorkRun").Args(t); got["status"] != "failed" || got["errorCode"] != "automation_not_runnable" {
				t.Errorf("FailRun on a %s run wrote %v, want it failed", status, got)
			}
		})
	}
}

// TestFailRunReadsUnderTheOwner: the status check is a read, and an owned run
// reads as absent to anybody but its owner -- so a check made under any other
// actor would find nothing and wave every overwrite through.
func TestFailRunReadsUnderTheOwner(t *testing.T) {
	eng := newRecordingEngine()
	eng.reply("workRunForOwner", map[string]any{"id": "run-1", "status": runStatusRunning})
	i := New(eng, nil)
	if err := i.FailRun(context.Background(), "v1:identity:user:u1", "run-1", "x", "y"); err != nil {
		t.Fatal(err)
	}
	if read := eng.callTo(t, "workRunForOwner"); read.Actor != "v1:identity:user:u1" {
		t.Errorf("the status was read as %q, want the run's owner", read.Actor)
	}
}
