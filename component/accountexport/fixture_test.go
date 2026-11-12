package accountexport

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The fixed input every test and the sample file are written from. Fixed
// times and fixed ids, so writing it twice gives the same bytes.
var (
	fixtureAt   = time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	fixtureStem = "acme-2026-11-12" + FolderSuffix
)

const fixtureSchema = "3f2a9c"

func fixtureManifest() Manifest {
	return Manifest{
		EngineVersion: "0.32.0",
		ExportID:      "exp0001",
		CreatedAt:     fixtureAt,
		Source:        Source{Domain: "memql.example", ClusterID: "cluster-1"},
		Account:       Account{ID: "acc0001", Name: "Acme", Primary: false},
		Kinds: []KindDecl{
			{Concept: "v1:accounts:account", BelongsTo: "self"},
			{Concept: "v1:library:file", BelongsTo: "account"},
			{Concept: "v1:todos:todo", BelongsTo: "account", Origin: "memql", MirroredTo: []string{"example"}},
		},
		Language:   []LanguageLine{{Domain: "acme", Memql: "1.0", Edition: "2026", ContentHash: "abc"}},
		Migrations: []string{"0001_example"},
		Embedder:   "embed-small",
	}
}

func fixtureRow(concept, id string, minute int, payload string) Row {
	return Row{
		Concept:    concept,
		ID:         concept + ":" + id,
		CreatedAt:  fixtureAt.Add(-time.Duration(60-minute) * time.Minute),
		CreatedBy:  "usr0001",
		Type:       "record",
		Schema:     json.RawMessage(`"` + fixtureSchema + `"`),
		Payload:    json.RawMessage(payload),
		Provenance: json.RawMessage(`{"source":"test"}`),
	}
}

// fixtureContent is what writeFixture puts in the file, and what a report
// over it must count.
type fixtureContent struct {
	accountRows []Row
	kinds       map[string][]Row // concept -> versions, in (id, createdAt) order
	people      []map[string]any
	object      []byte
}

func defaultFixture() fixtureContent {
	return fixtureContent{
		accountRows: []Row{
			fixtureRow("v1:accounts:account", "acc0001", 1, `{"name":"Acme"}`),
			fixtureRow("v1:accounts:account", "acc0001", 2, `{"name":"Acme Ltd"}`),
		},
		kinds: map[string][]Row{
			"v1:library:file": {
				fixtureRow("v1:library:file", "f1", 3, `{"title":"Café menu","blobUrl":"files/f1"}`),
				fixtureRow("v1:library:file", "f1", 4, `{"title":"Café menu v2","blobUrl":"files/f1"}`),
				fixtureRow("v1:library:file", "f2", 5, `{"title":"Gone","blobUrl":"files/f2"}`),
			},
			"v1:todos:todo": {
				fixtureRow("v1:todos:todo", "t1", 6, `{"title":"Ship"}`),
			},
		},
		people: []map[string]any{
			{"id": "usr0001", "name": "Member One", "email": "one@example.test", "role": "owner", "active": true},
			{"id": "usr0002", "kind": "operator"},
		},
		object: []byte("the bytes of f1, exactly as stored\n"),
	}
}

var fixtureKindOrder = []string{"v1:library:file", "v1:todos:todo"}

func (c fixtureContent) report() Report {
	r := Report{
		LeftOut:        []LeftOut{{Concept: "v1:platform:siteAccessSession", Reason: LeftOutSignin, Rows: 3}},
		InNoAccount:    []KindRows{{Concept: "v1:todos:todo", Rows: 2}},
		ScrubbedFields: []ScrubbedField{{Concept: "v1:todos:todo", Field: "token"}},
		Rebuilt:        []string{"vectors", "email statistics", "site health"},
		Bytes:          BytesReport{Objects: 1, Bytes: int64(len(c.object)), Missing: 1, NotCarriedObjects: 1, NotCarriedBytes: 10, NotCarriedUnknownSize: 0},
		Members: []MemberCount{
			{Name: MemberPeople, Lines: int64(len(c.people))},
			{Name: MemberReferences, Lines: 1},
			{Name: MemberConnections, Lines: 0},
		},
	}
	add := func(concept string, rows []Row) {
		ids := map[string]bool{}
		for _, row := range rows {
			ids[row.ID] = true
		}
		n, v := int64(len(ids)), int64(len(rows))
		r.Kinds = append(r.Kinds, KindCount{Concept: concept, ExpectedRows: n, ExpectedVersions: v, WrittenRows: n, WrittenVersions: v})
	}
	add("v1:accounts:account", c.accountRows)
	for _, k := range fixtureKindOrder {
		add(k, c.kinds[k])
	}
	if len(c.object) == 0 {
		r.Bytes = BytesReport{}
	}
	return r
}

// writeFixture writes c as a whole file and answers its bytes.
func writeFixture(t *testing.T, c fixtureContent) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, fixtureStem, fixtureManifest())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for _, row := range c.accountRows {
		if err := w.AccountRow(row); err != nil {
			t.Fatalf("AccountRow: %v", err)
		}
	}
	for _, k := range fixtureKindOrder {
		rows := c.kinds[k]
		if len(rows) == 0 {
			continue
		}
		p := w.Parted(RowsDir(k))
		for _, row := range rows {
			if err := p.Write(row); err != nil {
				t.Fatalf("Write %s: %v", k, err)
			}
		}
		if err := p.Close(); err != nil {
			t.Fatalf("Close %s: %v", k, err)
		}
	}
	if err := w.Schema(fixtureSchema, json.RawMessage(`{"type":"object"}`)); err != nil {
		t.Fatalf("Schema: %v", err)
	}
	for _, p := range c.people {
		if err := w.Line(MemberPeople, p); err != nil {
			t.Fatalf("Line people: %v", err)
		}
	}
	if err := w.Line(MemberReferences, map[string]any{"concept": "v1:cluster:node", "id": "n1", "title": "Node"}); err != nil {
		t.Fatalf("Line references: %v", err)
	}
	if len(c.object) > 0 {
		meta := BytesMeta{Key: "files/f1", UsedBy: []RowField{{Concept: "v1:library:file", ID: "f1", Field: "blobUrl"}}}
		if err := w.Bytes(meta, int64(len(c.object)), bytes.NewReader(c.object)); err != nil {
			t.Fatalf("Bytes: %v", err)
		}
		if err := w.MissingBytes(BytesMeta{Key: "files/f2", UsedBy: []RowField{{Concept: "v1:library:file", ID: "f2", Field: "blobUrl"}}}); err != nil {
			t.Fatalf("MissingBytes: %v", err)
		}
	}
	if err := w.Finish(c.report()); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return buf.Bytes()
}

func mustContain(t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want an error naming %q, got none", want)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("error %q does not name %q", err, w)
		}
	}
}
