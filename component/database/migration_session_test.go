package database

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
)

// A MIGRATION SESSION NEVER OPENS A SECOND CONNECTION. The lock is held by the
// session's backend, so a pool that reconnected would run the attempt's next
// statement on a backend holding nothing while another replica took the lock.
func TestAMigrationSessionNeverOpensASecondConnection(t *testing.T) {
	c := &migrationSessionConnector{inner: pingableConnector{}}
	first, err := c.Connect(context.Background())
	if err != nil {
		t.Fatalf("the session's first connection: %v", err)
	}
	defer first.Close()
	if _, err := c.Connect(context.Background()); !errors.Is(err, errMigrationSessionEnded) {
		t.Fatalf("a second connection was opened, or refused for another reason: %v", err)
	}
	// What a refusal means to the migrations that read it: their statement may
	// still be running on the lost connection's backend.
	if !migrationMayStillBeRunning(errMigrationSessionEnded) {
		t.Fatal("a lost migration session does not read as a statement that may still be running")
	}
}

// ON THE MIGRATION SESSION, A COLLAPSE STEP CUT OFF MIDWAY DEFERS AT ONCE. The
// collapse used to wait, through its pool, until the cut step's backend had no
// transaction open, so that nothing overlapped it. On the migration session
// there is no other connection to ask on, and nothing to wait for: that
// backend holds the migration lock until its transaction ends, so the next
// attempt cannot start before it. Waiting would end the attempt as a failure
// after 30 s, which the migrate Job reads as fatal instead of as progress.
func TestACutCollapseStepOnTheMigrationSessionDefers(t *testing.T) {
	ended := sql.OpenDB(&migrationSessionConnector{inner: pingableConnector{}, opened: true})
	defer ended.Close()
	c := &readinessCollapse{db: bun.NewDB(ended, pgdialect.New()), logger: slog.New(slog.DiscardHandler), started: time.Now()}

	began := time.Now()
	err := c.awaitOrphan(4242, errors.New("read tcp 10.0.0.1:5432: i/o timeout"))
	if !errors.Is(err, errReadinessCollapseDeferred) {
		t.Fatalf("a cut step on an ended migration session did not defer: %v", err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("the deferral waited %v for a backend it cannot ask about", took)
	}
}
