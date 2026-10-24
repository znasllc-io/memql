package release

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
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
	// Listing stays entirely independent of live configuration and blob wiring.
	// Detail may add fresh public catalog status, but unavailable trust must not
	// hide the owner's immutable record or durable approval.
	catalogAllowed := false
	catalogCalls := 0
	catalogRaw := ""
	var catalogError error
	i := NewIntegration(nil, &tripwireEngine{t: t}, resolver{
		systemVariable: func(ctx context.Context, name string) (string, error) {
			if !catalogAllowed {
				t.Fatal("history list required live configuration")
			}
			if name != ReleaseCatalogConfigurationVariable || !memql.FreshReadFromContext(ctx) {
				t.Fatal("history detail requested non-catalog or cached configuration")
			}
			catalogCalls++
			return catalogRaw, catalogError
		},
		systemSecret: func(context.Context, string) (string, error) {
			t.Fatal("history read requested secrets")
			return "", nil
		},
	})
	i.candidateDB = func() *sql.DB { return db }
	nodes, err := i.handleCandidateList(ctx, map[string]any{"cursor": page.NextCursor, "limit": float64(2)}, 0)
	if err != nil || len(nodes) != 1 {
		t.Fatal("history without object storage", err)
	}
	var tail candidatePage
	if err := json.Unmarshal(nodes[0].Payload, &tail); err != nil || len(tail.Candidates) != 1 || tail.Candidates[0].ID != keys[2] || tail.Candidates[0].State != "approved" || tail.NextCursor != "" || catalogCalls != 0 {
		t.Fatal("continuation duplicated, omitted, crossed owner scope or read configuration", tail, err)
	}
	catalogAllowed = true
	for _, tc := range []struct {
		name, raw, state string
		err              error
	}{
		{name: "resolver_unavailable", err: errors.New("catalog unavailable"), state: "unavailable"},
		{name: "missing_configuration", state: "unavailable"},
		{name: "malformed_configuration", raw: `{`, state: "unavailable"},
		{name: "invalid_trust", raw: `{"formatVersion":1,"publisher":"fixture","publicKeys":{"fixture-key":"invalid"}}`, state: "unavailable"},
		{name: "verified_lookup_absent", raw: `{"formatVersion":1,"publisher":"fixture","publicKeys":{"fixture-key":"` + base64.StdEncoding.EncodeToString(make([]byte, 32)) + `"}}`, state: "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalogRaw, catalogError = tc.raw, tc.err
			before := catalogCalls
			nodes, err := i.handleCandidateGet(ctx, map[string]any{"candidateId": keys[2]}, 0)
			if err != nil || len(nodes) != 1 {
				t.Fatal("catalog availability hid owner history", err)
			}
			var recovered struct {
				candidateRecord
				Catalog candidateCatalogStatus `json:"catalog"`
			}
			if err := json.Unmarshal(nodes[0].Payload, &recovered); err != nil || !reflect.DeepEqual(recovered.candidateRecord, approval) || recovered.Catalog != (candidateCatalogStatus{State: tc.state}) || catalogCalls != before+1 {
				t.Fatal("history changed immutable identity, approval or honest catalog status", recovered, err)
			}
			before = catalogCalls
			if _, err := i.handleCandidateGet(ctx, map[string]any{"candidateId": foreign.ID}, 0); err == nil || catalogCalls != before {
				t.Fatal("foreign history reached catalog configuration or crossed owner scope", err)
			}
		})
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
