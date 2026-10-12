package database

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun/migrate"
)

// recipient_email_digest_test.go -- the parts of the recipient digest backfill
// (memql#6022, F9) that need no database. The walk itself is in
// recipient_email_digest_db_test.go.

// THE BACKFILL IS GO, REGISTERED ONCE, AND NAMED FROM ITS FILE. bun derives a Go
// migration's version and comment from the file its Register call sits in, so a
// registration moved into another file would run under the wrong version --
// possibly one a cluster has already recorded as applied. And bun's Register
// appends without looking, so a set registered twice would run it twice.
func TestTheRecipientEmailDigestBackfillIsAGoMigrationRegisteredOnceUnderItsFileName(t *testing.T) {
	m := migrate.NewMigrations()
	registerTimescaleMigrations(m, nil)
	registerTimescaleMigrations(m, nil)

	version, comment, _ := strings.Cut(recipientEmailDigestMigrationName, "_")
	var got []migrate.Migration
	for _, mig := range m.Sorted() {
		if mig.Name == version {
			got = append(got, mig)
		}
	}
	if len(got) != 1 {
		t.Fatalf("migration %s is registered %d times, want exactly once", version, len(got))
	}
	if got[0].Comment != comment {
		t.Errorf("migration %s carries comment %q, want %q: bun names a Go migration after the file its Register "+
			"call sits in, and this one no longer sits in %s.go", version, got[0].Comment, comment, recipientEmailDigestMigrationName)
	}
	if got[0].Up == nil || got[0].Down == nil {
		t.Errorf("migration %s: Up set = %v, Down set = %v; both must be functions", version, got[0].Up != nil, got[0].Down != nil)
	}
	leftover, err := fs.Glob(timescaleMigrationsFS, "memory-nodes/migrations/"+version+"_*")
	if err != nil {
		t.Fatal(err)
	}
	if len(leftover) != 0 {
		t.Errorf("the embedded migrations carry %v; %s is a Go migration", leftover, version)
	}
	// It runs after the migrations it reads through: the latest-row index the
	// walk is bounded by is built by 20260913000000 and verified by 20260914000000.
	position := map[string]int{}
	for i, mig := range m.Sorted() {
		position[mig.Name] = i
	}
	for _, index := range []string{"20260913000000", "20260914000000"} {
		if p, ok := position[index]; !ok || p > position[version] {
			t.Errorf("%s (position %d, present %v) must sort before %s (position %d): the backfill's cursor walk is an index scan on the index it builds",
				index, p, ok, version, position[version])
		}
	}
}

// TestRecipientEmailDigestFoldsCaseAndWhitespaceOnly: the stored format. The
// digest is the SHA-256 of the address trimmed and lower-cased -- nothing else --
// and an address that cannot be a mailbox has none. campaigns.EmailDigest is the
// original; component/campaigns holds the two together
// (TestRecipientDigestMigrationMatchesEmailDigest).
func TestRecipientEmailDigestFoldsCaseAndWhitespaceOnly(t *testing.T) {
	const person = "143b53b0840f6e1fcf8537a38513b59a9f326c72f577f17d1da67393eff1d413" // sha256("person@example.test")
	for _, spelling := range []string{"person@example.test", "PERSON@EXAMPLE.TEST", "  Person@Example.Test\n", "\tperson@EXAMPLE.test "} {
		if got := RecipientEmailDigest(spelling); got != person {
			t.Errorf("RecipientEmailDigest(%q) = %q, want %q", spelling, got, person)
		}
	}
	for _, other := range []string{"person+news@example.test", "per.son@example.test"} {
		if got := RecipientEmailDigest(other); got == person || got == "" {
			t.Errorf("RecipientEmailDigest(%q) = %q: sub-addressing and dots are the person's own distinctions", other, got)
		}
	}
	for _, unusable := range []string{"", "   ", "no-at-sign", "@leading", "trailing@", "a b@c.test", "line\nbreak@example.test"} {
		if got := RecipientEmailDigest(unusable); got != "" {
			t.Errorf("RecipientEmailDigest(%q) = %q, want empty: there is no mailbox to key", unusable, got)
		}
	}
}

// TestTheBackfillDefersInsteadOfBeingCutOff: an attempt with no time left, or
// one that was cancelled, ends BETWEEN statements with the deferral -- not with
// a context error, which the runner reads as "a statement may still be running"
// and keeps its migration lock for until an operator clears it
// (migrationMayStillBeRunning; the readiness collapse's memql#5604). No
// database is reached: the check comes before the first statement.
func TestTheBackfillDefersInsteadOfBeingCutOff(t *testing.T) {
	for name, ctx := range map[string]func() (context.Context, context.CancelFunc){
		"a deadline too near for one step": func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), time.Second)
		},
		"an attempt already cancelled": func() (context.Context, context.CancelFunc) {
			c, cancel := context.WithCancel(context.Background())
			cancel()
			return c, func() {}
		},
		"an attempt already out of time": func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, cancel := ctx()
			defer cancel()
			report, err := backfillRecipientEmailDigests(c, newStubBun(), nil, recipientEmailDigestBatch)
			if !errors.Is(err, ErrMigrationDeferred) {
				t.Fatalf("an attempt with no time left must defer, got %v", err)
			}
			if migrationMayStillBeRunning(err) {
				t.Fatalf("the runner would keep its migration lock over a run with nothing in flight: %v", err)
			}
			if report.Stamped != 0 || report.Batches != 0 {
				t.Errorf("a deferred attempt that never started stamped %+v", report)
			}
		})
	}
}
