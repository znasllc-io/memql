package library

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/znasllc-io/memql/component/memql"
)

// Memory-search sources owned by the Library integration (the rebuild recipes
// a memory-search model switch walks; see component/memql/memory_sources.go).
// Each must produce the text its live writer embeds.

// activeTrainingReader is the reader the lesson source pages through. The
// source registers from init() (a registration is process-wide and refuses a
// duplicate), while the database arrives with the plug-in; the factory points
// this at the live reader. Unset, the source reports itself unavailable
// instead of silently yielding no lessons, which would let a switch finish
// with every lesson missing from the new index.
var activeTrainingReader atomic.Pointer[trainingReader]

func init() {
	// analysis.go (libraryAnalysisEmbed -> embedChunk) embeds the chunk's text
	// as given, and createLibraryFileChunk stores that same text, so the row's
	// `text` is the embedded text.
	memql.RegisterMemorySource(memql.MemorySource{
		Name: "libraryFileChunk", Concept: conceptFileChunk, VectorField: "content",
		Rows: func(p map[string]any) (string, bool) {
			text, _ := p["text"].(string)
			return text, strings.TrimSpace(text) != ""
		},
	})
	memql.RegisterMemorySource(memql.MemorySource{
		Name: "trainingLesson", Concept: "v1:work:observation", VectorField: "content",
		Items: trainingLessonItems,
	})
}

// lessonEvent is one stored lesson event: its row id and its payload.
type lessonEvent struct {
	ID      string
	Payload []byte
}

// lessonHeadItems turns lesson events into the items to index.
//
// Every lesson revision and decision is its own immutable observation, written
// by AppendTrainingLessonEvent with a label as content. handleIndexTrainingLesson
// later embeds lessonPassage under the observation id "v1:work:observation:"+
// receipt of ONE of them: the current head (highest sequence of its lesson),
// only if enabled and reviewed. Older events, and heads that are disabled or
// not reviewed, carry no passage vector, so a rebuild must not give them one.
// The workObservation source skips every lesson event for the same reason.
//
// An event that carries a lesson which does not read as one (it fails to
// decode, or lacks its schema version, id or sequence) comes back as an item
// with Failure set, keyed by the event's own id, and so does a current head
// with no receipt to key its vector by: neither can be rebuilt, and the index
// job must count them rather than let a lesson vanish from search. A row with
// no lesson at all is not a lesson event and is not reported.
func lessonHeadItems(events []lessonEvent) []memql.MemoryItem {
	type head struct {
		lesson trainingLesson
		event  string
	}
	heads := map[string]head{}
	var items []memql.MemoryItem
	for _, ev := range events {
		var row map[string]any
		if err := json.Unmarshal(ev.Payload, &row); err != nil {
			items = append(items, memql.MemoryItem{Id: ev.ID, Failure: "the lesson event does not decode: " + err.Error()})
			continue
		}
		if data, _ := row["data"].(map[string]any); data == nil || data["trainingLesson"] == nil {
			continue
		}
		l, err := lessonFromRow(row)
		if err != nil {
			items = append(items, memql.MemoryItem{Id: ev.ID, Failure: "the lesson event does not read as a lesson: " + err.Error()})
			continue
		}
		// A lesson belongs to one owner; the key keeps two owners' ids apart.
		key := fmt.Sprint(row["ownerUserId"]) + "\x00" + l.ID
		if cur, ok := heads[key]; !ok || l.Sequence > cur.lesson.Sequence {
			heads[key] = head{lesson: l, event: ev.ID}
		}
	}
	for _, h := range heads {
		l := h.lesson
		if !l.Enabled || l.Review != "reviewed" {
			continue
		}
		if l.Receipt == "" {
			items = append(items, memql.MemoryItem{Id: h.event, Failure: "the current lesson head has no receipt to key its passage by"})
			continue
		}
		items = append(items, memql.MemoryItem{Id: "v1:work:observation:" + l.Receipt, Text: lessonPassage(l)})
	}
	sort.Slice(items, func(a, b int) bool { return items[a].Id < items[b].Id })
	return items
}

// pageMemoryItems returns up to limit of the id-sorted items after `after`,
// and the id to resume after ("" when none remain).
func pageMemoryItems(items []memql.MemoryItem, after string, limit int) ([]memql.MemoryItem, string) {
	start := sort.Search(len(items), func(n int) bool { return items[n].Id > after })
	items = items[start:]
	if len(items) <= limit {
		return items, ""
	}
	return items[:limit], items[limit-1].Id
}

// lessonEventsSQL reads every lesson event's newest version. Events are
// immutable, so there is one version each; the collapse is only for safety.
//
// staged-data: MUST-NOT-GATE -- this feeds the WRITE side of the index (a
// rebuild), not a reader. A lesson's passage was embedded when it was indexed
// whether or not its concept was staged; hiding rows here would drop it from
// the rebuilt index.
const lessonEventsSQL = `SELECT DISTINCT ON (id) id, payload FROM "MemoryNodes"
 WHERE concept = 'v1:work:observation' AND payload->'data'->'trainingLesson' IS NOT NULL
 ORDER BY id, "createdAt" DESC`

// trainingLessonItems pages the lessons to index. Lessons are few (a person
// writes each by hand, at most 100 revisions apiece), so each call reads the
// events and pages the resulting heads in memory rather than keeping a cursor
// over a grouped result.
func trainingLessonItems(ctx context.Context, after string, limit int) ([]memql.MemoryItem, string, error) {
	r := activeTrainingReader.Load()
	if r == nil || r.db == nil || r.db() == nil {
		return nil, "", fmt.Errorf("Training lessons cannot be read: the Library plug-in has no database")
	}
	rows, err := r.db().QueryContext(ctx, lessonEventsSQL)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var events []lessonEvent
	for rows.Next() {
		var ev lessonEvent
		if err := rows.Scan(&ev.ID, &ev.Payload); err != nil {
			return nil, "", err
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	items, next := pageMemoryItems(lessonHeadItems(events), after, limit)
	return items, next, nil
}
