package sitetraffic

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// edgeTestDB opens the shared test database, or skips (fails under
// MEMQL_REQUIRE_DB=1) when it is unreachable.
func edgeTestDB(t *testing.T) *bun.DB {
	t.Helper()
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		dbtest.Unreachable(t, "the edge visitor secret", dsn, err)
		return nil
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func utcDay(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func TestTwoKeepersAgreeOnTheDaysSecret(t *testing.T) {
	db := edgeTestDB(t)
	ctx := context.Background()
	day := utcDay(2041, 5, 10)
	at := day.Add(13 * time.Hour)
	a := NewVisitorSecrets(quietLogger(), func() *bun.DB { return db }, func() time.Time { return at })
	b := NewVisitorSecrets(quietLogger(), func() *bun.DB { return db }, func() time.Time { return at })
	if err := a.rotate(ctx, day); err != nil {
		t.Fatal(err)
	}
	if err := b.rotate(ctx, day); err != nil {
		t.Fatal(err)
	}
	va := a.Visitor("site", "k", "ua", at)
	if va == "" || va != b.Visitor("site", "k", "ua", at) {
		t.Fatalf("replicas must agree on the visitor: %q vs %q", va, b.Visitor("site", "k", "ua", at))
	}
}

func TestRotationDeletesEarlierDays(t *testing.T) {
	db := edgeTestDB(t)
	ctx := context.Background()
	d1, d2 := utcDay(2042, 5, 10), utcDay(2042, 5, 11)
	v := NewVisitorSecrets(quietLogger(), func() *bun.DB { return db }, time.Now)
	if err := v.rotate(ctx, d1); err != nil {
		t.Fatal(err)
	}
	if err := v.rotate(ctx, d2); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.NewRaw("SELECT count(*) FROM edge_visitor_secret WHERE day = ?", d1).Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("day %v must be deleted at rotation, found %d rows", d1, n)
	}
	if err := db.NewRaw("SELECT count(*) FROM edge_visitor_secret WHERE day = ?", d2).Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the current day must remain, found %d rows", n)
	}
}

func TestARequestFromYesterdayGetsNoVisitorAfterRotation(t *testing.T) {
	db := edgeTestDB(t)
	day := utcDay(2043, 5, 10)
	v := NewVisitorSecrets(quietLogger(), func() *bun.DB { return db }, time.Now)
	if err := v.rotate(context.Background(), day); err != nil {
		t.Fatal(err)
	}
	if got := v.Visitor("s", "k", "ua", day.Add(-time.Millisecond)); got != "" {
		t.Fatalf("a request from the previous day must get no visitor, got %q", got)
	}
	if got := v.Visitor("s", "k", "ua", day.Add(time.Second)); got == "" {
		t.Fatal("a request from the held day must get a visitor")
	}
}
