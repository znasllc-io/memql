package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	p := ArtifactProvenance{App: "codex", Model: "reported-model", Effort: "reported-effort", SessionId: "session", RunId: "v1:work:run:" + prefix, StepKey: "v1:work:step:step"}
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
	files, err := store.executeAndExtract(appSessionReadContext(ctx, owner), fmt.Sprintf(`query libraryFilesForOwner(runId: %q, stepKey: "step")`, p.RunId))
	if err != nil || len(files) != 1 {
		t.Fatalf("native step filter cannot find returned evidence: files=%d err=%v", len(files), err)
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

// Separate engine instances model replicas, including independent result caches.
// The fixtures below always pause AFTER both owner-scoped reads, so the second
// writer cannot pass merely because a scheduling accident let it see the winner.
type provenanceExecutorFunc func(context.Context, string) (*memql.ExecuteResult, error)

func (f provenanceExecutorFunc) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	return f(ctx, query)
}

type provenanceDBFixture struct {
	db            *bun.DB
	engines       [2]*memql.MemQLEngine
	owner, prefix string
	ids           []string
}

func newProvenanceDBFixture(t *testing.T) *provenanceDBFixture {
	t.Helper()
	f := &provenanceDBFixture{prefix: fmt.Sprintf("provenance-race-%d", time.Now().UnixNano())}
	f.owner = "v1:identity:user:" + f.prefix
	for n := range f.engines {
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
		t.Cleanup(func() { _ = db.Close() })
		if err := db.PingContext(context.Background()); err != nil {
			dbtest.Unreachable(t, "app artifact provenance race", dbtest.DSN(), err)
			return nil
		}
		if n == 0 {
			f.db = db
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
		f.engines[n] = engine
	}
	t.Cleanup(func() {
		for _, id := range f.ids {
			_, _ = f.db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
		}
	})
	return f
}

func (f *provenanceDBFixture) seed(t *testing.T, concept, key string, payload map[string]any) string {
	t.Helper()
	id := concept + ":" + f.prefix + "-" + key
	payload["ownerUserId"] = f.owner
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	row := memorynodes.MemoryNode{ID: id, CreatedAt: time.Now().UTC(), CreatedBy: f.owner, Concept: concept, Type: memorynodes.NodeTypeObject, Schema: json.RawMessage(`{}`), Payload: data, Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
	if _, err := f.db.NewInsert().Model(&row).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.ids = append(f.ids, id)
	return id
}

func (f *provenanceDBFixture) file(t *testing.T, key string) string {
	return f.seed(t, "v1:library:file", key, map[string]any{"name": "proof.png", "mimeType": "image/png", "size": 20, "sha256": "original-digest", "blobUrl": "library/test/proof.png", "source": "uploaded", "status": "ready", "archived": true})
}

func (f *provenanceDBFixture) artifact(t *testing.T, key, kind, ref string) string {
	return f.seed(t, "v1:library:artifact", key, map[string]any{"title": "Proof", "lens": "artifact", "kind": kind, "source": "uploaded", "sourceConceptRef": ref, "labels": []string{"keep"}, "archived": true})
}

func (f *provenanceDBFixture) read(t *testing.T, id string) map[string]any {
	t.Helper()
	var row memorynodes.MemoryNode
	if err := f.db.NewSelect().Model(&row).Where("id = ?", id).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestAppArtifactProvenanceCompetingRunsAcrossReplicas(t *testing.T) {
	f := newProvenanceDBFixture(t)
	for _, scenario := range []string{"same artifact", "two indexes share file", "non-file artifact"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			kind, fileID := "file", ""
			if scenario == "non-file artifact" {
				kind = "note"
			} else {
				fileID = f.file(t, scenario+"-file")
			}
			firstID := f.artifact(t, scenario+"-first", kind, fileID)
			secondID := firstID
			if scenario == "two indexes share file" {
				// Bare and canonical references to the same file must share one lock.
				secondID = f.artifact(t, scenario+"-second", kind, memql.BareShortId(fileID))
			}
			ready := make(chan int, 2)
			releases := []chan struct{}{make(chan struct{}), make(chan struct{})}
			results := []chan error{make(chan error, 1), make(chan error, 1)}
			stamps := []ArtifactProvenance{
				{App: "codex", SessionId: "first", RunId: "v1:work:run:" + f.prefix + "-first", StepKey: "first"},
				{App: "codex", SessionId: "second", RunId: "v1:work:run:" + f.prefix + "-second", StepKey: "second"},
			}
			for n, artifactID := range []string{firstID, memql.BareShortId(secondID)} {
				engine := f.engines[n]
				first := true
				store := &EngineStore{Engine: provenanceExecutorFunc(func(ctx context.Context, query string) (*memql.ExecuteResult, error) {
					if first && strings.HasPrefix(query, "stamp") {
						first = false
						ready <- n
						select {
						case <-releases[n]:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					return engine.Execute(ctx, query)
				})}
				go func() { results[n] <- store.StampArtifactProvenance(ctx, f.owner, []string{artifactID}, stamps[n]) }()
			}
			for range 2 {
				select {
				case <-ready:
				case <-ctx.Done():
					t.Fatal("both replicas did not reach their first write")
				}
			}
			close(releases[0])
			if err := <-results[0]; err != nil {
				t.Fatalf("first claim: %v", err)
			}
			close(releases[1])
			if err := <-results[1]; !errors.Is(err, memql.ErrRowVersionChanged) {
				t.Fatalf("stale competing writer was not fenced: %v", err)
			}
			if got := f.read(t, firstID)["producedByRunId"]; got != stamps[0].RunId {
				t.Fatalf("winner index reassigned: %v", got)
			}
			if fileID != "" {
				if got := f.read(t, fileID)["producedByRunId"]; got != stamps[0].RunId {
					t.Fatalf("backing file split from winner: %v", got)
				}
			}
			if secondID != firstID {
				if got := f.read(t, secondID)["producedByRunId"]; got != nil {
					t.Fatalf("losing index was partially claimed: %v", got)
				}
			}
			retry := &EngineStore{Engine: f.engines[1]}
			if err := retry.StampArtifactProvenance(ctx, f.owner, []string{secondID}, stamps[1]); err == nil {
				t.Fatal("fresh retry reassigned another run's evidence")
			}
		})
	}
}

func TestAppArtifactProvenanceRetriesFileClaimAfterInterruptedIndex(t *testing.T) {
	f := newProvenanceDBFixture(t)
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint("cancelled-", cancelled), func(t *testing.T) {
			fileID := f.file(t, fmt.Sprint(cancelled)+"-file")
			artifactID := f.artifact(t, fmt.Sprint(cancelled)+"-index", "file", fileID)
			otherID := f.artifact(t, fmt.Sprint(cancelled)+"-other", "file", fileID)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("lost file claim response")
			store := &EngineStore{Engine: provenanceExecutorFunc(func(ctx context.Context, query string) (*memql.ExecuteResult, error) {
				result, err := f.engines[0].Execute(ctx, query)
				if err == nil && strings.HasPrefix(query, "stampLibraryFileProvenance(") {
					if cancelled {
						cancel()
						return result, nil
					}
					return nil, failure
				}
				return result, err
			})}
			stamp := ArtifactProvenance{App: "codex", SessionId: "retained", RunId: "v1:work:run:" + f.prefix + "-retained", StepKey: "exact-step"}
			if err := store.StampArtifactProvenance(ctx, f.owner, []string{artifactID}, stamp); err == nil {
				t.Fatal("interrupted completion reported success")
			}
			if got := f.read(t, fileID)["producedByRunId"]; got != stamp.RunId {
				t.Fatalf("durable file claim lost: %v", got)
			}
			if got := f.read(t, artifactID)["producedByRunId"]; got != nil {
				t.Fatalf("index should remain unfinished: %v", got)
			}
			retry := &EngineStore{Engine: f.engines[1]}
			other := stamp
			other.RunId += "-other"
			if err := retry.StampArtifactProvenance(context.Background(), f.owner, []string{otherID}, other); err == nil {
				t.Fatal("another index stole an interrupted file claim")
			}
			if err := retry.StampArtifactProvenance(context.Background(), f.owner, []string{artifactID}, stamp); err != nil {
				t.Fatalf("same-run recovery: %v", err)
			}
			for _, id := range []string{artifactID, fileID} {
				if got := f.read(t, id)["producedByRunId"]; got != stamp.RunId {
					t.Fatalf("retry split association: %v", got)
				}
			}
		})
	}
}
