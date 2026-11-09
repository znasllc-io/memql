package identity

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// legal_acceptance_db_test.go -- the cross-replica half of recording legal
// acceptance (memql#5431's shape of the problem).
//
// Two identity replicas can finish two sign-ins of one person within
// milliseconds of each other: the same browser retrying, two tabs, a passkey
// login beside a device-code redemption. Each reads the history, finds the
// version in force unrecorded, and writes it. Without coordination both
// writes land and the person's record says they accepted the same version
// twice, or -- the lost-update shape -- the second write replaces the first
// with an array that never saw it.
//
// POSTGRES-GATED BECAUSE THE THING UNDER TEST IS A POSTGRES ADVISORY LOCK.
// The engine is faked and the fake sleeps between the read and the write so
// the window a real engine leaves open for microseconds is one a scheduler
// cannot miss. The control test below runs the same harness unlocked and
// expects the race, so a pass here is known to be the lock working.

func openDirectDBForLegal(t *testing.T) *sql.DB {
	t.Helper()
	dsn := dbtest.DSN()
	bdb := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := bdb.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "legal acceptance once-per-version across replicas", dsn, err)
		return nil
	}
	t.Cleanup(func() { _ = bdb.Close() })
	return bdb.DB
}

func raceLegalRecorders(store *Store, user string, n int) (writes int, recordedBy int, errs []error) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs = make([]error, n)
	results := make([][]LegalAcceptanceEntry, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = store.RecordLegalAcceptance(context.Background(), user, legalInForce, "203.0.113.9", legalNow)
		}(i)
	}
	close(start)
	wg.Wait()
	for _, r := range results {
		if len(r) > 0 {
			recordedBy++
		}
	}
	return store.Engine.(*legalFakeEngine).writeCount(), recordedBy, errs
}

func TestConcurrentSignInsRecordEachVersionOnce(t *testing.T) {
	db := openDirectDBForLegal(t)
	if db == nil {
		return
	}
	const user = "v1:identity:user:legal-cas-5431"
	eng := &legalFakeEngine{userId: user, version: legalNow.Add(-time.Hour), readWriteGap: 250 * time.Millisecond}
	store := &Store{Engine: eng, DirectDB: func() *sql.DB { return db }}

	writes, recordedBy, errs := raceLegalRecorders(store, user, 4)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("recorder %d: %v", i, err)
		}
	}
	if len(eng.unknown) > 0 {
		t.Fatalf("the recorder issued constructs the fake does not model: %s", strings.Join(eng.unknown, "; "))
	}
	if writes != 1 {
		t.Errorf("%d writes reached the engine for 4 concurrent sign-ins, want 1.\n"+
			"Each extra write is the same version recorded again, or an array written from a read "+
			"that never saw a sibling's entry. The lock serialises the read and the write; the "+
			"next holder re-reads and finds nothing due.", writes)
	}
	if recordedBy != 1 {
		t.Errorf("%d recorders reported having recorded, want 1", recordedBy)
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.entries) != 2 {
		t.Errorf("the row ends with %d entries, want 2 (one per document)", len(eng.entries))
	}
}

// TestConcurrentSignInsWithoutTheGateRace proves the test above measures the
// lock: the same harness, no DirectDB, must double-write. Against the real
// engine the row-version fence would refuse the later writes (covered by
// TestRecordLegalAcceptanceRereadsAfterAConcurrentWrite); the fake has no
// fence, which is what makes the race visible here.
func TestConcurrentSignInsWithoutTheGateRace(t *testing.T) {
	const user = "v1:identity:user:legal-unlocked-5431"
	eng := &legalFakeEngine{userId: user, version: legalNow.Add(-time.Hour), readWriteGap: 250 * time.Millisecond}
	store := &Store{Engine: eng} // no DirectDB: no lock
	writes, _, _ := raceLegalRecorders(store, user, 4)
	if writes < 2 {
		t.Fatalf("the unlocked path produced %d writes; expected the race to double-write.\n"+
			"This test exists to prove the gated test is measuring the advisory lock. If the race "+
			"cannot be reproduced here, a green result there says nothing.", writes)
	}
}

// TestRecordLegalAcceptanceAgainstARealEngine is the round trip the fakes
// cannot see: the real DSL tree's shape projecting row.createdAt as the
// version, the @serverOnly gates admitting the recorder's internal origin,
// the row-version fence inside the engine's own transaction, and update{}
// replacing the array whole. A sign-in, a second sign-in on the same
// versions, then new terms.
func TestRecordLegalAcceptanceAgainstARealEngine(t *testing.T) {
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "legal acceptance real-engine round trip", dsn, err)
		return
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memqlengine.New(db)
	if err != nil {
		t.Fatalf("engine New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine Init: %v", err)
	}
	store := &Store{Engine: eng, Logger: eng.Logger, DirectDB: func() *sql.DB { return db.DB }}

	stamp := time.Now().UTC().Format("20060102T150405.000000000")
	userId := "v1:identity:user:legal-" + stamp
	// The identity service's requests carry the system actor (middleware.go),
	// which is what createdBy stamping needs on this path too.
	ctx := ContextWithSystemCredentialActor(context.Background())
	if err := store.CreateUserOnFirstLogin(ctx, userId, "Legal Tester", "legal-"+stamp+"@example.test", "reader", false, UserProfileSeed{}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	first, err := store.RecordLegalAcceptance(ctx, userId, legalInForce, "203.0.113.5", legalNow)
	if err != nil {
		t.Fatalf("first sign-in: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("first sign-in recorded %d entries, want 2: %+v", len(first), first)
	}
	again, err := store.RecordLegalAcceptance(ctx, userId, legalInForce, "203.0.113.6", legalNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("second sign-in: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second sign-in on the same versions recorded %+v, want nothing", again)
	}
	newer := []LegalDocumentVersion{{DocumentType: "tos", Version: "2.1.0"}, {DocumentType: "privacy", Version: "2.0.0"}}
	third, err := store.RecordLegalAcceptance(ctx, userId, newer, "203.0.113.7", legalNow.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("sign-in after new terms: %v", err)
	}
	if len(third) != 1 || third[0].DocumentType != "tos" || third[0].Version != "2.1.0" {
		t.Fatalf("sign-in after new terms recorded %+v, want tos 2.1.0 alone", third)
	}

	entries, version, err := store.LegalAcceptanceForUser(ctx, userId)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("the row holds %d entries, want 3 (two from the first sign-in, one from the new terms): %+v", len(entries), entries)
	}
	if entries[0].DocumentType != "tos" || entries[0].Version != "2.0.0" || entries[0].SourceIP != "203.0.113.5" ||
		entries[1].DocumentType != "privacy" || entries[2].Version != "2.1.0" || entries[2].SourceIP != "203.0.113.7" {
		t.Errorf("entries = %+v, want the first sign-in's two kept verbatim and tos 2.1.0 appended", entries)
	}
	if version.IsZero() {
		t.Errorf("the read carries no row version; the fence would have nothing to compare")
	}
}
