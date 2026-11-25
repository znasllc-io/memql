package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// The memory-search index stores vectors and NO text, so switching the
// memory-search model (design 2026-11-23-memory-search-model-switch, section 5)
// has to rebuild each indexed item's text from its own row. A MemorySource is
// that rebuild recipe for one kind of indexed item.
//
// THE PROPERTY THAT MATTERS: for every source, the text rebuilt here must be
// EXACTLY the text the item's live writer embeds. Otherwise a switched index
// answers searches differently from the one it replaced. So the writers call
// the same recipe function a source uses (workObservationEmbedText below,
// KnowledgeChunkEmbedText in integrations/knowledge, the lesson passage in
// integrations/library), and a test per recipe holds the pair together.
//
// Readers join vectors on (id, vector_field) only, so an item's Id must be the
// id its writer passed as nodeId.
//
// Writers (every pair a switch must be able to rebuild):
//
//	v1:work:observation      executor_mutation.go embedWorkObservation
//	v1:work:observation      integrations/library/training_lessons.go handleIndexTrainingLesson
//	v1:memory:conversationEvidence  work_memory_index.go indexConversationEvidence
//	v1:memory:workAcknowledgement   work_acknowledgement.go
//	v1:authoring:construct   authoring_catalog_promote.go embedCatalogConstruct
//	v1:library:fileChunk     integrations/library/analysis.go embedChunk
//	v1:knowledge:documentChunk      integrations/knowledge (ingest, embed_domain, seeds)

// MemoryItem is one item to embed: the id its vector is keyed by and its text.
//
// Failure is set, instead of Text, for an item that exists but whose text
// cannot be rebuilt -- a row whose stored payload does not decode, a lesson
// event that does not read as a lesson. The index job counts it as failed and
// logs it, and never embeds it: an item a rebuild cannot produce must show in
// the switch's counts, never silently drop out of search (design record
// 2026-11-23, anti-goals).
//
// Version is when the row version the text was rebuilt from was written. The
// index job takes a vector older than it as missing: the row changed after it
// was embedded, a vector older than its row being as missing as an absent one.
// Zero for a row whose id is a hash of its own text
// (MemorySource.ContentAddressed), and for an item of a source that pages its
// own items, where the text under an id never changes.
type MemoryItem struct {
	Id, Text, Failure string
	Version           time.Time
}

// MemorySource says how to rebuild the text of one kind of indexed item.
// Exactly one of Rows and Items is set.
//
// Rows is a pure function of one row's latest payload; the engine walks the
// latest version of every row of Concept (memorySourceItems). A row is skipped
// when ok is false.
//
// Items is for a source whose item is not simply "a row": it pages through its
// own items in id order, after the given id, and returns the id to resume
// after ("" once exhausted). The Training lessons need it, because only the
// current head of each lesson is indexed, which no single row can say.
//
// ContentAddressed, when set, says of one row -- its canonical id and latest
// payload -- whether the id is a hash of the row's own text, so the text under
// it never changes. Such a row is written again without its text changing (a
// review stamp, a boot's re-seed), so a newer row version is NOT a reason to
// embed the item again, and the walk gives it no Version. It is asked of each
// row, never assumed of a source: a knowledge chunk ingested or seeded from a
// corpus hashes its text, while an LLM-seeded chunk's id names its recipe
// version and index, and a re-seed writes new text under it. Nil: no row is.
type MemorySource struct {
	Name, Concept, VectorField string
	Rows                       func(payload map[string]any) (text string, ok bool)
	Items                      func(ctx context.Context, after string, limit int) (items []MemoryItem, next string, err error)
	ContentAddressed           func(id string, payload map[string]any) bool
}

var memorySources = struct {
	sync.Mutex
	byName map[string]MemorySource
}{byName: map[string]MemorySource{}}

// RegisterMemorySource adds a source. A duplicate name is refused (it panics,
// like a duplicate registration at init time elsewhere in the engine): two
// recipes under one name would let one silently shadow the other, and the
// index would be rebuilt from whichever registered last.
func RegisterMemorySource(s MemorySource) {
	if s.Name == "" || s.Concept == "" || s.VectorField == "" {
		panic("memory source: name, concept and vector field are required")
	}
	if (s.Rows == nil) == (s.Items == nil) {
		panic(fmt.Sprintf("memory source %q: exactly one of Rows and Items must be set", s.Name))
	}
	memorySources.Lock()
	defer memorySources.Unlock()
	if _, dup := memorySources.byName[s.Name]; dup {
		panic(fmt.Sprintf("memory source %q is already registered", s.Name))
	}
	memorySources.byName[s.Name] = s
}

func unregisterMemorySource(name string) {
	memorySources.Lock()
	defer memorySources.Unlock()
	delete(memorySources.byName, name)
}

// MemorySources returns every registered source, sorted by name.
func MemorySources() []MemorySource {
	memorySources.Lock()
	defer memorySources.Unlock()
	out := make([]MemorySource, 0, len(memorySources.byName))
	for _, s := range memorySources.byName {
		out = append(out, s)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// workObservationEmbedText is the text a v1:work:observation is indexed under,
// and whether it is indexed at all. embedWorkObservation (the live writer)
// calls it, so a rebuilt index cannot disagree with the one it replaces.
//
//   - Accounting writes (providerAdmission, progressCredit) hold the
//     admission/renewal locks; embedding their labels would request another
//     provider admission while those locks are held. They carry no reusable
//     meaning either.
//   - execution, continuation and delegatedEvidence rows are administrative
//     labels ("Saved execution progress"); their exact receipts stay reachable
//     by run/source reference.
//   - A context archive (contextHash) is indexed by its SUMMARY; an exact-only
//     archive has no summary yet and is skipped rather than indexing the hash.
//   - A Training lesson event is skipped (data.trainingLesson). Its row's
//     content is only "Manual Training lesson: <action> (<title>)", written
//     under the SAME id the lesson index later overwrites with the lesson's
//     passage, and only the current enabled, reviewed head is ever indexed.
//     The trainingLesson source owns those ids; indexing the label here would
//     either duplicate or, on a rebuild, clobber the passage.
func workObservationEmbedText(payload map[string]any) (string, bool) {
	content, _ := payload["content"].(string)
	if data, ok := payload["data"].(map[string]any); ok {
		if data["trainingLesson"] != nil {
			return "", false
		}
		if data["providerAdmission"] != nil || data["progressCredit"] != nil {
			return "", false
		}
		if data["execution"] != nil || data["continuation"] == true || data["delegatedEvidence"] == true {
			return "", false
		}
		if data["contextHash"] != nil {
			content, _ = data["summary"].(string)
		}
	}
	if strings.TrimSpace(content) == "" {
		return "", false
	}
	return content, true
}

// memoryPayloadText returns a string field when it is not blank.
func memoryPayloadText(field string) func(map[string]any) (string, bool) {
	return func(p map[string]any) (string, bool) {
		s, _ := p[field].(string)
		return s, strings.TrimSpace(s) != ""
	}
}

// workAcknowledgementEmbedText mirrors work_acknowledgement.go: the text is the
// goal statement stored as `request`, never indexed when blank or over 1200
// runes (the writer returns before storing), and the row expires after 24 h so
// an expired one is not rebuilt into a new index.
func workAcknowledgementEmbedText(p map[string]any) (string, bool) {
	request, _ := p["request"].(string)
	if strings.TrimSpace(request) == "" || len([]rune(request)) > 1200 {
		return "", false
	}
	if raw, _ := p["expiresAt"].(string); raw != "" {
		if at, err := time.Parse(time.RFC3339Nano, raw); err == nil && !at.After(time.Now()) {
			return "", false
		}
	}
	return request, true
}

// authoringConstructEmbedText mirrors embedCatalogConstruct: the stored
// catalogMatchText of a catalogued construct. A construct that was never
// catalogued has none, and automations are not catalogued (their catalog key
// is refused outright), so neither has a vector to rebuild.
func authoringConstructEmbedText(p map[string]any) (string, bool) {
	if p["kind"] == "automation" {
		return "", false
	}
	return memoryPayloadText("catalogMatchText")(p)
}

func init() {
	for _, s := range []MemorySource{
		{Name: "workObservation", Concept: memorynodes.ConceptWorkObservation, VectorField: "content", Rows: workObservationEmbedText},
		{Name: "conversationEvidence", Concept: conversationEvidenceConcept, VectorField: "content", Rows: memoryPayloadText("content")},
		{Name: "workAcknowledgement", Concept: workAcknowledgementConcept, VectorField: "content", Rows: workAcknowledgementEmbedText},
		{Name: "authoringConstruct", Concept: memorynodes.ConceptAuthoringConstruct, VectorField: "content", Rows: authoringConstructEmbedText},
	} {
		RegisterMemorySource(s)
	}
}

// memorySourceRowsQuery is one page of a Rows source's walk: the newest version
// of each row of concept after cursor, in id order, reading ONLY the columns
// the walk uses -- id, createdAt (the item's Version) and payload. A full row
// also carries schema, metadata and provenance, which nothing in the walk
// reads, and every batch of every walk would carry them: the backfill walks
// the whole corpus every ten minutes.
func memorySourceRowsQuery(db *bun.DB, nodes *[]memorynodes.MemoryNode, concept, cursor string, limit int) *bun.SelectQuery {
	return db.NewSelect().Model(nodes).Column("id", "createdAt", "payload").DistinctOn("id").
		Where("concept = ?", concept).Where("id > ?", cursor).
		OrderExpr(`id ASC, "createdAt" DESC`).Limit(int64(limit))
}

// memorySourceItems returns up to limit items of a source with id > after, in
// id order, and the id to resume after ("" when the source is exhausted).
//
// A Rows source reads the LATEST version of each row (rows are append-only, so
// an older version would index text the row no longer has), under internal
// origin and with a fresh read: a rebuild runs on the cluster's behalf, not an
// actor's, and must not be answered from a replica's result cache. A row marked
// deleted is skipped, and so is one its recipe does not index. A skipped row
// still advances the cursor. A row whose payload does not decode is NOT
// skipped: it comes back as an item with Failure set, so the index job counts
// it instead of the row vanishing from search unnoticed.
//
// staged-data: MUST-NOT-GATE -- this feeds the WRITE side of the index, not a
// reader. The live writers embed a row the moment it is written, staged or not;
// hiding staged rows here would leave their vectors out of the rebuilt index,
// and an unindexed row stays unretrievable once its concept goes live.
func (e *MemQLEngine) memorySourceItems(ctx context.Context, s MemorySource, after string, limit int) ([]MemoryItem, string, error) {
	ctx = ContextWithFreshRead(auth.ContextWithInternalOrigin(ctx))
	if limit <= 0 {
		limit = 100
	}
	if s.Items != nil {
		return s.Items(ctx, after, limit)
	}
	db := e.database()
	if db == nil {
		return nil, "", fmt.Errorf("memory source %q: no database", s.Name)
	}
	var items []MemoryItem
	cursor := after
	for {
		var nodes []memorynodes.MemoryNode
		err := memorySourceRowsQuery(db, &nodes, s.Concept, cursor, limit).Scan(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("memory source %q: %w", s.Name, err)
		}
		for _, n := range nodes {
			cursor = n.ID
			var payload map[string]any
			if err := json.Unmarshal(n.Payload, &payload); err != nil || payload == nil {
				why := "its stored payload is not an object"
				if err != nil {
					why = "its stored payload does not decode: " + err.Error()
				}
				items = append(items, MemoryItem{Id: n.ID, Failure: why})
				continue
			}
			if payload["deleted"] == true {
				continue
			}
			if text, ok := s.Rows(payload); ok {
				item := MemoryItem{Id: n.ID, Text: text}
				if s.ContentAddressed == nil || !s.ContentAddressed(n.ID, payload) {
					item.Version = n.CreatedAt.UTC()
				}
				items = append(items, item)
			}
		}
		if len(nodes) < limit {
			return items, "", nil
		}
		if len(items) >= limit {
			return items, cursor, nil
		}
	}
}
