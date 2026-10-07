package release

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/id"
)

func TestCandidateHistoryPagesByImmutablePositionAndOwner(t *testing.T) {
	db := candidateTestDB(t)
	ledger := &candidateLedger{db: func() *sql.DB { return db }}
	owner := id.NewShortId()
	ctx := auth.ContextWithAccess(t.Context(), &auth.AccessContext{UserId: owner, Role: auth.RoleOwner})
	created := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	keys := []string{}
	for range 3 {
		manifest := candidateTestManifest()
		manifest.OwnerUserID = owner
		record, err := ledger.prepare(ctx, manifest)
		if err != nil {
			t.Fatal(err)
		}
		cleanupCandidate(t, db, record.ID)
		keys = append(keys, record.ID)
		// Tied timestamps must page by identity, without duplicates or omission.
		if _, err := db.Exec(`UPDATE release_candidates SET created_at=$2 WHERE candidate_id=$1`, record.ID, created); err != nil {
			t.Fatal(err)
		}
	}
	slices.SortFunc(keys, func(a, b string) int { return strings.Compare(b, a) })
	foreign, err := ledger.prepare(ownerCtx(), candidateTestManifest())
	if err != nil {
		t.Fatal(err)
	}
	cleanupCandidate(t, db, foreign.ID)
	page, err := ledger.list(ctx, "", 2)
	if err != nil || len(page.Candidates) != 2 || page.NextCursor == "" {
		t.Fatal("first page", page, err)
	}
	for n, item := range page.Candidates {
		if item.ID != keys[n] || item.State != "preparing" || len(item.Components) != 1 || item.Components[0].Version != "0.25.0" || item.EvidenceCount != 1 {
			t.Fatal("candidate summary lost its owner, order or review identity", item)
		}
	}
	// Approval changes status/updated_at, never the continuation position.
	if _, err := ledger.ready(ctx, keys[2]); err != nil {
		t.Fatal(err)
	}
	approval, err := ledger.approve(ctx, keys[2])
	if err != nil {
		t.Fatal(err)
	}
	// Another instance can recover review history with neither blob wiring nor
	// valid operator configuration; configuration must not hide interrupted work.
	i := NewIntegration(nil, &tripwireEngine{t: t}, resolver{systemVariable: func(context.Context, string) (string, error) {
		t.Fatal("history read required live configuration")
		return "", nil
	}})
	i.candidateDB = func() *sql.DB { return db }
	nodes, err := i.handleCandidateList(ctx, map[string]any{"cursor": page.NextCursor, "limit": float64(2)}, 0)
	if err != nil || len(nodes) != 1 {
		t.Fatal("history without object storage", err)
	}
	var tail candidatePage
	if err := json.Unmarshal(nodes[0].Payload, &tail); err != nil || len(tail.Candidates) != 1 || tail.Candidates[0].ID != keys[2] || tail.Candidates[0].State != "approved" || tail.NextCursor != "" {
		t.Fatal("continuation duplicated, omitted or crossed owner scope", tail, err)
	}
	nodes, err = i.handleCandidateGet(ctx, map[string]any{"candidateId": keys[2]}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var recovered candidateRecord
	if err := json.Unmarshal(nodes[0].Payload, &recovered); err != nil || recovered.ApprovalID != approval.ApprovalID {
		t.Fatal("review did not recover the durable approval", err)
	}
	if _, err := i.handleCandidateGet(ctx, map[string]any{"candidateId": foreign.ID}, 0); err == nil {
		t.Fatal("owner read another owner's candidate")
	}
	if _, err := db.Exec(`UPDATE release_candidates SET manifest=jsonb_set(manifest,'{components,0,version}','"99.0.0"'::jsonb) WHERE candidate_id=$1`, keys[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.list(ctx, page.NextCursor, 2); err == nil {
		t.Fatal("history displayed a tampered manifest under its old identity")
	}
}

func TestCandidateHistoryBoundsBeforeDatabase(t *testing.T) {
	i := NewIntegration(nil, nil, resolver{})
	i.candidateDB = func() *sql.DB { t.Fatal("invalid history request reached database"); return nil }
	for _, args := range []map[string]any{
		{"limit": 0}, {"limit": 51}, {"limit": 1.5}, {"limit": "2"}, {"limit": 1e40},
		{"cursor": 12}, {"cursor": "not-a-cursor"}, {"cursor": strings.Repeat("a", 1025)},
		{"cursor": base64.RawURLEncoding.EncodeToString([]byte(`{"createdAt":"2026-10-07T00:00:00Z","id":"sha256:x"}`))},
	} {
		if _, err := i.handleCandidateList(ownerCtx(), args, 0); err == nil {
			t.Fatal("invalid history request accepted", args)
		}
	}
}
