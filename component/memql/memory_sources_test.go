package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// A page of a Rows source's walk reads only what the walk uses: the id, the
// version and the payload. The full row carries schema and metadata as well,
// which nothing in the walk reads and which would cross the wire for every
// item of every walk, and the backfill walks the whole corpus every ten
// minutes. Pinned on the statement itself, which needs no database to build.
func TestTheSourceWalkReadsOnlyTheColumnsItUses(t *testing.T) {
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN("postgres://memql@127.0.0.1:1/none?sslmode=disable"))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	var nodes []memorynodes.MemoryNode
	statement := memorySourceRowsQuery(db, &nodes, "v1:test:item", "v1:test:item:007", 32).String()
	selected, _, found := strings.Cut(strings.TrimPrefix(statement, "SELECT "), " FROM ")
	require.True(t, found, statement)
	require.Equal(t, `DISTINCT ON (id) "mn"."id", "mn"."createdAt", "mn"."payload"`, selected)
	require.Contains(t, statement, `WHERE (concept = 'v1:test:item') AND (id > 'v1:test:item:007')`)
	require.Contains(t, statement, `ORDER BY id ASC, "createdAt" DESC LIMIT 32`)
}

func sourceByName(t *testing.T, name string) MemorySource {
	t.Helper()
	for _, s := range MemorySources() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no memory source %q", name)
	return MemorySource{}
}

// The sources this package registers cover its own indexed pairs. The two
// integrations' pairs are checked beside their recipes (library, knowledge)
// and all six together in app (TestEveryIndexedPairHasASource).
func TestTheEngineOwnedPairsHaveASource(t *testing.T) {
	have := map[[2]string]bool{}
	for _, s := range MemorySources() {
		have[[2]string{s.Concept, s.VectorField}] = true
	}
	for _, p := range [][2]string{
		{memorynodes.ConceptWorkObservation, "content"},    // executor_mutation.go embedWorkObservation
		{conversationEvidenceConcept, "content"},           // work_memory_index.go indexConversationEvidence
		{workAcknowledgementConcept, "content"},            // work_acknowledgement.go
		{memorynodes.ConceptAuthoringConstruct, "content"}, // authoring_catalog_promote.go embedCatalogConstruct
	} {
		require.True(t, have[p], "%v has no memory source", p)
	}
}

// Cases are the writer's own (TestWorkObservationIndexesMeaningInsteadOfArchiveLabels).
func TestTheObservationRecipeMatchesTheWriter(t *testing.T) {
	for _, tc := range []struct{ name, payload, want string }{
		{"continuation", `{"content":"Saved execution progress","data":{"continuation":true}}`, ""},
		{"receipt", `{"content":"Delegated tool source receipt","data":{"delegatedEvidence":true}}`, ""},
		{"provider admission", `{"content":"Reserved one provider inference attempt","data":{"providerAdmission":{"admittedCalls":1}}}`, ""},
		{"progress credit", `{"content":"Consumed native evidence","data":{"progressCredit":{"window":2}}}`, ""},
		{"execution", `{"content":"Execution progress: response streaming","data":{"execution":{"kind":"response"}}}`, ""},
		{"exact archive", `{"content":"Archived work context abc","data":{"contextHash":"abc","summary":""}}`, ""},
		{"semantic checkpoint", `{"content":"Archived work context abc","data":{"contextHash":"abc","summary":"Verified design guidance at revision 123"}}`, "Verified design guidance at revision 123"},
		{"finding", `{"content":"Verified source at revision 456","data":{}}`, "Verified source at revision 456"},
		{"blank", `{"content":"  "}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p map[string]any
			require.NoError(t, json.Unmarshal([]byte(tc.payload), &p))
			rows := sourceByName(t, "workObservation").Rows
			got, ok := rows(p)
			require.Equal(t, tc.want != "", ok)
			require.Equal(t, tc.want, got)

			// The writer embeds exactly what the recipe says.
			e := auditTestEngine(t)
			var indexed []string
			require.NoError(t, e.integrations.Register(&mockProvider{name: "embedding", capabilities: []IntegrationCapability{{Name: "store", Handler: func(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
				indexed = append(indexed, args["text"].(string))
				return nil, nil
			}}}}))
			e.embedWorkObservation(context.Background(), "observation", []byte(tc.payload))
			if tc.want == "" {
				require.Empty(t, indexed)
			} else {
				require.Equal(t, []string{tc.want}, indexed)
			}
		})
	}
}

func TestTheOtherRecipesMatchTheirWriters(t *testing.T) {
	text := func(name string, p map[string]any) (string, bool) {
		return sourceByName(t, name).Rows(p)
	}
	got, ok := text("conversationEvidence", map[string]any{"content": "User: hi\nAssistant (historical response, not independent evidence): yo"})
	require.True(t, ok)
	require.Equal(t, "User: hi\nAssistant (historical response, not independent evidence): yo", got)

	soon := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	got, ok = text("workAcknowledgement", map[string]any{"request": "find the report", "expiresAt": soon})
	require.True(t, ok)
	require.Equal(t, "find the report", got)
	_, ok = text("workAcknowledgement", map[string]any{"request": "find the report", "expiresAt": past})
	require.False(t, ok, "an expired acknowledgement is not rebuilt")
	long := make([]rune, 1201)
	for i := range long {
		long[i] = 'a'
	}
	_, ok = text("workAcknowledgement", map[string]any{"request": string(long), "expiresAt": soon})
	require.False(t, ok, "the writer never stores a statement over 1200 runes")
	_, ok = text("workAcknowledgement", map[string]any{"request": " ", "expiresAt": soon})
	require.False(t, ok)

	got, ok = text("authoringConstruct", map[string]any{"kind": "query", "catalogMatchText": "kind:query form:x"})
	require.True(t, ok)
	require.Equal(t, "kind:query form:x", got)
	_, ok = text("authoringConstruct", map[string]any{"kind": "automation", "catalogMatchText": "kind:automation"})
	require.False(t, ok, "automations are not catalogued")
	_, ok = text("authoringConstruct", map[string]any{"kind": "query"})
	require.False(t, ok, "an uncatalogued construct has no vector")
}

// The lesson source owns lesson ids; the observation recipe must not touch
// them (a lesson event row's content is only its label).
func TestLessonRowsComeFromTheLessonSourceOnly(t *testing.T) {
	var p map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{"content":"Manual Training lesson: approve (T)","data":{"trainingLesson":{"id":"l1","sequence":1}}}`), &p))
	_, ok := sourceByName(t, "workObservation").Rows(p)
	require.False(t, ok)

	e := auditTestEngine(t)
	var indexed []string
	require.NoError(t, e.integrations.Register(&mockProvider{name: "embedding", capabilities: []IntegrationCapability{{Name: "store", Handler: func(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		indexed = append(indexed, args["text"].(string))
		return nil, nil
	}}}}))
	raw, _ := json.Marshal(p)
	e.embedWorkObservation(context.Background(), "v1:work:observation:r1", raw)
	require.Empty(t, indexed, "the writer must not index the label the lesson index would overwrite")
}

func TestADuplicateSourceNameIsRefused(t *testing.T) {
	name := "dupTest"
	defer unregisterMemorySource(name)
	s := MemorySource{Name: name, Concept: "v1:x:y", VectorField: "content", Rows: func(map[string]any) (string, bool) { return "", false }}
	RegisterMemorySource(s)
	require.Panics(t, func() { RegisterMemorySource(s) })
	require.Panics(t, func() { RegisterMemorySource(MemorySource{Name: "none", Concept: "c", VectorField: "content"}) })
	names := []string{}
	for _, m := range MemorySources() {
		names = append(names, m.Name)
	}
	require.IsIncreasing(t, names)
}
