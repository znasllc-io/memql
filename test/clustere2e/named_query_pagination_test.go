//go:build clustere2e

// named_query_pagination_test.go is the CROSS-NODE proof for issue 5.2
// (memql#1966): the three known hot offenders now declare `sort` + `paginate`
// and must page through the KEYSET CURSOR primitive (5.12 / memql#1985) over
// the live NAMED-QUERY surface -- not the inline `sort(paginate(...))` raw
// string the 5.12 codec proof (keyset_cursor_test.go) exercises.
//
// WHY A SEPARATE TEST FROM keyset_cursor_test.go
// ----------------------------------------------
// keyset_cursor_test.go proves the engine primitive against a handwritten
// query string. THIS test proves the actual authored queries
// (`spaceUtterances`, `queryActiveSpaces`) -- the strings the generated
// SDK *Build helpers emit -- carry the sort+paginate directives end-to-end and
// thread the opaque cursor through `ExecuteQueryMsg.cursor` / `ResultMeta.cursor`
// on the generic executeNamed path. If a future edit drops the paginate
// directive from one of the offenders, this test regresses; the 5.12 codec
// test would not.
//
// HOW IT EXERCISES THE HOP
// ------------------------
// nginx round-robins each new gRPC connection across the bff replicas, so two
// separate connections (connA, connB) typically land on different replicas.
// We seed an ordered set in a fresh space, take PAGE 1 (+ its nextCursor) on
// connA via the named query, then replay that cursor on connB and assert
// PAGE 2 continues with no overlap (no dup) and no gap -- regardless of which
// replica minted vs. resolved the cursor. The cursor carries only the keyset
// position (createdAt, id) + a sort-signature guard, no server session state,
// so it is replica-agnostic by construction.
//
// RUN
//
//	MEMQL_E2E_TOKEN=<user JWT> go test -tags clustere2e -count=1 \
//	  -timeout=300s ./test/clustere2e/... -run TestNamedQueryPaginationCrossNode
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

// assertNoDupNoGap checks the walked ids equal the expected newest-first set
// exactly -- which simultaneously rules out overlap (a dup would make the walk
// longer than the set) and gaps (a skip would break the position match).
func assertNoDupNoGap(t *testing.T, got, wantNewestFirst []string) {
	t.Helper()
	seen := map[string]int{}
	for _, idv := range got {
		seen[idv]++
		if seen[idv] > 1 {
			t.Fatalf("cross-node walk produced an OVERLAP: %s appears %d times", idv, seen[idv])
		}
	}
	if len(got) != len(wantNewestFirst) {
		t.Fatalf("walk returned %d rows, want %d (gap or truncation across the replica hop)", len(got), len(wantNewestFirst))
	}
	for i, want := range wantNewestFirst {
		if got[i] != want {
			t.Fatalf("walk position %d = %s, want %s (gap/order break across the replica hop)", i, got[i], want)
		}
	}
}

// TestNamedQueryPaginationCrossNode proves the 5.2 hot-offender NAMED queries
// page via the keyset cursor across replicas: a bounded first page + cursor,
// then a clean cross-node continuation with no dup / no gap.
func TestNamedQueryPaginationCrossNode(t *testing.T) {
	tok := token(t)
	userID := userIDFromToken(t, tok)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	conns := openConnections(ctx, t, tok, 2)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	connA, connB := conns[0], conns[1]

	// namedPageSize MUST match the `paginate <N>` literal authored on the three
	// hot-offender queries in dsl/. The cross-node proof seeds strictly more than
	// this so the NAMED query itself mints a real nextCursor on its full first
	// page (rather than exhausting the set in one page).
	const namedPageSize = 50

	t.Run("spaceUtterances", func(t *testing.T) {
		qcA := memqlclient.NewQueryClient(connA.Dispatcher())
		spaceID, participantID := newSpaceWithHuman(ctx, t, connA, userID)

		// Seed > one page so the named query mints + we resolve a real cursor.
		const total = namedPageSize + 7
		sent := make([]string, 0, total) // oldest-first send order.
		for i := 0; i < total; i++ {
			uid := "v1:cognition:utterance:" + id.NewShortId()
			if _, err := qcA.SendTextUtterance(ctx, memqlclient.SendTextUtteranceArgs{
				UtteranceId:     uid,
				PartitionId:     spaceID,
				ParticipantId:   participantID,
				ParticipantType: "human",
				Text:            fmt.Sprintf("named-query pagination probe %03d", i),
			}); err != nil {
				t.Fatalf("send utterance %d: %v", i, err)
			}
			sent = append(sent, uid)
			time.Sleep(8 * time.Millisecond) // strictly increasing createdAt.
		}

		// The SDK *Build helper emits the exact authored named-query call string;
		// the sort+paginate directives are baked into the query DEFINITION, so the
		// cursor rides ExecuteQueryMsg.cursor and we never pass a page size.
		query := memqlclient.SpaceUtterancesBuild(memqlclient.SpaceUtterancesArgs{
			PartitionId: spaceID,
		})

		// PAGE 1 on connA (replica X mints the cursor from the named query).
		page1, err := qcA.ExecutePaginated(ctx, query, "")
		if err != nil {
			t.Fatalf("named query page 1: %v", err)
		}
		if len(page1.Rows) != namedPageSize {
			t.Fatalf("named query first page returned %d rows, want the bounded %d", len(page1.Rows), namedPageSize)
		}
		if page1.NextCursor == "" {
			t.Fatal("a full first page of the named query must mint a nextCursor")
		}

		// PAGE 2 on connB (replica Y resolves the named query's cursor).
		page2, err := memqlclient.NewQueryClient(connB.Dispatcher()).
			ExecutePaginated(ctx, query, page1.NextCursor)
		if err != nil {
			t.Fatalf("named query page 2 (cross-node cursor): %v", err)
		}
		if len(page2.Rows) != total-namedPageSize {
			t.Fatalf("named query page 2 returned %d rows, want %d (remainder)", len(page2.Rows), total-namedPageSize)
		}

		got := append(utteranceRowIDs(page1.Rows), utteranceRowIDs(page2.Rows)...)
		wantNewestFirst := make([]string, 0, total)
		for i := len(sent) - 1; i >= 0; i-- {
			wantNewestFirst = append(wantNewestFirst, sent[i])
		}
		assertNoDupNoGap(t, got, wantNewestFirst)
	})

	t.Run("queryActiveSpaces", func(t *testing.T) {
		qcA := memqlclient.NewQueryClient(connA.Dispatcher())

		// Seed > one page of active spaces owned by this user so the named query
		// mints a real cursor on its full first page. Each create stamps
		// ownerUserId=actor.userId, satisfying queryActiveSpaces' authz gate.
		const total = namedPageSize + 6
		sent := make(map[string]struct{}, total)
		for i := 0; i < total; i++ {
			sid := "v1:cognition:space:" + id.NewShortId()
			if _, err := qcA.ExecuteNamed(ctx, "mutationCreateSpace", buildMutationCreateSpace(sid, fmt.Sprintf("named-query space probe %03d", i), "active")); err != nil {
				t.Fatalf("create space %d: %v", i, err)
			}
			sent[sid] = struct{}{}
			time.Sleep(8 * time.Millisecond)
		}

		// queryActiveSpaces is self-scoped (filters on ownerUserId==actor.userId),
		// so the result is bounded to OUR spaces -- but the cluster may carry
		// other active spaces this user owns from prior runs. We therefore assert
		// page mechanics (bounded first page + cursor) and that every seeded id is
		// walked exactly once with no dup, rather than an exact total count.
		query := buildQueryActiveSpaces()

		qcB := memqlclient.NewQueryClient(connB.Dispatcher())
		cursor := ""
		walked := map[string]struct{}{}
		var firstPageLen int
		var mintedCursor bool
		for page := 0; ; page++ {
			qc := qcB
			if page == 0 {
				qc = qcA // mint on A, resolve later pages on B.
			}
			res, err := qc.ExecutePaginated(ctx, query, cursor)
			if err != nil {
				t.Fatalf("active-spaces named query page %d: %v", page, err)
			}
			if page == 0 {
				firstPageLen = len(res.Rows)
			}
			if len(res.Rows) > namedPageSize {
				t.Fatalf("active-spaces page %d returned %d rows, exceeds the bounded %d", page, len(res.Rows), namedPageSize)
			}
			for _, r := range res.Rows {
				rid := rowID(r)
				if _, dup := walked[rid]; dup {
					t.Fatalf("active-spaces cross-node walk OVERLAP on %s", rid)
				}
				walked[rid] = struct{}{}
			}
			if res.NextCursor == "" {
				break
			}
			mintedCursor = true
			cursor = res.NextCursor
			if page > 64 {
				t.Fatal("active-spaces pagination did not terminate")
			}
		}
		// Every seeded space must appear exactly once across the cross-node walk.
		for sid := range sent {
			if _, ok := walked[sid]; !ok {
				t.Fatalf("active-spaces cross-node walk dropped seeded space %s (gap)", sid)
			}
		}
		// We seeded > namedPageSize, so the named query MUST have minted a cursor
		// (proving the bounded-page + continuation path on the real query def).
		if !mintedCursor {
			t.Fatalf("active-spaces seeded %d (> page %d) but the named query never minted a cursor", total, namedPageSize)
		}
		if firstPageLen != namedPageSize {
			t.Fatalf("active-spaces first page size = %d, want the bounded %d", firstPageLen, namedPageSize)
		}
	})
}
