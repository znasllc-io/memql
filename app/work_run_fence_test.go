package app

import (
	"github.com/znasllc-io/memql/component/automations"
	work "github.com/znasllc-io/memql/integrations/work"
	"testing"
	"time"
)

func TestExecutionHopFencesLiveStepAfterClaimLeaseExpires(t *testing.T) {
	now := time.Now()
	req := work.DispatchRequest{RunId: "r", GoalId: "g", Status: "running"}
	// The execution lease can be retaken after four minutes, while a model
	// still runs on another replica. Only the stored heartbeat/intent is current.
	j := &automations.RunJournal{RunId: "r", GoalId: "g", Status: "running", HasRunningStep: true, HeartbeatAt: now.Add(-15 * time.Second), FailedStep: "old-failure"}
	if workRunCanStart(req, j, now) {
		t.Fatal("second replica would re-enter a live step after lease expiry")
	}
	j.HasRunningStep = false
	if !workRunCanStart(req, j, now) {
		t.Fatal("failed-step retry was fenced despite no running intent")
	}
	j.HasRunningStep = true
	j.HeartbeatAt = now.Add(-5 * time.Minute)
	if !workRunCanStart(req, j, now) {
		t.Fatal("stale crashed step cannot be recovered")
	}
	j.Status = "succeeded"
	if workRunCanStart(req, j, now) {
		t.Fatal("terminal run admitted a stale event")
	}
}

// TestTheStoredJournalFencesAnIdOnlyEvent: an id-only run event carries no
// triggeredBy, so the subscriber passes it to this privileged read -- and the
// read is the one place left to recognise a run its driver owns. Asked of the
// REQUEST alone, a journal (the Library's analysis pass, an app session's
// recording) is admitted here, fails to load a template called
// "libraryAnalyzeFile", and is failed automation_not_runnable.
func TestTheStoredJournalFencesAnIdOnlyEvent(t *testing.T) {
	now := time.Now()
	idOnly := work.DispatchRequest{RunId: "r", Status: "running"}
	for _, stored := range []string{"journal:libraryAnalyzeFile", "journal:appSession", "procedure:canary"} {
		j := &automations.RunJournal{RunId: "r", GoalId: "g", Status: "running", TriggeredBy: stored}
		if workRunCanStart(idOnly, j, now) {
			t.Errorf("a stored %s run was admitted on an id-only event; its driver owns it", stored)
		}
	}
	// The control: a compiled goal run is still admitted on the same event.
	if !workRunCanStart(idOnly, &automations.RunJournal{RunId: "r", GoalId: "g", Status: "running", TriggeredBy: "api"}, now) {
		t.Fatal("the control: a compiled goal run is no longer admitted on an id-only event")
	}
}
