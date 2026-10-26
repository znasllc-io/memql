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

func TestVerificationEvidenceCannotBecomePublishedArtifactAcrossFreshJournalRead(t *testing.T) {
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { db.Close() })
	probe, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(probe); err != nil {
		dbtest.Unreachable(t, "image verification publication boundary", dbtest.DSN(), err)
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
	owner := "verify-only-" + id.NewShortId()
	actor := auth.ContextWithToken(auth.ContextWithAccess(t.Context(), &auth.AccessContext{UserId: owner, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: owner})
	journal := workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) { return engine.Execute(ctx, q) }), slog.New(slog.DiscardHandler), "producer-node")
	work := workjournal.Work{OwnerUserID: owner, Template: "pipeline:verification", GoalKey: id.NewShortId(), RunKey: "1", Statement: "Verify image publication boundary", TriggeredBy: pl.WorkTriggerPrefix + "full",
		Input: map[string]any{"repository": "acme/engine", "sha": strings.Repeat("b", 40), "mode": "full", "event": "push", "pipelineId": "fixture", "pipelineRunId": "fixture-run", "attempt": 1}}
	for _, key := range []string{"test.verify", "build.image"} {
		call := map[string]any{"construct": "pipeline", "pipelineKind": "command", "definitionFingerprint": "work-definition-v2:" + strings.Repeat("d", 64)}
		if key == "test.verify" {
			call["imageVerificationOnly"] = true
		}
		work.Steps = append(work.Steps, workjournal.StepDecl{Key: key, Kind: workjournal.KindDeterministic, StepType: "exec", Call: call})
	}
	_, runID, err := workjournal.IDs(work)
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Begin(actor, work)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"test.verify", "build.image"} {
		step, err := run.Step(actor, key)
		if err != nil {
			t.Fatal(err)
		}
		if err := step.Finish(actor, workjournal.Receipt{Status: "done", Result: map[string]any{"status": "succeeded", "metadata": map[string]any{"exitCode": 0, "artifactIntentIds": []string{strings.Repeat("c", 64)}}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Succeeded(actor, map[string]any{"status": "succeeded"}); err != nil {
		t.Fatal(err)
	}
	// The reader receives only durable rows, with no producing driver's local state.
	reader := candidateEvidenceReader{engine: evidenceFreshRead{t, engine}}
	candidate := candidateTestManifest()
	candidate.OwnerUserID = owner
	candidate.Evidence = nil
	proofs := map[string]pl.ReleaseWorkReceipt{}
	for _, key := range []string{"test.verify", "build.image"} {
		proof, err := reader.read(actor, runID, key)
		if err != nil {
			t.Fatal(err)
		}
		proofs[key] = proof
		candidate.Evidence = append(candidate.Evidence, pl.ReleaseEvidence{Name: key, Component: "engine", WorkRunID: runID, StepKey: key, Attempt: proof.Attempt, ReceiptID: proof.ReceiptID, ReceiptDigest: proof.ReceiptDigest, DefinitionDigest: proof.DefinitionDigest, ArtifactIntentIDs: proof.ArtifactIntentIDs})
	}
	for _, key := range []string{"test.verify", "build.image"} {
		proof := proofs[key]
		candidate.Components[0].Artifacts[0].Receipt = pl.ReleaseReceiptReference{WorkRunID: runID, StepKey: key, Attempt: proof.Attempt, IntentID: strings.Repeat("c", 64), DefinitionDigest: proof.DefinitionDigest, ReceiptDigest: proof.ReceiptDigest}
		_, err := reader.coverage(actor, candidate)
		if key == "test.verify" {
			if err == nil || !strings.Contains(err.Error(), "verification") {
				t.Fatal("verification proof promoted", err)
			}
		} else if err != nil {
			t.Fatal("ordinary image publication blocked", err)
		}
	}
}
