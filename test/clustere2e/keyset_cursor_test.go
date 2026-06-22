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
// We seed an ordered set of utterances in a fresh space, take PAGE 1 (+ its
// nextCursor) on connA, then replay that cursor on connB and assert PAGE 2:
//   - does not overlap page 1 (no dup),
//   - continues the descending-createdAt order from exactly where page 1 left
//     off (no gap),
//   - is reproducible regardless of which replica served the mint vs. the
//     resolve.
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

// keysetSpaceQuery pages a space's utterances newest-first. The space id is
// already canonical (minted by mutationCreateSpace), so the FK comparison on
// payload.spaceId matches the stored canonical value. The cursor rides the
// request via ExecutePaginated, not the query string.
func keysetSpaceQuery(spaceID string, pageSize int) string {
	return fmt.Sprintf(
		`sort(paginate(concept==v1:cognition:utterance;payload.spaceId==%q, %d), "createdAt", "desc")`,
		spaceID, pageSize)
}

func utteranceRowIDs(rows []memqlclient.Row) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if pid := rowID(r); pid != "" {
			ids = append(ids, pid)
		}
	}
	return ids
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

	// Fresh space + human, seeded on connA.
	spaceID, participantID := newSpaceWithHuman(ctx, t, connA, userID)

	// Seed an ordered set of utterances. Each send is its own row; the engine
	// stamps createdAt at insert, monotonically increasing, so the newest-first
	// page order is deterministic by send order.
	const total = 12
	sent := make([]string, 0, total) // in send order (oldest first)
	for i := 0; i < total; i++ {
		uid := "v1:cognition:utterance:" + id.NewShortId()
		if _, err := qcA.MutationSendTextUtterance(ctx, memqlclient.MutationSendTextUtteranceArgs{
			UtteranceId:     uid,
			SpaceId:         spaceID,
			ParticipantId:   participantID,
			ParticipantType: "human",
			Text:            fmt.Sprintf("keyset cross-node probe %02d", i),
		}); err != nil {
			t.Fatalf("send utterance %d: %v", i, err)
		}
		sent = append(sent, uid)
		// Small spacing so createdAt is strictly increasing even at coarse
		// timestamp resolution; keeps the (createdAt, id) order unambiguous.
		time.Sleep(15 * time.Millisecond)
	}

	const pageSize = 5

	// PAGE 1 on connA (replica X mints the cursor).
	page1, err := qcA.ExecutePaginated(ctx, keysetSpaceQuery(spaceID, pageSize), "")
	if err != nil {
		t.Fatalf("page 1 (connA): %v", err)
	}
	page1IDs := utteranceRowIDs(page1.Rows)
	if len(page1IDs) != pageSize {
		t.Fatalf("page 1 returned %d rows, want %d", len(page1IDs), pageSize)
	}
	if page1.NextCursor == "" {
		t.Fatal("a full first page must mint a nextCursor")
	}

	// PAGE 2 on connB (replica Y resolves the cursor minted on X).
	page2, err := qcB.ExecutePaginated(ctx, keysetSpaceQuery(spaceID, pageSize), page1.NextCursor)
	if err != nil {
		t.Fatalf("page 2 (connB, cursor from connA): %v", err)
	}
	page2IDs := utteranceRowIDs(page2.Rows)
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
		if gotWalk[i] != want {
			t.Fatalf("cross-node walk position %d = %s, want %s (gap/order break across the replica hop)", i, gotWalk[i], want)
		}
	}
}
