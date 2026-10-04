package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/znasllc-io/memql/component/database"
	memoryNodesDatabase "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestRunMigrateSubcommand_Help exits 0 for the help flags without touching
// the database (the arg loop returns before app.Build).
func TestRunMigrateSubcommand_Help(t *testing.T) {
	for _, arg := range []string{"--help", "-h", "help"} {
		if code := runMigrateSubcommand([]string{arg}); code != 0 {
			t.Errorf("migrate %s: got exit %d, want 0", arg, code)
		}
	}
}

// TestRunMigrateSubcommand_UnexpectedArg rejects unknown args with exit 2
// (a usage error) rather than proceeding to build + migrate.
func TestRunMigrateSubcommand_UnexpectedArg(t *testing.T) {
	if code := runMigrateSubcommand([]string{"--bogus"}); code != 2 {
		t.Errorf("migrate --bogus: got exit %d, want 2", code)
	}
}

// The subcommand finds the database by asserting restartableDatabase on the
// dependency, and a dependency that stopped satisfying it would skip the
// migration check altogether -- exiting 0 over a failed migration, the #671
// shape. So the real one is held to it here, at compile time.
var _ restartableDatabase = (*memoryNodesDatabase.MemoryNodesDatabase)(nil)

// fakeMigratingDatabase answers MigrationError from a script, one answer per
// start: the first is the start the subcommand made before asking, each later
// one a start awaitMigrations made. The last answer repeats.
type fakeMigratingDatabase struct {
	answers       []error
	starts, stops int
}

func (f *fakeMigratingDatabase) Start(context.Context) { f.starts++ }
func (f *fakeMigratingDatabase) Stop(context.Context)  { f.stops++ }
func (f *fakeMigratingDatabase) MigrationError() error {
	return f.answers[min(f.starts, len(f.answers)-1)]
}

// A MIGRATION THAT DEFERS IS GIVEN ANOTHER ATTEMPT IN-PROCESS (memql#5604). The
// readiness collapse can need more than one attempt, and ends each but the last
// with database.ErrMigrationDeferred: progress committed, nothing left running.
// Exiting non-zero on that would leave the migrate Job's backoffLimit -- 2 --
// to decide whether a long walk finishes, failing `make up` at its migrate step
// while the walk was still making progress. Any other failure still ends the
// subcommand at once, and a walk that never finishes still ends at the bound.
func TestAwaitMigrationsStartsAnotherAttemptWhileAMigrationDefers(t *testing.T) {
	deferred := fmt.Errorf("migrate: 20260921000000: up: module readiness history collapse %w: 8s was left", database.ErrMigrationDeferred)
	failed := errors.New("migrate: 20260922000000: up: ERROR: relation does not exist")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		name     string
		answers  []error
		attempts int
		// want is nil for success, or an error the outcome must wrap.
		want error
		// restarts is how many times the database must be stopped and started.
		restarts int
	}{
		{name: "applied at once", answers: []error{nil}, attempts: 5},
		{name: "applied after two deferrals", answers: []error{deferred, deferred, nil}, attempts: 5, restarts: 2},
		{name: "a failure is not retried", answers: []error{failed}, attempts: 5, want: failed},
		{name: "a failure after a deferral ends it", answers: []error{deferred, failed}, attempts: 5, want: failed, restarts: 1},
		{name: "a walk deferring every attempt ends at the bound", answers: []error{deferred}, attempts: 3, want: database.ErrMigrationDeferred, restarts: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeMigratingDatabase{answers: tc.answers}
			err := awaitMigrations(context.Background(), logger, db, tc.attempts, 0)
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("got %v, want the migrations applied", err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("got %v, want an error wrapping %v", err, tc.want)
			}
			if db.starts != tc.restarts || db.stops != tc.restarts {
				t.Fatalf("the database was stopped %d and started %d times, want %d each", db.stops, db.starts, tc.restarts)
			}
		})
	}
}
