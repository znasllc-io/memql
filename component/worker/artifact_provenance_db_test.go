package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// Exercise the real owner-gated queries and read-merge mutations. A mock stamp
// call alone cannot detect an index/file id mismatch or missing DSL arguments.
func TestAppArtifactProvenanceLinksOwnedFileAndRun(t *testing.T) {
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "app artifact provenance", dbtest.DSN(), err)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	engine, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	store := &EngineStore{Engine: engine}
	prefix := fmt.Sprintf("provenance-%d", time.Now().UnixNano())
	owner, foreign := "v1:identity:user:"+prefix, "v1:identity:user:"+prefix+"-other"
	var ids []string
	seed := func(concept, key string, payload map[string]any) string {
		t.Helper()
		id := concept + ":" + prefix + "-" + key
		data, _ := json.Marshal(payload)
		row := memorynodes.MemoryNode{ID: id, CreatedAt: time.Now().UTC(), CreatedBy: owner, Concept: concept, Type: memorynodes.NodeTypeObject, Schema: json.RawMessage(`{}`), Payload: data, Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
		if _, err := db.NewInsert().Model(&row).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		return id
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", id).Exec(ctx)
		}
	})
	file := func(key, who string) string {
		return seed("v1:library:file", key, map[string]any{"ownerUserId": who, "name": "proof.png", "mimeType": "image/png", "size": 20, "sha256": "original-digest", "blobUrl": "library/test/proof.png", "source": "uploaded", "status": "ready", "archived": true})
	}
	artifact := func(key, who, kind, ref string) string {
		return seed("v1:library:artifact", key, map[string]any{"ownerUserId": who, "title": "Proof", "lens": "artifact", "kind": kind, "source": "uploaded", "sourceConceptRef": ref, "labels": []string{"keep"}, "archived": true})
	}
	read := func(concept, id string) map[string]any {
		t.Helper()
		var row memorynodes.MemoryNode
		if err := db.NewSelect().Model(&row).Where("id = ?", id).Where("concept = ?", concept).OrderExpr("\"createdAt\" DESC").Limit(1).Scan(ctx); err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	fileId := file("file", owner)
	artifactId := artifact("index", owner, "file", fileId)
	p := ArtifactProvenance{App: "codex", Model: "reported-model", Effort: "reported-effort", SessionId: "session", RunId: "v1:work:run:" + prefix, StepKey: "step"}
	if err := store.StampArtifactProvenance(ctx, owner, []string{artifactId}, p); err != nil {
		t.Fatal(err)
	}
	for concept, id := range map[string]string{"v1:library:artifact": artifactId, "v1:library:file": fileId} {
		row := read(concept, id)
		stamp, _ := row["producedBy"].(map[string]any)
		if row["producedByRunId"] != p.RunId || row["source"] != "app_session" || stamp["sessionId"] != p.SessionId || stamp["model"] != p.Model || stamp["effort"] != p.Effort || row["archived"] != true {
			t.Fatalf("lost identity, app report or retained row metadata: %s: %+v", concept, row)
		}
	}
	if row := read("v1:library:file", fileId); row["producedByStepKey"] != "step" || row["sha256"] != "original-digest" || row["blobUrl"] != "library/test/proof.png" {
		t.Fatalf("file identity or original bytes changed: %+v", row)
	}
	// A later unbound conversation must preserve the existing native identity.
	unbound := p
	unbound.RunId, unbound.StepKey = "", ""
	if err := store.StampArtifactProvenance(ctx, owner, []string{artifactId}, unbound); err != nil {
		t.Fatal(err)
	}
	if row := read("v1:library:file", fileId); row["producedByRunId"] != p.RunId || row["producedByStepKey"] != "step" {
		t.Fatalf("unbound session erased native work identity: %+v", row)
	}
	wrongRun := p
	wrongRun.RunId += "-different"
	if err := store.StampArtifactProvenance(ctx, owner, []string{artifactId}, wrongRun); err == nil {
		t.Fatal("reassigned an artifact from another producing run")
	}
	foreignFile := file("foreign-file", foreign)
	foreignArtifact := artifact("foreign-index", foreign, "file", foreignFile)
	badLink := artifact("bad-link", owner, "file", foreignFile)
	missingLink := artifact("missing-link", owner, "file", "v1:library:file:missing-"+prefix)
	for _, id := range []string{foreignArtifact, badLink, missingLink, "v1:library:artifact:missing-" + prefix} {
		if err := store.StampArtifactProvenance(ctx, owner, []string{id}, p); err == nil {
			t.Fatalf("accepted a foreign or missing evidence link: %s", id)
		}
	}
	for _, id := range []string{foreignArtifact, badLink, missingLink} {
		if row := read("v1:library:artifact", id); row["producedBy"] != nil || row["source"] != "uploaded" {
			t.Fatalf("partially stamped an invalid evidence link: %+v", row)
		}
	}
	// Non-file records are valid outputs; do not invent a backing Library file.
	record := artifact("record", owner, "note", "v1:notes:note:"+prefix)
	if err := store.StampArtifactProvenance(ctx, owner, []string{record}, p); err != nil {
		t.Fatal(err)
	}
	if row := read("v1:library:artifact", record); row["producedByRunId"] != p.RunId {
		t.Fatalf("non-file output was not associated: %+v", row)
	}
	if err := store.StampArtifactProvenance(ctx, "", []string{artifactId}, p); err == nil {
		t.Fatal("accepted a stamp without an owner")
	}
}
