package release

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/id"
)

type evidenceTripwire struct{ t *testing.T }

func (e evidenceTripwire) Execute(context.Context, string) (*memql.ExecuteResult, error) {
	e.t.Fatal("unauthorized evidence reached engine")
	return nil, nil
}

func TestCandidateEvidenceOwnerWall(t *testing.T) {
	r := candidateEvidenceReader{engine: evidenceTripwire{t}}
	for _, ctx := range []context.Context{context.Background(), actorContext(auth.RoleDeveloper), actorContext(auth.RoleAdmin)} {
		if _, err := r.read(ctx, "run", "step"); err == nil {
			t.Fatal("nonowner read evidence")
		}
		if err := r.verify(ctx, candidateTestManifest()); err == nil {
			t.Fatal("nonowner verified evidence")
		}
	}
}

type evidenceFreshRead struct {
	t      *testing.T
	engine *memql.MemQLEngine
}

func (e evidenceFreshRead) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if !memql.FreshReadFromContext(ctx) || auth.OriginFromContext(ctx) != auth.OriginClient {
		e.t.Fatal("evidence used cache or escalated origin")
	}
	return e.engine.Execute(ctx, q)
}

func TestCandidateEvidenceThroughRealJournalAndOwnerQueries(t *testing.T) {
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "release work evidence", dbtest.DSN(), err)
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	engine, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	engine.Logger = slog.New(slog.DiscardHandler)
	if err := engine.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	owner := "release-evidence-" + id.NewShortId()
	actor := auth.ContextWithAccess(t.Context(), &auth.AccessContext{UserId: owner, Role: auth.RoleOwner})
	actor = auth.ContextWithToken(actor, &auth.TokenInfo{Subject: owner})
	journal := workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) { return engine.Execute(ctx, q) }), slog.New(slog.DiscardHandler), "proof-node")
	fp := "work-definition-v2:" + strings.Repeat("d", 64)
	work := workjournal.Work{OwnerUserID: owner, Template: "pipeline:evidence", GoalKey: id.NewShortId(), RunKey: "1", Statement: "Candidate evidence fixture", TriggeredBy: pl.WorkTriggerPrefix + "full",
		Input: map[string]any{"repository": "acme/engine", "sha": strings.Repeat("b", 40), "mode": "full", "event": "push", "pipelineId": "pipeline-fixture", "pipelineRunId": "run-fixture", "attempt": 1},
		Steps: []workjournal.StepDecl{{Key: "build.image", Kind: workjournal.KindDeterministic, StepType: "exec", Call: map[string]any{"construct": "pipeline", "definitionFingerprint": fp}}}}
	_, runID, err := workjournal.IDs(work)
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Begin(actor, work)
	if err != nil {
		t.Fatal(err)
	}
	step, err := run.Step(actor, "build.image")
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]any{"status": "succeeded", "metadata": map[string]any{"exitCode": 0, "artifactIntentIds": []string{strings.Repeat("c", 64)}}}
	if err := step.Finish(actor, workjournal.Receipt{Status: "done", Result: result}); err != nil {
		t.Fatal(err)
	}
	reader := candidateEvidenceReader{engine: evidenceFreshRead{t, engine}}
	if _, err := reader.read(actor, runID, "build.image"); err == nil {
		t.Fatal("unfinished run accepted")
	}
	if err := run.Succeeded(actor, map[string]any{"status": "succeeded"}); err != nil {
		t.Fatal(err)
	}
	got, err := reader.read(actor, runID, "build.image")
	if err != nil {
		t.Fatal(err)
	}
	c := candidateTestManifest()
	c.OwnerUserID = owner
	a := &c.Components[0].Artifacts[0]
	a.Receipt = pl.ReleaseReceiptReference{WorkRunID: runID, StepKey: "build.image", Attempt: got.Attempt, IntentID: strings.Repeat("c", 64), DefinitionDigest: got.DefinitionDigest, ReceiptDigest: got.ReceiptDigest}
	c.Evidence = []pl.ReleaseEvidence{{Name: "build", Component: "engine", WorkRunID: runID, StepKey: "build.image", Attempt: got.Attempt, ReceiptID: got.ReceiptID, ReceiptDigest: got.ReceiptDigest, DefinitionDigest: got.DefinitionDigest, ArtifactIntentIDs: got.ArtifactIntentIDs}}
	if err := reader.verify(actor, c); err != nil {
		t.Fatal(err)
	}
	stranger := auth.ContextWithAccess(t.Context(), &auth.AccessContext{UserId: "stranger-" + owner, Role: auth.RoleOwner})
	if _, err := reader.read(stranger, runID, "build.image"); err == nil {
		t.Fatal("stranger read evidence")
	}
	c.Components[0].Commit = strings.Repeat("e", 40)
	if err := reader.verify(actor, c); err == nil {
		t.Fatal("different source reused evidence")
	}
	c.Components[0].Commit = got.Commit
	// A second journal version cannot silently reuse the approved receipt ID.
	if err := step.Finish(actor, workjournal.Receipt{Status: "failed", Result: map[string]any{"status": "failed"}, Code: "late-failure"}); err != nil {
		t.Fatal(err)
	}
	if err := reader.verify(actor, c); err == nil {
		t.Fatal("changed receipt retained success")
	}
}
