package seeddata

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/dsl"
)

type Source struct {
	DomainID  string `json:"domainId"`
	SourceRef string `json:"sourceRef"`
	Catalog   string `json:"catalog,omitempty"`
	File      string `json:"file,omitempty"`
	SHA256    string `json:"sha256"`
	Lesson    bool   `json:"lesson,omitempty"`
}
type ChunkRecord struct {
	ID        string `json:"id"`
	Domain    string `json:"domain"`
	SourceRef string `json:"sourceRef"`
	Text      string `json:"text"`
	Seq       int    `json:"seq"`
	Revision  string `json:"revision"`
	Lesson    bool   `json:"lesson"`
}

// Release is the explicit release selection, never all registered/future corpora.
// Invalid hashes refuse startup rather than approving changed bytes implicitly.
var Release = sync.OnceValue(func() []ChunkRecord {
	raw, err := fs.ReadFile(dsl.Tree(), "knowledge/training/manifest.json")
	if err != nil {
		panic(err)
	}
	var manifest struct {
		Version    int
		Sources    []Source
		Provenance []struct{ File, SHA256 string }
	}
	if err = json.Unmarshal(raw, &manifest); err != nil {
		panic(err)
	}
	if manifest.Version != 1 || len(manifest.Sources) == 0 || len(manifest.Sources) > 64 {
		panic("invalid release knowledge manifest")
	}
	var catalog map[string][]struct{ SourceRef, Text string }
	// The catalog also has non-corpus fields, so decode the chosen value only.
	var encoded map[string]json.RawMessage
	if err = json.Unmarshal(CatalogJSON(), &encoded); err != nil {
		panic(err)
	}
	catalog = map[string][]struct{ SourceRef, Text string }{}
	for _, name := range []string{"computerUse", "workbench", "workGuidance"} {
		var catalogEntryBuffer []struct{ SourceRef, Text string }
		if err = json.Unmarshal(encoded[name], &catalogEntryBuffer); err != nil {
			panic(err)
		}
		catalog[name] = append([]struct{ SourceRef, Text string }(nil), catalogEntryBuffer...)
	}
	read := func(path, hash string) string {
		data, err := fs.ReadFile(dsl.Tree(), path)
		if err != nil {
			panic(err)
		}
		if fmt.Sprintf("%x", sha256.Sum256(data)) != hash {
			panic("release knowledge source hash changed: " + path)
		}
		return string(data)
	}
	for _, origin := range manifest.Provenance {
		read(origin.File, origin.SHA256)
	}
	var result []ChunkRecord
	seen := map[string]bool{}
	for _, source := range manifest.Sources {
		if source.DomainID == "" || source.SourceRef == "" || seen[source.SourceRef] {
			panic("invalid release knowledge source")
		}
		seen[source.SourceRef] = true
		var text string
		if source.File != "" && source.Catalog == "" {
			text = read(source.File, source.SHA256)
		} else if source.Catalog != "" && source.File == "" {
			for _, entry := range catalog[source.Catalog] {
				if entry.SourceRef == source.SourceRef {
					text = entry.Text
					break
				}
			}
			if fmt.Sprintf("%x", sha256.Sum256([]byte(text))) != source.SHA256 {
				panic("release knowledge catalog hash changed: " + source.SourceRef)
			}
		} else {
			panic("release knowledge source needs exactly one origin")
		}
		if strings.TrimSpace(text) == "" || len(text) > 32000 {
			panic("invalid release knowledge text bound")
		}
		for seq, part := range Chunk(text, 1800, 180) {
			key := string(id.New().MustFromMap(map[string]any{"kind": "knowledge-chunk", "domainId": source.DomainID, "sourceRef": source.SourceRef, "seq": seq, "text": part}))
			result = append(result, ChunkRecord{key, source.DomainID, source.SourceRef, part, seq, source.SHA256, source.Lesson})
		}
	}
	if len(result) > 256 {
		panic("release knowledge chunk bound exceeded")
	}
	return result
})

func Find(key string) (ChunkRecord, bool) {
	key = strings.TrimPrefix(key, "v1:knowledge:documentChunk:")
	for _, chunk := range Release() {
		if chunk.ID == key {
			return chunk, true
		}
	}
	return ChunkRecord{}, false
}
func (c ChunkRecord) Matches(p map[string]any) bool {
	seq, ok := p["seq"].(float64)
	return ok && seq == float64(c.Seq) && p["source"] == "appStructure" && p["domainId"] == c.Domain && p["sourceRef"] == c.SourceRef && p["text"] == c.Text
}

// CurrentSQL is a predicate over an internal SQL row alias. It leaves ordinary
// uploads and unselected corpora alone, but never retrieves obsolete/tampered
// release revisions. Review pages intentionally do not use it. No caller text
// enters the SQL: the JSON is compiled, hash-checked release data.
func CurrentSQL(alias string, lessonsOnly bool) string {
	raw, _ := json.Marshal(Release())
	records := `jsonb_to_recordset('` + strings.ReplaceAll(string(raw), "'", "''") + `'::jsonb) AS seed(id text, domain text, "sourceRef" text, text text, seq int, lesson bool)`
	matching := `EXISTS (SELECT 1 FROM ` + records + ` WHERE ` + alias + `.id='v1:knowledge:documentChunk:'||seed.id AND ` + alias + `.payload->>'source'='appStructure' AND ` + alias + `.payload->>'domainId'=seed.domain AND ` + alias + `.payload->>'sourceRef'=seed."sourceRef" AND ` + alias + `.payload->>'text'=seed.text AND ` + alias + `.payload->'seq'=to_jsonb(seed.seq)`
	if lessonsOnly {
		return matching + ` AND seed.lesson)`
	}
	return `(` + matching + `) OR (NOT EXISTS (SELECT 1 FROM ` + records + ` WHERE ` + alias + `.id='v1:knowledge:documentChunk:'||seed.id) AND (` + alias + `.payload->>'source' IS DISTINCT FROM 'appStructure' OR NOT EXISTS (SELECT 1 FROM ` + records + ` WHERE ` + alias + `.payload->>'sourceRef'=seed."sourceRef"))))`
}

// Allows applies the same revision rule to non-ranking consumers such as the
// Trainer refresh context. Review/catalog reads deliberately remain broader.
func Allows(key string, p map[string]any) bool {
	if seed, ok := Find(key); ok {
		return seed.Matches(p)
	}
	if p["source"] != "appStructure" {
		return true
	}
	for _, seed := range Release() {
		if p["sourceRef"] == seed.SourceRef {
			return false
		}
	}
	return true
}
