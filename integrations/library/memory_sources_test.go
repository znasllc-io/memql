package library

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/memql"
)

func librarySource(t *testing.T, name string) memql.MemorySource {
	t.Helper()
	for _, s := range memql.MemorySources() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no memory source %q", name)
	return memql.MemorySource{}
}

// analysis.go libraryAnalysisEmbed passes the chunk text through verbatim, and
// the chunk row stores that same text, so the row's text IS the embedded text.
func TestTheFileChunkRecipeIsTheRowText(t *testing.T) {
	s := librarySource(t, "libraryFileChunk")
	require.Equal(t, "v1:library:fileChunk", s.Concept)
	require.Equal(t, "content", s.VectorField)
	got, ok := s.Rows(map[string]any{"text": "  a passage\nwith lines "})
	require.True(t, ok)
	require.Equal(t, "  a passage\nwith lines ", got)
	_, ok = s.Rows(map[string]any{"text": " "})
	require.False(t, ok)
}

func lessonRow(t *testing.T, owner string, l trainingLesson) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"ownerUserId": owner, "content": "Manual Training lesson: " + l.Action + " (" + l.Draft.Title + ")",
		"data": map[string]any{"trainingLesson": l},
	})
	require.NoError(t, err)
	return raw
}

func TestOnlyTheCurrentEnabledReviewedLessonHeadIsRebuilt(t *testing.T) {
	d := lessonDraft{Title: "T", Text: "Do X", Applies: "when A", Exceptions: "unless B"}
	mk := func(id, receipt string, seq int, enabled bool, review string) trainingLesson {
		return trainingLesson{SchemaVersion: 1, ID: id, Receipt: receipt, Sequence: seq, Draft: d, Enabled: enabled, Review: review, Action: "approve"}
	}
	events := []lessonEvent{
		{ID: "v1:work:observation:e1", Payload: lessonRow(t, "u1", mk("a", "r1", 1, true, "reviewed"))}, // superseded by r2
		{ID: "v1:work:observation:e2", Payload: lessonRow(t, "u1", mk("a", "r2", 2, true, "reviewed"))}, // head
		{ID: "v1:work:observation:e3", Payload: lessonRow(t, "u1", mk("b", "r3", 1, true, "reviewed"))},
		{ID: "v1:work:observation:e4", Payload: lessonRow(t, "u1", mk("b", "r4", 2, false, "reviewed"))}, // head disabled
		{ID: "v1:work:observation:e5", Payload: lessonRow(t, "u1", mk("c", "r5", 1, true, "pending"))},   // head not reviewed
		{ID: "v1:work:observation:e6", Payload: []byte(`{"content":"plain","data":{}}`)},                 // not a lesson event
	}
	items := lessonHeadItems(events)
	require.Equal(t, []memql.MemoryItem{{
		Id:   "v1:work:observation:r2",
		Text: lessonPassage(mk("a", "r2", 2, true, "reviewed")),
	}}, items)
	// Same text the live writer (handleIndexTrainingLesson) embeds.
	require.Equal(t, "Do X\nApplies when: when A\nExceptions: unless B", items[0].Text)
}

// A lesson event the rebuild cannot read, and a head with no receipt to key its
// passage by, come back as FAILED items under the event's own id: the index job
// counts them, so a lesson cannot drop out of search unnoticed.
func TestALessonTheRebuildCannotReadIsCountedNotDropped(t *testing.T) {
	d := lessonDraft{Title: "T", Text: "Do X", Applies: "when A", Exceptions: "unless B"}
	items := lessonHeadItems([]lessonEvent{
		{ID: "v1:work:observation:bad", Payload: []byte(`{"content":"Manual Training lesson","data":{"trainingLesson":{"schemaVersion":1}}}`)},
		{ID: "v1:work:observation:nr", Payload: lessonRow(t, "u1", trainingLesson{SchemaVersion: 1, ID: "n", Sequence: 1, Draft: d, Enabled: true, Review: "reviewed"})},
		{ID: "v1:work:observation:ok", Payload: lessonRow(t, "u1", trainingLesson{SchemaVersion: 1, ID: "o", Receipt: "r9", Sequence: 1, Draft: d, Enabled: true, Review: "reviewed"})},
	})
	require.Len(t, items, 3)
	byID := map[string]memql.MemoryItem{}
	for _, item := range items {
		byID[item.Id] = item
	}
	require.Contains(t, byID["v1:work:observation:bad"].Failure, "does not read as a lesson")
	require.Empty(t, byID["v1:work:observation:bad"].Text)
	require.Contains(t, byID["v1:work:observation:nr"].Failure, "no receipt")
	require.Empty(t, byID["v1:work:observation:r9"].Failure)
	require.Equal(t, "Do X\nApplies when: when A\nExceptions: unless B", byID["v1:work:observation:r9"].Text)
}

func TestLessonItemsPageInIdOrder(t *testing.T) {
	items := []memql.MemoryItem{{Id: "a"}, {Id: "b"}, {Id: "c"}}
	page, next := pageMemoryItems(items, "", 2)
	require.Equal(t, []string{"a", "b"}, []string{page[0].Id, page[1].Id})
	require.Equal(t, "b", next)
	page, next = pageMemoryItems(items, next, 2)
	require.Equal(t, []string{"c"}, []string{page[0].Id})
	require.Empty(t, next)
}

func TestTheLessonSourceIsRegistered(t *testing.T) {
	s := librarySource(t, "trainingLesson")
	require.NotNil(t, s.Items)
	require.Nil(t, s.Rows)
	require.Equal(t, "v1:work:observation", s.Concept)
}
