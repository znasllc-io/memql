package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/uptrace/bun/migrate"
)

// recipient_email_digest_test.go -- the parts of the recipient digest sweep
// (memql#6022, F9) that need no database. The walk itself is in
// recipient_email_digest_db_test.go.

// THE SWEEP IS NOT A MIGRATION. It adds a field the previous release's replicas
// cannot write, and a migration runs in the pre-deploy Job, before they are
// gone: every unsubscribe one of them took in the meantime would fail. The
// campaigns worker runs the sweep after a rollout instead
// (component/campaigns/digest_sweep.go). A cluster that applied the first form of
// this change recorded a migration named 20261011000000; the runner ignores a
// recorded migration it no longer registers, and this test is what keeps it from
// being registered again.
func TestTheRecipientEmailDigestSweepIsNotAMigration(t *testing.T) {
	m := migrate.NewMigrations()
	registerTimescaleMigrations(m, nil)

	for _, mig := range m.Sorted() {
		if mig.Name == "20261011000000" || strings.Contains(mig.Comment, "recipient_email_digest") || strings.Contains(mig.Comment, "recipient-email-digest") {
			t.Errorf("migration %s (%s) stamps recipients before a rollout: the previous release's replicas cannot write a recipient that carries emailDigest, "+
				"so the digest is filled in by the campaigns worker's sweep after one", mig.Name, mig.Comment)
		}
	}
	leftover, err := fs.Glob(timescaleMigrationsFS, "memory-nodes/migrations/*recipient*digest*")
	if err != nil {
		t.Fatal(err)
	}
	if len(leftover) != 0 {
		t.Errorf("the embedded migrations carry %v; the recipient digest is not a migration", leftover)
	}
}

// TestRecipientEmailDigestFoldsCaseAndWhitespaceOnly: the lookup key. The digest
// is the SHA-256 of the address trimmed and lower-cased -- nothing else -- and an
// address that cannot be a mailbox has none. campaigns.EmailDigest is the
// original; component/campaigns holds the two together
// (TestRecipientDigestBackfillMatchesEmailDigest).
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

// TestRecipientEmailStampHashesAnyText: the stamp is what addRecipient writes for
// whatever text it was given, so a recipient the sweep fills in is the recipient
// addRecipient would have written. For an address it is the lookup key; for text
// that is not one it is still the hash of the folded text, which no lookup asks
// for, and it is what takes a scrubbed or malformed recipient out of the rows
// without a digest.
func TestRecipientEmailStampHashesAnyText(t *testing.T) {
	sha := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	for text, folded := range map[string]string{
		"":                     "",
		"   ":                  "",
		"no-at-sign":           "no-at-sign",
		" No-At-Sign\n":        "no-at-sign",
		"a b@c.test":           "a b@c.test",
		"trailing@":            "trailing@",
		" Person@Example.TEST": "person@example.test",
		"ÄBC@x.test":           "äbc@x.test",
	} {
		if got, want := RecipientEmailStamp(text), sha(folded); got != want {
			t.Errorf("RecipientEmailStamp(%q) = %q, want the SHA-256 of %q: %q", text, got, folded, want)
		}
	}
	for _, addr := range []string{"person@example.test", " Person+News@Example.TEST ", "ÄBC@x.test"} {
		if got, want := RecipientEmailDigest(addr), RecipientEmailStamp(addr); got != want || got == "" {
			t.Errorf("an address is looked up by the digest it was stamped with: digest %q, stamp %q for %q", got, want, addr)
		}
	}
	// Two different texts never share a digest, so a stamp of text that is not an
	// address cannot answer for an address.
	if RecipientEmailStamp("person") == RecipientEmailStamp("person@example.test") {
		t.Error("distinct texts share a stamp")
	}
}

// TestTheSweepStopsBetweenStepsWhenCancelled: a pass whose context is already
// cancelled ends before its first statement with an error that wraps the
// context's, which the worker reads as a node shutting down. No database is
// reached: the check comes before the first step.
func TestTheSweepStopsBetweenStepsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	report, err := backfillRecipientEmailDigests(ctx, newStubBun(), nil, recipientEmailDigestBatch)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled pass must end with the context's error, got %v", err)
	}
	if report.Stamped != 0 || report.Batches != 0 {
		t.Errorf("a pass that never started stamped %+v", report)
	}
	if _, err := backfillRecipientEmailDigests(context.Background(), newStubBun(), nil, 0); err == nil {
		t.Error("a batch size of 0 was accepted: the walk would examine nothing and stop at once, reporting a clean pass")
	}
}
