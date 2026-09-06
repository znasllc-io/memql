//go:build clustere2e

// keyset_cursor_test.go is the CROSS-NODE proof for keyset cursor pagination
// (epic 5, issue 5.12 / memql#1985): a continuation cursor minted on ONE bff
// replica must resolve identically on ANOTHER. The cursor encodes only the
// keyset position (createdAt, id) + a sort-signature guard -- it carries NO
// server session state -- so it is replica-agnostic by construction. This test
// is the live, mesh-topology confirmation of that property (the single-node
// codec + walk proofs live in component/memql/{cursor,keyset_pagination}_test.go).
//
// HOW IT EXERCISES THE HOP
// ------------------------
// nginx round-robins each new gRPC connection across the bff replicas, so two
// separate connections (connA, connB) typically land on different replicas.
// We seed an ordered set of rows in a fresh scope, take PAGE 1 (+ its
// nextCursor) on connA, then replay that cursor on connB and assert PAGE 2:
//   - does not overlap page 1 (no dup),
//   - continues the descending-createdAt order from exactly where page 1 left
//     off (no gap),
//   - is reproducible regardless of which replica served the mint vs. the
//     resolve.
//
// The rows are v1:planner:plan rather than the v1:cognition:utterance this
// suite used to seed (memql#4988 deleted cognition). The cursor primitive is
// concept-agnostic -- it keys on (createdAt, id) and a sort signature -- so
// the swap costs the proof nothing; what it needs from a concept is only that
// createdAt be strictly increasing across the seeded set, which the spacing
// below still guarantees.
//
// RUN
//
//	MEMQL_E2E_TOKEN=<user JWT> go test -tags clustere2e -count=1 \
//	  -timeout=300s ./test/clustere2e/... -run TestKeysetCursorCrossNode
//
// or `make cluster-e2e`.
package clustere2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

// keysetScopeQuery pages a scope's plans newest-first. The cursor rides the
// request via ExecutePaginated, not the query string.
func keysetScopeQuery(scope string, pageSize int) string {
	return fmt.Sprintf(
		`sort(paginate(concept==v1:planner:plan;payload.partitionId==%q, %d), "createdAt", "desc")`,
		scope, pageSize)
}

// TestKeysetCursorCrossNode proves a cursor minted on one replica resolves on
// another.
func TestKeysetCursorCrossNode(t *testing.T) {
	tok := token(t)
	userID := userIDFromToken(t, tok)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Two connections -> two (typically distinct) replicas behind nginx.
	conns := openConnections(ctx, t, tok, 2)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	connA, connB := conns[0], conns[1]

	qcA := memqlclient.NewQueryClient(connA.Dispatcher())
	qcB := memqlclient.NewQueryClient(connB.Dispatcher())

	// Fresh scope, seeded on connA.
	scope := newProbeScope()

	// Seed an ordered set of rows. Each create is its own row; the engine
	// stamps createdAt at insert, monotonically increasing, so the newest-first
	// page order is deterministic by send order.
	const total = 12
	sent := make([]string, 0, total) // in send order (oldest first)
	for i := 0; i < total; i++ {
		pid := "v1:planner:plan:" + id.NewShortId()
		if _, err := qcA.CreatePlan(ctx, probePlanArgs(scope, pid,
			fmt.Sprintf("keyset cross-node probe %02d", i), userID)); err != nil {
			t.Fatalf("seed plan %d: %v", i, err)
		}
		sent = append(sent, pid)
		// Small spacing so createdAt is strictly increasing even at coarse
		// timestamp resolution; keeps the (createdAt, id) order unambiguous.
		time.Sleep(15 * time.Millisecond)
	}

	const pageSize = 5

	// PAGE 1 on connA (replica X mints the cursor).
	page1, err := qcA.ExecutePaginated(ctx, keysetScopeQuery(scope, pageSize), "")
	if err != nil {
		t.Fatalf("page 1 (connA): %v", err)
	}
	page1IDs := rowIDs(page1.Rows)
	if len(page1IDs) != pageSize {
		t.Fatalf("page 1 returned %d rows, want %d", len(page1IDs), pageSize)
	}
	if page1.NextCursor == "" {
		t.Fatal("a full first page must mint a nextCursor")
	}

	// PAGE 2 on connB (replica Y resolves the cursor minted on X).
	page2, err := qcB.ExecutePaginated(ctx, keysetScopeQuery(scope, pageSize), page1.NextCursor)
	if err != nil {
		t.Fatalf("page 2 (connB, cursor from connA): %v", err)
	}
	page2IDs := rowIDs(page2.Rows)
	if len(page2IDs) != pageSize {
		t.Fatalf("page 2 returned %d rows, want %d", len(page2IDs), pageSize)
	}

	// No overlap between the two pages (the cross-node continuation excludes
	// everything page 1 already returned).
	seen := map[string]struct{}{}
	for _, idv := range page1IDs {
		seen[idv] = struct{}{}
	}
	for _, idv := range page2IDs {
		if _, dup := seen[idv]; dup {
			t.Fatalf("cross-node cursor produced an OVERLAP: %s appears on both pages", idv)
		}
	}

	// No gap: the full descending-createdAt set is sent newest-first, so pages
	// 1+2 must equal the 10 newest sent ids in reverse send order.
	wantNewestFirst := make([]string, 0, total)
	for i := len(sent) - 1; i >= 0; i-- {
		wantNewestFirst = append(wantNewestFirst, sent[i])
	}
	gotWalk := append(append([]string{}, page1IDs...), page2IDs...)
	for i, want := range wantNewestFirst[:2*pageSize] {
		// #2441: query results now carry BARE ids; compare on the bare form.
		if bareID(gotWalk[i]) != bareID(want) {
			t.Fatalf("cross-node walk position %d = %s, want %s (gap/order break across the replica hop)", i, gotWalk[i], want)
		}
	}
}
