package campaigns

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/accountexport"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

// account_handover_db_test.go -- the email blocks hand-over against the real
// row table.
//
// "MemoryNodes" is shadowed by a TEMP table on one connection, so the fixture
// is the whole world and nothing is left behind. Rows are written as the
// mutations would have stored them: canonical ids, the payload as JSON.
//
// Postgres-gated: CI's db-tests lane runs this package with MEMQL_REQUIRE_DB=1.

func blocksDB(t *testing.T) *bun.DB {
	t.Helper()
	ctx := context.Background()
	reachable, err := dbtest.EnsureSchema(ctx)
	if err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if !reachable {
		dbtest.Unreachable(t, "the email blocks hand-over", dbtest.DSN(), sql.ErrConnDone)
		return nil
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `CREATE TEMP TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`); err != nil {
		t.Fatalf("shadow MemoryNodes: %v", err)
	}
	return db
}

var blocksClock = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

func blocksVersion(t *testing.T, db *bun.DB, concept, id string, minutes int, payload map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(context.Background(),
		`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", schema, payload, concept) VALUES ($1, $2, 'test', '{}', $3, $4)`,
		id, blocksClock.Add(time.Duration(minutes)*time.Minute), string(encoded), concept); err != nil {
		t.Fatal(err)
	}
}

type blocksSink struct {
	lines map[string][]map[string]any
	dirs  []string
}

func (s *blocksSink) Parted(dir string) accountexport.LineSink {
	s.dirs = append(s.dirs, dir)
	return nil
}

func (s *blocksSink) Line(member string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	s.lines[member] = append(s.lines[member], m)
	return nil
}

func runEmailBlocks(t *testing.T, db *bun.DB, carried map[string][]string) *blocksSink {
	t.Helper()
	ctx := context.Background()
	tx, err := db.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	out := &blocksSink{lines: map[string][]map[string]any{}}
	in := accountexport.HandOverInput{Account: "acme", RowIDs: func(concept string) []string { return carried[concept] }}
	if err := accountexport.RunHandOver(ctx, "email-blocks", tx, in, out); err != nil {
		t.Fatalf("email-blocks: %v", err)
	}
	return out
}

func TestEmailBlocksHandOverWritesTheClusterBlocksOfTheAccountsOwnAddresses(t *testing.T) {
	db := blocksDB(t)
	if db == nil {
		return
	}
	const (
		recipient   = "v1:campaigns:recipient"
		signup      = "v1:campaigns:newsletterSignup"
		suppression = "v1:campaigns:suppression"
	)
	blockedEverywhere := EmailDigest("bounced@example.com")
	blockedElsewhere := EmailDigest("left-globex@example.com")
	noDigestYet := EmailDigest("Old.Row@Example.com")
	lifted := EmailDigest("lifted@example.com")
	fromSignup := EmailDigest("signup@example.com")
	notOurs := EmailDigest("globex-only@example.com")
	fine := EmailDigest("fine@example.com")

	// The account's own addresses.
	blocksVersion(t, db, recipient, recipient+":r1", 1, map[string]any{"accountId": "acme", "email": "bounced@example.com", "emailDigest": blockedEverywhere})
	blocksVersion(t, db, recipient, recipient+":r2", 1, map[string]any{"accountId": "acme", "email": "left-globex@example.com", "emailDigest": blockedElsewhere})
	// Written before the digest field: the address decides.
	blocksVersion(t, db, recipient, recipient+":r3", 1, map[string]any{"accountId": "acme", "email": "Old.Row@Example.com"})
	blocksVersion(t, db, recipient, recipient+":r4", 1, map[string]any{"accountId": "acme", "email": "lifted@example.com", "emailDigest": lifted})
	blocksVersion(t, db, recipient, recipient+":r5", 1, map[string]any{"accountId": "acme", "email": "fine@example.com", "emailDigest": fine})
	blocksVersion(t, db, signup, signup+":s1", 1, map[string]any{"accountId": "acme", "emailDigest": fromSignup})
	// Another account's recipient, not in the file.
	blocksVersion(t, db, recipient, recipient+":g1", 1, map[string]any{"accountId": "globex", "email": "globex-only@example.com", "emailDigest": notOurs})

	// Cluster-wide blocks: no account, the digest as the id.
	blocksVersion(t, db, suppression, suppression+":"+blockedEverywhere, 1, map[string]any{"reason": "hard_bounce", "domain": "example.com", "suppressedAt": "2026-11-01T10:00:00Z"})
	blocksVersion(t, db, suppression, suppression+":"+noDigestYet, 1, map[string]any{"reason": "complaint", "domain": "example.com", "suppressedAt": "2026-11-02T10:00:00Z"})
	blocksVersion(t, db, suppression, suppression+":"+notOurs, 1, map[string]any{"reason": "manual", "suppressedAt": "2026-11-03T10:00:00Z"})
	blocksVersion(t, db, suppression, suppression+":"+fromSignup, 1, map[string]any{"reason": "unsubscribed", "suppressedAt": "2026-11-04T10:00:00Z"})
	// Its newest version lifts it: no longer a block. That version spells
	// the id bare, and is still the same row's newest.
	blocksVersion(t, db, suppression, suppression+":"+lifted, 1, map[string]any{"reason": "unsubscribed", "suppressedAt": "2026-11-05T10:00:00Z"})
	blocksVersion(t, db, suppression, lifted, 2, map[string]any{"reason": "unsubscribed", "suppressedAt": "2026-11-05T10:00:00Z", "liftedAt": "2026-11-06T10:00:00Z"})
	// Blocked by another account only.
	blocksVersion(t, db, suppression, suppression+":"+organizationSuppressionID("globex", blockedElsewhere), 1,
		map[string]any{"accountId": "globex", "emailDigest": blockedElsewhere, "reason": "unsubscribed", "suppressedAt": "2026-11-07T10:00:00Z"})

	out := runEmailBlocks(t, db, map[string][]string{
		recipient: {"r1", "r2", "r3", "r4", "r5"},
		signup:    {"s1"},
	})

	got := out.lines[accountexport.MemberBlocks]
	want := []map[string]any{
		{"emailDigest": blockedEverywhere, "reason": "hard_bounce", "blockedAt": "2026-11-01T10:00:00Z", "source": "cluster"},
		{"emailDigest": noDigestYet, "reason": "complaint", "blockedAt": "2026-11-02T10:00:00Z", "source": "cluster"},
		{"emailDigest": fromSignup, "reason": "unsubscribed", "blockedAt": "2026-11-04T10:00:00Z", "source": "cluster"},
	}
	sortBlocks(want)
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	if string(a) != string(b) {
		t.Fatalf("blocks.jsonl:\n got %s\nwant %s", a, b)
	}
	for member := range out.lines {
		if member != accountexport.MemberBlocks {
			t.Fatalf("wrote %s", member)
		}
	}
	if len(out.dirs) != 0 {
		t.Fatalf("opened folders %v", out.dirs)
	}
	if strings.Contains(string(a), "@") {
		t.Fatalf("an address in the clear: %s", a)
	}
}

func TestEmailBlocksHandOverWithNoAddressesWritesNothing(t *testing.T) {
	db := blocksDB(t)
	if db == nil {
		return
	}
	d := EmailDigest("someone@example.com")
	blocksVersion(t, db, "v1:campaigns:suppression", "v1:campaigns:suppression:"+d, 1, map[string]any{"reason": "manual", "suppressedAt": "2026-11-01T10:00:00Z"})
	out := runEmailBlocks(t, db, nil)
	if len(out.lines) != 0 {
		t.Fatalf("an account with no addresses wrote %v", out.lines)
	}
}

func sortBlocks(lines []map[string]any) {
	for i := 1; i < len(lines); i++ {
		for j := i; j > 0 && lines[j]["emailDigest"].(string) < lines[j-1]["emailDigest"].(string); j-- {
			lines[j], lines[j-1] = lines[j-1], lines[j]
		}
	}
}
