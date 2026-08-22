//go:build clustere2e

package clustere2e

// Cross-node DEFAULT-ON result caching, LIVE (epic5 5.6 / memql#1970).
//
// CONTRACT under test. 5.6 graduates pure reads to cache BY DEFAULT --
// no per-query @cache annotation -- and decouples cross-node eviction
// from per-concept routing rules: every graph write emits a dedicated
// cache.invalidate.<concept> event, forwarded to every replica by a
// SINGLE broadcast routing rule (cache.invalidate.*), and ONLY the
// result-cache evictor consumes it. So a default-cached read on replica
// B must be evicted when a write to its concept happens on replica A,
// with ZERO per-concept routing rules.
//
// notes carries NO @cache annotation, so it is cached purely by the 5.6
// default. Its concept (v1:notes:note) is NOT denylisted, has NO per-concept
// routing rule in component/node/routing.go (v1:cognition:* is forwarded
// wholesale, which is why a space-keyed read could not prove this), and the
// query is owner-scoped (ownerUserId == actor.userId), so this also exercises
// the actor-folded cache key (a stale read here would be BOTH a default-on
// regression AND a cross-user-collision regression).
//
// The read used to be the product pack's queryActiveSpaces, which the engine
// tree does not declare and the parity cluster does not load (memql#4212);
// notes is the engine-owned owner-scoped read with the same shape.
//
// The deterministic, always-in-CI proof of the same wired path -- two
// engines, two buses, the real EventBridge forward + inbound republish
// gated by the real cache.invalidate.* broadcast rule, with NO
// per-concept rule -- lives in component/node
// TestResultCacheInvalidation_CrossNode.
//
// RUN
//
//	MEMQL_E2E_TOKEN=<user JWT> go test -tags clustere2e -count=1 \
//	  -timeout=300s ./test/clustere2e/... -run TestResultCacheDefaultOn_CrossReplica
//
// or `make cluster-e2e`.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

func TestResultCacheDefaultOn_CrossReplica(t *testing.T) {
	tok := token(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// Several connections so a warm-read and a write land on different
	// replicas behind nginx with high probability. Read on connB, write on
	// connA.
	conns := openConnections(ctx, t, tok, 4)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	connA, connB := conns[0], conns[1]

	qcA := memqlclient.NewQueryClient(connA.Dispatcher())
	qcB := memqlclient.NewQueryClient(connB.Dispatcher())

	createNote := func(title string) string {
		noteID := "v1:notes:note:" + id.NewShortId()
		if _, err := qcA.CreateNote(ctx, memqlclient.CreateNoteArgs{
			NoteId: noteID,
			Title:  title,
			Body:   "clustere2e default-on cache probe",
		}); err != nil {
			t.Fatalf("create note: %v", err)
		}
		return noteID
	}

	// Seed a couple of notes so the read returns a stable first page.
	const seed = 2
	for i := 0; i < seed; i++ {
		createNote(fmt.Sprintf("default-on cache seed %02d", i))
		time.Sleep(15 * time.Millisecond)
	}

	noteCount := func(qc *memqlclient.QueryClient) int {
		res, err := qc.Notes(ctx, memqlclient.NotesArgs{})
		if err != nil {
			t.Fatalf("notes: %v", err)
		}
		return len(res.Rows())
	}

	// Warm the DEFAULT cache on connB: two reads so the second is a HIT on
	// the serving replica(s). notes has NO @cache annotation -- it is cached
	// only because of the 5.6 default-on policy.
	warm1 := noteCount(qcB)
	if warm1 < seed {
		t.Fatalf("warm read 1 returned %d notes, want >= %d seeded", warm1, seed)
	}
	_ = noteCount(qcB) // second read -> default-cache HIT (warms only).

	// WRITE a new note on connA (typically a different replica than connB).
	// This is graph.node.created.v1:notes:note -> it ALSO emits
	// cache.invalidate.v1:notes:note, forwarded cross-node by the SINGLE
	// cache.invalidate.* broadcast rule (no per-concept rule for
	// v1:notes:note exists), so connB's evictor fires and drops the
	// default-cached notes result.
	newID := createNote("default-on cache POST-WRITE note")

	// The post-write read on connB MUST reflect the new note. If
	// invalidation did not evict the default cache cross-node, connB would
	// serve the stale page and miss newID -- a silent cross-node stale read.
	// We poll only a few seconds, far under the 60s default TTL, so a pass
	// proves eviction (not TTL lapse). notes sorts newest-first, so the new
	// row is on the first page whatever else the user owns.
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := qcB.Notes(ctx, memqlclient.NotesArgs{})
		if err != nil {
			t.Fatalf("post-write notes: %v", err)
		}
		found := false
		for _, row := range res.Rows() {
			if bareID(rowID(row)) == bareID(newID) { // #2441: bare wire ids
				found = true
				break
			}
		}
		if found {
			if len(res.Rows()) < seed+1 {
				t.Fatalf("post-write read shows the new note but only %d total, want >= %d", len(res.Rows()), seed+1)
			}
			return // SUCCESS: default cache evicted cross-node via the broadcast channel.
		}
		if time.Now().After(deadline) {
			t.Fatalf("post-write read on connB never reflected new note %s within 5s "+
				"(well under the 60s default TTL) -- cross-node DEFAULT-ON cache invalidation did not "+
				"evict the stale notes result on the sibling replica via the cache.invalidate.* "+
				"broadcast channel (with zero per-concept routing rules)", newID)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
