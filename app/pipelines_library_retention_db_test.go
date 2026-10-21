package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/server/fileversion"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type pipelineLibraryTestSQL interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func appendPipelineLibraryTestVersion(ctx context.Context, db pipelineLibraryTestSQL, rowID string, changes map[string]any) error {
	data, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO "MemoryNodes"
(id,"createdAt","createdBy",schema,payload,metadata,type,concept)
SELECT id,clock_timestamp(),"createdBy",schema,payload||$2::jsonb,metadata,type,concept
FROM "MemoryNodes" WHERE id=$1 ORDER BY "createdAt" DESC LIMIT 1`, rowID, string(data))
	return err
}

func testPipelineRetentionLibraryDB(t *testing.T, replicas pipelineDBReplicas) {
	for _, mode := range []string{"purge", "edit_wins", "retirement_wins", "interrupted_version"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			u := &pipelineRetirementFake{}
			factory := replicas.caseFactory()
			writer, engine, db := factory(t, u)
			cleaner, _, _ := factory(t, u)
			bus := events.NewBus()
			previousBus := engine.EventBus()
			engine.SetEventBus(bus)
			t.Cleanup(func() { engine.SetEventBus(previousBus); bus.Close() })
			deleted := make(chan events.Event, 4)
			unsubscribe := bus.Subscribe("graph.node.deleted.v1:library:artifact", func(event events.Event) { deleted <- event })
			t.Cleanup(unsubscribe)
			app := &App{engine: engine}
			writer.notifyRetired = app.notifyRetiredPipelineFile
			file := pipelineStreamFile(id.NewShortId(), "retention evidence")
			stored, err := writer.StoreRunFileStream(ctx, file)
			if err != nil {
				t.Fatal("stream finalization must not deadlock against the Library fence", err)
			}
			owner := auth.ContextWithUserActor(ctx, file.OwnerUserID)
			templateMutation(t, engine, owner, "createArtifact", map[string]any{
				"sourceConceptRef": "v1:library:file:" + stored.FileID,
				"ownerUserId":      file.OwnerUserID, "lens": "artifact", "kind": "file",
				"source": "pipeline", "title": file.Name,
			})
			scoped := auth.ContextWithInternalOrigin(owner)
			_, scope := pipelineLifecycleScope(file)
			artifactID := pipelineLibraryArtifactID(stored.FileID)
			if mode == "interrupted_version" {
				templateMutation(t, engine, scoped, "createLibraryFileVersion", map[string]any{
					"versionId": fileversion.DerivedVersionId(stored.FileID, 1), "fileId": stored.FileID, "versionNumber": 1,
					"name": file.Name, "mimeType": file.MimeType, "size": file.Size, "sha256": file.SHA256, "blobUrl": stored.Receipt.URL, "uploadedAt": time.Now().UTC().Format(time.RFC3339Nano),
				})
				if _, err := cleaner.RetireRunFile(scoped, scope, stored.Receipt.IntentID); !errors.Is(err, pipelinesteps.ErrArtifactRetained) {
					t.Fatal("interrupted supersession lost its bytes", err)
				}
				return
			}
			if err := cleaner.PurgeRetiredRunFile(scoped, scope, stored.Receipt.IntentID); err == nil {
				t.Fatal("presentation disappeared before confirmed byte retirement")
			}
			if mode == "edit_wins" {
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if err := appendPipelineLibraryTestVersion(ctx, tx, artifactID, map[string]any{"labels": []string{"keep"}}); err != nil {
					t.Fatal(err)
				}
				result := make(chan error, 1)
				go func() { _, err := cleaner.RetireRunFile(scoped, scope, stored.Receipt.IntentID); result <- err }()
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				if err := <-result; !errors.Is(err, pipelinesteps.ErrArtifactRetained) {
					t.Fatal("other replica ignored the committed Library edit", err)
				}
				if len(u.tokens) != 0 {
					t.Fatal("retained file reached storage retirement")
				}
				return
			}
			if mode == "retirement_wins" {
				entered, proceed := make(chan struct{}), make(chan struct{})
				u.entered, u.proceed = entered, proceed
				result := make(chan error, 1)
				go func() { _, err := cleaner.RetireRunFile(scoped, scope, stored.Receipt.IntentID); result <- err }()
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				err := appendPipelineLibraryTestVersion(ctx, db, artifactID, map[string]any{"labels": []string{"too late"}})
				close(proceed)
				if err == nil || !strings.Contains(err.Error(), "permanently retired") {
					t.Fatal("late Library write crossed the durable retirement fence", err)
				}
				if err := <-result; err != nil {
					t.Fatal(err)
				}
			} else if _, err := cleaner.RetireRunFile(scoped, scope, stored.Receipt.IntentID); err != nil {
				t.Fatal(err)
			}
			foreign := scope
			foreign.OwnerUserID = id.NewShortId()
			if err := writer.PurgeRetiredRunFile(scoped, foreign, stored.Receipt.IntentID); err == nil {
				t.Fatal("cleanup accepted another owner")
			}
			for n := 0; n < 2; n++ {
				if err := writer.PurgeRetiredRunFile(scoped, scope, stored.Receipt.IntentID); err != nil {
					t.Fatal("replacement/repeated presentation cleanup", err)
				}
				select {
				case event := <-deleted:
					if event.Payload["id"] != artifactID || event.Payload["ownerUserId"] != file.OwnerUserID || event.Payload["sourceConceptRef"] != "v1:library:file:"+stored.FileID {
						t.Fatal("Library deletion escaped its owning file", event.Payload)
					}
				case <-ctx.Done():
					t.Fatal("committed cleanup did not notify the live Library", ctx.Err())
				}
			}
			var remaining int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE id=$1 OR id=$2`,
				"v1:library:file:"+stored.FileID, artifactID).Scan(&remaining); err != nil || remaining != 0 {
				t.Fatal("old Library versions remain visible", remaining, err)
			}
			var state, tombstone string
			if err := db.QueryRowContext(ctx, `SELECT state,retired_etag FROM pipeline_artifact_uploads WHERE intent_id=$1`, stored.Receipt.IntentID).Scan(&state, &tombstone); err != nil || state != "retired" || tombstone == "" {
				t.Fatal("cleanup lost the permanent native receipt", state, err)
			}
			// The ordinary promotion path cannot resurrect the deleted index.
			_, err = engine.Execute(owner, `mutation createArtifact(sourceConceptRef:"v1:library:file:`+stored.FileID+`",ownerUserId:"`+file.OwnerUserID+`",lens:"artifact",kind:"file",source:"pipeline",title:"late")`)
			if err == nil {
				t.Fatal("delayed promotion resurrected a retired artifact")
			}
		})
	}
}
