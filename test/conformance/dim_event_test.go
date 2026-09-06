package conformance

// dim_event_test.go -- the event-trigger dimension (#1706). An event-triggered
// automation must bind the triggering event into nested step-arg scope, so
// event.payload.<field> resolves inside the steps. bootstrapSession is the
// vehicle: given a participant.created-shaped event it threads event.payload.id
// + event.payload.partitionId into the existence-check decide logic AND the
// session-create mutation step. Before #1706 the event was unresolved, so the
// created session carried no participant link and a read-back by that id came
// back empty.
//
// #2271 update: bootstrapSession's inline write was migrated out of the `logic`
// (it is now PURE -- it decides; the automation persists). So this dimension
// fires the bootstrapSession AUTOMATION (the real event path) instead of calling
// the logic directly. The event-binding contract is unchanged and still the
// right end-to-end check: driving the automation must still create a session
// keyed by the event's participant id.

import (
	"testing"

	"github.com/znasllc-io/memql/component/events"
)

func eventTriggerCheck() check {
	return check{
		Issue:   "#1706",
		Dim:     "event-context-binding",
		NeedsDB: true,
		Run:     runEventTrigger,
	}
}

func runEventTrigger(t *testing.T, e *Env) {
	noteId := "v1:notes:note:evt-" + uniqueSuffix("evt")
	title := "evt-title-" + uniqueSuffix("evt")

	// Fire indexNoteOnCreate with a representative note.created event. Its one
	// step is a nested createArtifact whose every argument is bound from the
	// event payload (id, title, body, ownerUserId), so an artifact carrying
	// THIS title proves the payload threaded into the nested step's scope --
	// an unbound arg would write the "Untitled note" default instead.
	//
	// Chosen because it is builtin-free: the vehicle must not depend on a Go
	// integration the conformance harness does not register. It replaced
	// bootstrapSession, which asserted the identical property over a cognition
	// participant and went with that concept (epic memql#4988).
	fireForgeAutomation(t, e, "indexNoteOnCreate", "graph.node.created.v1:notes:note",
		events.KindNodeCreated, map[string]any{
			"id":          noteId,
			"title":       title,
			"body":        "body-" + noteId,
			"ownerUserId": ownerUID,
		})

	rows := asArray(t, e.runQuery(t, "libraryArtifacts", map[string]any{}))
	for _, r := range rows {
		row, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if row["title"] == title {
			t.Logf("#1706: event-triggered automation bound event.payload into nested step scope (artifact titled %q)", title)
			return
		}
	}
	t.Fatalf("#1706: no library artifact titled %q among %d rows -- event.payload.title did not thread "+
		"into the nested createArtifact step (an unbound arg writes the \"Untitled note\" default)", title, len(rows))
}
