package conformance

// conf_2235_test.go -- the Library index promotion dimension (#2235 / #2212).
//
// The Library index automations (indexMemory / indexNote / indexTodo /
// indexCalendarEvent / indexGeneratedOutput) fold a backing-row node.created
// event into a v1:library:artifact index row via an EVENT-BOUND createArtifact
// step (the logic-purity burn-down; the former promotion logics are gone).
//
// This path shipped with ZERO conformance coverage, which is exactly why two
// regressions slipped to a real-DB runtime undetected:
//   - #2238: the decide->persist `field(decide.result, "x")` form resolved to
//     nil (logic step result is a Bundle-wrapped envelope), so createArtifact
//     got all-missing args.
//   - #2265: the event-bound `sourceConceptRef: canonicalId(event.payload.id,
//     <concept>)` form -- canonicalId with a concept-ref arg does NOT evaluate
//     in a top-level step arg, so sourceConceptRef came back nil and
//     createArtifact failed with "required argument sourceConceptRef is missing".
// Both broke Library promotion silently. The working form binds
// `sourceConceptRef: event.payload.id` directly (a node.created payload.id is
// already the stored canonical concept id).
//
// This dimension fires a REAL node.created event for each backing concept
// through the REAL index automation (loaded from the embedded DSL, run on a real
// Executor against a seeded DB) and asserts the v1:library:artifact landed with
// the right kind AND the right sourceConceptRef (the source row's canonical id).
// It FAILS on either broken form and PASSES on the event.payload.id-direct form.

import (
	"testing"

	"github.com/znasllc-io/memql/component/events"
)

func libraryIndexPromotionCheck() check {
	return check{
		Issue:   "#2235",
		Dim:     "library-index-promotion",
		NeedsDB: true,
		Run:     runLibraryIndexPromotion,
	}
}

// libraryIndexCase describes one backing-concept -> artifact promotion.
type libraryIndexCase struct {
	label      string
	createMut  string
	createArgs map[string]any
	byIdQuery  string
	byIdArgKey string
	idValue    string
	automation string
	wantKind   string
	wantLens   string
}

func runLibraryIndexPromotion(t *testing.T, e *Env) {
	suffix := uniqueSuffix("2235lib")

	cases := []libraryIndexCase{
		{
			label:     "memory",
			createMut: "createMemory",
			createArgs: map[string]any{
				"memoryId": "mem-" + suffix, "title": "kdim memory", "content": "body", "summary": "s",
			},
			byIdQuery: "memoryById", byIdArgKey: "memoryId", idValue: "mem-" + suffix,
			automation: "indexMemoryOnCreate", wantKind: "memory", wantLens: "record",
		},
		{
			label:     "note",
			createMut: "createNote",
			createArgs: map[string]any{
				"noteId": "note-" + suffix, "title": "kdim note", "body": "note body",
			},
			byIdQuery: "noteById", byIdArgKey: "noteId", idValue: "note-" + suffix,
			automation: "indexNoteOnCreate", wantKind: "note", wantLens: "record",
		},
		{
			label:     "todo",
			createMut: "createTodo",
			createArgs: map[string]any{
				"todoId": "todo-" + suffix, "title": "kdim todo",
			},
			byIdQuery: "todoById", byIdArgKey: "todoId", idValue: "todo-" + suffix,
			automation: "indexTodoOnCreate", wantKind: "todo", wantLens: "record",
		},
		{
			label:     "calendarEvent",
			createMut: "createCalendarEvent",
			createArgs: map[string]any{
				"eventId": "cal-" + suffix, "title": "kdim event", "startsAt": "2026-07-01T10:00:00Z",
			},
			byIdQuery: "calendarEventById", byIdArgKey: "eventId", idValue: "cal-" + suffix,
			automation: "indexCalendarEventOnCreate", wantKind: "calendar_event", wantLens: "record",
		},
		{
			label:     "generatedOutput",
			createMut: "createGeneratedOutput",
			createArgs: map[string]any{
				"outputId": "gout-" + suffix, "ownerUserId": ownerUID, "title": "kdim output",
				"source": "agent_generated", "format": "markdown", "summary": "out summary",
			},
			byIdQuery: "generatedOutputById", byIdArgKey: "outputId", idValue: "gout-" + suffix,
			automation: "indexGeneratedOutputOnCreate", wantKind: "generated_output", wantLens: "artifact",
		},
	}

	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			e.runMutation(t, c.createMut, c.createArgs)

			rows := asArray(t, e.runQuery(t, c.byIdQuery, map[string]any{c.byIdArgKey: c.idValue}))
			if len(rows) != 1 {
				t.Fatalf("#2235: %s(%q) returned %d rows, want 1; rows=%v", c.byIdQuery, c.idValue, len(rows), rows)
			}
			row, _ := rows[0].(map[string]any)
			if row == nil {
				t.Fatalf("#2235: %s row is not an object: %#v", c.byIdQuery, rows[0])
			}
			canonicalId := asStr(row["id"])
			if canonicalId == "" {
				t.Fatalf("#2235: %s row has no canonical id; row=%#v", c.byIdQuery, row)
			}

			// Fire the REAL index automation with a node.created event whose
			// payload IS the source row (mirrors the engine's CDC envelope).
			fireForgeAutomation(t, e, c.automation, "node.created", events.KindNodeCreated, cloneStringMap(row))

			// Assert the artifact landed, keyed by the source row's canonical id.
			arts := asArray(t, e.runQuery(t, "libraryArtifacts", map[string]any{}))
			var got map[string]any
			for _, a := range arts {
				m, _ := a.(map[string]any)
				if m != nil && asStr(m["sourceConceptRef"]) == canonicalId {
					got = m
					break
				}
			}
			if got == nil {
				t.Fatalf("#2235: %s did not promote a v1:library:artifact with sourceConceptRef=%q "+
					"(the canonicalId-in-step-arg / field(decide.result) regression -- createArtifact got nil args); artifacts=%v",
					c.automation, canonicalId, arts)
			}
			if k := asStr(got["kind"]); k != c.wantKind {
				t.Fatalf("#2235: %s artifact kind=%q, want %q", c.automation, k, c.wantKind)
			}
			if l := asStr(got["lens"]); l != c.wantLens {
				t.Fatalf("#2235: %s artifact lens=%q, want %q", c.automation, l, c.wantLens)
			}
			t.Logf("#2235: %s -> artifact OK (sourceConceptRef=%s kind=%s lens=%s)", c.label, canonicalId, c.wantKind, c.wantLens)
		})
	}
}
