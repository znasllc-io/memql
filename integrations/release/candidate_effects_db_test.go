package release

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/id"
)

func TestCandidatePublicationHistoryRecoversPendingAndCompleteWithoutEffects(t *testing.T) {
	db := candidateTestDB(t)
	ledger := &candidateLedger{db: func() *sql.DB { return db }}
	record, err := ledger.prepare(ownerCtx(), candidateTestManifest())
	if err != nil {
		t.Fatal(err)
	}
	cleanupCandidate(t, db, record.ID)
	if _, err := ledger.ready(ownerCtx(), record.ID); err != nil {
		t.Fatal(err)
	}
	approved, err := ledger.approve(ownerCtx(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	i := NewIntegration(nil, &tripwireEngine{t: t}, resolver{systemVariable: func(context.Context, string) (string, error) {
		t.Fatal("history required configuration")
		return "", nil
	}, systemSecret: func(context.Context, string) (string, error) { t.Fatal("history fetched credential"); return "", nil }})
	i.candidateDB = func() *sql.DB { return db }
	read := func() []candidatePublication {
		t.Helper()
		rows, err := i.handleCandidateEffects(ownerCtx(), map[string]any{"candidateId": record.ID}, 0)
		if err != nil || len(rows) != 1 {
			t.Fatal("publication history failed", err)
		}
		var response struct {
			CandidateID  string                 `json:"candidateId"`
			Publications []candidatePublication `json:"publications"`
		}
		if err := json.Unmarshal(rows[0].Payload, &response); err != nil || response.CandidateID != record.ID || response.Publications == nil {
			t.Fatal("invalid history envelope", err)
		}
		return response.Publications
	}
	if history := read(); len(history) != 0 {
		t.Fatal("read invented publication", history)
	}
	intent, err := ledger.beginPublication(ownerCtx(), record.ID, approved.ApprovalID, "registry", "engine", "image")
	if err != nil {
		t.Fatal(err)
	}
	if history := read(); len(history) != 1 || history[0].State != "pending" || history[0].EffectID != intent.EffectID || history[0].ApprovalID != approved.ApprovalID {
		t.Fatal("lost-response intent was hidden", history)
	}
	proof := candidatePublicationReceipt{TargetDigest: intent.Target.TargetDigest, ArchiveDigest: intent.Artifact.Digest, ImageDigest: intent.Artifact.ImageDigest, Platform: intent.Artifact.Platform}
	if err := ledger.finishPublication(ownerCtx(), intent, proof); err != nil {
		t.Fatal(err)
	}
	if history := read(); len(history) != 1 || history[0].State != "complete" || history[0].EffectID != intent.EffectID {
		t.Fatal("completion history was lost", history)
	}
	foreign := auth.ContextWithAccess(t.Context(), &auth.AccessContext{UserId: id.NewShortId(), Role: auth.RoleOwner})
	if _, err := i.handleCandidateEffects(foreign, map[string]any{"candidateId": record.ID}, 0); err == nil {
		t.Fatal("publication history crossed owner boundary")
	}
	if _, err := db.Exec(`UPDATE release_publication_intents SET receipt='{}'::jsonb WHERE effect_id=$1`, intent.EffectID); err != nil {
		t.Fatal(err)
	}
	if _, err := i.handleCandidateEffects(ownerCtx(), map[string]any{"candidateId": record.ID}, 0); err == nil {
		t.Fatal("tampered completion was displayed as verified")
	}
}

func TestCandidatePublicationHistoryRejectsOutOfScopeIntent(t *testing.T) {
	db := candidateTestDB(t)
	l := &candidateLedger{db: func() *sql.DB { return db }}
	r, err := l.prepare(ownerCtx(), candidateTestManifest())
	if err != nil {
		t.Fatal(err)
	}
	cleanupCandidate(t, db, r.ID)
	if _, err := l.ready(ownerCtx(), r.ID); err != nil {
		t.Fatal(err)
	}
	a, err := l.approve(ownerCtx(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := l.beginPublication(ownerCtx(), r.ID, a.ApprovalID, "registry", "engine", "image")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE release_publication_intents SET target_id='unapproved' WHERE effect_id=$1`, intent.EffectID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.effects(ownerCtx(), r.ID); err == nil {
		t.Fatal("history admitted an intent outside approved destination")
	}
}
